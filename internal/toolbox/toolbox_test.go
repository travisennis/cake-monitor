package toolbox

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/travisennis/cake-monitor/internal/ops"
	"github.com/travisennis/cake-monitor/internal/store"
)

func createWatch(t *testing.T, root string) *store.Watch {
	t.Helper()
	now := time.Now().UTC()
	watch, err := store.New(root).Create(store.Meta{
		ID:        "1762429200000-000001",
		Name:      "ci",
		Command:   "gh run view",
		Mode:      "poll",
		CreatedAt: now,
		Deadline:  now.Add(30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return watch
}

func TestDescribeDocumentsEveryTool(t *testing.T) {
	for _, name := range ops.ToolNames {
		raw, err := Describe(name)
		if err != nil {
			t.Fatalf("describe %s: %v", name, err)
		}
		var manifest Manifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		if manifest.Name != name {
			t.Fatalf("%s manifest name = %q", name, manifest.Name)
		}
		if manifest.Description == "" {
			t.Fatalf("%s has no description", name)
		}
		if manifest.InputSchema["type"] != "object" {
			t.Fatalf("%s schema type = %v", name, manifest.InputSchema["type"])
		}
		if manifest.Replay != "never" {
			t.Fatalf("%s replay = %q", name, manifest.Replay)
		}
	}
	if _, err := Describe("monitor_watch"); err == nil {
		t.Fatal("expected an error for an unknown tool")
	}
}

func TestNameMapsOnlyKnownTools(t *testing.T) {
	for _, name := range ops.ToolNames {
		if got, ok := Name(name); !ok || got != name {
			t.Fatalf("Name(%q) = %q, %v", name, got, ok)
		}
	}
	if _, ok := Name("cake-monitor"); ok {
		t.Fatal("the binary name must not select a tool")
	}
}

func TestReadOnEmptyStore(t *testing.T) {
	text, err := Execute(t.TempDir(), "monitor_read", []byte(`{}`), "")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(text, "No unread monitor events") {
		t.Fatalf("text = %q", text)
	}
}

func TestListOnEmptyStore(t *testing.T) {
	text, err := Execute(t.TempDir(), "monitor_list", nil, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if text != "No monitor watches." {
		t.Fatalf("text = %q", text)
	}
}

func TestReadRendersRecordedEvents(t *testing.T) {
	root := t.TempDir()
	watch := createWatch(t, root)
	if err := watch.AppendEvent(store.Event{
		Time: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
		Kind: store.KindLine,
		Line: "build failed",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	text, err := Execute(root, "monitor_read", []byte(`{"limit":5}`), "")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(text, "1 monitor event(s)") {
		t.Fatalf("text = %q", text)
	}
	if !strings.Contains(text, "[ci] 2026-11-06T12:00:00Z line: build failed") {
		t.Fatalf("text = %q", text)
	}
	again, err := Execute(root, "monitor_read", []byte(`{}`), "")
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if !strings.Contains(again, "No unread monitor events") {
		t.Fatalf("second read = %q", again)
	}
}

func TestStartRejectsInvalidPatternWithoutSpawning(t *testing.T) {
	text, err := Execute(t.TempDir(), "monitor_start", []byte(`{"command":"true","match":"("}`), "")
	if err == nil {
		t.Fatalf("expected an error, got %q", text)
	}
	if !strings.Contains(err.Error(), "invalid match pattern") {
		t.Fatalf("error = %v", err)
	}
}

func TestStartRequiresACommand(t *testing.T) {
	if _, err := Execute(t.TempDir(), "monitor_start", []byte(`{}`), ""); err == nil {
		t.Fatal("expected an error for a missing command")
	}
}

func TestStopEndsAWatch(t *testing.T) {
	root := t.TempDir()
	watch := createWatch(t, root)
	text, err := Execute(root, "monitor_stop", []byte(`{"watch":"`+watch.Meta.ID+`"}`), "")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !strings.Contains(text, "is stopped") {
		t.Fatalf("text = %q", text)
	}
	if _, err := Execute(root, "monitor_stop", []byte(`{}`), ""); err == nil {
		t.Fatal("expected an error for a missing watch id")
	}
}

func TestExecuteRejectsUnknownTool(t *testing.T) {
	if _, err := Execute(t.TempDir(), "monitor_watch", nil, ""); err == nil {
		t.Fatal("expected an error")
	}
}

func TestDisplayLineIsBounded(t *testing.T) {
	long := strings.Repeat("x", displayLineBytes*2)
	got := displayLine(long)
	if len(got) > displayLineBytes+len("…") {
		t.Fatalf("length = %d", len(got))
	}
}
