# All targets run in Docker; no Go toolchain is needed on the host.
export HOST_UID := $(shell id -u)
export HOST_GID := $(shell id -g)
DC := docker compose
# Stamped into the binary (tkan version): the VERSION file, or git describe between tags.
export VERSION ?= $(or $(shell git describe --tags --match "v*" --dirty 2>/dev/null | sed "s/^v//"),$(shell cat VERSION)-dev)
export BUILD_DATE ?= $(shell date -u +%Y-%m-%d)
PREFIX ?= $(HOME)/.local

.PHONY: test-contrib check-version build build-arm build-armv7 build-arm64 build-darwin-amd64 build-darwin-arm64 build-windows-amd64 release \
	install install-backends test-install clean test race check tidy shell fmt vet
build:
	$(DC) run --rm build
build-arm:   # Raspberry Pi Zero W / 1
	GOOS=linux GOARCH=arm GOARM=6 OUT=bin/tkan-linux-armv6 $(DC) run --rm build
build-armv7: # Raspberry Pi 2 / 3 (32-bit OS)
	GOOS=linux GOARCH=arm GOARM=7 OUT=bin/tkan-linux-armv7 $(DC) run --rm build
build-arm64:
	GOOS=linux GOARCH=arm64 OUT=bin/tkan-linux-arm64 $(DC) run --rm build
build-darwin-amd64:
	GOOS=darwin GOARCH=amd64 OUT=bin/tkan-darwin-amd64 $(DC) run --rm build
build-darwin-arm64:
	GOOS=darwin GOARCH=arm64 OUT=bin/tkan-darwin-arm64 $(DC) run --rm build
build-windows-amd64:
	GOOS=windows GOARCH=amd64 OUT=bin/tkan-windows-amd64.exe $(DC) run --rm build
# Every binary plus bin/SHA256SUMS.
# The backend scripts (contrib/backends/*/tkan-*.sh) are release assets too, checksummed like the binaries.
release: build build-arm build-armv7 build-arm64 build-darwin-amd64 build-darwin-arm64 build-windows-amd64
	cp contrib/backends/*/tkan-*.sh bin/
	cd bin && sha256sum tkan tkan-* > SHA256SUMS
# Copy the host-arch binary to $(PREFIX)/bin (default ~/.local/bin): make install [PREFIX=/usr/local]
install: build
	install -d "$(PREFIX)/bin"
	install -m 0755 bin/tkan "$(PREFIX)/bin/tkan"
# Copy the backend scripts to $(PREFIX)/share/tkan/backends: make install-backends [PREFIX=/usr/local]
install-backends:
	install -d "$(PREFIX)/share/tkan/backends"
	install -m 0755 contrib/backends/*/tkan-*.sh "$(PREFIX)/share/tkan/backends/"
clean:
	rm -rf bin
test:
	$(DC) run --rm test
# The contrib backend scripts (bash + jq + sqlite3), alone and driven through the built tkan.
test-contrib: build
	$(DC) run --rm dev sh -c 'cd /app && contrib/backends/sqlite/test.sh /app/bin/tkan'
# Read-only gate: gofmt must not want changes, then vet and the tests.
check: check-version
	$(DC) run --rm dev sh -c 'out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi'
	$(DC) run --rm dev go vet ./...
	$(DC) run --rm test
	$(MAKE) test-contrib
	$(MAKE) test-install
# Every place that names the release must agree with the VERSION file.
check-version:
	@v="$$(tr -d '[:space:]' < VERSION)"; \
	test "$$(sed -n 's/^VERSION="\([^"]*\)".*/\1/p' install.sh)" = "$$v" || { echo "install.sh VERSION does not match the VERSION file ($$v)"; exit 1; }; \
	stale="$$(grep -on '/v[0-9]\+\.[0-9]\+\.[0-9]\+/' README.md | grep -v "/v$$v/" || true)"; \
	test -z "$$stale" || { echo "README.md install URLs are not v$$v:"; echo "$$stale"; exit 1; }; \
	grep -q "^## \[$$v\]" CHANGELOG.md || { echo "CHANGELOG.md has no '## [$$v]' heading"; exit 1; }
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
