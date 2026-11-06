// Package watcher runs one watch to completion.
//
// A watch is started detached (new session, no inherited terminal), so it
// survives the cake invocation that created it and keeps recording after the
// turn ends. It is not a daemon: it exits at its deadline, when its until
// pattern matches, or when a reader writes the stop sentinel.
package watcher

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/travisennis/cake-monitor/internal/store"
)

const (
	// DefaultInterval is the poll cadence when a watch does not declare one.
	DefaultInterval = 10 * time.Second
	// DefaultDeadline is how long a watch runs when the caller does not say.
	DefaultDeadline = 30 * time.Minute
	// MaxDeadline caps one watch. A longer watch belongs in a front end that
	// owns the turn loop, not in a background recorder.
	MaxDeadline = 24 * time.Hour
	// boundaryTick is how often a running watch checks its deadline and stop
	// sentinel.
	boundaryTick = 250 * time.Millisecond
	// commandOutputLimit bounds one polled command's captured stdout.
	commandOutputLimit = 256 * 1024
	// errorOutputLimit bounds the stderr tail recorded on a failed command.
	errorOutputLimit = 512
)

// Compiled is a validated watch instruction.
type Compiled struct {
	Match    *regexp.Regexp
	Until    *regexp.Regexp
	Interval time.Duration
}

// Compile validates a watch's patterns and cadence.
func Compile(meta store.Meta) (*Compiled, error) {
	compiled := &Compiled{Interval: DefaultInterval}
	if meta.Match != "" {
		match, err := regexp.Compile(meta.Match)
		if err != nil {
			return nil, fmt.Errorf("invalid match pattern: %w", err)
		}
		compiled.Match = match
	}
	if meta.Until != "" {
		until, err := regexp.Compile(meta.Until)
		if err != nil {
			return nil, fmt.Errorf("invalid until pattern: %w", err)
		}
		compiled.Until = until
	}
	if meta.Interval != "" {
		interval, err := time.ParseDuration(meta.Interval)
		if err != nil || interval <= 0 {
			return nil, fmt.Errorf("invalid interval %q: use a positive duration such as 10s", meta.Interval)
		}
		compiled.Interval = interval
	}
	if meta.Mode != "poll" && meta.Mode != "stream" {
		return nil, fmt.Errorf("invalid mode %q: use poll or stream", meta.Mode)
	}
	return compiled, nil
}

// Start spawns the detached watcher for a created watch.
func Start(root string, watch *store.Watch) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	logFile, err := os.OpenFile(
		filepath.Join(watch.Dir, "watcher.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("open watcher log: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	cmd := exec.Command(executable, "__watch", watch.Meta.ID)
	cmd.Env = append(cleanEnv(), "CAKE_MONITOR_DATA_DIR="+root)
	if watch.Meta.Cwd != "" {
		cmd.Dir = watch.Meta.Cwd
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	// A new session keeps cake's process-group cleanup from killing the watch
	// when the tool call or the invocation ends.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start watcher: %w", err)
	}
	status := store.Status{
		State:     store.StateStarting,
		PID:       cmd.Process.Pid,
		StartedAt: time.Now().UTC(),
	}
	if err := watch.WriteStatus(status); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	return cmd.Process.Release()
}

// Run watches one watch until it ends and records the terminal state.
func Run(root, id string) error {
	watch, err := store.New(root).Open(id)
	if err != nil {
		return err
	}
	compiled, err := Compile(watch.Meta)
	if err != nil {
		ended := time.Now().UTC()
		return watch.WriteStatus(store.Status{
			State:     store.StateError,
			StartedAt: ended,
			EndedAt:   &ended,
			Error:     err.Error(),
		})
	}
	runner := &runner{watch: watch, compiled: compiled, deadline: watch.Meta.Deadline}
	if runner.deadline.IsZero() {
		runner.deadline = time.Now().Add(DefaultDeadline)
	}
	started := time.Now().UTC()
	if err := watch.WriteStatus(store.Status{
		State:     store.StateRunning,
		PID:       os.Getpid(),
		StartedAt: started,
	}); err != nil {
		return err
	}

	state, runErr := store.StateStopped, error(nil)
	if watch.Meta.Mode == "stream" {
		state, runErr = runner.runStream()
	} else {
		state, runErr = runner.runPoll()
	}

	ended := time.Now().UTC()
	status := store.Status{
		State:     state,
		PID:       os.Getpid(),
		StartedAt: started,
		EndedAt:   &ended,
		Lines:     runner.lines,
	}
	if runErr != nil {
		status.State = store.StateError
		status.Error = runErr.Error()
	}
	return watch.WriteStatus(status)
}

type runner struct {
	watch    *store.Watch
	compiled *Compiled
	deadline time.Time
	lines    int
	errored  bool
	lastLine string
}

func (r *runner) runPoll() (string, error) {
	interval := r.compiled.Interval
	for {
		if state, done := r.boundary(); done {
			return state, nil
		}
		if state, done := r.pollOnce(interval); done {
			return state, nil
		}
		if state, done := r.sleep(interval); done {
			return state, nil
		}
	}
}

func (r *runner) pollOnce(interval time.Duration) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), interval)
	defer cancel()
	cmd := command(ctx, r.watch.Meta)
	out := &limitedBuffer{limit: commandOutputLimit}
	errOut := &limitedBuffer{limit: errorOutputLimit}
	cmd.Stdout = out
	cmd.Stderr = errOut
	if err := cmd.Run(); err != nil {
		r.recordError(fmt.Errorf("%s: %s", err, strings.TrimSpace(errOut.String())))
		return "", false
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if state, done := r.feed(line); done {
			return state, true
		}
	}
	return "", false
}

func (r *runner) runStream() (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := command(ctx, r.watch.Meta)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return store.StateError, fmt.Errorf("open stdout: %w", err)
	}
	errOut := &limitedBuffer{limit: errorOutputLimit}
	cmd.Stderr = errOut
	if err := cmd.Start(); err != nil {
		return store.StateError, fmt.Errorf("start command: %w", err)
	}

	lines := make(chan string)
	scanErr := make(chan error, 1)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), store.MaxLineBytes*8)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		scanErr <- scanner.Err()
	}()

	ticker := time.NewTicker(boundaryTick)
	defer ticker.Stop()
	state := ""
	reaped := false
	for state == "" {
		select {
		case line, ok := <-lines:
			if !ok {
				state = r.handleStreamClose(cmd, errOut, scanErr)
				reaped = true
				break
			}
			if matched, done := r.feed(line); done {
				state = matched
			}
		case <-ticker.C:
			if ended, done := r.boundary(); done {
				state = ended
			}
		}
	}
	cancel()
	if !reaped {
		_ = cmd.Wait()
	}
	return state, nil
}

func (r *runner) handleStreamClose(cmd *exec.Cmd, errOut *limitedBuffer, scanErr chan error) string {
	if err := <-scanErr; err != nil {
		r.recordError(err)
	}
	waitErr := cmd.Wait()
	if waitErr != nil {
		r.recordError(fmt.Errorf("%s: %s", waitErr, strings.TrimSpace(errOut.String())))
		// A command that never streamed anything is a failed watch; one that
		// already streamed lines reported what it saw.
		if r.lines == 0 {
			return store.StateError
		}
	}
	r.appendEvent(store.KindStopped, "command exited")
	return store.StateStopped
}

// boundary ends the watch at its deadline or on a stop request.
func (r *runner) boundary() (string, bool) {
	if r.watch.StopRequested() {
		return store.StateStopped, true
	}
	if !time.Now().Before(r.deadline) {
		r.appendEvent(store.KindExpired, "watch deadline reached")
		return store.StateExpired, true
	}
	return "", false
}

// sleep waits one interval in bounded slices so a deadline or stop request is
// honored promptly instead of after a long sleep.
func (r *runner) sleep(interval time.Duration) (string, bool) {
	deadline := time.Now().Add(interval)
	for time.Now().Before(deadline) {
		if state, done := r.boundary(); done {
			return state, true
		}
		remaining := time.Until(deadline)
		if remaining > boundaryTick {
			remaining = boundaryTick
		}
		time.Sleep(remaining)
	}
	return "", false
}

// feed handles one output line and reports whether the watch is over.
func (r *runner) feed(line string) (string, bool) {
	line = strings.TrimRight(line, "\r")
	if strings.TrimSpace(line) == "" {
		return "", false
	}
	if r.compiled.Until != nil && r.compiled.Until.MatchString(line) {
		r.appendEvent(store.KindUntil, line)
		return store.StateUntil, true
	}
	if r.compiled.Match == nil || r.compiled.Match.MatchString(line) {
		if r.watch.Meta.Dedupes() && line == r.lastLine {
			return "", false
		}
		r.lastLine = line
		r.appendEvent(store.KindLine, line)
	}
	return "", false
}

func (r *runner) recordError(err error) {
	if r.errored {
		return
	}
	r.errored = true
	r.appendEvent(store.KindError, err.Error())
}

func (r *runner) appendEvent(kind, line string) {
	r.lines++
	event := store.Event{Time: time.Now().UTC(), Kind: kind, Line: line}
	if err := r.watch.AppendEvent(event); err != nil {
		// Recording must never kill the watch; the watcher log holds the
		// diagnostic for a human.
		_, _ = fmt.Fprintf(os.Stderr, "cake-monitor: append event: %v\n", err)
	}
}

// command builds the shell invocation for a watch. The shell resolves
// pipelines, redirections, and compound commands the same way the Bash tool
// does, so a watch takes any command line a user would type.
func command(ctx context.Context, meta store.Meta) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", meta.Command)
	cmd.Env = os.Environ()
	if meta.Cwd != "" {
		cmd.Dir = meta.Cwd
	}
	return cmd
}

// limitedBuffer keeps at most limit bytes and reports success for the rest, so
// a chatty command cannot grow memory or the store without bound.
type limitedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buffer.Len() >= b.limit {
		return len(p), nil
	}
	if len(p) > b.limit-b.buffer.Len() {
		p = p[:b.limit-b.buffer.Len()]
	}
	_, err := b.buffer.Write(p)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	return b.buffer.String()
}

// cleanEnv copies the environment without cake's toolbox selector, so a
// detached watcher is an ordinary process rather than a nested tool call.
func cleanEnv() []string {
	env := os.Environ()
	cleaned := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, "TOOLBOX_ACTION=") {
			continue
		}
		cleaned = append(cleaned, entry)
	}
	return cleaned
}

// IsAlive reports whether a pid is still running.
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}

// Terminate signals a detached watcher's process group. The watcher is its own
// session leader, so its pid is also its group id.
func Terminate(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	}
}

// Kill force-ends a detached watcher's process group.
func Kill(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}
