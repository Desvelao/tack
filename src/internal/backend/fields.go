package backend

// LogicalFields are the task fields a store column can map to.
var LogicalFields = []string{"id", "title", "description", "status", "priority", "notes", "type", "assignee", "due", "start", "labels", "blocked_by", "lease", "claimed_at"}

// RequiredFields must be mapped by every store.
var RequiredFields = map[string]bool{"id": true, "title": true, "status": true}

// Synonyms lists column names recognised for each logical field, best first
// (used by the backends' sample generators).
var Synonyms = map[string][]string{
	"id":          {"id", "key", "ticket", "number", "#"},
	"title":       {"title", "summary", "name", "subject"},
	"status":      {"status", "state", "stage", "column"},
	"description": {"description", "desc", "details", "body"},
	"priority":    {"priority", "prio", "severity"},
	"notes":       {"notes", "note", "comment", "comments"},
	"type":        {"type", "kind", "category"},
	"assignee":    {"assignee", "owner", "assigned_to", "assigned"},
	"due":         {"due", "due_date", "deadline"},
	"start":       {"start", "start_date", "begin"},
	"labels":      {"labels", "label", "tags", "tag"},
	"blocked_by":  {"blocked_by", "depends_on", "dependencies", "blockers", "deps"},
	"lease":       {"lease", "lease_until", "claimed_until"},
	"claimed_at":  {"claimed_at", "claimed", "claim_start"},
}
