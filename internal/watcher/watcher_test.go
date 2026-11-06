package watcher

import (
	"strings"
	"testing"
	"time"

	"github.com/travisennis/cake-monitor/internal/store"
)

func createWatch(t *testing.T, root string, meta store.Meta) *store.Watch {
	t.Helper()
	now := time.Now().UTC()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	if meta.Deadline.IsZero() {
		meta.Deadline = now.Add(10 * time.Second)
	}
	if meta.ID == "" {
		meta.ID = "1762429200000-000001"
	}
	watch, err := store.New(root).Create(meta)
	if err != nil {
		t.Fatalf("create watch: %v", err)
	}
	return watch
}

func TestCompileRejectsBadInstructions(t *testing.T) {
	cases := []store.Meta{
		{Mode: "poll", Match: "("},
		{Mode: "poll", Until: "["},
		{Mode: "poll", Interval: "soon"},
		{Mode: "poll", Interval: "-1s"},
		{Mode: "sometimes"},
	}
	for _, meta := range cases {
		if _, err := Compile(meta); err == nil {
			t.Fatalf("expected an error for %+v", meta)
		}
	}
	compiled, err := Compile(store.Meta{Mode: "poll"})
	if err != nil {
		t.Fatalf("compile defaults: %v", err)
	}
	if compiled.Interval != DefaultInterval {
		t.Fatalf("interval = %s, want %s", compiled.Interval, DefaultInterval)
	}
}

func TestPollRecordsOnlyMatchesAndEndsAtUntil(t *testing.T) {
	root := t.TempDir()
	watch := createWatch(t, root, store.Meta{
		Command:  "printf 'alpha\\nbeta\\ndone\\n'",
		Mode:     "poll",
		Interval: "20ms",
		Match:    "beta",
		Until:    "done",
	})
	if err := Run(root, watch.Meta.ID); err != nil {
		t.Fatalf("run: %v", err)
	}
	status, err := watch.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.State != store.StateUntil {
		t.Fatalf("state = %q, want %q", status.State, store.StateUntil)
	}
	unread, err := watch.Unread()
	if err != nil {
		t.Fatalf("unread: %v", err)
	}
	if len(unread) != 2 {
		t.Fatalf("events = %d, want 2: %v", len(unread), unread)
	}
	if unread[0].Kind != store.KindLine || unread[0].Line != "beta" {
		t.Fatalf("first event = %+v", unread[0])
	}
	if unread[1].Kind != store.KindUntil || unread[1].Line != "done" {
		t.Fatalf("second event = %+v", unread[1])
	}
}

func TestPollDeduplicatesConsecutiveLines(t *testing.T) {
	root := t.TempDir()
	watch := createWatch(t, root, store.Meta{
		Command:  "printf 'same\\nsame\\nsame\\n'",
		Mode:     "poll",
		Interval: "20ms",
		Deadline: time.Now().Add(120 * time.Millisecond),
	})
	if err := Run(root, watch.Meta.ID); err != nil {
		t.Fatalf("run: %v", err)
	}
	unread, err := watch.Unread()
	if err != nil {
		t.Fatalf("unread: %v", err)
	}
	if len(unread) != 2 || unread[0].Line != "same" || unread[1].Kind != store.KindExpired {
		t.Fatalf("events = %v, want one deduplicated line and the expiry notice", eventLines(unread))
	}
}

func TestDedupeCanBeDisabled(t *testing.T) {
	root := t.TempDir()
	disabled := false
	watch := createWatch(t, root, store.Meta{
		Command:  "printf 'twice\\ntwice\\nstop\\n'",
		Mode:     "poll",
		Interval: "20ms",
		Dedupe:   &disabled,
		Until:    "stop",
		Deadline: time.Now().Add(500 * time.Millisecond),
	})
	if err := Run(root, watch.Meta.ID); err != nil {
		t.Fatalf("run: %v", err)
	}
	unread, err := watch.Unread()
	if err != nil {
		t.Fatalf("unread: %v", err)
	}
	if len(unread) != 3 {
		t.Fatalf("events = %v, want two lines and the until notice", eventLines(unread))
	}
	if unread[0].Line != "twice" || unread[1].Line != "twice" || unread[2].Kind != store.KindUntil {
		t.Fatalf("events = %v", eventLines(unread))
	}
}

func TestPollStopsOnDeadlineWithAnEvent(t *testing.T) {
	root := t.TempDir()
	watch := createWatch(t, root, store.Meta{
		Command:  "true",
		Mode:     "poll",
		Interval: "20ms",
		Deadline: time.Now().Add(150 * time.Millisecond),
	})
	if err := Run(root, watch.Meta.ID); err != nil {
		t.Fatalf("run: %v", err)
	}
	status, err := watch.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.State != store.StateExpired {
		t.Fatalf("state = %q, want %q", status.State, store.StateExpired)
	}
	unread, err := watch.Unread()
	if err != nil {
		t.Fatalf("unread: %v", err)
	}
	if len(unread) != 1 || unread[0].Kind != store.KindExpired {
		t.Fatalf("events = %v, want one expired event", unread)
	}
}

func TestPollHonorsStopRequest(t *testing.T) {
	root := t.TempDir()
	watch := createWatch(t, root, store.Meta{
		Command:  "true",
		Mode:     "poll",
		Interval: "20ms",
	})
	if err := watch.RequestStop(); err != nil {
		t.Fatalf("request stop: %v", err)
	}
	if err := Run(root, watch.Meta.ID); err != nil {
		t.Fatalf("run: %v", err)
	}
	status, err := watch.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.State != store.StateStopped {
		t.Fatalf("state = %q, want %q", status.State, store.StateStopped)
	}
}

func TestStreamEndsAtUntil(t *testing.T) {
	root := t.TempDir()
	watch := createWatch(t, root, store.Meta{
		Command: "printf 'one\\ntwo\\nthree\\n'",
		Mode:    "stream",
		Match:   "t",
		Until:   "three",
	})
	if err := Run(root, watch.Meta.ID); err != nil {
		t.Fatalf("run: %v", err)
	}
	status, err := watch.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.State != store.StateUntil {
		t.Fatalf("state = %q, want %q", status.State, store.StateUntil)
	}
	unread, err := watch.Unread()
	if err != nil {
		t.Fatalf("unread: %v", err)
	}
	lines := eventLines(unread)
	if !strings.Contains(lines, "line:two") || !strings.Contains(lines, "until:three") {
		t.Fatalf("events = %q", lines)
	}
}

func TestStreamCommandExitIsRecorded(t *testing.T) {
	root := t.TempDir()
	watch := createWatch(t, root, store.Meta{
		Command: "printf 'only\\n'",
		Mode:    "stream",
	})
	if err := Run(root, watch.Meta.ID); err != nil {
		t.Fatalf("run: %v", err)
	}
	status, err := watch.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.State != store.StateStopped {
		t.Fatalf("state = %q, want %q", status.State, store.StateStopped)
	}
	unread, err := watch.Unread()
	if err != nil {
		t.Fatalf("unread: %v", err)
	}
	if len(unread) != 2 {
		t.Fatalf("events = %v, want a line and a stop notice", unread)
	}
	if unread[1].Kind != store.KindStopped {
		t.Fatalf("second event = %+v", unread[1])
	}
}

func TestFailedCommandRecordsOneError(t *testing.T) {
	root := t.TempDir()
	watch := createWatch(t, root, store.Meta{
		Command:  "echo broken >&2; exit 7",
		Mode:     "poll",
		Interval: "20ms",
		Deadline: time.Now().Add(120 * time.Millisecond),
	})
	if err := Run(root, watch.Meta.ID); err != nil {
		t.Fatalf("run: %v", err)
	}
	unread, err := watch.Unread()
	if err != nil {
		t.Fatalf("unread: %v", err)
	}
	errors := 0
	for _, event := range unread {
		if event.Kind == store.KindError {
			errors++
		}
	}
	if errors != 1 {
		t.Fatalf("error events = %d, want 1: %v", errors, eventLines(unread))
	}
}

func eventLines(events []store.Event) string {
	var builder strings.Builder
	for _, event := range events {
		builder.WriteString(event.Kind)
		builder.WriteString(":")
		builder.WriteString(event.Line)
		builder.WriteString("\n")
	}
	return builder.String()
}
