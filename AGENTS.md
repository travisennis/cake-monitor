# Agent Instructions

## Project

`cake-monitor` is a single-binary Go CLI and Cake toolbox plugin. It records
background command output into a durable store and delivers it to Cake as hook
context or on-demand tool results. It exists because `BashSession`'s wait holds a
whole `cake-repl` session.

## Boundaries

- **Never block.** No command, tool, or hook may wait for new output. If a change
  introduces a wait, it belongs in a different project.
- **No wakeup claim.** Cake is one process per prompt. Only a front end that owns
  the turn loop can start a turn after Cake exits; do not pretend otherwise in
  descriptions or docs.
- State lives only under the monitor data root (`$CAKE_MONITOR_DATA_DIR`, else
  `$CAKE_DATA_DIR/monitor`, else `~/.local/share/cake/monitor`). Do not read or
  write Cake sessions.
- The hook adapter writes exactly one JSON document to stdout and exits 0 for
  ordinary conditions. It must tolerate any input and never fail a Cake turn.
- The toolbox protocol is one executable, four names, selected by `TOOLBOX_ACTION`
  (`describe` or `execute`). Descriptions are model-facing text; keep them
  accurate about immediacy and limits.
- Watchers are detached with `setsid`. Removing that re-introduces the problem
  this project solves: Cake's process-group cleanup would kill the watch.
- Unix only. `syscall.SysProcAttr{Setsid: true}` and process-group signalling do
  not exist on Windows.
- Do not add network clients, MCP, or a daemon. A watch is a short-lived process
  with a deadline.

## Commands

`justfile` is the authority:

```sh
just hooks
just test
just test-race
just ci
just build
just install
just install-tools
just doctor
```

Tests must not require Cake, a network call, or a model invocation.

## Workflow

- Conventional Commits; `just hooks` installs the commit-msg hook.
- `ahm` owns the backlog. The repository has no remote configured yet, so run
  `ahm init` only after the remote exists; initializing first keys the store to a
  path hash rather than the remote identity.
- Do not keep a `TODO.md`. Future work belongs in ahm tasks.
- Read [docs/design.md](docs/design.md) before changing the delivery model, the
  hook output, or the store layout. Those are the compatibility surfaces.
