// Package store owns cake-monitor's on-disk watch store.
//
// One watch is one directory:
//
//	<root>/watches/<id>/meta.json      the instruction that started it
//	<root>/watches/<id>/status.json    the watcher's own state, rewritten in place
//	<root>/watches/<id>/events/<id>.json   immutable, sortable, one per event
//	<root>/watches/<id>/cursor         last event id a reader delivered
//	<root>/watches/<id>/dropped        events discarded by the size bound
//	<root>/watches/<id>/stop           sentinel a reader writes to end the watch
//
// The layout is deliberately the same shape as cake-subagents' run store: a
// plain directory tree a human can inspect with ls and cat. Every file is
// written by one process, and reads tolerate a partially written file by
// skipping it.
package store

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxEvents bounds one watch's retained events. Older events are dropped
	// and counted instead of growing the store without limit.
	MaxEvents = 500
	// MaxLineBytes bounds one recorded line. A longer line is truncated so one
	// pathological command cannot blow the store or a prompt.
	MaxLineBytes = 2048
	// DefaultLimit is how many events one drain returns when the caller does
	// not ask for a count.
	DefaultLimit = 25
	// MaxLimit caps one drain. Undelivered events stay unread for the next one.
	MaxLimit = 100

	metaName      = "meta.json"
	statusName    = "status.json"
	cursorName    = "cursor"
	stopName      = "stop"
	droppedName   = "dropped"
	lastEventName = "last-event"
	lockName      = ".lock"
	eventsDirName = "events"
	watchesDir    = "watches"

	lockRetries  = 200
	lockDelay    = 5 * time.Millisecond
	lockStaleAge = 30 * time.Second
)

// Watch states reported in status.json.
const (
	StateStarting = "starting"
	StateRunning  = "running"
	StateStopped  = "stopped"
	StateExpired  = "expired"
	StateUntil    = "until"
	StateError    = "error"
)

// Event kinds. Every kind is model-visible text once delivered.
const (
	KindLine    = "line"
	KindUntil   = "until"
	KindExpired = "expired"
	KindStopped = "stopped"
	KindError   = "error"
)

// Meta is the instruction that started a watch. It is immutable after create.
type Meta struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Command  string `json:"command"`
	Mode     string `json:"mode"`
	Interval string `json:"interval,omitempty"`
	Match    string `json:"match,omitempty"`
	Until    string `json:"until,omitempty"`
	Session  string `json:"session,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
	// Dedupe suppresses a line identical to the previous recorded line. Nil
	// means the default, which is on. A polled status command repeats its
	// answer every interval, and that repetition is not news.
	Dedupe    *bool     `json:"dedupe,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Deadline  time.Time `json:"deadline"`
}

// Dedupes reports whether identical consecutive lines are suppressed.
func (m Meta) Dedupes() bool {
	return m.Dedupe == nil || *m.Dedupe
}

// Label is the human and model facing name for a watch.
func (m Meta) Label() string {
	if m.Name != "" {
		return m.Name
	}
	return m.ID
}

// Status is the watcher's own report. Only the watcher rewrites it, apart from
// the terminal-state repair a `stop` performs on a dead watcher.
type Status struct {
	State     string     `json:"state"`
	PID       int        `json:"pid,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Error     string     `json:"error,omitempty"`
	Lines     int        `json:"lines"`
}

// Event is one recorded observation.
type Event struct {
	ID      string    `json:"id"`
	WatchID string    `json:"watch"`
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Line    string    `json:"line,omitempty"`
}

// Item is one delivered event with enough identity to render it.
type Item struct {
	WatchID string `json:"watch"`
	Name    string `json:"name,omitempty"`
	Event   Event  `json:"event"`
}

// Filter selects what a drain considers.
type Filter struct {
	// WatchID limits the drain to one watch when set.
	WatchID string
	// Session limits the drain to watches started for that session, plus
	// watches with no session scope. Empty means every watch.
	Session string
	// Limit caps the returned items. Zero means DefaultLimit.
	Limit int
}

// Store is a resolved data root.
type Store struct {
	Root string
}

// New returns a store rooted at root.
func New(root string) *Store {
	return &Store{Root: root}
}

// Root resolves the data root from the environment.
func Root() (string, error) {
	if root := os.Getenv("CAKE_MONITOR_DATA_DIR"); root != "" {
		return filepath.Clean(root), nil
	}
	if data := os.Getenv("CAKE_DATA_DIR"); data != "" {
		return filepath.Join(data, "monitor"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "cake", "monitor"), nil
}

func (s *Store) watchesDir() string {
	return filepath.Join(s.Root, watchesDir)
}

// Create writes a new watch directory and returns it.
func (s *Store) Create(meta Meta) (*Watch, error) {
	if strings.TrimSpace(meta.Command) == "" {
		return nil, errors.New("watch command must not be empty")
	}
	if meta.ID == "" {
		meta.ID = newID()
	}
	if err := validID(meta.ID); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.watchesDir(), meta.ID)
	if err := os.MkdirAll(filepath.Join(dir, eventsDirName), 0o700); err != nil {
		return nil, fmt.Errorf("create watch directory: %w", err)
	}
	watch := &Watch{Dir: dir, Meta: meta}
	if err := watch.writeJSON(metaName, meta); err != nil {
		return nil, err
	}
	if err := watch.WriteStatus(Status{
		State:     StateStarting,
		StartedAt: meta.CreatedAt,
	}); err != nil {
		return nil, err
	}
	return watch, nil
}

// Open loads one watch by id.
func (s *Store) Open(id string) (*Watch, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.watchesDir(), id)
	var meta Meta
	if err := readJSON(filepath.Join(dir, metaName), &meta); err != nil {
		return nil, fmt.Errorf("open watch %s: %w", id, err)
	}
	return &Watch{Dir: dir, Meta: meta}, nil
}

// List loads every watch, newest first.
func (s *Store) List() ([]*Watch, error) {
	entries, err := os.ReadDir(s.watchesDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list watches: %w", err)
	}
	watches := make([]*Watch, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		watch, err := s.Open(entry.Name())
		if err != nil {
			// A directory without a readable meta.json is a half-created or
			// removed watch, not a reason to fail the listing.
			continue
		}
		watches = append(watches, watch)
	}
	sort.Slice(watches, func(i, j int) bool {
		return watches[i].Meta.CreatedAt.After(watches[j].Meta.CreatedAt)
	})
	return watches, nil
}

// Watch is one directory in the store.
type Watch struct {
	Dir  string
	Meta Meta
}

// Status reads the watcher's current report.
func (w *Watch) Status() (Status, error) {
	var status Status
	if err := readJSON(filepath.Join(w.Dir, statusName), &status); err != nil {
		return Status{}, err
	}
	return status, nil
}

// WriteStatus replaces the status file.
func (w *Watch) WriteStatus(status Status) error {
	return w.writeJSON(statusName, status)
}

// AppendEvent records one event, assigning its id and pruning the oldest
// events past MaxEvents.
func (w *Watch) AppendEvent(event Event) error {
	return withLock(w.Dir, func() error {
		dir := filepath.Join(w.Dir, eventsDirName)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create events directory: %w", err)
		}
		if event.ID == "" {
			id, err := w.nextEventID(event.Time)
			if err != nil {
				return err
			}
			event.ID = id
		}
		event.WatchID = w.Meta.ID
		event.Line = truncate(event.Line)
		if err := writeJSON(filepath.Join(dir, event.ID+".json"), event); err != nil {
			return err
		}
		return w.pruneLocked()
	})
}

func (w *Watch) pruneLocked() error {
	ids, err := w.eventIDs()
	if err != nil {
		return err
	}
	if len(ids) <= MaxEvents {
		return nil
	}
	excess := ids[:len(ids)-MaxEvents]
	for _, id := range excess {
		path := filepath.Join(w.Dir, eventsDirName, id+".json")
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("prune event: %w", err)
		}
	}
	return w.addDropped(len(excess))
}

func (w *Watch) addDropped(count int) error {
	path := filepath.Join(w.Dir, droppedName)
	current, _ := w.Dropped()
	value := strconv.Itoa(current + count)
	return os.WriteFile(path, []byte(value), 0o600)
}

// Dropped reports how many events the size bound discarded.
func (w *Watch) Dropped() (int, error) {
	raw, err := os.ReadFile(filepath.Join(w.Dir, droppedName))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(raw)))
}

// Unread returns the events recorded after the cursor, oldest first.
func (w *Watch) Unread() ([]Event, error) {
	cursor, err := w.Cursor()
	if err != nil {
		return nil, err
	}
	ids, err := w.eventIDs()
	if err != nil {
		return nil, err
	}
	events := make([]Event, 0, len(ids))
	for _, id := range ids {
		if id <= cursor {
			continue
		}
		var event Event
		if err := readJSON(filepath.Join(w.Dir, eventsDirName, id+".json"), &event); err != nil {
			// A reader racing a prune should skip the vanished file rather
			// than fail the drain.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			continue
		}
		events = append(events, event)
	}
	return events, nil
}

// Cursor returns the last delivered event id.
func (w *Watch) Cursor() (string, error) {
	raw, err := os.ReadFile(filepath.Join(w.Dir, cursorName))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// AdvanceCursor moves the cursor forward. It never moves backward, so a
// concurrent drain cannot rewind delivery.
func (w *Watch) AdvanceCursor(id string) error {
	if id == "" {
		return nil
	}
	return withLock(w.Dir, func() error {
		current, err := w.Cursor()
		if err != nil {
			return err
		}
		if id <= current {
			return nil
		}
		return os.WriteFile(filepath.Join(w.Dir, cursorName), []byte(id), 0o600)
	})
}

// StopRequested reports whether a reader asked the watcher to end.
func (w *Watch) StopRequested() bool {
	_, err := os.Stat(filepath.Join(w.Dir, stopName))
	return err == nil
}

// RequestStop writes the stop sentinel.
func (w *Watch) RequestStop() error {
	return os.WriteFile(filepath.Join(w.Dir, stopName), []byte("stop"), 0o600)
}

// Remove deletes the watch directory.
func (w *Watch) Remove() error {
	return os.RemoveAll(w.Dir)
}

func (w *Watch) eventIDs() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(w.Dir, eventsDirName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read events directory: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(name, ".json"))
	}
	sort.Strings(ids)
	return ids, nil
}

func (w *Watch) writeJSON(name string, value any) error {
	return writeJSON(filepath.Join(w.Dir, name), value)
}

// Drain collects unread events across watches and advances their cursors.
// Delivery is at-most-once: a crash between reading and advancing the cursor
// can replay an event, but it never drops one.
func Drain(root string, filter Filter) ([]Item, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	watches, err := New(root).List()
	if err != nil {
		return nil, err
	}
	var items []Item
	for _, watch := range watches {
		if filter.WatchID != "" && watch.Meta.ID != filter.WatchID {
			continue
		}
		if filter.Session != "" &&
			watch.Meta.Session != "" &&
			watch.Meta.Session != filter.Session {
			continue
		}
		events, err := watch.Unread()
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			items = append(items, Item{
				WatchID: watch.Meta.ID,
				Name:    watch.Meta.Label(),
				Event:   event,
			})
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Event.ID < items[j].Event.ID
	})
	truncated := false
	if len(items) > limit {
		items = items[:limit]
		truncated = true
	}
	lastByWatch := make(map[string]string, len(items))
	for _, item := range items {
		lastByWatch[item.WatchID] = item.Event.ID
	}
	for watchID, eventID := range lastByWatch {
		watch, err := New(root).Open(watchID)
		if err != nil {
			continue
		}
		if err := watch.AdvanceCursor(eventID); err != nil {
			return nil, err
		}
	}
	_ = truncated
	return items, nil
}

// Remaining reports how many unread events the filter still selects.
func Remaining(root string, filter Filter) (int, error) {
	watches, err := New(root).List()
	if err != nil {
		return 0, err
	}
	total := 0
	for _, watch := range watches {
		if filter.WatchID != "" && watch.Meta.ID != filter.WatchID {
			continue
		}
		if filter.Session != "" &&
			watch.Meta.Session != "" &&
			watch.Meta.Session != filter.Session {
			continue
		}
		events, err := watch.Unread()
		if err != nil {
			return 0, err
		}
		total += len(events)
	}
	return total, nil
}

// nextEventID returns a strictly increasing id under the watch lock, so two
// events in the same millisecond still order by arrival and a cursor can never
// skip one.
func (w *Watch) nextEventID(at time.Time) (string, error) {
	millis := at.UnixMilli()
	if at.IsZero() {
		millis = time.Now().UnixMilli()
	}
	sequence := 0
	raw, err := os.ReadFile(filepath.Join(w.Dir, lastEventName))
	if err == nil {
		var lastMillis int64
		var lastSequence int
		if _, scanErr := fmt.Sscanf(string(raw), "%d %d", &lastMillis, &lastSequence); scanErr == nil {
			if millis <= lastMillis {
				millis = lastMillis
				sequence = lastSequence + 1
			}
		}
	}
	encoded := fmt.Sprintf("%d %d", millis, sequence)
	if err := os.WriteFile(filepath.Join(w.Dir, lastEventName), []byte(encoded), 0o600); err != nil {
		return "", fmt.Errorf("record event sequence: %w", err)
	}
	return fmt.Sprintf("%013d-%06d", millis, sequence), nil
}

func newID() string {
	return fmt.Sprintf("%013d-%06x", time.Now().UnixMilli(), randomUint32()&0xffffff)
}

func newEventID(at time.Time) string {
	if at.IsZero() {
		at = time.Now()
	}
	return fmt.Sprintf("%013d-%06x", at.UnixMilli(), randomUint32()&0xffffff)
}

func randomUint32() uint32 {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return uint32(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint32(buf[:])
}

func validID(id string) error {
	if len(id) != 20 {
		return fmt.Errorf("invalid watch id %q", id)
	}
	for i, r := range id {
		if i == 13 {
			if r != '-' {
				return fmt.Errorf("invalid watch id %q", id)
			}
			continue
		}
		isDigit := r >= '0' && r <= '9'
		isHex := r >= 'a' && r <= 'f'
		if i < 13 && !isDigit {
			return fmt.Errorf("invalid watch id %q", id)
		}
		if i > 13 && !(isDigit || isHex) {
			return fmt.Errorf("invalid watch id %q", id)
		}
	}
	return nil
}

func truncate(line string) string {
	if len(line) <= MaxLineBytes {
		return line
	}
	cut := MaxLineBytes
	for cut > 0 && cut < len(line) && (line[cut]&0xc0) == 0x80 {
		cut--
	}
	return line[:cut] + "…"
}

func writeJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	// Write then rename so a reader never sees a half-written document.
	temp := path + ".tmp"
	if err := os.WriteFile(temp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(temp, path); err != nil {
		return fmt.Errorf("replace %s: %w", filepath.Base(path), err)
	}
	return nil
}

func readJSON(path string, value any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, value); err != nil {
		return fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return nil
}

// withLock serializes writers for one watch. The lock is advisory and bounded:
// a lock older than lockStaleAge is treated as abandoned.
func withLock(dir string, fn func() error) error {
	path := filepath.Join(dir, lockName)
	var file *os.File
	var err error
	for attempt := 0; attempt < lockRetries; attempt++ {
		file, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("lock %s: %w", filepath.Base(dir), err)
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > lockStaleAge {
			_ = os.Remove(path)
			continue
		}
		time.Sleep(lockDelay)
	}
	if file == nil {
		return fmt.Errorf("timed out waiting for the store lock in %s", dir)
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(path)
	}()
	return fn()
}
