package menubarui

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Poller drives background status refreshes for a Controller. It never
// overlaps refreshes, never blocks callers, backs off after failures, and
// preserves whatever the backend cached.
type Poller struct {
	ctrl     Controller
	timeout  time.Duration
	interval time.Duration
	maxBack  time.Duration

	refreshing atomic.Bool
	lastOK     atomic.Int64 // unix nanos of last success, 0 = never
	failures   int32

	notify chan struct{} // coalesced wake-up
	stop   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup

	mu         sync.Mutex
	lastErr    string
	lastStart  time.Time
	lastFinish time.Time
}

// NewPoller builds a Poller. interval is the low-rate idle cadence; timeout
// bounds each CLI-backed refresh.
func NewPoller(ctrl Controller, interval, timeout time.Duration) *Poller {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Poller{
		ctrl:     ctrl,
		timeout:  timeout,
		interval: interval,
		maxBack:  10 * interval,
		notify:   make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}
}

// Start begins the background refresh loop.
func (p *Poller) Start() {
	p.wg.Add(1)
	go p.loop()
}

// Stop ends the loop. In-flight refreshes are allowed to finish; a load in
// progress is never canceled here.
func (p *Poller) Stop() {
	p.once.Do(func() { close(p.stop) })
	p.wg.Wait()
}

// Request schedules an out-of-band refresh (manual or on-open). Requests
// coalesce; it never blocks.
func (p *Poller) Request() {
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

// RefreshNow runs one refresh synchronously on the caller's goroutine. The
// app calls this only from a background goroutine when the menu is opened.
func (p *Poller) RefreshNow(ctx context.Context) error {
	if !p.refreshing.CompareAndSwap(false, true) {
		return nil
	}
	defer p.refreshing.Store(false)
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	p.mu.Lock()
	p.lastStart = time.Now()
	p.mu.Unlock()
	err := p.ctrl.Refresh(ctx)
	p.mu.Lock()
	p.lastFinish = time.Now()
	if err != nil {
		p.failures++
		p.lastErr = FriendlyError(err)
	} else {
		p.failures = 0
		p.lastErr = ""
		p.lastOK.Store(time.Now().UnixNano())
	}
	p.mu.Unlock()
	return err
}

// LastRefresh describes the most recent refresh for diagnostics.
func (p *Poller) LastRefresh() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastFinish.IsZero() {
		return "not yet"
	}
	if p.lastErr != "" {
		return "failed " + HumanAge(time.Since(p.lastFinish)) + ": " + p.lastErr
	}
	return "ok " + HumanAge(time.Since(p.lastFinish))
}

func (p *Poller) loop() {
	defer p.wg.Done()
	for {
		// Refresh, then wait for the scheduled idle interval, an explicit
		// wake-up, or shutdown. Coalesced wake-ups collapse into one early
		// refresh. This loop always keeps refreshing on its own cadence.
		p.RefreshNow(context.Background())
		delay := p.nextDelay()
		if !p.wait(delay) {
			return
		}
	}
}

// wait blocks until the delay elapses (returns true to refresh again), an
// explicit request arrives (returns true immediately), or the poller stops
// (returns false).
func (p *Poller) wait(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-p.stop:
		return false
	case <-timer.C:
		return true
	case <-p.notify:
		// Drain queued wake-ups so a burst causes only one extra refresh.
		select {
		case <-p.notify:
		default:
		}
		return true
	}
}

// nextDelay returns the idle cadence, backing off exponentially on repeated
// failures up to maxBack.
func (p *Poller) nextDelay() time.Duration {
	p.mu.Lock()
	f := p.failures
	p.mu.Unlock()
	if f <= 0 {
		return p.interval
	}
	back := p.interval << uint(minInt(f, 8))
	if back > p.maxBack || back <= 0 {
		back = p.maxBack
	}
	return back
}

// LastSuccess returns the last successful refresh time, zero if never.
func (p *Poller) LastSuccess() time.Time {
	n := p.lastOK.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func minInt(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}
