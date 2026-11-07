package hookio

import (
	"strings"
	"testing"
	"time"

	"github.com/travisennis/cake-monitor/internal/store"
)

func item(name, line string) store.Item {
	return store.Item{
		WatchID: "1762429200000-000001",
		Name:    name,
		Event: store.Event{
			ID:   "1762429200000-000000",
			Time: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
			Kind: store.KindLine,
			Line: line,
		},
	}
}

func TestFormatEmpty(t *testing.T) {
	if got := Format(nil, 0); got != "" {
		t.Fatalf("Format(nil) = %q, want empty", got)
	}
	if len(Result(Payload{}, nil, 0)) != 0 {
		t.Fatal("Result(nil) should be an empty document")
	}
}

func TestResultRequestsFollowUpOnlyOnStop(t *testing.T) {
	items := []store.Item{item("ci", "build failed")}

	stop := Result(Payload{HookEventName: "Stop"}, items, 0)
	followUp, ok := stop["follow_up"].(bool)
	if !ok || !followUp {
		t.Fatalf("Stop follow_up = %v, want true", stop["follow_up"])
	}
	context, ok := stop["additional_context"].(string)
	if !ok || !strings.Contains(context, "build failed") {
		t.Fatalf("Stop context = %q", context)
	}

	for _, event := range []string{"PreToolUse", "PostToolUse", "SessionStart", "UserPromptSubmit", ""} {
		result := Result(Payload{HookEventName: event}, items, 0)
		if _, ok := result["follow_up"]; ok {
			t.Fatalf("%q result requested a follow-up: %v", event, result)
		}
		if _, ok := result["additional_context"]; !ok {
			t.Fatalf("%q result has no context: %v", event, result)
		}
	}
}

func TestResultWithoutEventsIsEmpty(t *testing.T) {
	for _, event := range []string{"Stop", "PreToolUse"} {
		if result := Result(Payload{HookEventName: event}, nil, 0); len(result) != 0 {
			t.Fatalf("%q result = %v, want empty", event, result)
		}
	}
}

func TestFormatRendersEveryEvent(t *testing.T) {
	context := Format([]store.Item{
		item("ci", "build failed"),
		item("logs", "second line"),
	}, 0)
	for _, want := range []string{
		"2 monitor event(s):",
		"[ci] 2026-11-06T12:00:00Z line: build failed",
		"[logs] 2026-11-06T12:00:00Z line: second line",
		"Act on them only where they change what you are doing.",
	} {
		if !strings.Contains(context, want) {
			t.Fatalf("context missing %q:\n%s", want, context)
		}
	}
}

func TestFormatCollapsesWhitespace(t *testing.T) {
	context := Format([]store.Item{item("ci", "a\n\tb   c")}, 0)
	if !strings.Contains(context, "line: a b c") {
		t.Fatalf("context = %q", context)
	}
}

func TestFormatRespectsByteCap(t *testing.T) {
	items := make([]store.Item, 0, 20)
	long := strings.Repeat("x", 400)
	for range 20 {
		items = append(items, item("chatty", long))
	}
	context := Format(items, 0)
	if len(context) > MaxContextBytes+200 {
		t.Fatalf("context length = %d, want about %d", len(context), MaxContextBytes)
	}
	if !strings.Contains(context, "more event(s); call monitor_read to drain them") {
		t.Fatalf("context does not report omitted events:\n%s", context)
	}
}

func TestFormatReportsDropped(t *testing.T) {
	context := Format([]store.Item{item("ci", "one")}, 4)
	if !strings.Contains(context, "4 earlier event(s) were discarded") {
		t.Fatalf("context = %q", context)
	}
}

func TestReadPayload(t *testing.T) {
	payload := ReadPayload(strings.NewReader(`{"hook_event_name":"PreToolUse","session_id":"abc"}`))
	if payload.SessionID != "abc" || payload.HookEventName != "PreToolUse" {
		t.Fatalf("payload = %+v", payload)
	}
	if got := ReadPayload(strings.NewReader("not json")); got.SessionID != "" {
		t.Fatalf("unexpected payload for garbage input: %+v", got)
	}
}
