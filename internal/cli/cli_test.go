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
	for _, event := range []string{"PreToolUse", "Stop"} {
		t.Run(event, func(t *testing.T) {
			testRoot(t)
			var stdout, stderr bytes.Buffer
			payload := fmt.Sprintf(`{"hook_event_name":%q,"session_id":"s1"}`, event)
			code := Run([]string{"hook"}, strings.NewReader(payload), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("code = %d, stderr = %s", code, stderr.String())
			}
			if strings.TrimSpace(stdout.String()) != "{}" {
				t.Fatalf("stdout = %q", stdout.String())
			}
		})
	}
}

func TestHookDeliversSessionEventsAsAdditionalContext(t *testing.T) {
	root := testRoot(t)
	seedEvent(t, root, "s1", "build failed")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"hook"}, strings.NewReader(`{"hook_event_name":"PreToolUse","session_id":"s1"}`), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	var result map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode %q: %v", stdout.String(), err)
	}
	context, ok := result["additional_context"].(string)
	if !ok {
		t.Fatalf("no additional_context in %v", result)
	}
	if !strings.Contains(context, "build failed") {
		t.Fatalf("context = %q", context)
	}
	if _, ok := result["follow_up"]; ok {
		t.Fatalf("non-Stop result requested a follow-up: %v", result)
	}
	// A second hook call must not repeat the delivery.
	stdout.Reset()
	code = Run([]string{"hook"}, strings.NewReader(`{"hook_event_name":"PostToolUse","session_id":"s1"}`), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("second code = %d", code)
	}
	if strings.TrimSpace(stdout.String()) != "{}" {
		t.Fatalf("second stdout = %q", stdout.String())
	}
}

func TestHookStopRequestsFollowUpTurn(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"without budget fields", `{"hook_event_name":"Stop","session_id":"s1"}`},
		{"with budget fields", `{"hook_event_name":"Stop","session_id":"s1","follow_up_index":0,"max_follow_ups":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := testRoot(t)
			seedEvent(t, root, "s1", "build failed")
			var stdout, stderr bytes.Buffer
			code := Run([]string{"hook"}, strings.NewReader(tc.payload), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("code = %d, stderr = %s", code, stderr.String())
			}
			var result map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("decode %q: %v", stdout.String(), err)
			}
			followUp, ok := result["follow_up"].(bool)
			if !ok || !followUp {
				t.Fatalf("follow_up = %v, want true in %v", result["follow_up"], result)
			}
			context, ok := result["additional_context"].(string)
			if !ok || !strings.Contains(context, "build failed") {
				t.Fatalf("context = %q", context)
			}

			// The drained event must not request another follow-up.
			stdout.Reset()
			code = Run([]string{"hook"}, strings.NewReader(tc.payload), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("second code = %d, stderr = %s", code, stderr.String())
			}
			if strings.TrimSpace(stdout.String()) != "{}" {
				t.Fatalf("second stdout = %q", stdout.String())
			}
		})
	}
}

func TestHookIgnoresAnotherSessionsEvents(t *testing.T) {
	root := testRoot(t)
	seedEvent(t, root, "s2", "not yours")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"hook"}, strings.NewReader(`{"hook_event_name":"Stop","session_id":"s1"}`), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if strings.TrimSpace(stdout.String()) != "{}" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestDoctorPrintsEveryHookChannel(t *testing.T) {
	testRoot(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := Run([]string{"doctor"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	for _, event := range []string{"PreToolUse", "SessionStart", "Stop"} {
		if !strings.Contains(stdout.String(), `"`+event+`"`) {
			t.Fatalf("doctor output has no %s hook:\n%s", event, stdout.String())
		}
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
