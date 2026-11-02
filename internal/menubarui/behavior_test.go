package menubarui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sparky/internal/menubar"
)

type fakeCtrl struct {
	mu       sync.Mutex
	snap     menubar.Snapshot
	catalog  []menubar.Choice
	catErr   error
	refresh  func(context.Context) error
	loads    []string
	loadFunc func(string) error
}

func (f *fakeCtrl) Snapshot() menubar.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return deepCopy(f.snap)
}

func (f *fakeCtrl) Refresh(ctx context.Context) error {
	f.mu.Lock()
	fn := f.refresh
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return nil
}

func (f *fakeCtrl) Catalog(ctx context.Context) ([]menubar.Choice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.catErr != nil {
		return nil, f.catErr
	}
	return append([]menubar.Choice(nil), f.catalog...), nil
}

func (f *fakeCtrl) Load(ctx context.Context, id string) error {
	f.mu.Lock()
	f.loads = append(f.loads, id)
	fn := f.loadFunc
	f.mu.Unlock()
	if fn != nil {
		return fn(id)
	}
	return nil
}

func deepCopy(s menubar.Snapshot) menubar.Snapshot {
	out := s
	out.Models = append([]menubar.ModelStatus(nil), s.Models...)
	out.Devices = append([]menubar.DeviceStatus(nil), s.Devices...)
	return out
}

func u64(v uint64) *uint64 { return &v }
func iptr(v int) *int      { return &v }

func TestMemoryUsesTotalMinusAvailableAndUnknown(t *testing.T) {
	d := menubar.DeviceStatus{ID: "spark01", TotalBytes: u64(64 * gib), AvailableBytes: u64(24 * gib)}
	if got := InUseGiB(d); got != "40.0 GiB" {
		t.Fatalf("in-use = %q", got)
	}
	if got := NodeMemory(d); got != "spark01: 40.0 GiB in use of 64.0 GiB, 24.0 GiB available" {
		t.Fatalf("node line = %q", got)
	}
	unknown := menubar.DeviceStatus{ID: "spark02"}
	if got := InUseGiB(unknown); got != "" {
		t.Fatalf("unknown in-use must be empty, got %q", got)
	}
	if got := NodeMemory(unknown); got != "spark02: memory unknown" {
		t.Fatalf("unknown node line = %q", got)
	}
}

func TestRequestsUnknownIsNotZero(t *testing.T) {
	if got := RequestsLabel(menubar.ModelStatus{}); got != "requests unknown" {
		t.Fatalf("unknown requests = %q", got)
	}
	if got := RequestsLabel(menubar.ModelStatus{ActiveRequests: iptr(0)}); got != "0 requests" {
		t.Fatalf("zero requests = %q", got)
	}
	if got := RequestsLabel(menubar.ModelStatus{ActiveRequests: iptr(1)}); got != "1 request" {
		t.Fatalf("one request = %q", got)
	}
}

func TestLoadedModelsShowsActualState(t *testing.T) {
	s := menubar.Snapshot{Models: []menubar.ModelStatus{
		{ID: "garuda/deepseek", Loaded: true, State: "ready", Nodes: []string{"spark01"}},
		{ID: "garuda/other", Loaded: false, State: "stopped"},
	}}
	got := LoadedModels(s)
	if len(got) != 1 || got[0].ID != "garuda/deepseek" {
		t.Fatalf("loaded = %+v", got)
	}
	if lbl := StateLabel(s.Models[0], s); lbl != "Ready" {
		t.Fatalf("ready label = %q", lbl)
	}
	if lbl := StateLabel(s.Models[1], s); lbl != "Stopped" {
		t.Fatalf("stopped label = %q", lbl)
	}
}

func TestErrorStateIsUnreachable(t *testing.T) {
	s := menubar.Snapshot{Error: "gateway timeout"}
	if got := StateLabel(menubar.ModelStatus{Loaded: true}, s); got != "Unreachable" {
		t.Fatalf("error label = %q", got)
	}
	if got := StatusLine(s); got != "Controller unreachable" {
		t.Fatalf("status line = %q", got)
	}
}

func TestCatalogChoicesSortedNoSynthetic(t *testing.T) {
	got := CatalogChoices([]Choice{
		{ID: "garuda/z", Name: "Z"},
		{ID: "garuda/spark-auto", Name: "Auto"},
		{ID: "garuda/a", Name: "A"},
		{ID: "", Name: "empty"},
	})
	if len(got) != 2 || got[0].ID != "garuda/a" || got[1].ID != "garuda/z" {
		t.Fatalf("catalog = %+v", got)
	}
}

func TestFriendlyErrorTrimsMultiline(t *testing.T) {
	err := errors.New("authentication failed\nstacktrace: secret")
	if got := FriendlyError(err); got != "authentication failed" {
		t.Fatalf("friendly = %q", got)
	}
}

func TestLoadTrackerRejectsDuplicate(t *testing.T) {
	var ctrl fakeCtrl
	release := make(chan struct{})
	ctrl.loadFunc = func(string) error { <-release; return nil }
	tracker := &LoadTracker{}
	if !LoadInBackground(&ctrl, tracker, "garuda/a") {
		t.Fatal("first load must start")
	}
	if LoadInBackground(&ctrl, tracker, "garuda/b") {
		t.Fatal("duplicate load must be refused")
	}
	if _, running := tracker.Running(); !running {
		t.Fatal("tracker must report running")
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for _, running := tracker.Running(); running; _, running = tracker.Running() {
		if time.Now().After(deadline) {
			t.Fatal("load did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(ctrl.loads) != 1 || ctrl.loads[0] != "garuda/a" {
		t.Fatalf("loads = %v", ctrl.loads)
	}
	if lbl := tracker.Label(); !strings.HasPrefix(lbl, "Load finished in") {
		t.Fatalf("finished label = %q", lbl)
	}
}

func TestLoadTrackerRecordsError(t *testing.T) {
	var ctrl fakeCtrl
	ctrl.loadFunc = func(string) error { return errors.New("model rejected\nraw stderr") }
	tracker := &LoadTracker{}
	LoadInBackground(&ctrl, tracker, "garuda/a")
	deadline := time.Now().Add(2 * time.Second)
	for _, running := tracker.Running(); running; _, running = tracker.Running() {
		if time.Now().After(deadline) {
			t.Fatal("load did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if lbl := tracker.Label(); lbl != "Load failed: model rejected" {
		t.Fatalf("error label = %q", lbl)
	}
}

func TestLoadResultLineVisibleFailures(t *testing.T) {
	// Plain failure is reported as a historical last-load result, not as the
	// current model being failed.
	s := menubar.Snapshot{LoadError: "model rejected"}
	if got := LoadResultLine(s); got != "Last load failed — see diagnostics" {
		t.Fatalf("failure line = %q", got)
	}
	// Readiness-unknown load gets its own wording.
	s = menubar.Snapshot{
		LoadError:   "The load reported ready, but readiness could not be confirmed: timeout",
		LoadMessage: "glm: readiness not confirmed. Refresh in a moment.",
	}
	if got := LoadResultLine(s); got != "Load readiness not confirmed — see diagnostics" {
		t.Fatalf("readiness line = %q", got)
	}
	// Current model rows keep showing the real state; the result line never
	// rewrites them.
	s.Models = []menubar.ModelStatus{{ID: "glm", Name: "GLM", State: "ready", Loaded: true}}
	rows := ModelRows(s)
	if len(rows) != 1 || !strings.HasPrefix(rows[0], "GLM") {
		t.Fatalf("rows = %v", rows)
	}
	if strings.Contains(rows[0], "failed") {
		t.Fatalf("current model row must not inherit historical failure: %q", rows[0])
	}
}

func TestLoadResultLineClearsOnRecovery(t *testing.T) {
	if got := LoadResultLine(menubar.Snapshot{}); got != "" {
		t.Fatalf("clean snapshot must show no result: %q", got)
	}
	// A running load shows its own elapsed line, not a stale error.
	if got := LoadResultLine(menubar.Snapshot{Loading: true, LoadError: "old"}); got != "" {
		t.Fatalf("loading snapshot must not show stale failure: %q", got)
	}
	// Next successful load clears LoadError and the root result disappears.
	s := menubar.Snapshot{LoadMessage: "glm is ready."}
	if got := LoadResultLine(s); got != "" {
		t.Fatalf("recovered snapshot = %q", got)
	}
}

func TestAcceptReservesTrackerSynchronously(t *testing.T) {
	var ctrl fakeCtrl
	release := make(chan struct{})
	ctrl.loadFunc = func(string) error { <-release; return nil }
	tracker := &LoadTracker{}
	if !Accept(tracker, "garuda/a") {
		t.Fatal("first accept must succeed")
	}
	// Acceptance is synchronous: the tracker is active immediately, before the
	// remote work starts, so a concurrent quit sees the in-progress load.
	if _, running := tracker.Running(); !running {
		t.Fatal("tracker must be running before Run starts")
	}
	if Accept(tracker, "garuda/b") {
		t.Fatal("duplicate accept must be refused")
	}
	go Run(&ctrl, tracker, "garuda/a")
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for _, running := tracker.Running(); running; _, running = tracker.Running() {
		if time.Now().After(deadline) {
			t.Fatal("load did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(ctrl.loads) != 1 || ctrl.loads[0] != "garuda/a" {
		t.Fatalf("loads = %v", ctrl.loads)
	}
}

func TestPollerDoesNotOverlap(t *testing.T) {
	var active, maxActive int32
	var ctrl fakeCtrl
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	ctrl.refresh = func(ctx context.Context) error {
		n := atomic.AddInt32(&active, 1)
		for {
			cur := atomic.LoadInt32(&maxActive)
			if n <= cur || atomic.CompareAndSwapInt32(&maxActive, cur, n) {
				break
			}
		}
		started <- struct{}{}
		<-release
		atomic.AddInt32(&active, -1)
		return nil
	}
	p := NewPoller(&ctrl, 10*time.Millisecond, time.Second)
	// Force concurrent attempts on separate goroutines.
	done := make(chan struct{}, 2)
	go func() { defer func() { done <- struct{}{} }(); _ = p.RefreshNow(context.Background()) }()
	<-started
	go func() { defer func() { done <- struct{}{} }(); _ = p.RefreshNow(context.Background()) }()
	close(release)
	<-done
	<-done
	if maxActive != 1 {
		t.Fatalf("max concurrent refreshes = %d", maxActive)
	}
}

func TestPollerBacksOffAfterFailure(t *testing.T) {
	var calls int32
	var ctrl fakeCtrl
	ctrl.refresh = func(context.Context) error { atomic.AddInt32(&calls, 1); return errors.New("offline") }
	p := NewPoller(&ctrl, 5*time.Millisecond, time.Second)
	_ = p.RefreshNow(context.Background())
	if p.failures != 1 {
		t.Fatalf("failures = %d", p.failures)
	}
	if got := p.LastRefresh(); got == "" || got[:6] != "failed" {
		t.Fatalf("last refresh = %q", got)
	}
}

func TestPollerPreservesSnapshotOnFailure(t *testing.T) {
	var ctrl fakeCtrl
	ctrl.snap = menubar.Snapshot{
		UpdatedAt: time.Now(),
		Models:    []menubar.ModelStatus{{ID: "garuda/a", Loaded: true}},
	}
	ctrl.refresh = func(context.Context) error { return errors.New("offline") }
	p := NewPoller(&ctrl, time.Second, time.Second)
	_ = p.RefreshNow(context.Background())
	s := ctrl.Snapshot()
	if len(s.Models) != 1 || !s.Models[0].Loaded {
		t.Fatalf("snapshot changed on failure: %+v", s)
	}
}

func TestFetchCatalogStoresSorted(t *testing.T) {
	var ctrl fakeCtrl
	ctrl.catalog = []Choice{{ID: "garuda/b"}, {ID: "garuda/a"}, {ID: "garuda/spark-auto"}}
	cache := &CatalogCache{}
	if err := FetchCatalog(context.Background(), &ctrl, cache, time.Second); err != nil {
		t.Fatal(err)
	}
	got := cache.Choices()
	if len(got) != 2 || got[0].ID != "garuda/a" || got[1].ID != "garuda/b" {
		t.Fatalf("cache = %+v", got)
	}
}

func TestFetchCatalogErrorKeepsCache(t *testing.T) {
	cache := &CatalogCache{}
	cache.Store([]Choice{{ID: "garuda/a"}})
	var ctrl fakeCtrl
	ctrl.catErr = errors.New("catalog unavailable")
	if err := FetchCatalog(context.Background(), &ctrl, cache, time.Second); err == nil {
		t.Fatal("expected error")
	}
	if got := cache.Choices(); len(got) != 1 || got[0].ID != "garuda/a" {
		t.Fatalf("cache changed on error: %+v", got)
	}
}

func TestDiagnosticsHasNoSecrets(t *testing.T) {
	d := Diagnostics{
		Version:  "0.1.0",
		CLIPath:  "/usr/local/bin/sparky",
		Login:    false,
		Snapshot: menubar.Snapshot{Error: "auth failed\npassword=secret"},
	}
	text := DiagnosticsText(d)
	for _, banned := range []string{"secret", "password"} {
		if strings.Contains(text, banned) {
			t.Fatalf("diagnostics leaked %q: %s", banned, text)
		}
	}
	if !strings.Contains(text, "Sparky menu bar 0.1.0") {
		t.Fatalf("missing version: %s", text)
	}
}

func TestIconTitleStates(t *testing.T) {
	if IconTitle(menubar.Snapshot{}, false) != "" {
		t.Fatal("idle title must be blank")
	}
	if IconTitle(menubar.Snapshot{Loading: true}, false) != "…" {
		t.Fatal("loading title")
	}
	if IconTitle(menubar.Snapshot{Error: "x"}, true) != "!" {
		t.Fatal("error title")
	}
	one := menubar.Snapshot{Models: []menubar.ModelStatus{{ID: "garuda/deepseek-v3", Loaded: true}}}
	if got := IconTitle(one, true); got != "deepseek-v3" {
		t.Fatalf("single title = %q", got)
	}
	if got := IconTitle(one, false); got != "" {
		t.Fatalf("title must be blank when disabled, got %q", got)
	}
	two := menubar.Snapshot{Models: []menubar.ModelStatus{
		{ID: "a", Loaded: true}, {ID: "b", Loaded: true},
	}}
	if got := IconTitle(two, true); got != "2 models" {
		t.Fatalf("multi title = %q", got)
	}
	// Recovery clears the glyph.
	if got := IconTitle(menubar.Snapshot{UpdatedAt: time.Now()}, true); got != "" {
		t.Fatalf("recovered title = %q", got)
	}
}

func TestModelRowsShowLoadedState(t *testing.T) {
	s := menubar.Snapshot{UpdatedAt: time.Now(), Models: []menubar.ModelStatus{
		{ID: "garuda/deepseek", Name: "DeepSeek", State: "ready", Loaded: true,
			Nodes: []string{"spark01", "spark02"}, ActiveRequests: iptr(3)},
		{ID: "garuda/other", Loaded: false, State: "stopped"},
	}}
	rows := ModelRows(s)
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	want := "DeepSeek · Ready · spark01, spark02 · 3 requests"
	if rows[0] != want {
		t.Fatalf("row = %q, want %q", rows[0], want)
	}
	if got := ModelsTitle(s); got != "Loaded models (1)" {
		t.Fatalf("title = %q", got)
	}
}

func TestModelRowsExplicitEmptyStates(t *testing.T) {
	if got := ModelRows(menubar.Snapshot{}); len(got) != 1 || !strings.Contains(got[0], "Unknown") {
		t.Fatalf("no data rows = %v", got)
	}
	empty := menubar.Snapshot{UpdatedAt: time.Now()}
	if got := ModelRows(empty); len(got) != 1 || got[0] != "No model loaded" {
		t.Fatalf("empty rows = %v", got)
	}
}

func TestModelRowsLoadedWithoutStateIsUnknown(t *testing.T) {
	s := menubar.Snapshot{UpdatedAt: time.Now(), Models: []menubar.ModelStatus{
		{ID: "garuda/x", Loaded: true},
	}}
	rows := ModelRows(s)
	if !strings.Contains(rows[0], "Unknown (loaded)") {
		t.Fatalf("row = %q", rows[0])
	}
}

func TestStateLabelLoadedIsNotReady(t *testing.T) {
	s := menubar.Snapshot{}
	if got := StateLabel(menubar.ModelStatus{Loaded: true, State: "loaded"}, s); got != "Loaded" {
		t.Fatalf("loaded label = %q", got)
	}
	if got := StateLabel(menubar.ModelStatus{Loaded: true}, s); got != "Unknown (loaded)" {
		t.Fatalf("empty-state label = %q", got)
	}
	if got := StateLabel(menubar.ModelStatus{State: "ready"}, s); got != "Unknown" {
		t.Fatalf("ready-not-loaded label = %q", got)
	}
}

func TestFreshnessPreservesTimeWhenStale(t *testing.T) {
	at := time.Now().Add(-5 * time.Minute)
	s := menubar.Snapshot{UpdatedAt: at, Error: "offline"}
	got := FreshnessLine(s)
	if !strings.Contains(got, "Last update:") || !strings.Contains(got, "stale") {
		t.Fatalf("stale line = %q", got)
	}
	if got := FreshnessLine(menubar.Snapshot{}); got != "No successful status yet" {
		t.Fatalf("no-data line = %q", got)
	}
	if got := StatusLine(menubar.Snapshot{}); got != "No successful status yet" {
		t.Fatalf("no-data status = %q", got)
	}
}

func TestNodeMemorySaysAvailable(t *testing.T) {
	d := menubar.DeviceStatus{ID: "spark01", TotalBytes: u64(10 * gib), AvailableBytes: u64(4 * gib)}
	if got := NodeMemory(d); !strings.Contains(got, "available") || strings.Contains(got, "free") {
		t.Fatalf("node line = %q", got)
	}
}

func TestPollerAutomaticRefreshes(t *testing.T) {
	var n int32
	var ctrl fakeCtrl
	ctrl.refresh = func(context.Context) error { atomic.AddInt32(&n, 1); return nil }
	p := NewPoller(&ctrl, 10*time.Millisecond, time.Second)
	p.Start()
	defer p.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&n) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("automatic refreshes = %d, want at least 3", atomic.LoadInt32(&n))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPollerCoalescesRequests(t *testing.T) {
	var n int32
	var ctrl fakeCtrl
	release := make(chan struct{}, 1)
	ctrl.refresh = func(context.Context) error { atomic.AddInt32(&n, 1); return nil }
	p := NewPoller(&ctrl, time.Hour, time.Second)
	p.Start()
	defer p.Stop()
	// Burst many requests; they must collapse into a few refreshes, not one per
	// request. One initial automatic refresh plus a small number of wake-ups.
	for i := 0; i < 50; i++ {
		p.Request()
	}
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&n) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("refresh count = %d after requests", atomic.LoadInt32(&n))
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if got := atomic.LoadInt32(&n); got > 5 {
		t.Fatalf("50 coalesced requests produced %d refreshes", got)
	}
	_ = release
}

func TestPollerBackoffDelayGrows(t *testing.T) {
	var ctrl fakeCtrl
	ctrl.refresh = func(context.Context) error { return errors.New("offline") }
	p := NewPoller(&ctrl, 10*time.Millisecond, time.Second)
	base := p.nextDelay()
	if base != 10*time.Millisecond {
		t.Fatalf("base delay = %v", base)
	}
	for i := 0; i < 3; i++ {
		_ = p.RefreshNow(context.Background())
	}
	got := p.nextDelay()
	if got <= base {
		t.Fatalf("backoff did not grow: %v", got)
	}
	if got > p.maxBack {
		t.Fatalf("backoff exceeded max: %v", got)
	}
}

func TestPollerStopIsClean(t *testing.T) {
	var ctrl fakeCtrl
	p := NewPoller(&ctrl, time.Hour, time.Second)
	p.Start()
	p.Stop()
	p.Stop() // idempotent
}
