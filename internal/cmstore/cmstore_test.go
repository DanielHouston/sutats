package cmstore

import (
	"bytes"
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	for _, compress := range []bool{false, true} {
		client := fake.NewSimpleClientset()
		s := New(client, "sutats", "sutats-history", "history.json", compress)
		ctx := context.Background()

		if b, err := s.Load(ctx); err != nil || b != nil {
			t.Fatalf("compress=%v: empty load = %v, %v", compress, b, err)
		}
		blob := bytes.Repeat([]byte(`{"a":1}`), 1000)
		if err := s.Save(ctx, blob); err != nil {
			t.Fatalf("compress=%v: first save: %v", compress, err)
		}
		if err := s.Save(ctx, append(blob, '!')); err != nil {
			t.Fatalf("compress=%v: second save (update): %v", compress, err)
		}
		got, err := s.Load(ctx)
		if err != nil || !bytes.Equal(got, append(blob, '!')) {
			t.Fatalf("compress=%v: load = %d bytes, %v", compress, len(got), err)
		}
		cm, _ := client.CoreV1().ConfigMaps("sutats").Get(ctx, "sutats-history", metav1.GetOptions{})
		if compress {
			if len(cm.BinaryData["history.json.gz"]) == 0 || len(cm.BinaryData["history.json.gz"]) >= len(blob) {
				t.Fatalf("compressed payload should be in binaryData and smaller, got %d", len(cm.BinaryData["history.json.gz"]))
			}
		} else if cm.Data["history.json"] == "" {
			t.Fatal("plain payload should be in data")
		}
	}
}

func TestSaveRejectsOversize(t *testing.T) {
	s := New(fake.NewSimpleClientset(), "sutats", "x", "k", false)
	if err := s.Save(context.Background(), make([]byte, 950*1024)); err == nil {
		t.Fatal("expected an error near the ConfigMap limit")
	}
}

func TestWatchDeliversSaves(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := New(client, "sutats", "sutats-live", "live.json", false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan []byte, 4)
	go s.Watch(ctx, func(b []byte) { got <- b })
	time.Sleep(50 * time.Millisecond)

	if err := s.Save(ctx, []byte("one")); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-got:
		if string(b) != "one" {
			t.Fatalf("got %q", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not deliver the create")
	}
	if err := s.Save(ctx, []byte("two")); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-got:
		if string(b) != "two" {
			t.Fatalf("got %q", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not deliver the update")
	}
}
