PREFIX ?= $(HOME)/.local
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build install test test-race vet check live-test clean

build:
	mkdir -p bin
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/mohuddle ./cmd/mohuddle

install: build
	install -d "$(DESTDIR)$(PREFIX)/bin"
	install -m 0755 bin/mohuddle "$(DESTDIR)$(PREFIX)/bin/mohuddle"

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

check: test test-race vet

live-test:
	MOHUDDLE_LIVE=1 go test -v ./internal/integration -run TestLiveCodingAgentsShareWorkspace

clean:
	rm -rf bin coverage.out
