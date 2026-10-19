# Filtering

Press `/` on the board, type a query and press `enter`. The same language is available on the
command line as `tkan list -q QUERY`, and `tkan help filter` prints this reference.
In the board, `?` shows the keys and `tab` flips to the filter syntax; typing `?` in an empty
filter prompt jumps straight to it.

## Syntax

Terms are separated by spaces and **all** must match. Matching is case-insensitive.

| Term | Meaning |
|---|---|
| `text` | contains `text` in any of the searched fields |
| `-text` | none of the searched fields contains `text` |
| `"two words"` | quote values that contain spaces (`\"` for a literal quote) |
| `field:value` | field **contains** value |
| `field!:value` | field does **not** contain value |
| `field=value` | field **equals** value exactly (whole value, ignoring case and surrounding spaces; on `labels`: one whole label) |
| `field!=value` | field is **not** equal to value |
| `field^value` | field **starts with** value |
| `field~regex` | field matches the regular expression (Go/RE2 syntax, case-insensitive) |
| `field<value` | field sorts before value; also `>`, `<=`, `>=` (numbers compare as numbers, so `story_points>=5` works; otherwise text order, so ISO dates work: `due<2026-12-01`; empty fields never match). Dates may be relative: `today`, `tomorrow`, `yesterday`, `today+7d`, `today-2w`, `today+1m` (`d` days, `w` weeks, `m` months, `y` years; a malformed one such as `today+7` or `due<tomorow` is an error, not text), also with `=` and `!=` |
| `field=` | field is **empty** |
| `field!=` | field **has a value** |
| `a OR b` | either side matches (also `\|`); terms bind tighter than `OR`, so `priority=high col:doing OR type=fix` is *(high and doing)* or *fix*. A quoted `"OR"` is plain text, lower-case `or` too. No parentheses |
| `a,b` | in a value: any of them (`priority=high,medium`). With `!:`/`!=`: none of them. Not split for `~` |

`labels` is a list: `labels=ops`, `labels!=ops` and `labels^op` test the single labels (whole label, or label starting with the value), so `labels=ops` does not match `devops`; `labels:ops` still searches the whole text, `labels=` means no labels.

`field:` / `field^` / `field~` without a value are errors; use `field=` to test for empty.

### Fields

The same text is on the third page of the board's help (`?`, then `tab` twice) and in `tkan help filter`.

| Field | Meaning |
|---|---|
| `id` | the task's id (unique within its backend) |
| `title` | one-line summary |
| `description` | the longer text of the task |
| `status` | the raw stored value (`col` is the board column) |
| `priority` | free text, usually low / medium / high |
| `notes` | free text added while working on it |
| `type` | kind of task, for example fix, feature, chore |
| `assignee`, `lease`, `claimed_at` | claim: who, until when, since when (`tkan claim`) |
| `due`, `start` | due and start dates, `YYYY-MM-DD`: `due<today+7d` |
| `labels` | comma separated; `labels=ops` is one whole label |
| `blocked_by` | ids of tasks to finish first, comma separated |
| `column` (`col`) | name of the board column the card is shown in |
| `backend` | name of the backend the task comes from |
| `blocked`, `ready` | yes/no: a `blocked_by` task is not done yet (or is) |
| `running` | yes/no: an action started from the board is running on the card (board only) |
| *your columns* | the file's own columns, by their name (lower case, spaces as `_`, for example `story_points`) |

`blocked` and `ready` are computed from the whole task list: a task is **blocked** (`blocked=yes`,
`ready=no`) while one of the ids in its `blocked_by` names a task of the same backend that is not in
a finished column (Done, Closed, Released, ... as for overdue dates; `failed` is not finished). An id
that names no task (deleted, a typo, a backend that failed to load) and the task's own id do not
block. They work in the board, `list`, `count` and `labels`, but not in a hook's `when:` (an event
carries one task); test `blocked_by` there.

`running` is `yes` while a background action started from this board (`x`, `.`, marked cards) is
going on the card, `no` otherwise; it follows the runs live, so `running=yes` is a view of the cards
being worked on. Only the board knows its runs: in `list`, `count` and `labels` the field is empty
(`running=yes` matches nothing) and a hook's `when:` rejects it. Runs started by `tkan run` in
another process or by hooks are not counted.

An unknown field is rejected with the list of valid ones. To search for text that looks like a field, quote it: `"http://x"`.

#### Audit fields (`tkan audit -q`, `A` in the board)

`?` in the filter prompt of the audit view opens the same help for these; `tkan help filter audit` prints it.

| Field | Meaning |
|---|---|
| `op` | create, update, delete, reload, run, or `hook:EVENT` |
| `date` | local day of the entry, `YYYY-MM-DD`: `date=today` |
| `time` | local date and time, `YYYY-MM-DD HH:MM:SS` |
| `details` | what happened: title, status, note, error text |
| `result` | ok, error or skipped (a hook event dropped by a limit) |
| `source` | who did it: tui, cli, mcp, or hook (started by a hook) |
| `backend` | name of the backend the entry is about |
| `id` | id of the task the entry is about |
| `session` | current, previous, or the id of an older session |

A plain word matches `details`, `op` and `date`.

#### Action run fields (`tkan runs -q`, `R` in the board)

`?` in the filter prompt of the runs view, or `tkan help filter runs`.

| Field | Meaning |
|---|---|
| `action` | name of the action that ran |
| `mode` | background or interactive (had the terminal) |
| `result` | ok or error |
| `date` | local day the run started, `YYYY-MM-DD`: `date=today` |
| `time` | local date and time the run started |
| `source` | tui, cli, mcp or hook |
| `event` | event that fired a hook run: `event:task.moved` |
| `backend` | backend of the task it ran on |
| `id` | id of the task it ran on |
| `title` | title of that task when it ran |
| `exit` | exit status of the command (not set when killed) |

A plain word matches `action`, `title`, `id` and `error`.

### Plain text and the searched fields

A term without `field` operator is matched against the fields listed in `board.search`
(default: `id, title, description, notes`):

```yaml
board:
  search: [id, title, description, notes, assignee]
```

Names are the same field names as above. Unknown names are reported at start-up.

## Examples

| Query | Finds |
|---|---|
| `login` | cards mentioning login in id/title/description/notes |
| `priority=high col:doing` | high priority cards in the Doing column |
| `title:"fix bug" -wip` | titles with "fix bug", nothing mentioning wip |
| `type!=chore notes=` | not chores, with empty notes |
| `assignee=ann due<2026-12-01` | ann's tasks due before December |
| `due<today status!=done` | overdue and not finished |
| `priority=high OR due<today+7d` | important or due within a week |
| `assignee=ann,bob` | owned by ann or bob |
| `status=in_progress,review` | by raw status value |
| `id~^A-[0-9]+$` | ids like `A-12` |
| `title^fix` | titles starting with "fix" |
| `ready=yes col:pending` | pending cards whose `blocked_by` tasks are all finished |
| `blocked=yes` | cards still waiting for another task |
| `running=yes` | cards an action of this board session is running on |

## Behaviour in the board

- The filter is applied when you press `enter`. An invalid query shows its error under the prompt,
  keeps the prompt open and leaves the previous filter active.
- An empty query clears the filter; `esc` on the board clears it too, `esc` in the prompt cancels.
- Column counts show the matching cards; the header shows the active filter.
- The filter survives live refreshes; it is not saved between sessions.
