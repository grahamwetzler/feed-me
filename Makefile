VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test lint vet validate
build:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o bin/rss-er ./cmd/rss-er

test:
	go test ./...

vet:
	go vet ./...

lint: build
	./bin/rss-er config lint

# Posts each rendered feed to the W3C Feed Validation Service. Needs a store
# populated by `rss-er build`. Run it locally or in CI, not on every commit.
validate: build
	./bin/rss-er validate --w3c
