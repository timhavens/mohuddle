PREFIX ?= $(HOME)/.local
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/timhavens/mohuddle/internal/buildinfo.Version=$(VERSION)

.PHONY: build install package package-dry-run package-validate test test-race vet security check live-test clean

# Never overwrite a running executable. On WSL even a rename can leave an ELF
# mapping stale through another spelling of a Windows path. Keep each compiled
# image at a fresh path and publish a text launcher there instead.
build:
	mkdir -p bin
	@set -eu; \
	build_dir=$$(mktemp -d bin/.mohuddle-build.XXXXXX); \
	keep_build=no; \
	trap '[ "$$keep_build" = yes ] || rmdir "$$build_dir" 2>/dev/null || true' EXIT; \
	go build -trimpath -ldflags="$(LDFLAGS)" -o "$$build_dir/mohuddle" ./cmd/mohuddle; \
	case "$$(uname -r)" in \
	  *[Mm]icrosoft*) \
	    printf '%s\n' "$$build_dir/mohuddle" > "$$build_dir/install-source"; \
	    mv -f "$$build_dir/install-source" bin/.mohuddle-install-source; \
	    keep_build=yes; \
	    if ! cmp -s scripts/mohuddle-launcher.sh bin/mohuddle; then \
	      install -m 0755 scripts/mohuddle-launcher.sh "$$build_dir/launcher"; \
	      mv -f "$$build_dir/launcher" bin/mohuddle; \
	    fi; \
	    ;; \
	  *) \
	    mv -f "$$build_dir/mohuddle" bin/mohuddle; \
	    printf '%s\n' bin/mohuddle > "$$build_dir/install-source"; \
	    mv -f "$$build_dir/install-source" bin/.mohuddle-install-source; \
	    ;; \
	esac; \
	./bin/mohuddle --version

install: build
	install -d "$(DESTDIR)$(PREFIX)/bin"
	@set -eu; \
	install_dir=$$(mktemp -d "$(DESTDIR)$(PREFIX)/bin/.mohuddle-install.XXXXXX"); \
	trap 'rmdir "$$install_dir" 2>/dev/null || true' EXIT; \
	install -m 0755 "$$(cat bin/.mohuddle-install-source)" "$$install_dir/mohuddle"; \
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

security:
	bash ./scripts/security-check.sh

check: test test-race vet security

live-test:
	MOHUDDLE_LIVE=1 go test -v ./internal/integration -run TestLiveCodingAgentsShareWorkspace

clean:
	rm -rf bin coverage.out
