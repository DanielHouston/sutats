// Package cmstore keeps a blob in a Kubernetes ConfigMap so every replica
// can read it and one (the leader) can write it. It is the persistence
// layer for uptime history and live probe results when running in-cluster.
package cmstore

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// Store is one ConfigMap holding one blob under one key.
type Store struct {
	client    kubernetes.Interface
	namespace string
	name      string
	key       string
	compress  bool
}

// New builds a Store. With compress the blob is gzipped and kept in
// binaryData, which lets far more history fit under the 1 MiB limit.
func New(client kubernetes.Interface, namespace, name, key string, compress bool) *Store {
	if compress {
		key += ".gz"
	}
	return &Store{client: client, namespace: namespace, name: name, key: key, compress: compress}
}

// Load returns the blob, or nil if the ConfigMap or key does not exist.
func (s *Store) Load(ctx context.Context) ([]byte, error) {
	cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading configmap %s/%s: %w", s.namespace, s.name, err)
	}
	return s.extract(cm)
}

func (s *Store) extract(cm *corev1.ConfigMap) ([]byte, error) {
	var raw []byte
	if s.compress {
		raw = cm.BinaryData[s.key]
	} else {
		raw = []byte(cm.Data[s.key])
	}
	if len(raw) == 0 {
		return nil, nil
	}
	if !s.compress {
		return raw, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decompressing %s: %w", s.key, err)
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// Save writes the blob, creating the ConfigMap if needed. Conflicts are
// retried a few times; callers run this on a timer, so a lost round is fine.
func (s *Store) Save(ctx context.Context, blob []byte) error {
	payload := blob
	if s.compress {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if _, err := zw.Write(blob); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		payload = buf.Bytes()
	}
	if len(payload) > 900*1024 {
		return fmt.Errorf("%s is %d bytes, too close to the ConfigMap limit; reduce retention", s.key, len(payload))
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: s.name, Namespace: s.namespace,
				Labels: map[string]string{"app.kubernetes.io/name": "sutats", "app.kubernetes.io/component": "state"},
			}}
			s.put(cm, payload)
			if _, err := s.client.CoreV1().ConfigMaps(s.namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("creating configmap %s/%s: %w", s.namespace, s.name, err)
			} else if err == nil {
				return nil
			}
			lastErr = err
		case err != nil:
			return fmt.Errorf("reading configmap %s/%s: %w", s.namespace, s.name, err)
		default:
			s.put(cm, payload)
			_, err := s.client.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
			if err == nil {
				return nil
			}
			if !apierrors.IsConflict(err) {
				return fmt.Errorf("updating configmap %s/%s: %w", s.namespace, s.name, err)
			}
			lastErr = err
		}
		time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
	}
	return fmt.Errorf("saving configmap %s/%s: %w", s.namespace, s.name, lastErr)
}

func (s *Store) put(cm *corev1.ConfigMap, payload []byte) {
	if s.compress {
		if cm.BinaryData == nil {
			cm.BinaryData = map[string][]byte{}
		}
		cm.BinaryData[s.key] = payload
	} else {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[s.key] = string(payload)
	}
}

// Watch calls fn with the blob whenever the ConfigMap changes, including
// once at start if it exists. It blocks until ctx is done. Followers use
// this to mirror what the leader writes.
func (s *Store) Watch(ctx context.Context, fn func([]byte)) {
	f := informers.NewSharedInformerFactoryWithOptions(s.client, 10*time.Minute,
		informers.WithNamespace(s.namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = fields.OneTermEqualSelector("metadata.name", s.name).String()
		}),
	)
	handle := func(obj interface{}) {
		cm, ok := obj.(*corev1.ConfigMap)
		if !ok {
			return
		}
		blob, err := s.extract(cm)
		if err != nil {
			slog.Warn("ignoring unreadable configmap", "name", s.name, "err", err)
			return
		}
		if blob != nil {
			fn(blob)
		}
	}
	inf := f.Core().V1().ConfigMaps().Informer()
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: handle,
		UpdateFunc: func(old, obj interface{}) {
			o, _ := old.(*corev1.ConfigMap)
			n, _ := obj.(*corev1.ConfigMap)
			if o != nil && n != nil && n.ResourceVersion != "" && o.ResourceVersion == n.ResourceVersion {
				return // resync, nothing changed
			}
			handle(obj)
		},
	})
	f.Start(ctx.Done())
	f.WaitForCacheSync(ctx.Done())
	<-ctx.Done()
}
