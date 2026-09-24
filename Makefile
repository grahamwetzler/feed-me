VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test lint vet validate docker
build:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o bin/feed-me ./cmd/feed-me

test:
	go test ./...

vet:
	go vet ./...

lint: build
	./bin/feed-me config lint

# Posts each rendered feed to the W3C Feed Validation Service. Needs a store
# populated by `feed-me build`. Run it locally or in CI, not on every commit.
validate: build
	./bin/feed-me validate --w3c

# The container image (§8.1). Run it with `docker compose up -d`.
docker:
	docker build --build-arg VERSION=$(VERSION) -t feed-me:latest .
