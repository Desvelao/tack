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
| `field=value` | field **equals** value exactly (whole value, ignoring case and surrounding spaces) |
| `field!=value` | field is **not** equal to value |
| `field^value` | field **starts with** value |
| `field~regex` | field matches the regular expression (Go/RE2 syntax, case-insensitive) |
| `field<value` | field sorts before value; also `>`, `<=`, `>=` (numbers compare as numbers, so `story_points>=5` works; otherwise text order, so ISO dates work: `due<2026-12-01`; empty fields never match). Dates may be relative: `today`, `tomorrow`, `yesterday`, `today+7d`, `today-2w` (`d` days, `w` weeks), also with `=` and `!=` |
| `field=` | field is **empty** |
| `field!=` | field **has a value** |
| `a OR b` | either side matches (also `\|`); terms bind tighter than `OR`, so `priority=high col:doing OR type=fix` is *(high and doing)* or *fix*. A quoted `"OR"` is plain text, lower-case `or` too. No parentheses |
| `a,b` | in a value: any of them (`priority=high,medium`). With `!:`/`!=`: none of them. Not split for `~` |

`field:` / `field^` / `field~` without a value are errors; use `field=` to test for empty.

### Fields

`id`, `title`, `description`, `status` (the raw stored value), `priority`, `notes`, `type`, `assignee`, `due`, `start`, `labels`,
`column` (the board column name; `col` is an alias), `backend` (the name of the backend the
task belongs to) and every other column of the file by its
name (lower case, spaces as `_`, for example `story_points`). An unknown field is rejected with
the list of valid ones. To search for text that looks like a field, quote it: `"http://x"`.

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

## Behaviour in the board

- The filter is applied when you press `enter`. An invalid query shows its error under the prompt,
  keeps the prompt open and leaves the previous filter active.
- An empty query clears the filter; `esc` on the board clears it too, `esc` in the prompt cancels.
- Column counts show the matching cards; the header shows the active filter.
- The filter survives live refreshes; it is not saved between sessions.
