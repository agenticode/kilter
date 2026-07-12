VERSION ?= $(shell command git describe --tags --always --dirty 2>/dev/null || git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS  = -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
IMAGE   ?= ghcr.io/agenticode/kilter

.PHONY: all build test race bench e2e lint docker docker-multiarch clean

all: lint test build

build: ## static binary for the host platform
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/kilter ./cmd/kilter

build-all: ## release binaries
	@for os in linux darwin; do for arch in amd64 arm64; do \
		echo "  $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/kilter-$$os-$$arch ./cmd/kilter; \
	done; done

test:
	go test -count=1 ./...

race:
	go test -race -count=1 ./...

bench:
	go test -bench=. -benchmem -run=NONE ./pkg/binpack/ ./pkg/histogram/ ./pkg/recommend/

e2e: build ## full kind e2e (docker + kind + kubectl required)
	./test/e2e/e2e.sh

lint:
	@test -z "$$(gofmt -l . | grep -v '^tools/')" || (echo "gofmt needed:"; gofmt -l . | grep -v '^tools/'; exit 1)
	go vet ./...

docker:
	docker build -t $(IMAGE):$(VERSION) .

docker-multiarch:
	docker buildx build --platform linux/amd64,linux/arm64 -t $(IMAGE):$(VERSION) .

clean:
	rm -rf bin dist
