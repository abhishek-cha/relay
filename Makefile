GO   ?= go
DIST ?= dist

.PHONY: all build test fmt vet e2e tidy clean

all: fmt vet test build

build:
	$(GO) build -o $(DIST)/relay ./cmd/relay
	$(GO) build -o $(DIST)/relayd ./cmd/relayd

test:
	$(GO) test ./...

fmt:
	gofmt -l -w .

vet:
	$(GO) vet ./...

e2e:
	bash scripts/e2e.sh

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(DIST) build
