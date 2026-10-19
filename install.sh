#!/bin/sh
# tkan installer: downloads one pinned release for this OS/arch, verifies its
# checksum and installs it. Fetch it from the tag you want, so the script and
# the binary have the same version:
#   curl -fsSL https://raw.githubusercontent.com/Desvelao/tkan/v0.2.0/install.sh | sh
#
# Usage: install.sh [-v VERSION] [-d DIR]
#   -v VERSION  release to install, X.Y.Z (default: the version of this script,
#               or $TKAN_VERSION). "latest" is deliberately not supported.
#   -d DIR      install directory (default: $TKAN_INSTALL_DIR or ~/.local/bin)
set -eu

VERSION="0.2.0" # keep in sync with the VERSION file (checked by make check and CI)
REPO="Desvelao/tkan"

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

version="${TKAN_VERSION:-$VERSION}"
dir="${TKAN_INSTALL_DIR:-$HOME/.local/bin}"
while [ $# -gt 0 ]; do
	case "$1" in
	-v) [ $# -ge 2 ] || die "-v needs a version"; version="$2"; shift 2 ;;
	-d) [ $# -ge 2 ] || die "-d needs a directory"; dir="$2"; shift 2 ;;
	-h | --help) sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) die "unknown argument: $1 (see -h)" ;;
	esac
done

version="${version#v}"
printf '%s' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$' ||
	die "'$version' is not a release version (X.Y.Z); installing \"latest\" is not supported"

os=$(uname -s)
arch=$(uname -m)
case "$os/$arch" in
Linux/x86_64 | Linux/amd64) asset=tkan ;;
Linux/aarch64 | Linux/arm64) asset=tkan-linux-arm64 ;;
Linux/armv7l | Linux/armv8l) asset=tkan-linux-armv7 ;;
Linux/armv6l) asset=tkan-linux-armv6 ;;
Darwin/x86_64) asset=tkan-darwin-amd64 ;;
Darwin/arm64) asset=tkan-darwin-arm64 ;;
*) die "no prebuilt binary for $os/$arch; see https://github.com/$REPO/releases/tag/v$version" ;;
esac

base="${TKAN_BASE_URL:-https://github.com/$REPO/releases/download}/v$version"
if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -qO "$2" "$1"; }
else
	die "curl or wget is required"
fi
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	die "sha256sum or shasum is required"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Installing tkan $version ($asset) into $dir"
fetch "$base/$asset" "$tmp/$asset" || die "download failed: $base/$asset"
fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" || die "download failed: $base/SHA256SUMS"

want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp/SHA256SUMS")
[ -n "$want" ] || die "$asset is not listed in SHA256SUMS"
[ "$(sha256 "$tmp/$asset")" = "$want" ] || die "checksum mismatch for $asset; not installing"

mkdir -p "$dir" 2>/dev/null || true
[ -w "$dir" ] || die "cannot write to $dir; use -d DIR, or: sudo sh install.sh -d $dir"
chmod 0755 "$tmp/$asset"
mv "$tmp/$asset" "$dir/tkan" 2>/dev/null || cp "$tmp/$asset" "$dir/tkan" || die "cannot install into $dir"

say "Installed: $("$dir/tkan" version)"
case ":$PATH:" in
*":$dir:"*) ;;
*) say "Note: $dir is not on your PATH. Add it with:"
	say "  export PATH=\"$dir:\$PATH\"" ;;
esac
