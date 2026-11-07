# Delivery design

## The problem

Cake's agent loop is synchronous: the provider answers with tool calls, Cake
executes them, and the next provider request is issued only after every result is
collected. There is no inbound channel into a live turn.

`BashSession`'s `read` can wait up to 120 seconds for new output. In a standalone
`cake` run that is a reasonable use of an otherwise idle turn. Inside `cake-repl`
the child process is the session: while it waits, the REPL's admission gate keeps
prompt entry, `/new`, `/resume`, and `/sessions` closed. The user cannot do
anything until the wait ends.

`cake-monitor` inverts the direction. A detached watcher writes events to disk,
and Cake reads them at a boundary it already has.

## Delivery channels

| Boundary | Mechanism | Cake code | Scope |
| --- | --- | --- | --- |
| Turn start | `SessionStart` / `UserPromptSubmit` `additional_context` | `src/main.rs` `prepare_agent_turn`, `append_developer_context` | developer message |
| Each tool call | `PreToolUse` / `PostToolUse` `additional_context` | `src/hooks.rs`, appended to the tool result | tool result text |
| Turn end | `Stop` `additional_context` + `follow_up` | `src/main.rs` follow-up loop, `src/hooks.rs` | one extra request (synthetic user message) |
| Model asks | `tb__monitor_read` | toolbox protocol | tool result text |
| Front end | `cake-monitor watch --json` | outside Cake | NDJSON |

The first three are push: the model receives the text without asking. The wait
they replace is zero — the hook reads files and exits. The turn-end channel opens
one extra request in the same invocation instead of injecting into the turn that
just ended, so it still cannot help a process that exited.

## Watch lifecycle

`start` writes `meta.json`, spawns `cake-monitor __watch <id>` with
`setsid`, and returns. The child writes `status.json`, evaluates its command, and
ends at the earliest of:

- an `until` match (state `until`, the matching line is recorded),
- its deadline (state `expired`, one `expired` event is recorded so the model
  learns at the next boundary),
- a stop sentinel plus `SIGTERM` (state `stopped`),
- a stream command exiting (state `stopped`, one `stopped` event).

`setsid` matters: Cake terminates process groups on toolbox timeout, output cap,
and invocation end. A watcher in its own session survives all three and keeps
recording after the turn that started it ends.

Poll mode runs the command each interval and reads its stdout. Stream mode runs
the command once and reads stdout as lines arrive. Both record through the same
path, so matching, dedupe, bounds, and cursor semantics are identical.

## Store and delivery

```
<root>/watches/<id>/meta.json      immutable instruction
<root>/watches/<id>/status.json    watcher state
<root>/watches/<id>/events/<id>.json   one immutable event
<root>/watches/<id>/cursor         last delivered event id
<root>/watches/<id>/last-event     monotonic id counter
<root>/watches/<id>/dropped        count discarded by the bound
<root>/watches/<id>/stop           stop sentinel
<root>/watches/<id>/watcher.log    stderr of the watcher itself
```

Event ids are `%013d-%06d`: millisecond timestamp plus a per-watch sequence that
only increases, so two events in the same millisecond still order by arrival and
a cursor cannot skip one.

Drains advance a per-watch cursor. Delivery is at-most-once: a crash between
render and advance can replay an event, never drop one. Events past 500 per watch
are pruned oldest-first and counted, so the bound is visible rather than silent.

## Decisions

1. **No waits anywhere.** `monitor_read` drains; it does not poll. The hook is
   cheap enough to run per tool call.
2. **Session scoping.** A watch started by a tool call inherits `CAKE_THREAD_ID`.
   The hook delivers only that session's events plus unscoped watches, so a
   watch cannot leak into an unrelated conversation.
3. **Consecutive dedupe on by default.** A polled status command repeats its
   answer; repetition is not news. `--dedupe=false` records every line.
4. **Bounded everywhere.** Deadline, events per watch, bytes per line, events per
   drain, and hook context bytes all have hard caps.
5. **No WebSocket source.** Cake's sandbox allows network, so a helper command
   covers the case. A native client would need a private-address, link-local, and
   metadata-endpoint denial policy this project does not have.
6. **Toolbox over built-ins.** Four executables in Cake's trusted toolbox
   directory need no Cake change and can be removed without touching the agent.

## What would make it proactive

1. **Cake: `Stop` context starts one bounded follow-up turn.** Delivered: Cake
   `445873c` (ADR 054) opens one extra request when a `Stop` result sets
   `follow_up: true`, and `cake-monitor`'s hook now requests it. The cap is
   core-owned: one follow-up per invocation, none under `--output-schema`. It
   still cannot help a process that exited.
2. **cake-repl: consume `watch --json`.** The REPL owns the turn loop. Reading the
   NDJSON stream would let it start a turn on an event while idle and queue the
   event while busy — the only design where "CI went green while I was away"
   reaches the user without typing a prompt.

Item 2 is a separate slice in cake-repl; it is not required for the toolbox and
hook channels above.
