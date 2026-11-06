// Package toolbox implements cake's toolbox protocol for the monitor tools.
//
// One binary serves four tools; cake discovers each by file name in its global
// toolbox directory and registers it as tb__<name>. Every action returns
// immediately: no tool in this package waits for new output.
package toolbox

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/travisennis/cake-monitor/internal/ops"
	"github.com/travisennis/cake-monitor/internal/store"
	"github.com/travisennis/cake-monitor/internal/watcher"
)

// Actions selected by cake's TOOLBOX_ACTION environment variable.
const (
	ActionDescribe = "describe"
	ActionExecute  = "execute"
)

// DefaultReadLimit is how many events one read returns. The cap keeps a tool
// result inside cake's output bound even when lines are long.
const DefaultReadLimit = 10

// displayLineBytes bounds one rendered event line.
const displayLineBytes = 500

// Name maps an executable basename to a toolbox tool name.
func Name(base string) (string, bool) {
	for _, name := range ops.ToolNames {
		if name == base {
			return name, true
		}
	}
	return "", false
}

// Manifest is cake's JSON describe document.
type Manifest struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Replay      string         `json:"replay"`
}

// Describe returns one tool's manifest.
func Describe(name string) ([]byte, error) {
	manifest, ok := manifests()[name]
	if !ok {
		return nil, fmt.Errorf("unknown monitor tool %q", name)
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode describe document: %w", err)
	}
	return raw, nil
}

// Execute runs one tool call and returns its model-facing text.
func Execute(root, name string, args []byte, session string) (string, error) {
	switch name {
	case "monitor_start":
		return start(root, args, session)
	case "monitor_list":
		return list(root)
	case "monitor_read":
		return read(root, args)
	case "monitor_stop":
		return stop(root, args)
	default:
		return "", fmt.Errorf("unknown monitor tool %q", name)
	}
}

func start(root string, args []byte, session string) (string, error) {
	var request struct {
		Command  string `json:"command"`
		Name     string `json:"name"`
		Match    string `json:"match"`
		Until    string `json:"until"`
		Mode     string `json:"mode"`
		Interval string `json:"interval"`
		Deadline string `json:"deadline"`
		Session  string `json:"session"`
		Cwd      string `json:"cwd"`
		Dedupe   *bool  `json:"dedupe"`
	}
	if err := json.Unmarshal(args, &request); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	deadline := watcher.DefaultDeadline
	if request.Deadline != "" {
		parsed, err := time.ParseDuration(request.Deadline)
		if err != nil || parsed <= 0 {
			return "", fmt.Errorf("invalid deadline %q: use a positive duration such as 30m", request.Deadline)
		}
		deadline = parsed
	}
	if request.Session == "" {
		request.Session = session
	}
	watch, err := ops.Start(root, ops.StartOptions{
		Command:  request.Command,
		Name:     request.Name,
		Match:    request.Match,
		Until:    request.Until,
		Mode:     request.Mode,
		Interval: request.Interval,
		Deadline: deadline,
		Session:  request.Session,
		Cwd:      request.Cwd,
		Dedupe:   request.Dedupe,
	})
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "Started monitor %s.", labelled(watch.Meta))
	fmt.Fprintf(&builder, " It runs detached and keeps recording after this turn, until %s.",
		watch.Meta.Deadline.UTC().Format(time.RFC3339))
	builder.WriteString(" Nothing here blocks: call monitor_read to drain events, or install the cake-monitor hook")
	builder.WriteString(" so unread events arrive with your next tool call.")
	return builder.String(), nil
}

func list(root string) (string, error) {
	summaries, err := ops.List(root)
	if err != nil {
		return "", err
	}
	if len(summaries) == 0 {
		return "No monitor watches.", nil
	}
	var builder strings.Builder
	for _, summary := range summaries {
		name := summary.Name
		if name == "" {
			name = summary.ID
		}
		fmt.Fprintf(&builder, "%s %s %s age=%s unread=%d — %s\n",
			summary.ID, name, summary.State, summary.Age, summary.Unread, summary.Command)
	}
	return strings.TrimRight(builder.String(), "\n"), nil
}

func read(root string, args []byte) (string, error) {
	var request struct {
		Watch string `json:"watch"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &request); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	limit := request.Limit
	if limit <= 0 {
		limit = DefaultReadLimit
	}
	items, remaining, dropped, err := ops.Read(root, request.Watch, limit)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		if remaining > 0 {
			return fmt.Sprintf("No new events returned; %d event(s) remain unread.", remaining), nil
		}
		return "No unread monitor events.", nil
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "%d monitor event(s):\n", len(items))
	for _, item := range items {
		fmt.Fprintf(&builder, "[%s] %s %s: %s\n",
			item.Name,
			item.Event.Time.UTC().Format(time.RFC3339),
			item.Event.Kind,
			displayLine(item.Event.Line),
		)
	}
	if remaining > 0 {
		fmt.Fprintf(&builder, "%d event(s) remain unread.\n", remaining)
	}
	if dropped > 0 {
		fmt.Fprintf(&builder, "%d earlier event(s) were discarded by the size bound.\n", dropped)
	}
	return strings.TrimRight(builder.String(), "\n"), nil
}

func stop(root string, args []byte) (string, error) {
	var request struct {
		Watch string `json:"watch"`
	}
	if err := json.Unmarshal(args, &request); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if request.Watch == "" {
		return "", fmt.Errorf("watch is required")
	}
	watch, err := store.New(root).Open(request.Watch)
	if err != nil {
		return "", err
	}
	state, err := ops.Stop(root, request.Watch)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Monitor %s is %s. Recorded events stay readable with monitor_read.",
		labelled(watch.Meta), state), nil
}

func labelled(meta store.Meta) string {
	if meta.Name == "" {
		return fmt.Sprintf("watch %s", meta.ID)
	}
	return fmt.Sprintf("%q (watch %s)", meta.Name, meta.ID)
}

func displayLine(text string) string {
	compact := strings.Join(strings.Fields(text), " ")
	if len(compact) <= displayLineBytes {
		return compact
	}
	cut := displayLineBytes
	for cut > 0 && (compact[cut]&0xc0) == 0x80 {
		cut--
	}
	return compact[:cut] + "…"
}

func manifests() map[string]Manifest {
	return map[string]Manifest{
		"monitor_start": {
			Name: "monitor_start",
			Description: "Start a background watch that records matched output for this session and returns immediately. " +
				"A poll watch runs the command every interval and reads its stdout; a stream watch runs the command once " +
				"and reads stdout as lines arrive. The watch runs detached, survives this turn, and ends at its deadline, " +
				"when its until pattern matches, or when monitor_stop ends it. Nothing blocks: call monitor_read to drain " +
				"recorded events, or install the cake-monitor hook so unread events arrive as additional context with your " +
				"next tool call.",
			InputSchema: schema(map[string]any{
				"command":  prop("string", "Shell command to run. Poll mode runs it each interval; stream mode runs it once and reads stdout lines continuously. Add 2>&1 yourself when stderr matters."),
				"name":     prop("string", "Short label shown with the watch and with every event."),
				"match":    prop("string", "RE2 regular expression. Only matching non-empty lines become events; omit to record every line."),
				"until":    prop("string", "RE2 regular expression. The watch ends when a line matches, and records that line."),
				"mode":     prop("string", "poll (default) for periodic status commands, stream for logs and long-running output."),
				"interval": prop("string", "Poll cadence such as 10s. Poll mode only; defaults to 10s."),
				"deadline": prop("string", "How long the watch may run, such as 30m. Defaults to 30m; 24h maximum."),
				"session":  prop("string", "Session id to scope hook delivery to. Defaults to this session; unscoped watches deliver to every session."),
				"cwd":      prop("string", "Working directory for the command. Defaults to the session working directory."),
				"dedupe":   prop("boolean", "Suppress a line identical to the previous recorded line. Defaults to true; set false to record every poll's output."),
			}, "command"),
			Replay: "never",
		},
		"monitor_list": {
			Name:        "monitor_list",
			Description: "List background monitor watches with their state, age, unread event count, and command. Returns immediately.",
			InputSchema: schema(map[string]any{}),
			Replay:      "never",
		},
		"monitor_read": {
			Name: "monitor_read",
			Description: "Drain unread events from one or all background watches and advance their cursors. Returns immediately; " +
				"it never waits for new output. Omitted events stay unread for the next call.",
			InputSchema: schema(map[string]any{
				"watch": prop("string", "Watch id to drain. Omit to drain every watch."),
				"limit": prop("integer", "Maximum events to return, up to 100. Defaults to 10."),
			}),
			Replay: "never",
		},
		"monitor_stop": {
			Name:        "monitor_stop",
			Description: "Stop a background watch and end its process group. Recorded events stay readable with monitor_read.",
			InputSchema: schema(map[string]any{
				"watch": prop("string", "Watch id to stop."),
			}, "watch"),
			Replay: "never",
		},
	}
}

func schema(properties map[string]any, required ...string) map[string]any {
	document := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		document["required"] = required
	}
	return document
}

func prop(kind, description string) map[string]any {
	return map[string]any{"type": kind, "description": description}
}
