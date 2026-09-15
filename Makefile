BINARY := opsagent
GO := go
IMAGE := opsagent:dev
KIND_CLUSTER := opsagent-demo

.PHONY: build run test vet fmt clean lint check
.PHONY: docker-build k8s-demo k8s-down

build:
	$(GO) build -o bin/$(BINARY) ./cmd/opsagent

run: build
	./bin/$(BINARY) -config config.yaml

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

# Format + vet + full race suite (the "are you done?" gate).
check:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	$(GO) vet ./...
	$(GO) build ./...
	$(GO) test -race ./...

lint: vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

clean:
	rm -rf bin data

docker-build:
	docker build -t $(IMAGE) .

# k8s-demo builds the image, loads it into a kind cluster (creating one if
# needed), deploys opsagent with the test repos and a test SSH host, seeds
# alerts and runs a diagnosis end-to-end.
k8s-demo: docker-build
	@command -v kubectl >/dev/null || (echo "kubectl is required"; exit 1)
	@command -v kind >/dev/null || (echo "kind is required"; exit 1)
	kind get clusters 2>/dev/null | grep -q '^$(KIND_CLUSTER)$$' || kind create cluster --name $(KIND_CLUSTER)
	kind load docker-image $(IMAGE) --name $(KIND_CLUSTER)
	kubectl apply -f deploy/k8s
	kubectl rollout status deploy/opsagent --timeout=120s
	kubectl rollout status deploy/test-host --timeout=120s
	kubectl port-forward -n opsagent svc/opsagent 8080:8080 >/tmp/opsagent-pf.log 2>&1 & echo $$! > /tmp/opsagent-pf.pid
	sleep 3
	./deploy/demo/seed.sh
	@echo "dashboard: http://localhost:8080  (stop: make k8s-down)"

k8s-down:
	@[ -f /tmp/opsagent-pf.pid ] && kill $$(cat /tmp/opsagent-pf.pid) 2>/dev/null; rm -f /tmp/opsagent-pf.pid || true
	@command -v kind >/dev/null && kind delete cluster --name $(KIND_CLUSTER) || true
