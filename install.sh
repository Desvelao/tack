#!/bin/sh
# tkan installer: downloads one pinned release for this OS/arch, verifies its
# checksum and installs it. Fetch it from the tag you want, so the script and
# the binary have the same version:
#   curl -fsSL https://raw.githubusercontent.com/Desvelao/tkan/v0.2.0/install.sh | sh
#
# Usage: install.sh [-v VERSION] [-d DIR] [-b NAMES] [-B DIR] [-s]
#   -v VERSION  release to install, X.Y.Z (default: the version of this script,
#               or $TKAN_VERSION). "latest" is deliberately not supported.
#   -d DIR      install directory (default: $TKAN_INSTALL_DIR or ~/.local/bin)
#   -b NAMES    also install backend scripts of the release: a comma list of
#               names (sqlite) or "all" (default: $TKAN_BACKENDS, none)
#   -B DIR      where the backend scripts go (default: $TKAN_BACKEND_DIR or
#               ~/.local/share/tkan/backends)
#   -s          scripts only: do not install the tkan binary (needs -b)
set -eu

VERSION="0.2.0" # keep in sync with the VERSION file (checked by make check and CI)
REPO="Desvelao/tkan"

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

version="${TKAN_VERSION:-$VERSION}"
dir="${TKAN_INSTALL_DIR:-$HOME/.local/bin}"
backends="${TKAN_BACKENDS:-}"
bdir="${TKAN_BACKEND_DIR:-$HOME/.local/share/tkan/backends}"
scripts_only=
known="sqlite" # the scripts of contrib/backends, released as tkan-NAME.sh
while [ $# -gt 0 ]; do
	case "$1" in
	-v) [ $# -ge 2 ] || die "-v needs a version"; version="$2"; shift 2 ;;
	-d) [ $# -ge 2 ] || die "-d needs a directory"; dir="$2"; shift 2 ;;
	-b) [ $# -ge 2 ] || die "-b needs backend names (${known}, or all)"; backends="$2"; shift 2 ;;
	-B) [ $# -ge 2 ] || die "-B needs a directory"; bdir="$2"; shift 2 ;;
	-s) scripts_only=1; shift ;;
	-h | --help) sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) die "unknown argument: $1 (see -h)" ;;
	esac
done

version="${version#v}"
printf '%s' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$' ||
	die "'$version' is not a release version (X.Y.Z); installing \"latest\" is not supported"

names=
if [ -n "$backends" ]; then
	[ "$backends" = all ] && backends=$(printf '%s' "$known" | tr ' ' ',')
	for n in $(printf '%s' "$backends" | tr ',' ' '); do
		case " $known " in
		*" $n "*) names="$names $n" ;;
		*) die "unknown backend '$n' (known: $known, or all)" ;;
		esac
	done
fi
[ -z "$scripts_only" ] || [ -n "$names" ] || die "-s installs only backend scripts: say which with -b (${known}, or all)"

asset=
if [ -z "$scripts_only" ]; then
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
fi

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

# verify FILE NAME: FILE must match the SHA256SUMS line of the asset NAME.
verify() {
	want=$(awk -v f="$2" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp/SHA256SUMS")
	[ -n "$want" ] || die "$2 is not listed in SHA256SUMS of v$version"
	[ "$(sha256 "$1")" = "$want" ] || die "checksum mismatch for $2; not installing"
}

# Everything is downloaded and verified before anything is installed.
fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" || die "download failed: $base/SHA256SUMS"
if [ -n "$asset" ]; then
	say "Downloading tkan $version ($asset)"
	fetch "$base/$asset" "$tmp/$asset" || die "download failed: $base/$asset"
	verify "$tmp/$asset" "$asset"
fi
for n in $names; do
	say "Downloading backend script $n"
	fetch "$base/tkan-$n.sh" "$tmp/tkan-$n.sh" || die "download failed: $base/tkan-$n.sh (this release may predate it)"
	verify "$tmp/tkan-$n.sh" "tkan-$n.sh"
done

# prepare DIR: create it and check that it is writable.
prepare() {
	mkdir -p "$1" 2>/dev/null || true
	[ -w "$1" ] || die "cannot write to $1; use ${2:--d} DIR, or run with sudo"
}
# place FILE DEST: install it, 0755.
place() {
	chmod 0755 "$1"
	mv "$1" "$2" 2>/dev/null || cp "$1" "$2" || die "cannot install $2"
}

if [ -n "$asset" ]; then
	prepare "$dir" -d
	place "$tmp/$asset" "$dir/tkan"
	say "Installed: $("$dir/tkan" version) in $dir"
	case ":$PATH:" in
	*":$dir:"*) ;;
	*) say "Note: $dir is not on your PATH. Add it with:"
		say "  export PATH=\"$dir:\$PATH\"" ;;
	esac
fi
if [ -n "$names" ]; then
	prepare "$bdir" -B
	for n in $names; do
		place "$tmp/tkan-$n.sh" "$bdir/tkan-$n.sh"
		say "Installed backend script: $bdir/tkan-$n.sh"
	done
	case " $names " in
	*" sqlite "*)
		for c in bash jq sqlite3; do
			command -v "$c" >/dev/null 2>&1 || say "Note: tkan-sqlite.sh needs $c, which is not installed"
		done
		say "Use it in tkan.yaml:  command: [$bdir/tkan-sqlite.sh, tasks.db, tasks]" ;;
	esac
fi
