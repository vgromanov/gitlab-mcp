.PHONY: all prep-dist build build-all dist install clean test test-integration test-all cover race fmt fmt-check vet lint tidy docker run-stdio run-http help

BINARY := gitlab-mcp
PKG := ./cmd/gitlab-mcp
DIST_DIR := dist
OUT_DIR := bin

GOOS_LIST := linux darwin windows
GOARCH_LIST := amd64 arm64

all: fmt vet test build

$(OUT_DIR):
	mkdir -p $(OUT_DIR)

.PHONY: prep-dist
prep-dist:
	mkdir -p $(DIST_DIR)

build: $(OUT_DIR)
	CGO_ENABLED=0 go build -trimpath -o $(OUT_DIR)/$(BINARY) $(PKG)

build-all: prep-dist
	@set -e; \
	for os in $(GOOS_LIST); do \
		for arch in $(GOARCH_LIST); do \
			if [ "$$os" = "windows" ] && [ "$$arch" = "arm64" ]; then \
				continue; \
			fi; \
			out="$(DIST_DIR)/$(BINARY)-$$os-$$arch"; \
			if [ "$$os" = "windows" ]; then out="$$out.exe"; fi; \
			echo "==> $$os/$$arch -> $$out"; \
			GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -o $$out $(PKG); \
		done; \
	done

dist: prep-dist
	@set -e; \
	for os in linux darwin; do \
		for arch in amd64 arm64; do \
			out="$(DIST_DIR)/$(BINARY)-$$os-$$arch"; \
			echo "==> $$os/$$arch -> $$out"; \
			GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -o $$out $(PKG); \
		done; \
	done

test:
	go test ./...

test-integration:
	go test -tags=integration -timeout=10m ./...

test-all: test test-integration

cover:
	go test ./... -coverprofile=coverage.out

race:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "Run 'make fmt' to format files"; gofmt -l .; exit 1)

lint: fmt-check vet

tidy:
	go mod tidy

run-stdio: build
	./$(OUT_DIR)/$(BINARY)

run-http: build
	STREAMABLE_HTTP=true ./$(OUT_DIR)/$(BINARY)

docker:
	docker build -t $(BINARY):local .

install:
	go install $(PKG)

clean:
	rm -rf $(OUT_DIR) $(DIST_DIR) coverage.out

help:
	@echo "Targets: all build build-all dist install clean test test-integration test-all cover race fmt fmt-check vet lint tidy docker run-stdio run-http help"
