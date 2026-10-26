# All targets run in Docker; no Go toolchain is needed on the host.
export HOST_UID := $(shell id -u)
export HOST_GID := $(shell id -g)
DC := docker compose
# Stamped into the binary (tack version): the VERSION file, or git describe between tags.
export VERSION ?= $(or $(shell git describe --tags --match "v*" --dirty 2>/dev/null | sed "s/^v//"),$(shell cat VERSION)-dev)
export BUILD_DATE ?= $(shell date -u +%Y-%m-%d)
PREFIX ?= $(HOME)/.local

.PHONY: test-contrib check-version build build-arm build-armv7 build-arm64 build-darwin-amd64 build-darwin-arm64 build-windows-amd64 release \
	install install-backends test-install clean test race check tidy shell fmt vet
build:
	$(DC) run --rm build
build-arm:   # Raspberry Pi Zero W / 1
	GOOS=linux GOARCH=arm GOARM=6 OUT=bin/tack-linux-armv6 $(DC) run --rm build
build-armv7: # Raspberry Pi 2 / 3 (32-bit OS)
	GOOS=linux GOARCH=arm GOARM=7 OUT=bin/tack-linux-armv7 $(DC) run --rm build
build-arm64:
	GOOS=linux GOARCH=arm64 OUT=bin/tack-linux-arm64 $(DC) run --rm build
build-darwin-amd64:
	GOOS=darwin GOARCH=amd64 OUT=bin/tack-darwin-amd64 $(DC) run --rm build
build-darwin-arm64:
	GOOS=darwin GOARCH=arm64 OUT=bin/tack-darwin-arm64 $(DC) run --rm build
build-windows-amd64:
	GOOS=windows GOARCH=amd64 OUT=bin/tack-windows-amd64.exe $(DC) run --rm build
# Every binary plus bin/SHA256SUMS, from an empty bin/ so a stale file can never be hashed or published.
# The backend scripts (contrib/backends/*/tack-*.sh) are release assets too, checksummed like the binaries;
# SHA256SUMS lists exactly the files built here.
RELEASE_BINARIES = tack tack-linux-armv6 tack-linux-armv7 tack-linux-arm64 tack-darwin-amd64 tack-darwin-arm64 tack-windows-amd64.exe
release:
	$(MAKE) clean
	$(MAKE) build build-arm build-armv7 build-arm64 build-darwin-amd64 build-darwin-arm64 build-windows-amd64
	cp contrib/backends/*/tack-*.sh bin/
	cd bin && sha256sum $(RELEASE_BINARIES) $(notdir $(wildcard contrib/backends/*/tack-*.sh)) > SHA256SUMS
# Copy the host-arch binary to $(PREFIX)/bin (default ~/.local/bin): make install [PREFIX=/usr/local]
install: build
	install -d "$(PREFIX)/bin"
	install -m 0755 bin/tack "$(PREFIX)/bin/tack"
# Copy the backend scripts to $(PREFIX)/share/tack/backends: make install-backends [PREFIX=/usr/local]
install-backends:
	install -d "$(PREFIX)/share/tack/backends"
	install -m 0755 contrib/backends/*/tack-*.sh "$(PREFIX)/share/tack/backends/"
clean:
	rm -rf bin
test:
	$(DC) run --rm test
# The contrib backend scripts (bash + jq + sqlite3), alone and driven through the built tack.
test-contrib: build
	$(DC) run --rm dev sh -c 'cd /app && contrib/backends/sqlite/test.sh /app/bin/tack'
# Read-only gate: gofmt must not want changes, then vet and the tests.
check: check-version
	$(DC) run --rm dev sh -c 'out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi'
	$(DC) run --rm dev go vet ./...
	$(DC) run --rm test
	$(MAKE) test-contrib
	$(MAKE) test-install
# Every place that names the release must agree with the VERSION file, and the Dockerfile's Go
# must be the toolchain go.mod asks for (CI on Windows takes it from go.mod).
check-version:
	@v="$$(tr -d '[:space:]' < VERSION)"; \
	test "$$(sed -n 's/^VERSION="\([^"]*\)".*/\1/p' install.sh)" = "$$v" || { echo "install.sh VERSION does not match the VERSION file ($$v)"; exit 1; }; \
	stale="$$(grep -on '/v[0-9]\+\.[0-9]\+\.[0-9]\+/' README.md | grep -v "/v$$v/" || true)"; \
	test -z "$$stale" || { echo "README.md install URLs are not v$$v:"; echo "$$stale"; exit 1; }; \
	grep -q "^## \[$$v\]" CHANGELOG.md || { echo "CHANGELOG.md has no '## [$$v]' heading"; exit 1; }; \
	d="$$(sed -n 's/^ARG GO_VERSION=//p' Dockerfile)"; g="$$(sed -n 's/^toolchain go//p' src/go.mod)"; \
	test "$$d" = "$$g" || { echo "Dockerfile GO_VERSION ($$d) does not match the toolchain line of src/go.mod ($$g)"; exit 1; }
# The installer against a fake release tree (no network).
test-install:
	$(DC) run --rm dev sh -c 'cd /app && sh contrib/install-test.sh'
# The race detector needs cgo: it runs in its own image with a C compiler.
race:
	$(DC) run --rm race
tidy:
	$(DC) run --rm dev go mod tidy
fmt:
	$(DC) run --rm dev gofmt -l -w .
vet:
	$(DC) run --rm dev go vet ./...
shell:
	$(DC) run --rm dev
