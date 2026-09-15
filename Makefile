PREFIX ?= $(HOME)/.local
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/timhavens/mohuddle/internal/buildinfo.Version=$(VERSION)

.PHONY: build install package package-dry-run package-validate test test-race vet check live-test clean

# Replace a complete executable on the same filesystem; never overwrite a running
# binary or leave stale executable mappings through WSL's case-insensitive paths.
build:
	mkdir -p bin
	@set -eu; \
	build_dir=$$(mktemp -d bin/.mohuddle-build.XXXXXX); \
	trap 'rm -rf "$$build_dir"' EXIT; \
	go build -trimpath -ldflags="$(LDFLAGS)" -o "$$build_dir/mohuddle" ./cmd/mohuddle; \
	mv -f "$$build_dir/mohuddle" bin/mohuddle

install: build
	install -d "$(DESTDIR)$(PREFIX)/bin"
	@set -eu; \
	install_dir=$$(mktemp -d "$(DESTDIR)$(PREFIX)/bin/.mohuddle-install.XXXXXX"); \
	trap 'rm -rf "$$install_dir"' EXIT; \
	install -m 0755 bin/mohuddle "$$install_dir/mohuddle"; \
	mv -f "$$install_dir/mohuddle" "$(DESTDIR)$(PREFIX)/bin/mohuddle"

package:
	./scripts/package-release.sh "$(VERSION)"

package-dry-run:
	./scripts/package-release.sh "$(VERSION)" --dry-run

package-validate:
	./scripts/package-release.sh "$(VERSION)" --validate

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
