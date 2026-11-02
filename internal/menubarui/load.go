package menubarui

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrLoadInProgress prevents duplicate load clicks.
var ErrLoadInProgress = errors.New("a model load is already in progress")

// LoadTracker serializes user-initiated loads and records elapsed time. The
// load runs on its own context so closing the menu cannot cancel it.
type LoadTracker struct {
	mu      sync.Mutex
	running bool
	model   string
	start   time.Time
	ended   time.Time
	lastErr string
}

// Start marks a load as running and reports whether it began.
func (t *LoadTracker) Start(model string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running {
		return false
	}
	t.running = true
	t.model = model
	t.start = time.Now()
	t.ended = time.Time{}
	t.lastErr = ""
	return true
}

// Finish records the outcome of the running load.
func (t *LoadTracker) Finish(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.running = false
	t.ended = time.Now()
	if err != nil {
		t.lastErr = FriendlyError(err)
	}
}

// Running reports whether a load is in progress and its model.
func (t *LoadTracker) Running() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.model, t.running
}

// AnyRunning reports whether any load is in progress.
func (t *LoadTracker) AnyRunning() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}

// RunningModel reports whether the given model is the one loading now.
func (t *LoadTracker) RunningModel(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running && t.model == id
}

// Elapsed returns the current or final duration of the last load.
func (t *LoadTracker) Elapsed() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.start.IsZero() {
		return 0
	}
	end := time.Now()
	if !t.running && !t.ended.IsZero() {
		end = t.ended
	}
	return end.Sub(t.start)
}

// Label is the menu status line for a load click.
func (t *LoadTracker) Label() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.start.IsZero() {
		return ""
	}
	if t.running {
		return ElapsedLabel(time.Since(t.start))
	}
	if t.lastErr != "" {
		return "Load failed: " + t.lastErr
	}
	return "Load finished in " + t.elapsedLocked().Round(time.Second).String()
}

func (t *LoadTracker) elapsedLocked() time.Duration {
	end := time.Now()
	if !t.running && !t.ended.IsZero() {
		end = t.ended
	}
	return end.Sub(t.start)
}

// LoadInBackground starts one load on a detached context. It returns false
// when another load is already running, so the caller can ignore the click.
func LoadInBackground(ctrl Controller, tracker *LoadTracker, model string) bool {
	if !tracker.Start(model) {
		return false
	}
	go func() {
		Run(ctrl, tracker, model)
	}()
	return true
}

// Accept synchronously reserves the tracker for one load and reports whether
// the click was accepted. Callers that coordinate shutdown can call this
// while holding their own mutex, then release the mutex and launch Run on a
// background goroutine. The tracker is active before the mutex is released,
// so a concurrent quit observes the in-progress load. Remote subprocess work
// never happens inside Accept.
func Accept(tracker *LoadTracker, model string) bool {
	return tracker.Start(model)
}

// Run performs the accepted load on a detached context and records the
// outcome. It must be called after a successful Accept, on a background
// goroutine. It never cancels an accepted load.
func Run(ctrl Controller, tracker *LoadTracker, model string) {
	err := ctrl.Load(context.Background(), model)
	tracker.Finish(err)
}

// CatalogCache holds the last successfully fetched catalog so the Load
// submenu can render immediately without a blocking CLI call.
type CatalogCache struct {
	mu      sync.Mutex
	choices []Choice
	loaded  bool
}

// Choices returns a copy of the cached catalog.
func (c *CatalogCache) Choices() []Choice {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Choice, len(c.choices))
	copy(out, c.choices)
	return out
}

// Loaded reports whether a catalog has been stored.
func (c *CatalogCache) Loaded() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loaded
}

// Store replaces the cache with a sorted catalog.
func (c *CatalogCache) Store(in []Choice) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.choices = CatalogChoices(in)
	c.loaded = true
}
