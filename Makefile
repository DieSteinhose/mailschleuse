# Convenience targets. Everything here is plain Go and Docker underneath.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/diesteinhose/mailschleuse

.PHONY: help build test lint run docker docker-run clean

help: ## Show this help
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-12s %s\n", $$1, $$2}'

build: ## Build the binary into ./mailschleuse
	CGO_ENABLED=0 go build -trimpath -tags timetzdata \
		-ldflags "-s -w -X main.version=$(VERSION)" -o mailschleuse ./cmd/mailschleuse

test: ## Run the test suite with the race detector
	go test -race ./...

lint: ## Check formatting and run go vet
	@test -z "$$(gofmt -l .)" || (echo "not gofmt-clean:"; gofmt -l .; exit 1)
	go vet ./...

run: ## Run locally on the default ports
	go run ./cmd/mailschleuse

docker: ## Build the container image
	docker build -t $(IMAGE):$(VERSION) --build-arg VERSION=$(VERSION) .

docker-run: docker ## Build and run the container image
	docker run --rm -p 8080:8080 -p 1025:1025 -p 1110:1110 $(IMAGE):$(VERSION)

clean: ## Remove build output
	rm -rf mailschleuse dist coverage.out
