package ops

import (
	"fmt"
	"testing"
	"time"

	"github.com/travisennis/cake-monitor/internal/store"
	"github.com/travisennis/cake-monitor/internal/watcher"
)

// stubSpawn records watches without starting a process.
func stubSpawn(t *testing.T) {
	t.Helper()
	original := spawnWatcher
	spawnWatcher = func(string, *store.Watch) error { return nil }
	t.Cleanup(func() { spawnWatcher = original })
}

func testID(n int) string {
	return fmt.Sprintf("%013d-%06x", 1762429200000+n, n)
}

func TestStartAppliesDefaults(t *testing.T) {
	stubSpawn(t)
	root := t.TempDir()
	watch, err := Start(root, StartOptions{
		Command: "echo hello",
		Cwd:     "/tmp",
		Session: "session-1",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if watch.Meta.Mode != "poll" {
		t.Fatalf("mode = %q", watch.Meta.Mode)
	}
	if watch.Meta.Session != "session-1" {
		t.Fatalf("session = %q", watch.Meta.Session)
	}
	remaining := time.Until(watch.Meta.Deadline)
	if remaining < watcher.DefaultDeadline-time.Minute || remaining > watcher.DefaultDeadline {
		t.Fatalf("deadline is %s away, want about %s", remaining, watcher.DefaultDeadline)
	}
	if _, err := store.New(root).Open(watch.Meta.ID); err != nil {
		t.Fatalf("watch is not readable: %v", err)
	}
}

func TestStartRejectsBadInput(t *testing.T) {
	stubSpawn(t)
	root := t.TempDir()
	cases := []StartOptions{
		{Command: ""},
		{Command: "true", Match: "("},
		{Command: "true", Until: "["},
		{Command: "true", Mode: "burn"},
		{Command: "true", Deadline: watcher.MaxDeadline + time.Hour},
	}
	for _, options := range cases {
		if _, err := Start(root, options); err == nil {
			t.Fatalf("expected an error for %+v", options)
		}
	}
}

func TestReadDrainsAndHookScopesBySession(t *testing.T) {
	stubSpawn(t)
	root := t.TempDir()
	first := mustStart(t, root, StartOptions{Command: "true", Session: "s1"})
	second := mustStart(t, root, StartOptions{Command: "true", Session: "s2"})
	shared := mustStart(t, root, StartOptions{Command: "true"})
	appendEvent(t, first, "first")
	appendEvent(t, second, "second")
	appendEvent(t, shared, "shared")

	items, remaining, _, err := Hook(root, "s1", 10)
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("hook items = %d, want 2: %v", len(items), items)
	}
	if remaining != 0 {
		t.Fatalf("remaining = %d, want 0", remaining)
	}
	for _, delivered := range items {
		if delivered.WatchID == second.Meta.ID {
			t.Fatalf("hook delivered another session's event: %+v", delivered)
		}
	}

	items, _, _, err = Read(root, "", 10)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(items) != 1 || items[0].WatchID != second.Meta.ID {
		t.Fatalf("read items = %v, want the second session's event", items)
	}
}

func TestReadLimitLeavesRemainder(t *testing.T) {
	stubSpawn(t)
	root := t.TempDir()
	watch := mustStart(t, root, StartOptions{Command: "true"})
	for i := range 3 {
		appendEvent(t, watch, fmt.Sprintf("line %d", i))
	}
	items, remaining, _, err := Read(root, watch.Meta.ID, 2)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(items) != 2 || remaining != 1 {
		t.Fatalf("items = %d, remaining = %d, want 2 and 1", len(items), remaining)
	}
}

func TestStopEndsAWatchWithoutAProcess(t *testing.T) {
	stubSpawn(t)
	root := t.TempDir()
	watch := mustStart(t, root, StartOptions{Command: "true"})
	state, err := Stop(root, watch.Meta.ID)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if state != store.StateStopped {
		t.Fatalf("state = %q", state)
	}
	status, err := watch.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.State != store.StateStopped {
		t.Fatalf("recorded state = %q", status.State)
	}
}

func TestListMarksStaleWatches(t *testing.T) {
	stubSpawn(t)
	root := t.TempDir()
	if _, err := Start(root, StartOptions{Command: "true", Cwd: "/tmp"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	// A watch that never ran and has no process reads as stale rather than
	// running.
	summaries, err := List(root)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries = %d, want 1", len(summaries))
	}
	if summaries[0].State != StateStale {
		t.Fatalf("state = %q, want %q", summaries[0].State, StateStale)
	}
}

func TestPruneRemovesOldEndedWatches(t *testing.T) {
	stubSpawn(t)
	root := t.TempDir()
	watch := mustStart(t, root, StartOptions{Command: "true"})
	if _, err := Stop(root, watch.Meta.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// The watch was created a moment ago, so a short age keeps it.
	removed, err := Prune(root, time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if _, err := store.New(root).Open(watch.Meta.ID); err != nil {
		t.Fatalf("watch was removed: %v", err)
	}
}

func mustStart(t *testing.T, root string, options StartOptions) *store.Watch {
	t.Helper()
	if options.Cwd == "" {
		options.Cwd = "/tmp"
	}
	watch, err := Start(root, options)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return watch
}

func appendEvent(t *testing.T, watch *store.Watch, line string) {
	t.Helper()
	err := watch.AppendEvent(store.Event{
		Time: time.Now().UTC(),
		Kind: store.KindLine,
		Line: line,
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
}
