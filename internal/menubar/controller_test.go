package menubar

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCLI is a synthetic stand-in for `sparky --json ...`. It records every
// argument vector and returns scripted results keyed by the first argument.
type fakeCLI struct {
	mu    sync.Mutex
	calls [][]string
	// perCommand maps "status", "models", "load" to queued results.
	results map[string][]runResult
	errs    map[string][]error
	// onRun, when set, runs inside the call and can block for concurrency
	// tests.
	onRun func(ctx context.Context, binary string, args []string)
}

func newFakeCLI() *fakeCLI {
	return &fakeCLI{results: map[string][]runResult{}, errs: map[string][]error{}}
}

func (f *fakeCLI) queue(cmd string, result runResult, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[cmd] = append(f.results[cmd], result)
	f.errs[cmd] = append(f.errs[cmd], err)
}

func (f *fakeCLI) runner() runner {
	return func(ctx context.Context, binary string, args []string) (runResult, error) {
		f.mu.Lock()
		f.calls = append(f.calls, append([]string(nil), args...))
		onRun := f.onRun
		cmd := ""
		if len(args) > 1 && args[0] == "--json" {
			cmd = args[1]
		}
		var result runResult
		var err error
		if q := f.results[cmd]; len(q) > 0 {
			result = q[0]
			f.results[cmd] = q[1:]
			err = f.errs[cmd][0]
			f.errs[cmd] = f.errs[cmd][1:]
		} else {
			err = errors.New("fake CLI: no scripted result for " + cmd)
			result.Code = -1
		}
		f.mu.Unlock()
		if onRun != nil {
			onRun(ctx, binary, args)
		}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, err
	}
}

func (f *fakeCLI) callsFor(cmd string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if len(c) > 1 && c[0] == "--json" && c[1] == cmd {
			out = append(out, c)
		}
	}
	return out
}

func okJSON(t *testing.T, v any) runResult {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		// A fixture that cannot be marshalled would silently become empty
		// output and turn into a confusing product failure. Fail loudly.
		t.Fatalf("fixture marshal: %v", err)
	}
	return runResult{Stdout: b, Code: 0}
}

// statusJSON mirrors the real status wire shape: models keyed by canonical
// workload ID with kind/loaded/state/nodes/active_requests, and nodes keyed
// by spark01/spark02 with memory_bytes MemTotal/MemAvailable.
func statusJSON() object {
	return object{
		"models": object{
			"deepseek": object{
				"kind": "llm", "loaded": true, "state": "ready",
				"nodes": []any{"spark01", "spark02"}, "active_requests": json.Number("3"),
			},
			"glm": object{
				"kind": "llm", "loaded": true, "state": "ready",
				"nodes": []any{"spark01", "spark02"}, "active_requests": json.Number("0"),
			},
			"kokoro": object{
				"kind": "audio", "loaded": true, "state": "running",
				"nodes": []any{"spark02"},
			},
			"comfyui": object{
				"kind": "comfyui", "loaded": true, "state": "running",
				"nodes": []any{"spark01"},
			},
		},
		"activity_known": true,
		"nodes": object{
			"spark01": object{"memory_bytes": object{
				"MemTotal": json.Number("16090348"), "MemAvailable": json.Number("8000000"),
			}},
			"spark02": object{"memory_bytes": object{
				"MemTotal": json.Number("16090348"), "MemAvailable": nil,
			}},
		},
		"memory_usage": object{
			"spark01": object{"workloads": []any{
				object{"id": "deepseek", "gpu_bytes": json.Number("40000000000")},
			}},
		},
		"admission": object{"waiting": []any{}},
	}
}

// catalogJSON mirrors `sparky --json models`: a models array with id/name/
// aliases/context_tokens, including the synthetic spark-auto route.
func catalogJSON() object {
	return object{
		"gateway": "https://spark.example:8443",
		"models": []any{
			object{"id": "spark/spark-auto", "name": "Spark Auto", "aliases": []any{"auto"}, "context_tokens": json.Number("32768")},
			object{"id": "spark/glm", "name": "GLM", "aliases": []any{"glm"}, "context_tokens": json.Number("131072")},
			object{"id": "spark/deepseek", "name": "DeepSeek", "aliases": []any{"deep"}, "context_tokens": json.Number("65536")},
		},
	}
}

func newTestController(f *fakeCLI) *Controller {
	c := newController("/abs/path/sparky", f.runner())
	c.now = func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) }
	return c
}

func TestSnapshotEmptyIsSafe(t *testing.T) {
	c := NewController("/abs/path/sparky")
	s := c.Snapshot()
	if len(s.Models) != 0 || len(s.Devices) != 0 || s.Error != "" || s.Loading {
		t.Fatalf("unexpected zero snapshot: %+v", s)
	}
	if !s.UpdatedAt.IsZero() {
		t.Fatalf("fresh controller must have no last-success time")
	}
}

func TestRefreshParsesShape(t *testing.T) {
	f := newFakeCLI()
	f.queue("status", okJSON(t, statusJSON()), nil)
	c := newTestController(f)

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	s := c.Snapshot()
	if len(s.Models) != 2 {
		t.Fatalf("models = %d, want only the 2 language models (audio/comfyui excluded)", len(s.Models))
	}
	byID := map[string]ModelStatus{}
	for _, m := range s.Models {
		byID[m.ID] = m
	}
	if _, ok := byID["kokoro"]; ok {
		t.Fatalf("loaded audio must not appear as a language model")
	}
	if _, ok := byID["comfyui"]; ok {
		t.Fatalf("comfyui must not appear as a language model")
	}
	ds := byID["deepseek"]
	if !ds.Loaded || ds.State != "ready" || strings.Join(ds.Nodes, ",") != "spark01,spark02" {
		t.Fatalf("deepseek row wrong: %+v", ds)
	}
	if ds.ActiveRequests == nil || *ds.ActiveRequests != 3 {
		t.Fatalf("deepseek active requests = %v, want 3", ds.ActiveRequests)
	}
	if ds.Name != "deepseek" {
		t.Fatalf("missing name should default to ID, got %q", ds.Name)
	}
	if byID["glm"].ActiveRequests == nil || *byID["glm"].ActiveRequests != 0 {
		t.Fatalf("known zero requests must stay a known zero, got %v", byID["glm"].ActiveRequests)
	}
	// A row without active_requests must stay unknown (nil), not zero.
	s2 := statusJSON()
	obj(obj(s2["models"])["deepseek"])["active_requests"] = nil
	parsed, err := parseStatus(s2)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range parsed.Models {
		if m.ID == "deepseek" && m.ActiveRequests != nil {
			t.Fatalf("missing active_requests must stay unknown, got %d", *m.ActiveRequests)
		}
	}

	if len(s.Devices) != 2 {
		t.Fatalf("devices = %d, want 2", len(s.Devices))
	}
	d1, d2 := s.Devices[0], s.Devices[1]
	if d1.ID != "spark01" || d2.ID != "spark02" {
		t.Fatalf("devices not in deterministic order: %+v", s.Devices)
	}
	if d1.TotalBytes == nil || *d1.TotalBytes != 16090348 || d1.AvailableBytes == nil || *d1.AvailableBytes != 8000000 {
		t.Fatalf("spark01 memory wrong: %+v", d1)
	}
	if d2.TotalBytes == nil || *d2.TotalBytes != 16090348 {
		t.Fatalf("spark02 total wrong: %+v", d2)
	}
	if d2.AvailableBytes != nil {
		t.Fatalf("unknown available memory must stay nil, got %d", *d2.AvailableBytes)
	}
	if got := s.UpdatedAt; !got.Equal(time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("UpdatedAt = %v", got)
	}

	calls := f.callsFor("status")
	if len(calls) != 1 || strings.Join(calls[0], " ") != "--json status" {
		t.Fatalf("status args = %v", calls)
	}
}

func TestSnapshotIsDeepCopy(t *testing.T) {
	f := newFakeCLI()
	f.queue("status", okJSON(t, statusJSON()), nil)
	c := newTestController(f)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := c.Snapshot()
	// Mutate every pointer/slice the reader can touch.
	*first.Models[0].ActiveRequests = 99
	first.Models[0].Nodes = append(first.Models[0].Nodes, "evil")
	*first.Devices[0].TotalBytes = 1
	second := c.Snapshot()
	if *second.Models[0].ActiveRequests == 99 {
		t.Fatalf("mutating a snapshot changed controller state (active requests)")
	}
	if len(second.Models[0].Nodes) != 2 {
		t.Fatalf("mutating a snapshot changed controller state (nodes)")
	}
	if *second.Devices[0].TotalBytes == 1 {
		t.Fatalf("mutating a snapshot changed controller state (bytes)")
	}
}

func TestRefreshFailurePreservesLastData(t *testing.T) {
	f := newFakeCLI()
	f.queue("status", okJSON(t, statusJSON()), nil)
	f.queue("status", runResult{Stderr: []byte(`{"error":"SSH host unreachable","code":1}`), Code: 1}, nil)
	c := newTestController(f)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := c.Snapshot()

	err := c.Refresh(context.Background())
	if err == nil || err.Error() != "SSH host unreachable" {
		t.Fatalf("refresh error = %v, want friendly envelope message", err)
	}
	s := c.Snapshot()
	if len(s.Models) != len(first.Models) || len(s.Devices) != len(first.Devices) {
		t.Fatalf("failed refresh dropped last data: %+v", s)
	}
	if !s.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("failed refresh must not advance last-success time")
	}
	if s.Error != "SSH host unreachable" {
		t.Fatalf("Error = %q", s.Error)
	}
	// A later success clears the error.
	f.queue("status", okJSON(t, statusJSON()), nil)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.Snapshot().Error; got != "" {
		t.Fatalf("successful refresh should clear Error, got %q", got)
	}
}

func TestRefreshMalformedAndFailureMessages(t *testing.T) {
	cases := []struct {
		name   string
		result runResult
		err    error
		want   string
	}{
		{"not json", runResult{Stdout: []byte("<html>nope</html>"), Code: 0}, nil,
			"Status refresh returned invalid data. Update Sparky on this Mac and the Sparks."},
		{"missing models", runResult{Stdout: []byte(`{"nodes":{}}`), Code: 0}, nil,
			"Status refresh returned invalid data. Update Sparky on this Mac and the Sparks."},
		{"trailing data", runResult{Stdout: []byte(`{"models":{}} {"models":{}}`), Code: 0}, nil,
			"Status refresh returned invalid data. Update Sparky on this Mac and the Sparks."},
		{"bad model row", runResult{Stdout: []byte(`{"models":{"x":42}}`), Code: 0}, nil,
			"Status refresh returned invalid data. Update Sparky on this Mac and the Sparks."},
		{"usage", runResult{Stderr: []byte("Usage: sparky status"), Code: 2}, nil,
			"Status refresh was rejected by the Sparky CLI (usage error)."},
		{"interrupted", runResult{Code: 130}, nil,
			"Status refresh was interrupted on the Sparks. Remote work may continue."},
		{"no start", runResult{Code: -1}, errors.New("exec: not found"),
			"Could not start the Sparky CLI. Check the bundled CLI path."},
		{"generic failure hides stderr", runResult{Stderr: []byte("ssh: secret host token 12345"), Code: 1}, nil,
			"Status refresh failed. Run sparky status in a terminal for details."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCLI()
			f.queue("status", tc.result, tc.err)
			c := newTestController(f)
			err := c.Refresh(context.Background())
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if s := c.Snapshot(); s.Error != tc.want {
				t.Fatalf("snapshot Error = %q", s.Error)
			}
		})
	}
}

func TestRefreshCanceledContextLeavesState(t *testing.T) {
	f := newFakeCLI()
	f.queue("status", okJSON(t, statusJSON()), nil)
	c := newTestController(f)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Refresh(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	s := c.Snapshot()
	if len(s.Models) != 2 || s.Error != "" {
		t.Fatalf("canceled refresh must not change state: %+v", s)
	}
}

func TestRefreshSerializesConcurrentCalls(t *testing.T) {
	var active, overlaps, calls int32
	f := newFakeCLI()
	f.queue("status", okJSON(t, statusJSON()), nil)
	f.queue("status", okJSON(t, statusJSON()), nil)
	f.queue("status", okJSON(t, statusJSON()), nil)
	f.onRun = func(ctx context.Context, binary string, args []string) {
		cur := atomic.AddInt32(&active, 1)
		if cur > 1 {
			atomic.AddInt32(&overlaps, 1)
		}
		atomic.AddInt32(&calls, 1)
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&active, -1)
	}
	c := newTestController(f)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Refresh(context.Background()); err != nil {
				t.Errorf("concurrent refresh: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&overlaps); got != 0 {
		t.Fatalf("status requests overlapped %d times", got)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("calls = %d, want 3", got)
	}
}

func TestCatalogOmitsAutoAndIsDeterministic(t *testing.T) {
	f := newFakeCLI()
	f.queue("models", okJSON(t, catalogJSON()), nil)
	c := newTestController(f)
	choices, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, ch := range choices {
		ids = append(ids, ch.ID)
	}
	sort.Strings(ids)
	if strings.Join(ids, ",") != "spark/deepseek,spark/glm" {
		t.Fatalf("choices = %v, want concrete models only", ids)
	}
	if choices[0].Name != "DeepSeek" || choices[1].Name != "GLM" {
		t.Fatalf("names = %+v", choices)
	}
	calls := f.callsFor("models")
	if len(calls) != 1 || strings.Join(calls[0], " ") != "--json models" {
		t.Fatalf("models args = %v", calls)
	}
}

func TestCatalogInvalidShapes(t *testing.T) {
	cases := []struct {
		name   string
		result runResult
	}{
		{"not json", runResult{Stdout: []byte("nope"), Code: 0}},
		{"missing array", runResult{Stdout: []byte(`{"models":{}}`), Code: 0}},
		{"row not object", runResult{Stdout: []byte(`{"models":[1]}`), Code: 0}},
		{"row missing id", runResult{Stdout: []byte(`{"models":[{"name":"x"}]}`), Code: 0}},
		{"duplicate id", runResult{Stdout: []byte(`{"models":[{"id":"a"},{"id":"a"}]}`), Code: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCLI()
			f.queue("models", tc.result, nil)
			c := newTestController(f)
			_, err := c.Catalog(context.Background())
			if err == nil || !strings.Contains(err.Error(), "invalid data") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCatalogCLIErrorEnvelope(t *testing.T) {
	f := newFakeCLI()
	f.queue("models", runResult{Stderr: []byte(`{"error":"No usable cached model list.","code":1}`), Code: 1}, nil)
	c := newTestController(f)
	_, err := c.Catalog(context.Background())
	if err == nil || err.Error() != "No usable cached model list." {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadValidatesAndUsesExactArgs(t *testing.T) {
	f := newFakeCLI()
	f.queue("models", okJSON(t, catalogJSON()), nil)
	f.queue("load", okJSON(t, object{"status": "ready", "workload": "spark/glm"}), nil)
	f.queue("status", okJSON(t, statusJSON()), nil)
	c := newTestController(f)

	if err := c.Load(context.Background(), "glm"); err != nil {
		t.Fatalf("load: %v (snapshot error %q)", err, c.Snapshot().Error)
	}
	loads := f.callsFor("load")
	if len(loads) != 1 {
		t.Fatalf("load calls = %d, want 1", len(loads))
	}
	if strings.Join(loads[0], "\x00") != "--json\x00load\x00spark/glm" {
		t.Fatalf("load args = %q, want exact --json load <canonical id>", loads[0])
	}
	s := c.Snapshot()
	if s.Loading {
		t.Fatalf("Loading must be false after completion")
	}
	if s.LoadMessage != "spark/glm is ready." {
		t.Fatalf("LoadMessage = %q", s.LoadMessage)
	}
	// Actual readiness was refreshed after the load.
	if len(f.callsFor("status")) != 1 {
		t.Fatalf("load must refresh actual status once")
	}
}

func TestLoadSpecialCharactersStayOneArgument(t *testing.T) {
	// A canonical ID with shell metacharacters must arrive as exactly one
	// argv element; the package never builds a shell string.
	id := "spark/weird name;$(rm -rf)&|`x`"
	f := newFakeCLI()
	f.queue("models", okJSON(t, object{"models": []any{
		object{"id": id, "name": "Weird"},
	}}), nil)
	f.queue("load", okJSON(t, object{"status": "ready", "workload": id}), nil)
	f.queue("status", okJSON(t, statusWith(object{
		shortName(id): object{"kind": "llm", "loaded": true, "state": "ready", "nodes": []any{"spark01"}},
	})), nil)
	c := newTestController(f)
	if err := c.Load(context.Background(), id); err != nil {
		t.Fatalf("load: %v", err)
	}
	loads := f.callsFor("load")
	if len(loads) != 1 || len(loads[0]) != 3 || loads[0][2] != id {
		t.Fatalf("load argv = %q, want [--json load %q]", loads[0], id)
	}
}

func TestLoadUnknownAndAmbiguous(t *testing.T) {
	f := newFakeCLI()
	cat := object{"models": []any{
		object{"id": "spark/a", "aliases": []any{"twin"}},
		object{"id": "spark/b", "aliases": []any{"twin"}},
	}}
	f.queue("models", okJSON(t, cat), nil)
	f.queue("models", okJSON(t, cat), nil)
	c := newTestController(f)

	if err := c.Load(context.Background(), "nope"); err == nil || !strings.Contains(err.Error(), "not in the current model list") {
		t.Fatalf("unknown error = %v", err)
	}
	if err := c.Load(context.Background(), "twin"); err == nil || !strings.Contains(err.Error(), "more than one model") {
		t.Fatalf("ambiguous error = %v", err)
	}
	if got := len(f.callsFor("load")); got != 0 {
		t.Fatalf("invalid loads must never invoke load, got %d calls", got)
	}
}

func TestLoadRejectsEmpty(t *testing.T) {
	f := newFakeCLI()
	c := newTestController(f)
	err := c.Load(context.Background(), "   ")
	if err == nil || !strings.Contains(err.Error(), "Choose a model") {
		t.Fatalf("error = %v", err)
	}
	if len(f.callsFor("models")) != 0 || len(f.callsFor("load")) != 0 {
		t.Fatalf("empty load must not run the CLI")
	}
}

func TestLoadDuplicateExclusionAndImmediateLoading(t *testing.T) {
	f := newFakeCLI()
	f.queue("models", okJSON(t, catalogJSON()), nil)
	f.queue("load", okJSON(t, object{"status": "ready", "workload": "spark/glm"}), nil)
	f.queue("status", okJSON(t, statusJSON()), nil)
	c := newTestController(f)

	started := make(chan struct{})
	release := make(chan struct{})
	f.onRun = func(ctx context.Context, binary string, args []string) {
		if len(args) > 1 && args[1] == "load" {
			close(started)
			<-release
		}
	}
	errCh := make(chan error, 1)
	go func() { errCh <- c.Load(context.Background(), "glm") }()
	<-started

	// Loading is visible immediately, while the load is still running.
	if s := c.Snapshot(); !s.Loading || !strings.HasPrefix(s.LoadMessage, "Loading GLM") {
		t.Fatalf("Loading not visible immediately: %+v", s)
	}
	// A second load is refused and never reaches the CLI.
	if err := c.Load(context.Background(), "deepseek"); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("duplicate error = %v", err)
	}
	close(release)
	if err := <-errCh; err != nil {
		t.Fatalf("first load: %v", err)
	}
	if got := len(f.callsFor("load")); got != 1 {
		t.Fatalf("load calls = %d, want exactly 1", got)
	}
	if s := c.Snapshot(); s.Loading {
		t.Fatalf("Loading stuck true after load")
	}
}

func TestLoadFailureRecordsErrorAndRefreshes(t *testing.T) {
	f := newFakeCLI()
	f.queue("models", okJSON(t, catalogJSON()), nil)
	f.queue("load", runResult{Stderr: []byte(`{"error":"glm must stay loaded.","code":1}`), Code: 1}, nil)
	f.queue("status", okJSON(t, statusJSON()), nil)
	c := newTestController(f)

	err := c.Load(context.Background(), "glm")
	if err == nil || err.Error() != "glm must stay loaded." {
		t.Fatalf("error = %v", err)
	}
	s := c.Snapshot()
	if s.Loading || s.LoadError != "glm must stay loaded." || s.LoadMessage != "glm must stay loaded." {
		t.Fatalf("failure state = %+v", s)
	}
	if len(f.callsFor("status")) != 1 {
		t.Fatalf("failed load must still refresh actual readiness once")
	}
	// Next successful load clears LoadError.
	f.queue("load", okJSON(t, object{"status": "ready", "workload": "spark/glm"}), nil)
	f.queue("status", okJSON(t, statusJSON()), nil)
	if err := c.Load(context.Background(), "glm"); err != nil {
		t.Fatal(err)
	}
	if s := c.Snapshot(); s.LoadError != "" || s.LoadMessage == "" {
		t.Fatalf("success should clear LoadError: %+v", s)
	}
}

func TestLoadMalformedAndCanceled(t *testing.T) {
	t.Run("malformed", func(t *testing.T) {
		f := newFakeCLI()
		f.queue("models", okJSON(t, catalogJSON()), nil)
		f.queue("load", runResult{Stdout: []byte("not json"), Code: 0}, nil)
		f.queue("status", okJSON(t, statusJSON()), nil)
		c := newTestController(f)
		err := c.Load(context.Background(), "glm")
		if err == nil || !strings.Contains(err.Error(), "invalid data") {
			t.Fatalf("error = %v", err)
		}
		if s := c.Snapshot(); s.Loading || s.LoadError == "" {
			t.Fatalf("malformed load state = %+v", s)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		f := newFakeCLI()
		f.queue("models", okJSON(t, catalogJSON()), nil)
		f.queue("load", runResult{Stdout: []byte(`{"status":"ready","workload":"spark/glm"}`), Code: 0}, nil)
		c := newTestController(f)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := c.Load(ctx, "glm")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if got := len(f.callsFor("load")); got != 0 {
			t.Fatalf("canceled load must not invoke the CLI")
		}
		if s := c.Snapshot(); s.Loading {
			t.Fatalf("Loading must be false")
		}
	})
}

func TestLoadCanceledDuringRunDoesNotRefresh(t *testing.T) {
	f := newFakeCLI()
	f.queue("models", okJSON(t, catalogJSON()), nil)
	// The runner returns context.Canceled once ctx is canceled.
	f.queue("load", runResult{Stdout: []byte(`{"status":"ready","workload":"spark/glm"}`)}, nil)
	c := newTestController(f)
	ctx, cancel := context.WithCancel(context.Background())
	f.onRun = func(ctx context.Context, binary string, args []string) {
		if len(args) > 1 && args[1] == "load" {
			cancel()
		}
	}
	err := c.Load(ctx, "glm")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if got := len(f.callsFor("status")); got != 0 {
		t.Fatalf("canceled load must not run a refresh, got %d", got)
	}
	if s := c.Snapshot(); s.Loading {
		t.Fatalf("Loading must be cleared")
	}
}

// TestParseStatusKindFiltering covers real live workload kinds: a concrete
// LLM plus ComfyUI and audio services must yield only the LLM, while an
// unknown kind is admitted only when it is actually loaded.
func TestParseStatusKindFiltering(t *testing.T) {
	data := object{
		"models": object{
			"deepseek-v4.1-flash-1017": object{"kind": "llm", "loaded": true, "state": "ready", "nodes": []any{"spark01", "spark02"}, "active_requests": json.Number("2")},
			"comfyui":                  object{"kind": "comfyui", "loaded": true, "state": "running", "nodes": []any{"spark01"}},
			"kokoro":                   object{"kind": "audio", "loaded": true, "state": "running", "nodes": []any{"spark02"}},
			"parakeet":                 object{"kind": "audio", "loaded": false, "state": "stopped", "nodes": []any{}},
			"legacy-mystery":           object{"loaded": true, "state": "ready", "nodes": []any{"spark01"}},
			"legacy-stopped":           object{"loaded": false, "state": "stopped", "nodes": []any{}},
		},
		"nodes": object{
			"spark01": object{"memory_bytes": object{"MemTotal": json.Number("100"), "MemAvailable": json.Number("40")}},
			"spark02": object{"memory_bytes": object{"MemTotal": json.Number("200"), "MemAvailable": json.Number("80")}},
		},
	}
	snap, err := parseStatus(data)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range snap.Models {
		got[m.ID] = true
	}
	want := map[string]bool{"deepseek-v4.1-flash-1017": true, "legacy-mystery": true}
	if len(got) != len(want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("missing %s in %v", id, got)
		}
	}
}

func TestParseStatusGlobalActivityUnknown(t *testing.T) {
	data := statusJSON()
	data["activity_known"] = false
	obj(obj(data["models"])["deepseek"])["active_requests"] = json.Number("3")
	obj(obj(data["models"])["glm"])["active_requests"] = json.Number("0")
	snap, err := parseStatus(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range snap.Models {
		if m.ActiveRequests != nil {
			t.Fatalf("%s: global activity_known=false must yield nil counts, got %d", m.ID, *m.ActiveRequests)
		}
	}
}

// TestLoadDuplicateFailsPromptly is the supervisor regression: a second
// Load must return immediately while the first is still blocked in the CLI,
// and must never launch a second remote load.
func TestLoadDuplicateFailsPromptly(t *testing.T) {
	f := newFakeCLI()
	f.queue("models", okJSON(t, catalogJSON()), nil)
	f.queue("load", okJSON(t, object{"status": "ready", "workload": "spark/glm"}), nil)
	f.queue("status", okJSON(t, statusJSON()), nil)
	c := newTestController(f)

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f.onRun = func(ctx context.Context, binary string, args []string) {
		if len(args) > 1 && args[1] == "load" {
			once.Do(func() { close(started) })
			<-release
		}
	}
	first := make(chan error, 1)
	go func() { first <- c.Load(context.Background(), "glm") }()
	<-started

	second := make(chan error, 1)
	go func() { second <- c.Load(context.Background(), "deepseek") }()
	select {
	case err := <-second:
		if !errors.Is(err, errLoadInProgress) {
			t.Fatalf("duplicate err = %v, want errLoadInProgress", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("duplicate Load blocked; acceptance is not non-blocking")
	}
	if s := c.Snapshot(); !s.Loading {
		t.Fatalf("Loading must stay true while the first load runs")
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first load: %v", err)
	}
	if got := len(f.callsFor("load")); got != 1 {
		t.Fatalf("load calls = %d, want exactly 1", got)
	}
	if s := c.Snapshot(); s.Loading {
		t.Fatalf("Loading stuck after the first load finished")
	}
}

func TestLoadFailedStatusIsError(t *testing.T) {
	cases := []string{"failed", "error", "stopped", "starting", "queued", "weird"}
	for _, status := range cases {
		t.Run(status, func(t *testing.T) {
			f := newFakeCLI()
			f.queue("models", okJSON(t, catalogJSON()), nil)
			f.queue("load", okJSON(t, object{"status": status, "workload": "spark/glm"}), nil)
			f.queue("status", okJSON(t, statusJSON()), nil)
			c := newTestController(f)
			err := c.Load(context.Background(), "glm")
			if err == nil {
				t.Fatalf("status %q must be a load failure, not success", status)
			}
			s := c.Snapshot()
			if s.Loading || s.LoadError == "" {
				t.Fatalf("status %q state = %+v", status, s)
			}
			if strings.Contains(s.LoadMessage, "is ready") {
				t.Fatalf("status %q must never claim ready: %q", status, s.LoadMessage)
			}
		})
	}
}

func TestLoadSuccessRequiresConfirmedRefresh(t *testing.T) {
	f := newFakeCLI()
	f.queue("models", okJSON(t, catalogJSON()), nil)
	f.queue("load", okJSON(t, object{"status": "ready", "workload": "spark/glm"}), nil)
	// The post-load status refresh fails.
	f.queue("status", runResult{Stderr: []byte(`{"error":"SSH unreachable","code":1}`), Code: 1}, nil)
	c := newTestController(f)
	err := c.Load(context.Background(), "glm")
	if err == nil || !strings.Contains(err.Error(), "could not be confirmed") {
		t.Fatalf("err = %v, want readiness-unknown outcome", err)
	}
	s := c.Snapshot()
	if s.Loading || s.LoadError == "" {
		t.Fatalf("state = %+v", s)
	}
	if strings.Contains(s.LoadMessage, "is ready.") {
		t.Fatalf("unverified load must not claim ready: %q", s.LoadMessage)
	}
	if len(f.callsFor("status")) != 1 {
		t.Fatalf("post-load refresh must run exactly once")
	}
}

func TestLoadPostLoadRefreshIsBounded(t *testing.T) {
	f := newFakeCLI()
	f.queue("models", okJSON(t, catalogJSON()), nil)
	f.queue("load", okJSON(t, object{"status": "ready", "workload": "spark/glm"}), nil)
	c := newTestController(f)
	c.postRefreshTimeout = 150 * time.Millisecond
	// A status refresh that blocks until its context deadline, like a hung
	// SSH session.
	f.onRun = func(ctx context.Context, binary string, args []string) {
		if len(args) > 1 && args[1] == "status" {
			<-ctx.Done()
		}
	}
	start := time.Now()
	// The caller context is Background, so only the post-load bound can end
	// this refresh.
	err := c.Load(context.Background(), "glm")
	if err == nil {
		t.Fatalf("a timed-out post-load refresh must not report success")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("post-load refresh not bounded; took %v", elapsed)
	}
	if s := c.Snapshot(); s.Loading {
		t.Fatalf("Loading stuck after a bounded post-load refresh")
	}
}

func TestLoadPreCanceledContextNeverLaunches(t *testing.T) {
	f := newFakeCLI()
	c := newTestController(f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Load(ctx, "glm"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(f.callsFor("models")) != 0 || len(f.callsFor("load")) != 0 {
		t.Fatalf("pre-canceled load must not run the CLI")
	}
	if s := c.Snapshot(); s.Loading {
		t.Fatalf("Loading must be false")
	}
}

// statusWith returns a status fixture where the given workload rows replace
// the defaults, so readiness regressions can describe exact controller views.
func statusWith(models object) object {
	s := statusJSON()
	s["models"] = models
	return s
}

// TestLoadReadinessRequiresSelectedTarget covers the supervisor regression:
// a successful status refresh is not enough. The selected model itself must
// be loaded and exactly ready; a different ready model never counts.
func TestLoadReadinessRequiresSelectedTarget(t *testing.T) {
	deepseekReady := object{"kind": "llm", "loaded": true, "state": "ready", "nodes": []any{"spark01", "spark02"}}

	cases := []struct {
		name    string
		models  object
		wantErr bool
	}{
		{
			name: "selected target ready",
			models: object{
				"deepseek": deepseekReady,
				"glm":      object{"kind": "llm", "loaded": true, "state": "ready", "nodes": []any{"spark01"}},
			},
		},
		{
			name:    "old different model remains ready, target absent",
			models:  object{"deepseek": deepseekReady},
			wantErr: true,
		},
		{
			name: "target loaded but failed",
			models: object{
				"deepseek": deepseekReady,
				"glm":      object{"kind": "llm", "loaded": true, "state": "failed", "nodes": []any{"spark01"}},
			},
			wantErr: true,
		},
		{
			name: "target loaded but state missing",
			models: object{
				"deepseek": deepseekReady,
				"glm":      object{"kind": "llm", "loaded": true, "nodes": []any{"spark01"}},
			},
			wantErr: true,
		},
		{
			name: "target loaded but still starting",
			models: object{
				"deepseek": deepseekReady,
				"glm":      object{"kind": "llm", "loaded": true, "state": "starting", "nodes": []any{"spark01"}},
			},
			wantErr: true,
		},
		{
			name: "target present but not loaded",
			models: object{
				"deepseek": deepseekReady,
				"glm":      object{"kind": "llm", "loaded": false, "state": "stopped", "nodes": []any{}},
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCLI()
			f.queue("models", okJSON(t, catalogJSON()), nil)
			f.queue("load", okJSON(t, object{"status": "ready", "workload": "glm"}), nil)
			f.queue("status", okJSON(t, statusWith(tc.models)), nil)
			c := newTestController(f)
			err := c.Load(context.Background(), "glm")
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "could not be confirmed") {
					t.Fatalf("err = %v, want readiness-not-confirmed", err)
				}
				s := c.Snapshot()
				if s.Loading || s.LoadError == "" {
					t.Fatalf("readiness failure state = %+v", s)
				}
				if strings.Contains(s.LoadMessage, "is ready.") {
					t.Fatalf("must not claim ready: %q", s.LoadMessage)
				}
				return
			}
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if s := c.Snapshot(); s.Loading || s.LoadError != "" || !strings.Contains(s.LoadMessage, "is ready.") {
				t.Fatalf("ready state = %+v", s)
			}
		})
	}
}

// TestLoadCatalogFailureNeverLaunchesLoad proves the fallback is gone: with
// no cached catalog and a failing catalog fetch, zero load subprocesses run.
func TestLoadCatalogFailureNeverLaunchesLoad(t *testing.T) {
	f := newFakeCLI()
	f.queue("models", runResult{Stderr: []byte(`{"error":"Gateway unreachable","code":1}`), Code: 1}, nil)
	c := newTestController(f)
	err := c.Load(context.Background(), "glm")
	if err == nil || err.Error() != "Gateway unreachable" {
		t.Fatalf("err = %v, want catalog failure", err)
	}
	if got := len(f.callsFor("load")); got != 0 {
		t.Fatalf("load calls = %d, want 0 when the catalog failed", got)
	}
	if s := c.Snapshot(); s.Loading || s.LoadError == "" {
		t.Fatalf("state = %+v", s)
	}
}

// TestLoadCatalogFailureWithCachedCatalogStillValidates uses the cached
// catalog when a prior Catalog call succeeded; an unknown ID is refused
// without invoking load.
func TestLoadCatalogFailureWithCachedCatalogStillValidates(t *testing.T) {
	f := newFakeCLI()
	f.queue("models", okJSON(t, catalogJSON()), nil)
	c := newTestController(f)
	if _, err := c.Catalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := c.Load(context.Background(), "not-in-catalog")
	if err == nil || !strings.Contains(err.Error(), "not in the current model list") {
		t.Fatalf("err = %v", err)
	}
	if got := len(f.callsFor("load")); got != 0 {
		t.Fatalf("load calls = %d, want 0", got)
	}
}

// TestParseStatusPerModelActivityUnknown covers a per-model explicit
// activity_known=false alongside a globally known controller.
func TestParseStatusPerModelActivityUnknown(t *testing.T) {
	data := statusJSON()
	data["activity_known"] = true
	obj(obj(data["models"])["glm"])["activity_known"] = false
	obj(obj(data["models"])["glm"])["active_requests"] = json.Number("4")
	obj(obj(data["models"])["deepseek"])["active_requests"] = json.Number("2")
	snap, err := parseStatus(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range snap.Models {
		switch m.ID {
		case "glm":
			if m.ActiveRequests != nil {
				t.Fatalf("glm: per-model activity_known=false must be unknown, got %d", *m.ActiveRequests)
			}
		case "deepseek":
			if m.ActiveRequests == nil || *m.ActiveRequests != 2 {
				t.Fatalf("deepseek: known count lost, got %v", m.ActiveRequests)
			}
		}
	}
}

// TestLoadReadinessMatchesPrefixedStatusKey allows a status row spelled with
// the same provider prefix as the catalog (garuda/glm) while still requiring
// that exact row to be ready.
func TestLoadReadinessMatchesPrefixedStatusKey(t *testing.T) {
	f := newFakeCLI()
	f.queue("models", okJSON(t, catalogJSON()), nil)
	f.queue("load", okJSON(t, object{"status": "ready", "workload": "glm"}), nil)
	f.queue("status", okJSON(t, statusWith(object{
		"garuda/glm": object{"kind": "llm", "loaded": true, "state": "ready", "nodes": []any{"spark01"}},
		"deepseek":   object{"kind": "llm", "loaded": true, "state": "ready", "nodes": []any{"spark01", "spark02"}},
	})), nil)
	c := newTestController(f)
	if err := c.Load(context.Background(), "glm"); err != nil {
		t.Fatalf("load: %v", err)
	}
	if s := c.Snapshot(); !strings.Contains(s.LoadMessage, "is ready.") {
		t.Fatalf("LoadMessage = %q", s.LoadMessage)
	}
}
