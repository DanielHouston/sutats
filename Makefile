IMAGE ?= ghcr.io/danielhouston/sutats
TAG   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build run mock test lint image push deploy

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(TAG)" -o bin/sutats ./cmd/sutats

## Run against the cluster in your current kubeconfig context.
run: build
	./bin/sutats --data-dir ./data --incidents-file examples/incidents.yaml $(ARGS)

## Run with fake data, no cluster needed.
mock: build
	./bin/sutats --mock --data-dir "" --title "Demo" $(ARGS)

test:
	go test ./...

## Start a throwaway TimescaleDB in Docker and run the database tests too.
TSDB_URL ?= postgres://postgres:sutats@127.0.0.1:5433/sutats_test?sslmode=disable
test-db:
	docker inspect sutats-tsdb >/dev/null 2>&1 || docker run -d --name sutats-tsdb --restart unless-stopped \
	  -e POSTGRES_PASSWORD=sutats -e POSTGRES_DB=sutats -p 127.0.0.1:5433:5432 timescale/timescaledb:latest-pg16
	@until docker exec sutats-tsdb pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
	@docker exec sutats-tsdb psql -U postgres -tAc "select 1 from pg_database where datname='sutats_test'" | grep -q 1 || \
	  docker exec sutats-tsdb psql -U postgres -qc "create database sutats_test"
	SUTATS_TEST_DATABASE_URL=$(TSDB_URL) go test ./...

lint:
	go vet ./...

image:
	docker build --build-arg VERSION=$(TAG) -t $(IMAGE):$(TAG) -t $(IMAGE):latest .

push: image
	docker push $(IMAGE):$(TAG)
	docker push $(IMAGE):latest

deploy:
	kubectl apply -k deploy/base

## OpenShift: build with podman, push to the cluster's internal registry and
## apply the overlay. Needs `oc login` first (works as-is on CRC).
OCP_REGISTRY ?= $(shell oc get route default-route -n openshift-image-registry -o jsonpath='{.spec.host}' 2>/dev/null)
OCP_IMAGE    ?= $(OCP_REGISTRY)/sutats/sutats:dev
openshift-image:
	oc get ns sutats >/dev/null 2>&1 || oc create ns sutats
	podman build -t $(OCP_IMAGE) .
	oc registry login --insecure=true --skip-check
	podman push --tls-verify=false $(OCP_IMAGE)

openshift-deploy:
	oc apply -k deploy/openshift
	oc -n sutats rollout status deploy/sutats --timeout=180s

openshift-demo:
	oc apply -f examples/demo-cluster.yaml
