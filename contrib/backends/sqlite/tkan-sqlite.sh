#!/usr/bin/env bash
# tkan-sqlite.sh: a tkan `custom` backend that keeps tasks in a table of a
# SQLite database. Needs bash, jq and the sqlite3 command line tool (3.38+, for
# the JSON functions).
#
#   backend:
#     - name: tasks
#       command: [./tkan-sqlite.sh, tasks.db]            # database, [table]
#       options:
#         env: {TKAN_SQLITE_FIELDS: "id=key,title=summary,status=state"}
#         poll: 5s
#
# Arguments: DATABASE [TABLE] (table default: tasks).
# Environment:
#   TKAN_SQLITE_FIELDS  logical field=column pairs, comma separated, only where
#                       the column name differs from the field name
#   TKAN_SQLITE_CLI     the SQLite command line tool (default: sqlite3)
#
# Protocol: docs/backends.md#the-custom-backend. Values never travel as SQL
# text: the whole request is handed to SQLite as one hex blob literal and read
# back with json_extract, so no value can alter a statement. Writes run in one
# BEGIN IMMEDIATE transaction, which also makes the update `expect` guard atomic.
set -u

DB=${1:-}
TABLE=${2:-tasks}
CLI=${TKAN_SQLITE_CLI:-sqlite3}
FIELDS=${TKAN_SQLITE_FIELDS:-}

fail() { # fail CODE MESSAGE: answer with a protocol error (exit 0, the JSON says it)
	jq -cn --arg e "$2" --arg c "$1" '{error: $e} + (if $c != "" then {code: $c} else {} end)'
	exit 0
}

[ -n "$DB" ] || fail "" "usage: tkan-sqlite.sh DATABASE [TABLE]"
case $DB in -*) DB=./$DB ;; esac # never let the path read as an option
command -v jq >/dev/null 2>&1 || fail "" "tkan-sqlite.sh needs jq"
command -v "$CLI" >/dev/null 2>&1 || fail "" "tkan-sqlite.sh needs the SQLite command line tool ($CLI not found)"
for v in "$TABLE" "$FIELDS"; do
	[[ $v =~ [[:cntrl:]] ]] && fail "" "table and field names cannot contain control characters"
done

OUTF=$(mktemp) || fail "" "cannot create a temporary file"
ERRF=$(mktemp) || fail "" "cannot create a temporary file"
trap 'rm -f "$OUTF" "$ERRF"' EXIT

# run_sql SCRIPT: feed SCRIPT to the CLI; sets OUT (stdout) and ERR (stderr).
run_sql() {
	local rc=0
	printf '.timeout 5000\n%s\n' "$1" | "$CLI" -bail -json "$DB" >"$OUTF" 2>"$ERRF" || rc=$?
	OUT=$(<"$OUTF")
	ERR=$(<"$ERRF")
	return $rc
}

# lit TEXT: a SQL text literal no content can escape.
lit() {
	if [ -z "$1" ]; then
		printf "''"
	else
		printf "CAST(X'%s' AS TEXT)" "$(printf %s "$1" | od -An -v -tx1 | tr -d ' \n')"
	fi
}

DEFS='
def logical: ["id","title","description","status","priority","notes","type","assignee","due","start","labels","blocked_by","lease","claimed_at"];
def ident: "\"" + gsub("\""; "\"\"") + "\"";
def Q: [39] | implode;
def sq: Q + gsub(Q; Q + Q) + Q;
def J: "(SELECT j FROM temp._r)";
def totext: if . == null then "" elif type == "string" then . else tostring end;

# cfg: logical field -> configured column
def cfg($s):
  ($s | split(",") | map(gsub("^\\s+|\\s+$"; "") | select(length > 0))
      | map(split("=") | {(.[0] | gsub("\\s+$"; "") | ascii_downcase): (.[1:] | join("=") | gsub("^\\s+|\\s+$"; ""))})
      | add // {}) as $c
  | ([$c | keys[] | select(IN(logical[]) | not)] | .[0]) as $bad
  | ([$c | to_entries[] | select(.value == "") | .key] | .[0]) as $empty
  | if $bad != null then error("TKAN_SQLITE_FIELDS: unknown field \($bad) (valid: \(logical | join(", ")))")
    elif $empty != null then error("TKAN_SQLITE_FIELDS: field \($empty): empty column name")
    else $c end;

# plan: map each logical field to a real column (case-insensitive)
def plan($cols; $cfg):
  reduce logical[] as $f ({m: {}, used: {}};
    ($cfg[$f]) as $conf
    | ($conf // $f) as $want
    | (.used as $u | [$cols[].name | . as $n | select((($n | ascii_downcase) == ($want | ascii_downcase)) and ($u | has($n) | not))] | .[0]) as $found
    | if $found != null then .m[$f] = $found | .used[$found] = true
      elif ($f == "id" or $f == "title" or $f == "status" or $conf != null)
        then error("field \($f): no column \($want) in the table (columns: \([$cols[].name] | join(", "))); set TKAN_SQLITE_FIELDS")
      else . end)
  | . as $r
  | {mapped: $r.m, extra: [$cols[].name | . as $n | select($r.used | has($n) | not)]};

# layout: the table to write to; the default one when it does not exist yet
def layout($cols; $cfg; $table):
  (if $cols == null
   then {create: true, cols: [logical[] | . as $f | {name: ($cfg[$f] // $f), type: (if $f == "id" then "INTEGER" else "TEXT" end), pk: (if $f == "id" then 1 else 0 end), notnull: 0, dflt_value: null}]}
   else {create: false, cols: $cols} end) as $l
  | plan($l.cols; $cfg) as $p
  | ([$l.cols[] | select(.pk > 0)] | length) as $pks
  | $p.mapped.id as $idc
  | $p + {cols: $l.cols,
          idAlias: ([$l.cols[] | select(.name == $idc and .pk == 1 and $pks == 1 and (.type | ascii_downcase) == "integer")] | length > 0),
          create: (if $l.create
                   then "CREATE TABLE IF NOT EXISTS \($table | ident) (" + ([$l.cols[] | if .pk == 1 then "\(.name | ident) INTEGER PRIMARY KEY AUTOINCREMENT" else "\(.name | ident) TEXT NOT NULL DEFAULT \(Q)\(Q)" end] | join(", ")) + ");\n"
                   else "" end)};

# assignments: column and value expression for every writable column of $t; with
# a base only the ones whose value differs from it
def assignments($p; $t; $base):
  ([logical[] | . as $f | select($f != "id" and ($p.mapped | has($f)))
      | select($base == null or ($t[$f] // "") != ($base[$f] // ""))
      | {col: $p.mapped[$f], val: ("COALESCE(json_extract(" + J + ", " + (("$.task." + $f) | sq) + "), \(Q)\(Q))")}]
   + [$p.extra[] | . as $c | select(($t.extra // {}) | has($c))
      | select($base == null or (($base.extra // {}) | has($c) | not) or ($base.extra[$c] != $t.extra[$c]))
      | if ($c | contains("\"")) then error("column \($c | ident) has a double quote in its name") else . end
      | {col: $c, val: ("COALESCE(json_extract(" + J + ", " + (("$.task.extra.\"" + $c + "\"") | sq) + "), \(Q)\(Q))")}]);

def idmatch($p): "CAST(\($p.mapped.id | ident) AS TEXT) = CAST(json_extract(\(J), \(Q)$.task.id\(Q)) AS TEXT)";
'

die_jq() { # die_jq OUTPUT: OUTPUT is the {error,code} object a jq builder produced
	printf '%s\n' "$1"
	exit 0
}

CFG=$(jq -cn "$DEFS"' cfg($s)' --arg s "$FIELDS" 2>&1) || fail "" "${CFG#jq: error*: }"

# schema: sets VER (schema_version) and COLS (table_info as JSON, "null" when the table is missing)
schema() {
	if [ ! -e "$DB" ]; then VER=0 COLS=null; return; fi
	run_sql "BEGIN;
PRAGMA schema_version;
PRAGMA table_info($TQ);
COMMIT;" || fail "" "sqlite: $ERR"
	local arr
	arr=$(jq -cs '.' <<<"$OUT") || fail "" "unexpected output from the SQLite CLI"
	VER=$(jq -r '.[0][0].schema_version // 0' <<<"$arr")
	COLS=$(jq -c '.[1] // null' <<<"$arr")
}

# guard: the statement that opens a write transaction's script. It fails (integer
# overflow, which stops the CLI and rolls back) when the schema is no longer the
# one the columns were read at, or, for a table not seen yet, exists by now.
guard() {
	local f='SELECT abs(-9223372036854775807 - 1) WHERE '
	if [ "$COLS" = null ]; then
		printf "%sEXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = %s COLLATE NOCASE);\n" "$f" "$(lit "$TABLE")"
	else
		printf '%s(SELECT schema_version FROM pragma_schema_version) <> %s;\n' "$f" "$VER"
	fi
}

# jqrun PROGRAM [jq args]: run a builder over the request; a result with "error" is answered as is
jqrun() {
	local prog=$1 res
	shift
	res=$(jq -c "$@" "$DEFS $prog" <<<"$REQ" 2>&1) || fail "" "${res#jq: error*: }"
	RES=$res
	[ "$(jq -r 'if type == "object" then (.error // "") else "" end' <<<"$RES")" = "" ] || die_jq "$RES"
}

# write BUILDER: re-read the schema, build BODY (BUILDER sets SQL), run it in one
# immediate transaction; once more if another process changed the schema meanwhile.
write() {
	local try
	for try in 1 2; do
		schema
		"$1"
		if run_sql "CREATE TEMP TABLE _r AS SELECT CAST(X'$HEX' AS TEXT) AS j;
BEGIN IMMEDIATE;
$(guard)$SQL
COMMIT;"; then
			return 0
		fi
		if [ "$try" = 1 ] && [[ $ERR == *"integer overflow"* ]]; then continue; fi
		fail "" "sqlite: $ERR"
	done
}

REQ=$(cat)
case $REQ in *'\u0000'*) fail "" "sqlite: values cannot contain NUL characters" ;; esac
HEX=$(printf %s "$REQ" | od -An -v -tx1 | tr -d ' \n')
op=$(jq -r '.op // ""' <<<"$REQ" 2>/dev/null) || fail "" "invalid JSON request"
TQ=$(printf '"%s"' "${TABLE//\"/\"\"}")

case $op in
list)
	schema
	if [ "$COLS" = null ]; then
		jq -cn '{tasks: [], guard: true}'
		exit 0
	fi
	run_sql "SELECT * FROM $TQ;" || fail "" "sqlite: $ERR"
	RES=$(jq -c -s --argjson cols "$COLS" --argjson cfg "$CFG" "$DEFS"'
	  plan($cols; $cfg) as $p
	  | (.[0] // []) as $rows
	  | {tasks: ($rows | map(. as $r
	        | (reduce ($p.mapped | to_entries[]) as $e ({}; .[$e.key] = ($r[$e.value] | totext)))
	          + (($p.extra | map({(.): ($r[.] | totext)}) | add // {}) as $x | if ($x | length) > 0 then {extra: $x} else {} end))),
	     guard: true, fields: ($p.mapped | keys)}' <<<"$OUT" 2>&1) || fail "" "${RES#jq: error*: }"
	printf '%s\n' "$RES"
	;;
create)
	build_create() {
		jqrun '
		  (try (layout($cols; $cfg; $table) as $p
		    | ([$p.cols[] | select(.notnull != 0 and .dflt_value == null and .pk == 0) | .name | . as $n | select(IN($p.extra[])) | select(($t.extra // {}) | has($n) | not)]) as $miss
		    | if ($miss | length) > 0
		      then error("table \($table | ident) has a NOT NULL column \($miss[0] | ident) without a default that tkan does not map; give it a default or map it in TKAN_SQLITE_FIELDS")
		      else . end
		    | assignments($p; $t; null) as $a
		    | ($a | map(.col | ident)) as $names | ($a | map(.val)) as $vals
		    | (if $p.idAlias then {n: $names, v: $vals}
		       else {n: ([$p.mapped.id | ident] + $names), v: (["(SELECT COALESCE(MAX(CAST(\($p.mapped.id | ident) AS INTEGER)), 0) + 1 FROM \($table | ident))"] + $vals)} end) as $x
		    | {sql: ($p.create + "INSERT INTO \($table | ident) (\($x.n | join(", "))) VALUES (\($x.v | join(", ")));\nSELECT \($p.mapped.id | ident) AS id FROM \($table | ident) WHERE rowid = last_insert_rowid();\n")}
		  ) catch (if type == "object" then . else {error: .} end))' \
			--argjson cols "$COLS" --argjson cfg "$CFG" --arg table "$TABLE" --argjson t "$(jq -c '.task' <<<"$REQ")"
		SQL=$(jq -r '.sql' <<<"$RES")
	}
	write build_create
	id=$(jq -cs '.[0][0].id // empty' <<<"$OUT")
	[ -n "$id" ] || fail "" "sqlite: the new row has no id"
	jq -c --argjson id "$id" '{task: (.task + {id: ($id | tostring)})}' <<<"$REQ"
	;;
update)
	[ -e "$DB" ] || fail not_found "task not found"
	build_update() {
		[ "$COLS" != null ] || fail not_found "task not found"
		jqrun '
		  (try (layout($cols; $cfg; $table) as $p
		    | (.expect // null) as $exp
		    | assignments($p; $t; $base) as $a
		    | ($a | map("\(.col | ident) = \(.val)")) as $sets0
		    | (if ($sets0 | length) == 0 then ["\($p.mapped.id | ident) = \($p.mapped.id | ident)"] else $sets0 end) as $sets
		    | (idmatch($p)
		       + (if $exp == null then ""
		          elif ($p.mapped | has($exp.field)) then " AND COALESCE(CAST(\($p.mapped[$exp.field] | ident) AS TEXT), \(Q)\(Q)) = COALESCE(json_extract(\(J), \(Q)$.expect.value\(Q)), \(Q)\(Q))"
		          else error({code: "precondition", error: "field \($exp.field) is not a column of the table"}) end)) as $where
		    | {sql: ("UPDATE \($table | ident) SET \($sets | join(", ")) WHERE \($where);\nSELECT changes() AS n;\nSELECT EXISTS (SELECT 1 FROM \($table | ident) WHERE \(idmatch($p))) AS ex;\n")}
		  ) catch (if type == "object" then . else {error: .} end))' \
			--argjson cols "$COLS" --argjson cfg "$CFG" --arg table "$TABLE" \
			--argjson t "$(jq -c '.task' <<<"$REQ")" --argjson base "$(jq -c '.base // null' <<<"$REQ")"
		SQL=$(jq -r '.sql' <<<"$RES")
	}
	write build_update
	n=$(jq -cs '.[0][0].n // 0' <<<"$OUT")
	if [ "$n" = 0 ]; then
		if [ "$(jq -cs '.[1][0].ex // 0' <<<"$OUT")" = 1 ]; then
			field=$(jq -r '.expect.field // ""' <<<"$REQ")
			fail precondition "$field is not the expected value any more"
		fi
		fail not_found "task not found"
	fi
	echo '{}'
	;;
delete)
	[ -e "$DB" ] || fail not_found "task not found"
	build_delete() {
		[ "$COLS" != null ] || fail not_found "task not found"
		jqrun '
		  (try (layout($cols; $cfg; $table) as $p
		    | {sql: ("DELETE FROM \($table | ident) WHERE CAST(\($p.mapped.id | ident) AS TEXT) = CAST(json_extract(\(J), \(Q)$.id\(Q)) AS TEXT);\nSELECT changes() AS n;\n")}
		  ) catch (if type == "object" then . else {error: .} end))' \
			--argjson cols "$COLS" --argjson cfg "$CFG" --arg table "$TABLE"
		SQL=$(jq -r '.sql' <<<"$RES")
	}
	write build_delete
	[ "$(jq -cs '.[0][0].n // 0' <<<"$OUT")" != 0 ] || fail not_found "task not found"
	echo '{}'
	;;
*)
	fail unsupported "operation not supported: $op"
	;;
esac
