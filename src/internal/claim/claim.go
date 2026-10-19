// Package claim holds the rules of claiming a task: who holds it (its
// assignee), until when (its lease) and how a claim or a release is written
// as one guarded update (backend.WithExpect), so that of several agents racing
// for the same task exactly one wins. Claim and Release only compute the
// write; Take and Give also make it.
package claim

import (
	"context"
	"fmt"
	"strings"
	"time"

	"tkan/internal/backend"
	"tkan/internal/task"
)

// Holder says who holds t at now. A task with an assignee is held, for good
// when its lease is empty or not a time, else until the lease runs out.
func Holder(t task.Task, now time.Time) (agent string, until time.Time, held bool) {
	agent = strings.TrimSpace(t.Assignee)
	if agent == "" {
		return "", time.Time{}, false
	}
	lease := strings.TrimSpace(t.Lease)
	if lease == "" {
		return agent, time.Time{}, true
	}
	until, err := time.Parse(time.RFC3339, lease)
	if err != nil {
		return agent, time.Time{}, true
	}
	return agent, until, now.Before(until)
}

// Free reports whether anybody may claim t at now.
func Free(t task.Task, now time.Time) bool {
	_, _, held := Holder(t, now)
	return !held
}

// Request describes a claim.
type Request struct {
	Agent  string        // who claims; becomes the assignee
	Lease  time.Duration // how long the claim lasts; 0 = until released
	Status string        // raw status to move the task to; "" = keep
}

// Guard is the condition a write must meet, for backend.WithExpect.
type Guard struct{ Field, Value string }

// Claim returns t as the claim writes it and the guard that makes the write
// lose against anybody who changed the claim since t was read. The guard is
// on the assignee for a free task or a renewal by the same agent, and on the
// lease when taking over one that ran out. A task held by somebody else gives
// an error wrapping backend.ErrPrecondition.
func Claim(t task.Task, r Request, now time.Time) (task.Task, Guard, error) {
	agent := strings.TrimSpace(r.Agent)
	if agent == "" {
		return t, Guard{}, fmt.Errorf("claim: no agent name")
	}
	holder, until, held := Holder(t, now)
	var g Guard
	switch {
	case holder == "":
		g = Guard{"assignee", t.Assignee}
	case holder == agent:
		g = Guard{"assignee", t.Assignee}
	case held:
		return t, Guard{}, fmt.Errorf("%w: %s:%s is claimed by %s%s", backend.ErrPrecondition, t.Backend, t.ID, holder, untilText(until))
	default: // the lease ran out: take it over
		g = Guard{"lease", t.Lease}
	}
	out := t.Clone()
	if !(holder == agent && held && t.ClaimedAt != "") { // a renewal keeps the original start
		out.ClaimedAt = now.UTC().Truncate(time.Second).Format(time.RFC3339)
	}
	out.Assignee = agent
	out.Lease = ""
	if r.Lease > 0 {
		out.Lease = now.UTC().Add(r.Lease).Truncate(time.Second).Format(time.RFC3339)
	}
	if r.Status != "" {
		out.Status = r.Status
	}
	return out, g, nil
}

// Release returns t without its claim (assignee and lease cleared, moved to
// status unless that is "") and the guard for the write. Only agent may
// release its own claim; with force anybody may, guarded on the current
// assignee. changed is false when t holds no claim and no move is asked: then
// there is nothing to write.
func Release(t task.Task, agent string, force bool, status string) (out task.Task, g Guard, changed bool, err error) {
	holder := strings.TrimSpace(t.Assignee)
	agent = strings.TrimSpace(agent)
	switch {
	case holder == "" && strings.TrimSpace(t.Lease) == "" && (status == "" || status == t.Status):
		return t, Guard{}, false, nil
	case !force && holder != agent:
		if holder == "" {
			return t, Guard{}, false, fmt.Errorf("%w: %s:%s is not claimed by %s", backend.ErrPrecondition, t.Backend, t.ID, agent)
		}
		return t, Guard{}, false, fmt.Errorf("%w: %s:%s is claimed by %s, not %s (-force releases it anyway)", backend.ErrPrecondition, t.Backend, t.ID, holder, agent)
	}
	out = t.Clone()
	out.Assignee, out.Lease, out.ClaimedAt = "", "", ""
	if status != "" {
		out.Status = status
	}
	return out, Guard{"assignee", t.Assignee}, true, nil
}

// Store is what Take and Give write through and Check asks (backend.Set).
type Store interface {
	Update(ctx context.Context, t task.Task) error
	Stores(backendName, field string) bool
}

// Check refuses a claim the backend of t cannot keep: no assignee field, or
// no lease field when r asks for a lease.
func Check(s Store, t task.Task, r Request) error {
	if !s.Stores(t.Backend, "assignee") {
		return fmt.Errorf("backend %s has no assignee field, so its tasks cannot be claimed (add an assignee column)", t.Backend)
	}
	if r.Lease > 0 && !s.Stores(t.Backend, "lease") {
		return fmt.Errorf("backend %s has no lease field, so a lease cannot be kept (add a lease column, or claim without one)", t.Backend)
	}
	return nil
}

// Take writes the claim of t (read at now) as one guarded update and returns
// the task as written. It fails with backend.ErrPrecondition when somebody
// else holds t or changed its claim since t was read.
func Take(ctx context.Context, s Store, t task.Task, r Request, now time.Time) (task.Task, error) {
	out, g, err := Claim(t, r, now)
	if err != nil {
		return t, err
	}
	wctx := backend.WithExpect(ctx, g.Field, g.Value)
	if err := s.Update(backend.WithOld(wctx, t), out); err != nil {
		return t, err
	}
	return out, nil
}

// Give writes the release of t (see Release); changed is false when there was
// nothing to clear.
func Give(ctx context.Context, s Store, t task.Task, agent string, force bool, status string) (task.Task, bool, error) {
	out, g, changed, err := Release(t, agent, force, status)
	if err != nil || !changed {
		return t, false, err
	}
	wctx := backend.WithExpect(ctx, g.Field, g.Value)
	if err := s.Update(backend.WithOld(wctx, t), out); err != nil {
		return t, false, err
	}
	return out, true, nil
}

func untilText(until time.Time) string {
	if until.IsZero() {
		return ""
	}
	return " until " + until.UTC().Format(time.RFC3339)
}
