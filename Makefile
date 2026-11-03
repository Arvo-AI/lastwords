VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/arvo-ai/lastwords
LDFLAGS := -s -w -X github.com/Arvo-AI/lastwords/internal/agent.Version=$(VERSION)

.PHONY: build test lint image e2e clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/lastwords ./cmd/lastwords
	ln -sf lastwords dist/kubectl-lastwords

test:
	go test ./...

lint:
	go vet ./...
	helm lint deploy/helm/lastwords

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

e2e:
	./test/e2e/kind.sh

clean:
	rm -rf dist
