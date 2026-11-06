package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/travisennis/cake-monitor/internal/store"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(args, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func testRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CAKE_MONITOR_DATA_DIR", root)
	return root
}

func seedEvent(t *testing.T, root, session, line string) *store.Watch {
	t.Helper()
	now := time.Now().UTC()
	watch, err := store.New(root).Create(store.Meta{
		ID:        fmt.Sprintf("%013d-%06x", now.UnixMilli(), now.UnixNano()&0xffffff),
		Name:      "ci",
		Command:   "gh run view",
		Mode:      "poll",
		Session:   session,
		CreatedAt: now,
		Deadline:  now.Add(30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := watch.AppendEvent(store.Event{
		Time: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
		Kind: store.KindLine,
		Line: line,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	return watch
}

func TestHookWithoutEventsPrintsAnEmptyResult(t *testing.T) {
	testRoot(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"hook"}, strings.NewReader(`{"event":"PreToolUse","session_id":"s1"}`), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "{}" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestHookDeliversSessionEventsAsAdditionalContext(t *testing.T) {
	root := testRoot(t)
	seedEvent(t, root, "s1", "build failed")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"hook"}, strings.NewReader(`{"event":"PreToolUse","session_id":"s1"}`), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	var result map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode %q: %v", stdout.String(), err)
	}
	context, ok := result["additional_context"]
	if !ok {
		t.Fatalf("no additional_context in %v", result)
	}
	if !strings.Contains(context, "build failed") {
		t.Fatalf("context = %q", context)
	}
	// A second hook call must not repeat the delivery.
	stdout.Reset()
	code = Run([]string{"hook"}, strings.NewReader(`{"event":"PostToolUse","session_id":"s1"}`), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("second code = %d", code)
	}
	if strings.TrimSpace(stdout.String()) != "{}" {
		t.Fatalf("second stdout = %q", stdout.String())
	}
}

func TestHookIgnoresAnotherSessionsEvents(t *testing.T) {
	root := testRoot(t)
	seedEvent(t, root, "s2", "not yours")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"hook"}, strings.NewReader(`{"session_id":"s1"}`), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if strings.TrimSpace(stdout.String()) != "{}" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestReadJSONReportsRemaining(t *testing.T) {
	root := testRoot(t)
	seedEvent(t, root, "", "one")
	seedEvent(t, root, "", "two")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"read", "--json", "--limit", "1"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	var document struct {
		Events    []store.Item `json:"events"`
		Remaining int          `json:"remaining"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(document.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(document.Events))
	}
	if document.Remaining != 1 {
		t.Fatalf("remaining = %d, want 1", document.Remaining)
	}
}

func TestListJSON(t *testing.T) {
	root := testRoot(t)
	seedEvent(t, root, "", "one")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"list", "--json"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	var summaries []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &summaries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(summaries) != 1 || summaries[0]["name"] != "ci" {
		t.Fatalf("summaries = %v", summaries)
	}
}

func TestDescribeIsValidJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"describe", "monitor_start"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	var manifest map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if manifest["name"] != "monitor_start" {
		t.Fatalf("manifest = %v", manifest)
	}
}

func TestUnknownCommandExitsTwo(t *testing.T) {
	testRoot(t)
	code, _, stderr := run(t, "nope")
	if code != 2 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(stderr, "unknown command") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestVersionPrintsRevisionLine(t *testing.T) {
	code, stdout, _ := run(t, "-version")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(stdout, "revision ") {
		t.Fatalf("stdout = %q", stdout)
	}
}
