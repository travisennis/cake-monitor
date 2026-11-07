// Package cli implements the cake-monitor command line.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/travisennis/cake-monitor/internal/hookio"
	"github.com/travisennis/cake-monitor/internal/ops"
	"github.com/travisennis/cake-monitor/internal/store"
	"github.com/travisennis/cake-monitor/internal/toolbox"
	"github.com/travisennis/cake-monitor/internal/version"
	"github.com/travisennis/cake-monitor/internal/watcher"
)

const usage = `cake-monitor — record background events and deliver them to cake

Usage:
  cake-monitor start --command CMD [options]   Start a detached watch
  cake-monitor list [--json]                   List watches
  cake-monitor read [--json] [ID...]           Drain unread events
  cake-monitor stop ID...                      Stop watches
  cake-monitor hook                            cake hook adapter (JSON on stdin)
  cake-monitor watch [--json]                  Stream new events as NDJSON
  cake-monitor prune --older-than 24h          Remove ended watches
  cake-monitor doctor                          Report store and install state
  cake-monitor install-tools                   Link the toolbox tools into cake
  cake-monitor uninstall-tools                 Remove those links
  cake-monitor describe TOOL                   Print one toolbox manifest
  cake-monitor execute TOOL                    Run one toolbox call from stdin
  cake-monitor version                         Print build identity

Start options:
  --command CMD      Shell command to run (required)
  --name NAME        Short label shown with events
  --match REGEX      Record only matching lines (omit for every line)
  --until REGEX      End the watch when a line matches
  --mode MODE        poll (default) or stream
  --interval DUR     Poll cadence, default 10s
  --deadline DUR     Watch lifetime, default 30m, maximum 24h
  --session ID       Scope hook delivery to one session (default CAKE_THREAD_ID)
  --cwd DIR          Working directory for the command
  --dedupe=false     Record repeated identical lines
  --json             Print JSON instead of text

Nothing in this program waits for new output. Events are recorded on disk;
a cake hook delivers unread events as additional context, and monitor_read
drains them on demand.
`

// Run executes one command and returns a process exit code.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "-version", "--version", "version":
		fmt.Fprintln(stdout, version.String())
		return 0
	case "__watch":
		return runWatcher(args[1:], stderr)
	case "start":
		return runStart(args[1:], stdout, stderr)
	case "list":
		return runList(args[1:], stdout, stderr)
	case "read":
		return runRead(args[1:], stdout, stderr)
	case "stop":
		return runStop(args[1:], stdout, stderr)
	case "hook":
		return runHook(args[1:], stdin, stdout, stderr)
	case "watch":
		return runWatch(args[1:], stdout, stderr)
	case "prune":
		return runPrune(args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(stdout, stderr)
	case "install-tools":
		return runInstallTools(stdout, stderr)
	case "uninstall-tools":
		return runUninstallTools(stdout, stderr)
	case "describe":
		return runDescribe(args[1:], stdout, stderr)
	case "execute":
		return runExecute(args[1:], stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "cake-monitor: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// RunTool serves one toolbox tool selected by the executable name.
func RunTool(name string, stdin io.Reader, stdout, stderr io.Writer) int {
	action := os.Getenv("TOOLBOX_ACTION")
	root, err := store.Root()
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	switch action {
	case toolbox.ActionDescribe:
		raw, err := toolbox.Describe(name)
		if err != nil {
			fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
			return 1
		}
		_, _ = stdout.Write(raw)
		fmt.Fprintln(stdout)
		return 0
	case toolbox.ActionExecute:
		args, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
		if err != nil {
			fmt.Fprintf(stderr, "cake-monitor: read arguments: %v\n", err)
			return 1
		}
		session := os.Getenv("CAKE_THREAD_ID")
		text, err := toolbox.Execute(root, name, args, session)
		if err != nil {
			fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, text)
		return 0
	default:
		fmt.Fprintf(stderr, "cake-monitor: TOOLBOX_ACTION must be describe or execute, got %q\n", action)
		return 1
	}
}

func runWatcher(args []string, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "cake-monitor: __watch takes one watch id")
		return 2
	}
	root, err := store.Root()
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	if err := watcher.Run(root, args[0]); err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	return 0
}

func runStart(args []string, stdout, stderr io.Writer) int {
	set := newFlagSet("start", stderr)
	command := set.String("command", "", "shell command to run")
	name := set.String("name", "", "short label")
	match := set.String("match", "", "record only matching lines")
	until := set.String("until", "", "end when a line matches")
	mode := set.String("mode", "poll", "poll or stream")
	interval := set.String("interval", "", "poll cadence")
	deadline := set.Duration("deadline", watcher.DefaultDeadline, "watch lifetime")
	session := set.String("session", os.Getenv("CAKE_THREAD_ID"), "session id for hook delivery")
	cwd := set.String("cwd", "", "working directory")
	dedupe := set.Bool("dedupe", true, "suppress identical consecutive lines")
	asJSON := set.Bool("json", false, "print JSON")
	if code := parse(set, args); code != 0 {
		return code
	}
	root, err := store.Root()
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	if strings.TrimSpace(*command) == "" {
		fmt.Fprintln(stderr, "cake-monitor: --command is required")
		return 2
	}
	watch, err := ops.Start(root, ops.StartOptions{
		Command:  *command,
		Name:     *name,
		Match:    *match,
		Until:    *until,
		Mode:     *mode,
		Interval: *interval,
		Deadline: *deadline,
		Session:  *session,
		Cwd:      *cwd,
		Dedupe:   dedupe,
	})
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	if *asJSON {
		return printJSON(stdout, watch.Meta, stderr)
	}
	fmt.Fprintf(stdout, "watch %s (%s) started: %s every %s, deadline %s\n",
		watch.Meta.ID,
		labelOf(watch.Meta),
		watch.Meta.Mode,
		intervalOf(watch.Meta),
		watch.Meta.Deadline.UTC().Format(time.RFC3339),
	)
	fmt.Fprintln(stdout, "Events are recorded on disk. Read them with `cake-monitor read`, or install the hook so cake receives them.")
	return 0
}

func runList(args []string, stdout, stderr io.Writer) int {
	set := newFlagSet("list", stderr)
	asJSON := set.Bool("json", false, "print JSON")
	if code := parse(set, args); code != 0 {
		return code
	}
	root, err := store.Root()
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	summaries, err := ops.List(root)
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	if *asJSON {
		return printJSON(stdout, summaries, stderr)
	}
	if len(summaries) == 0 {
		fmt.Fprintln(stdout, "No monitor watches.")
		return 0
	}
	writer := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tNAME\tSTATE\tMODE\tAGE\tUNREAD\tCOMMAND")
	for _, summary := range summaries {
		name := summary.Name
		if name == "" {
			name = "-"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			summary.ID, name, summary.State, summary.Mode, summary.Age, summary.Unread, shorten(summary.Command, 60))
	}
	_ = writer.Flush()
	return 0
}

func runRead(args []string, stdout, stderr io.Writer) int {
	set := newFlagSet("read", stderr)
	asJSON := set.Bool("json", false, "print JSON")
	limit := set.Int("limit", store.DefaultLimit, "maximum events")
	code := parse(set, args)
	if code != 0 {
		return code
	}
	root, err := store.Root()
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	ids := set.Args()
	if len(ids) == 0 {
		ids = []string{""}
	}
	all := []store.Item{}
	remaining, dropped := 0, 0
	for _, id := range ids {
		items, left, discarded, err := ops.Read(root, id, *limit)
		if err != nil {
			fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
			return 1
		}
		all = append(all, items...)
		remaining += left
		dropped += discarded
	}
	if *asJSON {
		document := map[string]any{
			"events":    all,
			"remaining": remaining,
			"dropped":   dropped,
		}
		return printJSON(stdout, document, stderr)
	}
	if len(all) == 0 {
		if remaining > 0 {
			fmt.Fprintf(stdout, "No new events returned; %d event(s) remain unread.\n", remaining)
			return 0
		}
		fmt.Fprintln(stdout, "No unread monitor events.")
		return 0
	}
	for _, item := range all {
		fmt.Fprintf(stdout, "[%s] %s %s: %s\n",
			item.Name,
			item.Event.Time.UTC().Format(time.RFC3339),
			item.Event.Kind,
			item.Event.Line,
		)
	}
	if remaining > 0 {
		fmt.Fprintf(stdout, "%d event(s) remain unread.\n", remaining)
	}
	if dropped > 0 {
		fmt.Fprintf(stdout, "%d earlier event(s) were discarded by the size bound.\n", dropped)
	}
	return 0
}

func runStop(args []string, stdout, stderr io.Writer) int {
	set := newFlagSet("stop", stderr)
	remove := set.Bool("remove", false, "delete the watch after stopping")
	if code := parse(set, args); code != 0 {
		return code
	}
	ids := set.Args()
	if len(ids) == 0 {
		fmt.Fprintln(stderr, "cake-monitor: stop takes at least one watch id")
		return 2
	}
	root, err := store.Root()
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	exit := 0
	for _, id := range ids {
		watch, err := store.New(root).Open(id)
		if err != nil {
			fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
			exit = 1
			continue
		}
		state, err := ops.Stop(root, id)
		if err != nil {
			fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
			exit = 1
			continue
		}
		fmt.Fprintf(stdout, "watch %s (%s) is %s\n", id, labelOf(watch.Meta), state)
		if *remove {
			if err := watch.Remove(); err != nil {
				fmt.Fprintf(stderr, "cake-monitor: remove %s: %v\n", id, err)
				exit = 1
			}
		}
	}
	return exit
}

func runHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	set := newFlagSet("hook", stderr)
	session := set.String("session", "", "session id to deliver to")
	limit := set.Int("limit", 20, "maximum events per delivery")
	if code := parse(set, args); code != 0 {
		return code
	}
	root, err := store.Root()
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	payload := hookio.ReadPayload(stdin)
	target := *session
	if target == "" {
		target = payload.SessionID
	}
	items, _, dropped, err := ops.Hook(root, target, *limit)
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	return printJSON(stdout, hookio.Result(payload, items, dropped), stderr)
}

func runWatch(args []string, stdout, stderr io.Writer) int {
	set := newFlagSet("watch", stderr)
	poll := set.Duration("poll", time.Second, "drain cadence")
	limit := set.Int("limit", store.DefaultLimit, "maximum events per drain")
	session := set.String("session", "", "limit to one session")
	iterations := set.Int("iterations", 0, "exit after N polls (0 means forever)")
	if code := parse(set, args); code != 0 {
		return code
	}
	root, err := store.Root()
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	encoder := json.NewEncoder(stdout)
	polls := 0
	for {
		items, err := store.Drain(root, store.Filter{Session: *session, Limit: *limit})
		if err != nil {
			fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
			return 1
		}
		for _, item := range items {
			if err := encoder.Encode(item); err != nil {
				fmt.Fprintf(stderr, "cake-monitor: write event: %v\n", err)
				return 1
			}
		}
		polls++
		if *iterations > 0 && polls >= *iterations {
			return 0
		}
		time.Sleep(*poll)
	}
}

func runPrune(args []string, stdout, stderr io.Writer) int {
	set := newFlagSet("prune", stderr)
	olderThan := set.Duration("older-than", 24*time.Hour, "remove ended watches older than this")
	if code := parse(set, args); code != 0 {
		return code
	}
	root, err := store.Root()
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	removed, err := ops.Prune(root, *olderThan)
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "removed %d watch(es)\n", removed)
	return 0
}

func runDoctor(stdout, stderr io.Writer) int {
	root, err := store.Root()
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "store: %s\n", root)
	summaries, err := ops.List(root)
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	live, stale, ended := 0, 0, 0
	for _, summary := range summaries {
		switch summary.State {
		case store.StateRunning, store.StateStarting:
			live++
		case ops.StateStale:
			stale++
		default:
			ended++
		}
	}
	fmt.Fprintf(stdout, "watches: %d live, %d stale, %d ended\n", live, stale, ended)
	if stale > 0 {
		fmt.Fprintln(stdout, "hint: `cake-monitor list` shows stale watches; remove them with `cake-monitor stop ID --remove`")
	}
	dir, err := ops.ToolboxDir()
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "toolbox directory: %s\n", dir)
	for _, name := range ops.ToolNames {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		switch {
		case err != nil:
			fmt.Fprintf(stdout, "  %s: missing\n", name)
		case info.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(path)
			fmt.Fprintf(stdout, "  %s: link -> %s\n", name, target)
		default:
			fmt.Fprintf(stdout, "  %s: regular file (left alone)\n", name)
		}
	}
	configDir := filepath.Dir(dir)
	executable, err := os.Executable()
	if err != nil {
		executable = "cake-monitor"
	}
	fmt.Fprintf(stdout, "\nhook: add to %s/hooks.json to deliver events automatically:\n", configDir)
	fmt.Fprintf(stdout, `{
  "version": 1,
  "hooks": {
    "PreToolUse": [{"hooks": [{"type": "command", "command": %q, "timeout": 5}]}],
    "SessionStart": [{"hooks": [{"type": "command", "command": %q, "timeout": 5}]}]
  }
}
`, executable+" hook", executable+" hook")
	return 0
}

func runInstallTools(stdout, stderr io.Writer) int {
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	installed, err := ops.InstallTools(executable)
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	for _, path := range installed {
		fmt.Fprintf(stdout, "linked %s\n", path)
	}
	if len(installed) < len(ops.ToolNames) {
		fmt.Fprintln(stdout, "note: a non-symlink file blocked one or more names; those were left alone")
	}
	return 0
}

func runUninstallTools(stdout, stderr io.Writer) int {
	removed, err := ops.UninstallTools()
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	for _, path := range removed {
		fmt.Fprintf(stdout, "removed %s\n", path)
	}
	if len(removed) == 0 {
		fmt.Fprintln(stdout, "no cake-monitor links found")
	}
	return 0
}

func runDescribe(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "cake-monitor: describe takes one tool name")
		return 2
	}
	raw, err := toolbox.Describe(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, string(raw))
	return 0
}

func runExecute(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "cake-monitor: execute takes one tool name")
		return 2
	}
	root, err := store.Root()
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: read arguments: %v\n", err)
		return 1
	}
	text, err := toolbox.Execute(root, args[0], raw, os.Getenv("CAKE_THREAD_ID"))
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, text)
	return 0
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	set := flag.NewFlagSet("cake-monitor "+name, flag.ContinueOnError)
	set.SetOutput(stderr)
	return set
}

func parse(set *flag.FlagSet, args []string) int {
	if err := set.Parse(args); err != nil {
		return 2
	}
	return 0
}

func printJSON(stdout io.Writer, value any, stderr io.Writer) int {
	raw, err := json.Marshal(value)
	if err != nil {
		fmt.Fprintf(stderr, "cake-monitor: encode output: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, string(raw))
	return 0
}

func labelOf(meta store.Meta) string {
	if meta.Name == "" {
		return "unnamed"
	}
	return meta.Name
}

func intervalOf(meta store.Meta) string {
	if meta.Interval == "" {
		return "10s"
	}
	return meta.Interval
}

func shorten(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
