package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func testMeta(id string) Meta {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	return Meta{
		ID:        id,
		Command:   "echo hello",
		Mode:      "poll",
		CreatedAt: now,
		Deadline:  now.Add(30 * time.Minute),
	}
}

func testID(n int) string {
	return fmt.Sprintf("%013d-%06x", 1762429200000+n, n)
}

func TestCreateOpenList(t *testing.T) {
	root := testRoot(t)
	repository := New(root)
	first, err := repository.Create(testMeta(testID(1)))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repository.Create(testMeta(testID(2))); err != nil {
		t.Fatalf("create second: %v", err)
	}
	opened, err := repository.Open(first.Meta.ID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if opened.Meta.Command != "echo hello" {
		t.Fatalf("command = %q", opened.Meta.Command)
	}
	status, err := opened.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.State != StateStarting {
		t.Fatalf("state = %q, want %q", status.State, StateStarting)
	}
	watches, err := repository.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(watches) != 2 {
		t.Fatalf("listed %d watches, want 2", len(watches))
	}
}

func TestCreateRejectsInvalidInput(t *testing.T) {
	repository := New(testRoot(t))
	if _, err := repository.Create(Meta{ID: testID(1)}); err == nil {
		t.Fatal("expected an error for an empty command")
	}
	for _, id := range []string{"../../etc/passwd", "1234", testID(1) + "x"} {
		meta := testMeta(id)
		if _, err := repository.Create(meta); err == nil {
			t.Fatalf("expected an error for id %q", id)
		}
	}
	if _, err := repository.Open("../escape"); err == nil {
		t.Fatal("expected an error for a traversal id")
	}
}

func TestAppendUnreadAdvance(t *testing.T) {
	watch, err := New(testRoot(t)).Create(testMeta(testID(1)))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	base := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	for i := range 3 {
		err := watch.AppendEvent(Event{
			Time: base.Add(time.Duration(i) * time.Millisecond),
			Kind: KindLine,
			Line: fmt.Sprintf("line %d", i),
		})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	unread, err := watch.Unread()
	if err != nil {
		t.Fatalf("unread: %v", err)
	}
	if len(unread) != 3 {
		t.Fatalf("unread = %d, want 3", len(unread))
	}
	if unread[0].ID >= unread[1].ID || unread[1].ID >= unread[2].ID {
		t.Fatalf("event ids are not increasing: %v", unread)
	}
	if unread[0].WatchID != watch.Meta.ID {
		t.Fatalf("watch id = %q", unread[0].WatchID)
	}
	if err := watch.AdvanceCursor(unread[1].ID); err != nil {
		t.Fatalf("advance: %v", err)
	}
	unread, err = watch.Unread()
	if err != nil {
		t.Fatalf("unread after advance: %v", err)
	}
	if len(unread) != 1 || unread[0].Line != "line 2" {
		t.Fatalf("unread after advance = %v", unread)
	}
}

func TestCursorNeverRewinds(t *testing.T) {
	watch, err := New(testRoot(t)).Create(testMeta(testID(1)))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := watch.AppendEvent(Event{Kind: KindLine, Line: "first"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	unread, err := watch.Unread()
	if err != nil {
		t.Fatalf("unread: %v", err)
	}
	if err := watch.AdvanceCursor(unread[0].ID); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := watch.AdvanceCursor("0000000000001-000000"); err != nil {
		t.Fatalf("backward advance: %v", err)
	}
	cursor, err := watch.Cursor()
	if err != nil {
		t.Fatalf("cursor: %v", err)
	}
	if cursor != unread[0].ID {
		t.Fatalf("cursor = %q, want %q", cursor, unread[0].ID)
	}
}

func TestAppendPrunesOldestAndCounts(t *testing.T) {
	watch, err := New(testRoot(t)).Create(testMeta(testID(1)))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	base := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	for i := range MaxEvents + 5 {
		err := watch.AppendEvent(Event{
			Time: base.Add(time.Duration(i) * time.Millisecond),
			Kind: KindLine,
			Line: fmt.Sprintf("line %d", i),
		})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	unread, err := watch.Unread()
	if err != nil {
		t.Fatalf("unread: %v", err)
	}
	if len(unread) != MaxEvents {
		t.Fatalf("unread = %d, want %d", len(unread), MaxEvents)
	}
	dropped, err := watch.Dropped()
	if err != nil {
		t.Fatalf("dropped: %v", err)
	}
	if dropped != 5 {
		t.Fatalf("dropped = %d, want 5", dropped)
	}
	if unread[len(unread)-1].Line != fmt.Sprintf("line %d", MaxEvents+4) {
		t.Fatalf("newest event = %q", unread[len(unread)-1].Line)
	}
}

func TestEventLineIsTruncated(t *testing.T) {
	watch, err := New(testRoot(t)).Create(testMeta(testID(1)))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	long := make([]byte, MaxLineBytes*2)
	for i := range long {
		long[i] = 'a'
	}
	if err := watch.AppendEvent(Event{Kind: KindLine, Line: string(long)}); err != nil {
		t.Fatalf("append: %v", err)
	}
	unread, err := watch.Unread()
	if err != nil {
		t.Fatalf("unread: %v", err)
	}
	if len(unread[0].Line) > MaxLineBytes+len("…") {
		t.Fatalf("line length = %d, want at most %d", len(unread[0].Line), MaxLineBytes+len("…"))
	}
}

func TestDrainOrdersAcrossWatches(t *testing.T) {
	root := testRoot(t)
	repository := New(root)
	first, err := repository.Create(testMeta(testID(1)))
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := repository.Create(testMeta(testID(2)))
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	base := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	if err := first.AppendEvent(Event{Time: base.Add(time.Millisecond), Kind: KindLine, Line: "first"}); err != nil {
		t.Fatalf("append first: %v", err)
	}
	if err := second.AppendEvent(Event{Time: base.Add(2 * time.Millisecond), Kind: KindLine, Line: "second"}); err != nil {
		t.Fatalf("append second: %v", err)
	}
	items, err := Drain(root, Filter{})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if items[0].Event.Line != "first" || items[1].Event.Line != "second" {
		t.Fatalf("order = %q, %q", items[0].Event.Line, items[1].Event.Line)
	}
	again, err := Drain(root, Filter{})
	if err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second drain returned %d events", len(again))
	}
}

func TestDrainLimitLeavesRestUnread(t *testing.T) {
	root := testRoot(t)
	watch, err := New(root).Create(testMeta(testID(1)))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := range 4 {
		if err := watch.AppendEvent(Event{
			Time: time.Date(2026, 11, 6, 12, 0, 0, i*int(time.Millisecond), time.UTC),
			Kind: KindLine,
			Line: fmt.Sprintf("line %d", i),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	items, err := Drain(root, Filter{Limit: 2})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	remaining, err := Remaining(root, Filter{})
	if err != nil {
		t.Fatalf("remaining: %v", err)
	}
	if remaining != 2 {
		t.Fatalf("remaining = %d, want 2", remaining)
	}
}

func TestStopSentinel(t *testing.T) {
	watch, err := New(testRoot(t)).Create(testMeta(testID(1)))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if watch.StopRequested() {
		t.Fatal("stop requested before it was asked for")
	}
	if err := watch.RequestStop(); err != nil {
		t.Fatalf("request stop: %v", err)
	}
	if !watch.StopRequested() {
		t.Fatal("stop sentinel missing")
	}
}

func TestRemoveDeletesWatchDirectory(t *testing.T) {
	root := testRoot(t)
	watch, err := New(root).Create(testMeta(testID(1)))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := watch.Remove(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, watchesDir, watch.Meta.ID)); !os.IsNotExist(err) {
		t.Fatalf("watch directory still exists: %v", err)
	}
}
