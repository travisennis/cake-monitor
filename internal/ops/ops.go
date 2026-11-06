// Package ops is the shared behavior behind the CLI and the toolbox tools.
package ops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/travisennis/cake-monitor/internal/store"
	"github.com/travisennis/cake-monitor/internal/watcher"
)

// StateStale marks a watch whose status still says running but whose process
// is gone, for example after a reboot.
const StateStale = "stale"

// ToolNames are the toolbox tools this project installs. Cake registers each
// file name as tb__<name>.
var ToolNames = []string{"monitor_start", "monitor_list", "monitor_read", "monitor_stop"}

// StartOptions is one start request.
type StartOptions struct {
	Command  string
	Name     string
	Match    string
	Until    string
	Mode     string
	Interval string
	Deadline time.Duration
	Session  string
	Cwd      string
	Dedupe   *bool
}

// spawnWatcher starts the detached watcher process. It is a variable so tests
// can record a watch without re-executing the test binary.
var spawnWatcher = watcher.Start

// Start validates, records, and detaches one watch.
func Start(root string, opts StartOptions) (*store.Watch, error) {
	if opts.Mode == "" {
		opts.Mode = "poll"
	}
	if opts.Deadline <= 0 {
		opts.Deadline = watcher.DefaultDeadline
	}
	if opts.Deadline > watcher.MaxDeadline {
		return nil, fmt.Errorf("deadline %s exceeds the %s maximum", opts.Deadline, watcher.MaxDeadline)
	}
	if opts.Cwd == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve working directory: %w", err)
		}
		opts.Cwd = cwd
	}
	created := time.Now().UTC()
	meta := store.Meta{
		Name:      opts.Name,
		Command:   opts.Command,
		Mode:      opts.Mode,
		Interval:  opts.Interval,
		Match:     opts.Match,
		Until:     opts.Until,
		Session:   opts.Session,
		Cwd:       opts.Cwd,
		Dedupe:    opts.Dedupe,
		CreatedAt: created,
		Deadline:  created.Add(opts.Deadline),
	}
	if _, err := watcher.Compile(meta); err != nil {
		return nil, err
	}
	watch, err := store.New(root).Create(meta)
	if err != nil {
		return nil, err
	}
	if err := spawnWatcher(root, watch); err != nil {
		_ = watch.Remove()
		return nil, err
	}
	return watch, nil
}

// Summary is one row of a watch listing.
type Summary struct {
	ID       string    `json:"id"`
	Name     string    `json:"name,omitempty"`
	State    string    `json:"state"`
	Mode     string    `json:"mode"`
	Command  string    `json:"command"`
	Age      string    `json:"age"`
	Unread   int       `json:"unread"`
	Deadline time.Time `json:"deadline"`
}

// List summarizes every watch, newest first.
func List(root string) ([]Summary, error) {
	watches, err := store.New(root).List()
	if err != nil {
		return nil, err
	}
	summaries := make([]Summary, 0, len(watches))
	for _, watch := range watches {
		status, _ := watch.Status()
		state := status.State
		if state == "" {
			state = store.StateStarting
		}
		if (state == store.StateRunning || state == store.StateStarting) && !watcher.IsAlive(status.PID) {
			state = StateStale
		}
		unread, _ := watch.Unread()
		summaries = append(summaries, Summary{
			ID:       watch.Meta.ID,
			Name:     watch.Meta.Name,
			State:    state,
			Mode:     watch.Meta.Mode,
			Command:  watch.Meta.Command,
			Age:      age(watch.Meta.CreatedAt),
			Unread:   len(unread),
			Deadline: watch.Meta.Deadline,
		})
	}
	return summaries, nil
}

// Read drains unread events and reports what is left.
func Read(root, watchID string, limit int) ([]store.Item, int, int, error) {
	filter := store.Filter{WatchID: watchID, Limit: limit}
	items, err := store.Drain(root, filter)
	if err != nil {
		return nil, 0, 0, err
	}
	remaining, err := store.Remaining(root, store.Filter{WatchID: watchID})
	if err != nil {
		return nil, 0, 0, err
	}
	dropped, err := Dropped(root, watchID)
	if err != nil {
		return nil, 0, 0, err
	}
	return items, remaining, dropped, nil
}

// Hook drains events scoped to one session. Unscoped watches deliver to every
// session; a watch started with a session delivers only there.
func Hook(root, session string, limit int) ([]store.Item, int, int, error) {
	filter := store.Filter{Session: session, Limit: limit}
	items, err := store.Drain(root, filter)
	if err != nil {
		return nil, 0, 0, err
	}
	remaining, err := store.Remaining(root, store.Filter{Session: session})
	if err != nil {
		return nil, 0, 0, err
	}
	dropped, err := Dropped(root, "")
	if err != nil {
		return nil, 0, 0, err
	}
	return items, remaining, dropped, nil
}

// Dropped sums the discarded-event count for the selected watches.
func Dropped(root, watchID string) (int, error) {
	watches, err := store.New(root).List()
	if err != nil {
		return 0, err
	}
	total := 0
	for _, watch := range watches {
		if watchID != "" && watch.Meta.ID != watchID {
			continue
		}
		dropped, err := watch.Dropped()
		if err != nil {
			return 0, err
		}
		total += dropped
	}
	return total, nil
}

// Stop ends one watch and waits briefly for its terminal state.
func Stop(root, watchID string) (string, error) {
	watch, err := store.New(root).Open(watchID)
	if err != nil {
		return "", err
	}
	status, _ := watch.Status()
	if err := watch.RequestStop(); err != nil {
		return "", err
	}
	if status.PID > 0 {
		watcher.Terminate(status.PID)
	}
	if status.PID == 0 || !watcher.IsAlive(status.PID) {
		ended := time.Now().UTC()
		_ = watch.WriteStatus(store.Status{
			State:     store.StateStopped,
			StartedAt: status.StartedAt,
			EndedAt:   &ended,
			Lines:     status.Lines,
		})
		return store.StateStopped, nil
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, err := watch.Status()
		if err == nil && terminal(current.State) && !watcher.IsAlive(current.PID) {
			return current.State, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status.PID > 0 {
		watcher.Kill(status.PID)
	}
	ended := time.Now().UTC()
	state := store.StateStopped
	_ = watch.WriteStatus(store.Status{
		State:     state,
		PID:       status.PID,
		StartedAt: status.StartedAt,
		EndedAt:   &ended,
		Lines:     status.Lines,
	})
	return state, nil
}

// Prune removes watches that have ended and are older than the given age.
func Prune(root string, olderThan time.Duration) (int, error) {
	watches, err := store.New(root).List()
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, watch := range watches {
		status, err := watch.Status()
		if err != nil {
			continue
		}
		if !terminal(status.State) {
			continue
		}
		if time.Since(watch.Meta.CreatedAt) < olderThan {
			continue
		}
		if err := watch.Remove(); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// ToolboxDir resolves cake's global toolbox directory.
func ToolboxDir() (string, error) {
	if config := os.Getenv("XDG_CONFIG_HOME"); config != "" {
		return filepath.Join(config, "cake", "tools"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "cake", "tools"), nil
}

// InstallTools links the tool names at the executable path.
func InstallTools(executable string) ([]string, error) {
	dir, err := ToolboxDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create toolbox directory: %w", err)
	}
	target, err := filepath.Abs(executable)
	if err != nil {
		return nil, err
	}
	installed := make([]string, 0, len(ToolNames))
	for _, name := range ToolNames {
		path := filepath.Join(dir, name)
		if info, err := os.Lstat(path); err == nil {
			if info.Mode()&os.ModeSymlink == 0 {
				continue // never replace a real file
			}
			_ = os.Remove(path)
		}
		if err := os.Symlink(target, path); err != nil {
			return installed, fmt.Errorf("link %s: %w", name, err)
		}
		installed = append(installed, path)
	}
	sort.Strings(installed)
	return installed, nil
}

// UninstallTools removes links that point at a cake-monitor binary.
func UninstallTools() ([]string, error) {
	dir, err := ToolboxDir()
	if err != nil {
		return nil, err
	}
	removed := []string{}
	for _, name := range ToolNames {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return removed, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, err := os.Readlink(path)
		if err != nil {
			continue
		}
		if filepath.Base(target) != "cake-monitor" {
			continue
		}
		if err := os.Remove(path); err != nil {
			return removed, err
		}
		removed = append(removed, path)
	}
	return removed, nil
}

func terminal(state string) bool {
	return state == store.StateStopped || state == store.StateExpired ||
		state == store.StateUntil || state == store.StateError
}

func age(at time.Time) string {
	if at.IsZero() {
		return "unknown"
	}
	elapsed := time.Since(at)
	switch {
	case elapsed < time.Minute:
		return fmt.Sprintf("%ds", int(elapsed.Seconds()))
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm", int(elapsed.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(elapsed.Hours()))
	}
}
