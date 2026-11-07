// Package hookio renders unread monitor events as a cake hook result.
//
// Cake appends a hook's `additional_context` to a tool result (PreToolUse,
// PostToolUse) or to the turn's developer context (SessionStart,
// UserPromptSubmit). At Stop, a result may also set `follow_up` to start one
// bounded follow-up turn in the same invocation. That is the push channel: the
// model receives events without asking for them and without any call blocking.
package hookio

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/travisennis/cake-monitor/internal/store"
)

// MaxContextBytes bounds one injection. Cake truncates hook output anyway, and
// a wall of log lines would crowd out the work in progress.
const MaxContextBytes = 2000

// eventStop is the Cake hook event that ends a turn. Cake may start one bounded
// follow-up turn when a Stop result asks for it.
const eventStop = "Stop"

// Payload is the subset of cake's hook input this adapter uses. Cake names the
// event field `hook_event_name` in every hook payload.
type Payload struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
}

// ReadPayload decodes cake's hook input, tolerating anything unexpected: an
// unreadable payload must not stop event delivery.
func ReadPayload(r io.Reader) Payload {
	var payload Payload
	raw, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return payload
	}
	_ = json.Unmarshal(raw, &payload)
	return payload
}

// Format renders events as compact model-readable text.
func Format(items []store.Item, dropped int) string {
	if len(items) == 0 {
		return ""
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "%d monitor event(s):\n", len(items))
	omitted := 0
	for _, item := range items {
		line := fmt.Sprintf("[%s] %s %s: %s\n",
			item.Name,
			item.Event.Time.UTC().Format(time.RFC3339),
			item.Event.Kind,
			oneLine(item.Event.Line),
		)
		if builder.Len()+len(line) > MaxContextBytes {
			omitted++
			continue
		}
		builder.WriteString(line)
	}
	if omitted > 0 {
		fmt.Fprintf(&builder, "… and %d more event(s); call monitor_read to drain them.\n", omitted)
	}
	if dropped > 0 {
		fmt.Fprintf(&builder, "%d earlier event(s) were discarded by the size bound.\n", dropped)
	}
	builder.WriteString("These arrived from background watch(es). Act on them only where they change what you are doing.")
	return builder.String()
}

// Result is the hook's stdout document. An empty map is a valid no-op result.
// At Stop, a non-empty result also asks Cake for a follow-up turn so pending
// events drive the next request instead of waiting for a new prompt. Cake owns
// the cap: it ignores the request when the budget is exhausted or the run has
// an output schema.
func Result(payload Payload, items []store.Item, dropped int) map[string]any {
	context := Format(items, dropped)
	if context == "" {
		return map[string]any{}
	}
	result := map[string]any{"additional_context": context}
	if payload.HookEventName == eventStop {
		result["follow_up"] = true
	}
	return result
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
