#!/bin/sh
# Tests install.sh against a fake release tree served over file:// (no network):
#   sh contrib/install-test.sh
# The "binaries" are stand-ins that print a version; the backend script is the real one.
set -u
here=$(cd "$(dirname "$0")" && pwd)
root=$(dirname "$here")
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
pass=0 failed=0
ok() { pass=$((pass + 1)); }
bad() { failed=$((failed + 1)); echo "FAIL: $*"; }
check() { # check NAME COMMAND...: the command must succeed
	n=$1; shift
	if "$@"; then ok; else bad "$n"; fi
}
fails() { # fails NAME COMMAND...: the command must fail
	n=$1; shift
	if "$@" >/dev/null 2>&1; then bad "$n: expected a failure"; else ok; fi
}

rel=$work/rel/v0.2.0
mkdir -p "$rel"
for a in tkan tkan-linux-arm64 tkan-linux-armv7 tkan-linux-armv6 tkan-darwin-amd64 tkan-darwin-arm64; do
	printf '#!/bin/sh\necho "tkan 0.2.0"\n' >"$rel/$a"
done
cp "$here"/backends/sqlite/tkan-sqlite.sh "$rel/"
(cd "$rel" && sha256sum tkan* >SHA256SUMS)
export TKAN_BASE_URL="file://$work/rel"
inst() { sh "$root/install.sh" -v 0.2.0 "$@"; }

# binary only
inst -d "$work/b1" >/dev/null 2>&1
check "binary installed" test -x "$work/b1/tkan"
check "binary runs" test "$("$work/b1/tkan" version)" = "tkan 0.2.0"

# binary plus script
inst -d "$work/b2" -b sqlite -B "$work/s2" >"$work/out" 2>&1
check "script installed 0755" test -x "$work/s2/tkan-sqlite.sh"
check "script is the real one" cmp -s "$work/s2/tkan-sqlite.sh" "$here/backends/sqlite/tkan-sqlite.sh"
check "config hint printed" grep -q "command: \[$work/s2/tkan-sqlite.sh" "$work/out"

# all, scripts only
inst -s -b all -B "$work/s3" >/dev/null 2>&1
check "scripts only: script" test -x "$work/s3/tkan-sqlite.sh"
check "scripts only: no binary" test ! -e "$work/s3/tkan"
TKAN_BACKENDS=sqlite TKAN_BACKEND_DIR="$work/s4" inst -s >/dev/null 2>&1
check "environment variables" test -x "$work/s4/tkan-sqlite.sh"

# usage errors and unknown names
fails "unknown backend" inst -d "$work/e1" -b nope -B "$work/e1s"
fails "-s without -b" inst -s -d "$work/e2"
fails "-b without a value" inst -b
check "nothing installed on usage errors" test ! -e "$work/e1/tkan" -a ! -e "$work/e2/tkan"

# a tampered script aborts before anything is installed
printf 'echo pwned\n' >>"$rel/tkan-sqlite.sh"
fails "tampered script" inst -d "$work/t" -b sqlite -B "$work/ts"
check "tampered: nothing installed" test ! -e "$work/t/tkan" -a ! -e "$work/ts/tkan-sqlite.sh"

# a release without the script gives a clear error and installs nothing
rm "$rel/tkan-sqlite.sh"
(cd "$rel" && sha256sum tkan* >SHA256SUMS)
inst -d "$work/o" -b sqlite -B "$work/os" >"$work/out" 2>&1
check "old release: fails" test $? -ne 0
check "old release: message" grep -q "predate" "$work/out"
check "old release: nothing installed" test ! -e "$work/o/tkan"

echo "install-test: $pass passed, $failed failed"
[ "$failed" -eq 0 ]
