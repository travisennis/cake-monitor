# cake-monitor

Background watches for [Cake](https://github.com/travisennis/cake) that never
block the agent.

Cake already has `BashSession`, which reads new output from a background command
when the model asks, and can wait up to 120 seconds for it. That wait is fine
when Cake runs alone, but inside `cake-repl` it holds the whole session: no
prompt, no commands, nothing until the wait ends. `cake-monitor` removes the wait
instead of shortening it.

A watch is a detached process that records matching output to disk. Cake learns
about recorded events in four ways, none of which waits:

| Channel | When it delivers | Configuration |
| --- | --- | --- |
| `tb__monitor_read` | Whenever the model asks | Toolbox symlinks |
| `PreToolUse` hook `additional_context` | Appended to the next tool result | `hooks.json` |
| `SessionStart` / `UserPromptSubmit` hook `additional_context` | Appended to the next turn's developer context | `hooks.json` |
| `Stop` hook `additional_context` + `follow_up` | Starts one bounded follow-up turn in the same invocation | `hooks.json` |

`cake-monitor watch --json` streams new events as NDJSON for a front end that
owns the turn loop. That is how a future `cake-repl` integration reports an
event while you are idle; Cake itself cannot be woken once it exits.

## Install

```sh
git clone git@github.com:travisennis/cake-monitor.git ~/Projects/cake/cake-monitor
cd ~/Projects/cake/cake-monitor
just hooks         # conventional-commit hook
just ci
just install       # go install the CLI
just install-tools # link the four toolbox tools into cake
just doctor
```

`just install-tools` symlinks `monitor_start`, `monitor_list`, `monitor_read`,
and `monitor_stop` into `${XDG_CONFIG_HOME:-$HOME/.config}/cake/tools`, all
pointing at `dist/cake-monitor`. Cake follows symlinks during discovery and
registers them as `tb__monitor_start` and so on. If you keep an explicit
`[tools] enabled` list in `~/.config/cake/settings.toml`, add all four names.
Under `--sandbox read-only` Cake skips toolbox discovery entirely.

## Hook setup

Add to `~/.config/cake/hooks.json` (use the absolute path `just doctor`
prints):

```json
{
  "version": 1,
  "hooks": {
    "PreToolUse": [
      { "hooks": [
        { "type": "command", "command": "/Users/you/go/bin/cake-monitor hook", "timeout": 5 }
      ] }
    ],
    "SessionStart": [
      { "hooks": [
        { "type": "command", "command": "/Users/you/go/bin/cake-monitor hook", "timeout": 5 }
      ] }
    ],
    "Stop": [
      { "hooks": [
        { "type": "command", "command": "/Users/you/go/bin/cake-monitor hook", "timeout": 5 }
      ] }
    ]
  }
}
```

The hook drains only events scoped to the session in its payload, plus watches
with no session scope. A watch started by a tool call inherits the session from
`CAKE_THREAD_ID`, so it delivers only to that session. `PostToolUse` works too.
A `Stop` result sets `follow_up: true`, so Cake runs one bounded follow-up turn
with the events as context instead of ending the invocation (Cake `445873c`,
ADR 054). Cake owns the cap: one follow-up per invocation, and none when the run
uses `--output-schema`. Events that arrive during that turn wait for the next
delivery.

## Use

```sh
# Watch a CI run and end when it completes.
cake-monitor start \
  --command 'gh run view 12345 --json status,conclusion --jq ".status + \" \" + (.conclusion // \"\")"' \
  --name ci --match 'completed' --until 'completed' --interval 30s

# Tail a log and record only errors.
cake-monitor start --command 'tail -n0 -F /tmp/dev.log 2>&1' \
  --name logs --mode stream --match 'ERROR|panic'

# Wait for a dev server without holding the agent.
cake-monitor start --command 'curl -sf -o /dev/null -w ready\\n localhost:3000/healthz' \
  --name server --until ready --interval 2s

cake-monitor list
cake-monitor read            # drain everything unread
cake-monitor read <id>       # or one watch
cake-monitor stop <id> [--remove]
cake-monitor watch --json    # NDJSON stream for a front end
```

Inside Cake, the same four operations are the `tb__monitor_*` tools. Nothing in
them waits: `monitor_start` returns in milliseconds, and `monitor_read` returns
whatever is already recorded.

## Bounds

- Store: `$CAKE_MONITOR_DATA_DIR`, else `$CAKE_DATA_DIR/monitor`, else
  `~/.local/share/cake/monitor`.
- Per watch: 500 events, 2 KB per line, 30 minutes by default, 24 hours maximum.
- Per drain: 10 events by default for the tool, 25 for the CLI, 100 maximum.
  Undelivered events stay unread.
- Consecutive identical lines are recorded once. Pass `--dedupe=false` to record
  every line.
- Delivery is at-most-once: a crash between render and cursor advance can replay
  an event, never drop one.
- Watchers survive the end of the Cake invocation (they run in their own
  session), but nothing restarts them after a logout or reboot.

## What this does not do

- No blocking waits, by design.
- No WebSocket source. The sandbox allows network, so a helper command works;
  a native client would need private-address and link-local denial policy that
  neither Cake nor this tool has yet.
- No wakeup. If Cake has exited, only a front end can start the next turn.

See [docs/design.md](docs/design.md) for the delivery model and the two upstream
changes that would make it fully proactive.

## Development

`justfile` is the authority:

```sh
just test        # go test ./...
just test-race
just ci          # fmt-check vet test build version-check
```

Tests do not need Cake, network access, or a model call.
