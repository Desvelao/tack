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
for a in tack tack-linux-arm64 tack-linux-armv7 tack-linux-armv6 tack-darwin-amd64 tack-darwin-arm64; do
	printf '#!/bin/sh\necho "tack 0.2.0"\n' >"$rel/$a"
done
cp "$here"/backends/sqlite/tack-sqlite.sh "$rel/"
(cd "$rel" && sha256sum tack* >SHA256SUMS)
export TACK_BASE_URL="file://$work/rel"
inst() { sh "$root/install.sh" -v 0.2.0 "$@"; }

# binary only
inst -d "$work/b1" >/dev/null 2>&1
check "binary installed" test -x "$work/b1/tack"
check "binary runs" test "$("$work/b1/tack" version)" = "tack 0.2.0"

# binary plus script
inst -d "$work/b2" -b sqlite -B "$work/s2" >"$work/out" 2>&1
check "script installed 0755" test -x "$work/s2/tack-sqlite.sh"
check "script is the real one" cmp -s "$work/s2/tack-sqlite.sh" "$here/backends/sqlite/tack-sqlite.sh"
check "config hint printed" grep -q "command: \[$work/s2/tack-sqlite.sh" "$work/out"

# all, scripts only
inst -s -b all -B "$work/s3" >/dev/null 2>&1
check "scripts only: script" test -x "$work/s3/tack-sqlite.sh"
check "scripts only: no binary" test ! -e "$work/s3/tack"
TACK_BACKENDS=sqlite TACK_BACKEND_DIR="$work/s4" inst -s >/dev/null 2>&1
check "environment variables" test -x "$work/s4/tack-sqlite.sh"

# usage errors and unknown names
fails "unknown backend" inst -d "$work/e1" -b nope -B "$work/e1s"
fails "-s without -b" inst -s -d "$work/e2"
fails "-b without a value" inst -b
check "nothing installed on usage errors" test ! -e "$work/e1/tack" -a ! -e "$work/e2/tack"

# -h prints the whole usage (down to -s), also when the script comes through a pipe
inst -h >"$work/out" 2>&1
check "-h: exits 0" test $? -eq 0
check "-h: last option listed" grep -q -- "^  -s " "$work/out"
sh -s -- -h <"$root/install.sh" >"$work/out" 2>&1
check "-h piped: exits 0" test $? -eq 0
check "-h piped: last option listed" grep -q -- "^  -s " "$work/out"

# a tampered script aborts before anything is installed
printf 'echo pwned\n' >>"$rel/tack-sqlite.sh"
fails "tampered script" inst -d "$work/t" -b sqlite -B "$work/ts"
check "tampered: nothing installed" test ! -e "$work/t/tack" -a ! -e "$work/ts/tack-sqlite.sh"

# a release without the script gives a clear error and installs nothing
rm "$rel/tack-sqlite.sh"
(cd "$rel" && sha256sum tack* >SHA256SUMS)
inst -d "$work/o" -b sqlite -B "$work/os" >"$work/out" 2>&1
check "old release: fails" test $? -ne 0
check "old release: message" grep -q "predate" "$work/out"
check "old release: nothing installed" test ! -e "$work/o/tack"

# an interrupt stops the script (it used to go on after the temp dir was removed) and leaves no temp dir
mkdir -p "$work/fakebin" "$work/tmpd"
printf '#!/bin/sh\nkill -TERM $PPID\nsleep 1\n' >"$work/fakebin/curl"
chmod 0755 "$work/fakebin/curl"
PATH="$work/fakebin:$PATH" TMPDIR="$work/tmpd" inst -d "$work/i" >/dev/null 2>&1
check "TERM: exit status 143" test $? -eq 143
check "TERM: nothing installed" test ! -e "$work/i/tack"
check "TERM: temp dir removed" test -z "$(ls "$work/tmpd")"

# no partial files are left beside the installed binary
check "no .new files left" test -z "$(ls "$work/b1" | grep '\.new\.')"
printf '#!/bin/sh\nexit 1\n' >"$work/rel/v0.2.0/tack"
(cd "$work/rel/v0.2.0" && sha256sum tack* >SHA256SUMS)
inst -d "$work/bad" >"$work/out" 2>&1
check "unrunnable binary: fails" test $? -ne 0
check "unrunnable binary: message" grep -q "does not run" "$work/out"

echo "install-test: $pass passed, $failed failed"
[ "$failed" -eq 0 ]
