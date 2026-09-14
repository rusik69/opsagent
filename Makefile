BINARY := opsagent
GO := go

.PHONY: build run test vet clean fmt

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

clean:
	rm -rf bin data