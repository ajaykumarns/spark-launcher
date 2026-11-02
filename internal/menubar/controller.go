package menubar

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// Controller owns the app's last known state and the CLI children it runs.
// It is safe for concurrent use by many UI readers, and it serializes its
// own network-bound work so refreshes never overlap.
type Controller struct {
	binary string
	run    runner
	now    func() time.Time

	// mu guards the snapshot and the cached catalog.
	mu      sync.RWMutex
	snap    Snapshot
	catalog object

	// refreshMu serializes status requests. Blocking is intentional: two
	// concurrent refreshes queue instead of piling up SSH sessions.
	refreshMu sync.Mutex

	// postRefreshTimeout bounds the post-load status refresh. It is a field
	// only so tests can shorten it; NewController sets the production value.
	postRefreshTimeout time.Duration
}

// NewController returns a controller that runs the sparky CLI at binary.
// binary must be an absolute path resolved by the app (bundled CLI first,
// then an explicit override). Loads and refreshes are never started here.
func NewController(binary string) *Controller {
	return newController(binary, execRunner)
}

func newController(binary string, run runner) *Controller {
	return &Controller{binary: binary, run: run, now: time.Now, postRefreshTimeout: postLoadRefreshTimeout}
}

// Snapshot returns a deep copy of the last known state. Slices and pointers
// are copied so the UI can keep or mutate the value without touching the
// controller's state.
func (c *Controller) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshotLocked()
}

func (c *Controller) snapshotLocked() Snapshot {
	out := c.snap
	out.Models = make([]ModelStatus, len(c.snap.Models))
	for i, m := range c.snap.Models {
		cp := m
		if m.Nodes != nil {
			cp.Nodes = append([]string(nil), m.Nodes...)
		}
		if m.ActiveRequests != nil {
			n := *m.ActiveRequests
			cp.ActiveRequests = &n
		}
		out.Models[i] = cp
	}
	out.Devices = make([]DeviceStatus, len(c.snap.Devices))
	for i, d := range c.snap.Devices {
		cp := d
		if d.TotalBytes != nil {
			v := *d.TotalBytes
			cp.TotalBytes = &v
		}
		if d.AvailableBytes != nil {
			v := *d.AvailableBytes
			cp.AvailableBytes = &v
		}
		out.Devices[i] = cp
	}
	return out
}

// Refresh asks the controller for the actual status. On success it replaces
// the models, devices and UpdatedAt. On failure it keeps the last successful
// data, records the friendly error, and returns that error. Concurrent
// callers serialize; only one status request runs at a time.
func (c *Controller) Refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		// A canceled context is the caller's decision, not stale cluster
		// state, so the last data and its error stay untouched.
		return err
	}
	result, runErr := c.run(ctx, c.binary, []string{"--json", "status"})
	if runErr != nil || result.Code != 0 {
		return c.recordRefreshError(friendlyError(result, runErr, "Status refresh"))
	}
	data, err := decodeObject(result.Stdout)
	if err != nil {
		return c.recordRefreshError(errors.New("Status refresh returned invalid data. Update Sparky on this Mac and the Sparks."))
	}
	parsed, err := parseStatus(data)
	if err != nil {
		return c.recordRefreshError(errors.New("Status refresh returned invalid data. Update Sparky on this Mac and the Sparks."))
	}
	c.mu.Lock()
	c.snap.Models = parsed.Models
	c.snap.Devices = parsed.Devices
	c.snap.UpdatedAt = c.now()
	c.snap.Error = ""
	c.mu.Unlock()
	return nil
}

func (c *Controller) recordRefreshError(err error) error {
	message := err.Error()
	c.mu.Lock()
	c.snap.Error = message
	c.mu.Unlock()
	return err
}

// Catalog returns the configured concrete models in deterministic order. It
// omits the synthetic spark-auto route and never starts a load.
func (c *Controller) Catalog(ctx context.Context) ([]Choice, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result, runErr := c.run(ctx, c.binary, []string{"--json", "models"})
	if runErr != nil || result.Code != 0 {
		return nil, friendlyError(result, runErr, "Model list")
	}
	data, err := decodeObject(result.Stdout)
	if err != nil {
		return nil, errors.New("Model list returned invalid data. Update Sparky on this Mac.")
	}
	choices, err := parseCatalog(data)
	if err != nil {
		return nil, errors.New("Model list returned invalid data. Update Sparky on this Mac.")
	}
	c.mu.Lock()
	c.catalog = data
	c.mu.Unlock()
	return choices, nil
}

// postLoadRefreshTimeout bounds the status refresh performed after a load,
// independently of the load's own context. A load often finishes long after
// its caller's bounded context would; the readiness check must still be
// bounded so a hung SSH session cannot keep Loading set forever.
const postLoadRefreshTimeout = 30 * time.Second

// errLoadInProgress is returned promptly by a duplicate load. It is a typed
// value so callers can distinguish "busy" from a real failure.
var errLoadInProgress = errors.New("A model load is already in progress.")

// Load validates the chosen model against the catalog, refuses a duplicate
// load while one is in flight, marks Loading immediately, then runs the exact
// `sparky --json load ID` command. On completion it records the outcome and
// refreshes actual readiness.
//
// Acceptance is non-blocking: a second call never waits on the first. It
// claims the single load slot first, then validates and then runs the remote
// CLI without holding any controller lock across I/O. Load is meant to be
// called on a background goroutine with a context independent of menu
// visibility.
func (c *Controller) Load(ctx context.Context, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	id = strings.TrimSpace(id)

	// Claim the load slot before doing any validation or I/O. If another load
	// is already active, fail immediately instead of queueing.
	c.mu.Lock()
	if c.snap.Loading {
		c.mu.Unlock()
		return errLoadInProgress
	}
	c.snap.Loading = true
	c.snap.LoadError = ""
	c.snap.LoadMessage = "Checking the model list..."
	c.mu.Unlock()

	done := false
	// Clear Loading and record any early failure on every exit path.
	defer func() {
		if !done {
			c.mu.Lock()
			c.snap.Loading = false
			c.mu.Unlock()
		}
	}()

	fail := func(err error) error {
		c.mu.Lock()
		c.snap.Loading = false
		c.snap.LoadError = err.Error()
		c.snap.LoadMessage = err.Error()
		c.mu.Unlock()
		done = true
		return err
	}

	if id == "" {
		return fail(errors.New("Choose a model to load."))
	}
	if err := ctx.Err(); err != nil {
		// The caller canceled before launch; there was no load to cancel.
		c.mu.Lock()
		c.snap.Loading = false
		c.mu.Unlock()
		done = true
		return err
	}

	c.mu.RLock()
	cached := c.catalog
	c.mu.RUnlock()

	choice, err := c.resolveChoice(ctx, id, cached)
	if err != nil {
		if ctx.Err() != nil {
			c.mu.Lock()
			c.snap.Loading = false
			c.mu.Unlock()
			done = true
			return ctx.Err()
		}
		return fail(err)
	}

	// Publish the loading message before the first remote byte is read.
	c.mu.Lock()
	if c.snap.Loading {
		c.snap.LoadMessage = "Loading " + choice.Name + "..."
	}
	c.mu.Unlock()

	result, runErr := c.run(ctx, c.binary, []string{"--json", "load", choice.ID})
	var loadErr error
	var message string
	var verified bool
	if runErr != nil || result.Code != 0 {
		loadErr = friendlyError(result, runErr, "Load "+choice.Name)
	} else {
		data, derr := decodeObject(result.Stdout)
		switch {
		case derr != nil:
			loadErr = errors.New("The load returned invalid data. Update Sparky on this Mac.")
		default:
			message, loadErr = parseLoadResult(data)
			verified = loadErr == nil
		}
	}

	// A canceled caller leaves the remote command to finish on its own; do
	// not report a result we did not observe.
	if ctx.Err() != nil && !verified {
		c.mu.Lock()
		c.snap.Loading = false
		c.snap.LoadMessage = "Load " + choice.Name + " was canceled. It may still be running on the Sparks."
		c.mu.Unlock()
		done = true
		return ctx.Err()
	}

	if loadErr != nil {
		// The CLI failed. Record it, then let a refresh (with the load's
		// bounded context) show the actual state; a failed refresh must not
		// turn this into success.
		if ctx.Err() == nil {
			refreshCtx, cancel := context.WithTimeout(ctx, c.postRefreshTimeout)
			_ = c.Refresh(refreshCtx)
			cancel()
		}
		return fail(loadErr)
	}

	// The CLI reported ready, but that is only its own claim. Confirm actual
	// readiness with a bounded refresh that does not depend on the caller's
	// context, and then require THIS selected model to be loaded and exactly
	// ready. A different ready model must never satisfy the check.
	if verified {
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.postRefreshTimeout)
		rerr := c.Refresh(refreshCtx)
		cancel()
		if rerr == nil {
			rerr = c.targetNotReady(choice)
		}
		if rerr != nil {
			c.mu.Lock()
			c.snap.Loading = false
			c.snap.LoadError = "The load reported ready, but readiness could not be confirmed: " + rerr.Error()
			c.snap.LoadMessage = choice.Name + ": readiness not confirmed. Refresh in a moment."
			c.mu.Unlock()
			done = true
			return errors.New("The load reported ready, but readiness could not be confirmed. Refresh in a moment.")
		}
	}

	c.mu.Lock()
	c.snap.Loading = false
	c.snap.LoadError = ""
	c.snap.LoadMessage = message
	c.mu.Unlock()
	done = true
	return nil
}

// resolveChoice validates the requested model against the current catalog.
// It uses the catalog cached by a previous Catalog call when one exists, and
// otherwise fetches a fresh one. There is deliberately no fallback to an
// arbitrary ID: the UI only offers concrete configured catalog choices, so a
// failed catalog must fail the load before any subprocess is started.
func (c *Controller) resolveChoice(ctx context.Context, id string, cached object) (Choice, error) {
	if cached == nil {
		if _, err := c.Catalog(ctx); err != nil {
			return Choice{}, err
		}
		c.mu.RLock()
		cached = c.catalog
		c.mu.RUnlock()
		if cached == nil {
			return Choice{}, errors.New("The model list is unavailable. Refresh and try again.")
		}
	}
	return resolveInCatalog(cached, id)
}

// targetNotReady returns nil only when the selected catalog model is present
// in the snapshot, loaded, and exactly "ready". Catalog IDs may carry a
// provider prefix (for example "garuda/deepseek"); status keys are canonical
// workload IDs without that prefix, so the comparison uses the suffix.
func (c *Controller) targetNotReady(choice Choice) error {
	target := shortName(choice.ID)
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, m := range c.snap.Models {
		// Prefer the exact canonical key but tolerate a status row that
		// carries the same provider-prefixed spelling as the catalog.
		if m.ID != target && shortName(m.ID) != target {
			continue
		}
		switch {
		case !m.Loaded:
			return errors.New(target + " is not loaded.")
		case m.State != "ready":
			if m.State == "" {
				return errors.New(target + " readiness is unknown.")
			}
			return errors.New(target + " is " + m.State + ", not ready.")
		}
		return nil
	}
	return errors.New(target + " is missing from the status.")
}
