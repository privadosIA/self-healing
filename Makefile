VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w

# Pinned so local runs and CI behave the same.
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.2
# The same linter set as `golangci/golangci-lint` with no configuration, which is
# what the private CI runs: errcheck, govet, ineffassign, staticcheck, unused.
GOLANGCILINT := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.9.0
GOSEC       := github.com/securego/gosec/v2/cmd/gosec@v2.30.0

.PHONY: build test lint vuln docker

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/healerd ./cmd/healerd

test:
	go test ./... -race -coverprofile=coverage.out

lint:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed on:"; gofmt -l .; exit 1; }
	go vet ./...
	go run $(STATICCHECK) ./...
	go run $(GOLANGCILINT) run --timeout 5m

vuln:
	go run $(GOVULNCHECK) ./...
	go run $(GOSEC) -quiet ./...

docker:
	docker build --build-arg VERSION=$(VERSION) -t healerd:$(VERSION) .
