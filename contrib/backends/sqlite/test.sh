#!/usr/bin/env bash
# Tests tkan-sqlite.sh on a throw-away database (never a real backlog):
#   contrib/backends/sqlite/test.sh [path/to/tkan]
# Protocol-level checks always run; with a tkan binary the same script is
# driven through `tkan` itself (claim races included).
set -u
here=$(cd "$(dirname "$0")" && pwd)
S=$here/tkan-sqlite.sh
TKAN=${1:-}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$work" || exit 1
pass=0 failed=0

ok() { pass=$((pass + 1)); }
bad() { failed=$((failed + 1)); echo "FAIL: $*"; }
eq() { # eq NAME WANT GOT
	if [ "$2" = "$3" ]; then ok; else bad "$1: want [$2], got [$3]"; fi
}
call() { printf '%s' "$1" | "$S" "${@:2}"; } # call REQUEST DB [TABLE]
jqv() { jq -r "$1"; }

DB=t.db
# 1. create in a table that does not exist yet
r=$(call '{"op":"create","task":{"title":"First \"q\" '\''x'\''; DROP TABLE tasks;--","status":"todo","extra":{"ignored":"1"}}}' $DB)
eq create-id 1 "$(jqv .task.id <<<"$r")"
r=$(call '{"op":"create","task":{"title":"Second","status":"todo"}}' $DB)
eq create-id2 2 "$(jqv .task.id <<<"$r")"
r=$(call '{"op":"list"}' $DB)
eq list-count 2 "$(jqv '.tasks|length' <<<"$r")"
eq list-title-safe "First \"q\" 'x'; DROP TABLE tasks;--" "$(jqv '.tasks[0].title' <<<"$r")"
eq list-guard true "$(jqv .guard <<<"$r")"

# 2. update: replace, and write only what changed when base is given
r=$(call '{"op":"update","task":{"id":"1","title":"First","status":"doing","notes":"mine"}}' $DB)
eq update-ok '{}' "$r"
sqlite3 $DB "UPDATE tasks SET priority='high' WHERE id=1;" # somebody else edits another field
r=$(call '{"op":"update","base":{"id":"1","title":"First","status":"doing","notes":"mine"},"task":{"id":"1","title":"First","status":"done","notes":"mine"}}' $DB)
eq patch-ok '{}' "$r"
eq patch-keeps-other-writer high "$(sqlite3 $DB "SELECT priority FROM tasks WHERE id=1;")"
eq patch-status done "$(sqlite3 $DB "SELECT status FROM tasks WHERE id=1;")"

# 3. guarded update: precondition, not_found
r=$(call '{"op":"update","task":{"id":"2","title":"Second","status":"todo","assignee":"a"},"expect":{"field":"assignee","value":""}}' $DB)
eq claim-wins '{}' "$r"
r=$(call '{"op":"update","task":{"id":"2","title":"Second","status":"todo","assignee":"b"},"expect":{"field":"assignee","value":""}}' $DB)
eq claim-loses precondition "$(jqv .code <<<"$r")"
eq claim-kept a "$(sqlite3 $DB "SELECT assignee FROM tasks WHERE id=2;")"
r=$(call '{"op":"update","task":{"id":"99","title":"x","status":"todo"},"expect":{"field":"assignee","value":""}}' $DB)
eq update-missing not_found "$(jqv .code <<<"$r")"

# 4. a race: 8 writers, exactly one wins
sqlite3 $DB "UPDATE tasks SET assignee='' WHERE id=2;"
for i in 1 2 3 4 5 6 7 8; do
	(call '{"op":"update","task":{"id":"2","title":"Second","status":"todo","assignee":"w'$i'"},"expect":{"field":"assignee","value":""}}' $DB >"race.$i") &
done
wait
eq race-winners 1 "$(cat race.* | grep -c '^{}$')"
eq race-losers 7 "$(cat race.* | grep -c precondition)"

# 5. delete
r=$(call '{"op":"delete","id":"1"}' $DB)
eq delete-ok '{}' "$r"
r=$(call '{"op":"delete","id":"1"}' $DB)
eq delete-missing not_found "$(jqv .code <<<"$r")"

# 6. existing table: other column names, extra columns, NOT NULL checks
rm -f m.db
sqlite3 m.db "CREATE TABLE items (key INTEGER PRIMARY KEY, summary TEXT NOT NULL, state TEXT, owner TEXT, customer TEXT NOT NULL);
INSERT INTO items (summary, state, customer) VALUES ('Seven', 'open', 'acme');"
export TKAN_SQLITE_FIELDS="id=key,title=summary,status=state,assignee=owner"
r=$(call '{"op":"list"}' m.db items)
eq map-title Seven "$(jqv '.tasks[0].title' <<<"$r")"
eq map-extra acme "$(jqv '.tasks[0].extra.customer' <<<"$r")"
eq map-fields '["assignee","id","status","title"]' "$(jq -c .fields <<<"$r")"
r=$(call '{"op":"create","task":{"title":"New","status":"open"}}' m.db items)
case $(jqv .error <<<"$r") in *customer*) ok ;; *) bad "NOT NULL column not reported: $r" ;; esac
r=$(call '{"op":"create","task":{"title":"New","status":"open","extra":{"customer":"zed"}}}' m.db items)
eq extra-create 2 "$(jqv .task.id <<<"$r")"
eq extra-stored zed "$(sqlite3 m.db "SELECT customer FROM items WHERE key=2;")"
unset TKAN_SQLITE_FIELDS
r=$(call '{"op":"list"}' m.db items)
case $(jqv .error <<<"$r") in *'no column "id"'*|*'no column id'*) ok ;; *) bad "missing column not reported: $r" ;; esac

# 7. protocol errors
r=$(call '{"op":"frobnicate"}' $DB)
eq unsupported unsupported "$(jqv .code <<<"$r")"
r=$(call '{"op":"list"}' none.db)
eq no-db-lists-empty 0 "$(jqv '.tasks|length' <<<"$r")"
[ ! -e none.db ] && ok || bad "list must not create the database"

# 8. through tkan
if [ -n "$TKAN" ]; then
	cat >tkan.yaml <<EOF
backend:
  - name: db
    command: [$S, w.db]
    options: {poll: 2s}
EOF
	"$TKAN" -c tkan.yaml add -title "Via tkan" -status pending >/dev/null 2>&1 || bad "tkan add failed"
	id=$("$TKAN" -c tkan.yaml list -json 2>/dev/null | jq -r ".[0].id // empty")
	[ -n "$id" ] && ok || bad "tkan list shows no task"
	codes=$(for i in 1 2 3 4; do ("$TKAN" -c tkan.yaml claim "$id" -as "agent$i" >/dev/null 2>&1; echo $?) & done; wait)
	eq tkan-claim-winner 1 "$(grep -c '^0$' <<<"$codes")"
	eq tkan-claim-losers 3 "$(grep -c '^4$' <<<"$codes")"
fi

echo "$pass passed, $failed failed"
[ "$failed" = 0 ]
