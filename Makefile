# All targets run in Docker; no Go toolchain is needed on the host.
export HOST_UID := $(shell id -u)
export HOST_GID := $(shell id -g)
DC := docker compose
# Stamped into the binary (tkan version): the VERSION file, or git describe between tags.
export VERSION ?= $(or $(shell git describe --tags --match "v*" --dirty 2>/dev/null | sed "s/^v//"),$(shell cat VERSION)-dev)
export BUILD_DATE ?= $(shell date -u +%Y-%m-%d)
PREFIX ?= $(HOME)/.local

.PHONY: build build-arm build-armv7 build-arm64 build-darwin-amd64 build-darwin-arm64 build-windows-amd64 release \
	install clean test check tidy shell fmt vet
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
release: build build-arm build-armv7 build-arm64 build-darwin-amd64 build-darwin-arm64 build-windows-amd64
	cd bin && sha256sum tkan tkan-* > SHA256SUMS
# Copy the host-arch binary to $(PREFIX)/bin (default ~/.local/bin): make install [PREFIX=/usr/local]
install: build
	install -d "$(PREFIX)/bin"
	install -m 0755 bin/tkan "$(PREFIX)/bin/tkan"
clean:
	rm -rf bin
test:
	$(DC) run --rm test
# Read-only gate: gofmt must not want changes, then vet and the tests.
check:
	@test "$$(sed -n 's/^VERSION="\([^"]*\)".*/\1/p' install.sh)" = "$$(tr -d '[:space:]' < VERSION)" || { echo "install.sh VERSION does not match the VERSION file"; exit 1; }
	$(DC) run --rm dev sh -c 'out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi'
	$(DC) run --rm dev go vet ./...
	$(DC) run --rm test
tidy:
	$(DC) run --rm dev go mod tidy
fmt:
	$(DC) run --rm dev gofmt -l -w .
vet:
	$(DC) run --rm dev go vet ./...
shell:
	$(DC) run --rm dev
