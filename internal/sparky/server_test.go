package sparky

// HTTP tests for the controller ingress. A narrow fake implements the
// Controller interface so the real handler behavior (methods, framing,
// privacy, streaming, cancellation, release-once, failed binds) is exercised
// without any network access beyond httptest, and without faking the
// handler itself. The fake never returns placeholder successes for the
// failure paths under test.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeController struct {
	mu            sync.Mutex
	config        Object
	releaseCalls  []string // RAW release invocations; no dedup hiding
	cancelled     map[string]context.CancelFunc
	inferOrigin   string
	refreshErr    error
	wireErr       error
	snapshotErr   error
	llmErr        error
	plans         int
	fencedVal     bool
	admitCtx      context.Context
	admitDone     chan struct{}
	generated     Object
	generatePanic bool
}

func newFakeController() *fakeController {
	return &fakeController{
		config: Object{
			"controller_port": 0, // never bind from handler tests
			"nodes": Object{
				"spark01": Object{"direct_ip": "10.0.0.1"},
				"spark02": Object{"direct_ip": "10.0.0.2"},
			},
		},
		generated: Object{},
		cancelled: map[string]context.CancelFunc{},
	}
}

func (f *fakeController) ConfigPath() string { return "/cluster/config/models.json" }
func (f *fakeController) Config() Object     { return f.config }
func (f *fakeController) Refresh() (Object, error) {
	return f.config, f.refreshErr
}
func (f *fakeController) Snapshot(fresh bool) (Object, error) {
	if f.snapshotErr != nil {
		return nil, f.snapshotErr
	}
	return Object{"models": Object{}}, nil
}
func (f *fakeController) Admission() Object  { return Object{} }
func (f *fakeController) Fenced() bool       { return f.fencedVal }
func (f *fakeController) Fence(enabled bool) { f.fencedVal = enabled }
func (f *fakeController) Plan(model, action, on string) (Object, error) {
	f.plans++
	return Object{"workload": model}, nil
}
func (f *fakeController) Load(model string, force bool, on string) (Object, error) {
	result := Object{"workload": model}
	if on != "" {
		result["on"] = on
	}
	return result, nil
}
func (f *fakeController) Unload(model string, force bool) (Object, error) {
	return Object{"workload": model}, nil
}
func (f *fakeController) SetPlacement(model, on string) (Object, error) {
	return Object{"model": model}, nil
}
func (f *fakeController) SubmitComfy(workflow Object, on string) (Object, error) {
	return Object{"prompt_id": "abc"}, nil
}
func (f *fakeController) LLMRequest(ctx context.Context, model string) (string, string, error) {
	// Record the admission context BEFORE any wait so a queued cancellation
	// is observable; reads go through QueuedContext under the mutex.
	f.mu.Lock()
	f.admitCtx = ctx
	f.mu.Unlock()
	if f.admitDone != nil { // simulate a queued admission wait
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-f.admitDone:
		}
	}
	if f.llmErr != nil {
		return "", "", f.llmErr
	}
	return "glm-4", "tok-1", nil
}
func (f *fakeController) QueuedContext() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.admitCtx
}
func (f *fakeController) InferenceOrigin(token string) (string, error) {
	if f.inferOrigin == "" {
		return "", errors.New("no origin")
	}
	return f.inferOrigin, nil
}
func (f *fakeController) Release(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls = append(f.releaseCalls, token)
}
func (f *fakeController) RegisterInferenceCancel(token string, cancel context.CancelFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled[token] = cancel
}
func (f *fakeController) ComfyAcquire(node, promptID string, cpuOnly bool) (Object, error) {
	return Object{"node": node}, nil
}
func (f *fakeController) CancelJob(node, promptID string) {}
func (f *fakeController) CheckJobLease(token, node string) error {
	if node == "spark02" {
		return fail("Lease belongs to another workload.")
	}
	return nil
}
func (f *fakeController) ReconcileJobs()     {}
func (f *fakeController) ImageModels() []any { return []any{Object{"id": "flux"}} }

// numberCopy deep-copies an Object without any JSON roundtrip, so exact
// numeric spelling (json.Number, uint64 values) is preserved in the fake.
func numberCopy(v any) any {
	switch value := v.(type) {
	case Object:
		out := Object{}
		for k, item := range value {
			out[k] = numberCopy(item)
		}
		return out
	case []any:
		out := make([]any, 0, len(value))
		for _, item := range value {
			out = append(out, numberCopy(item))
		}
		return out
	}
	return v
}

func (f *fakeController) Generate(body Object) (Object, error) {
	if f.generatePanic {
		panic("the image runtime exploded before any output")
	}
	f.mu.Lock()
	f.generated = Object{}
	for k, v := range body {
		f.generated[k] = numberCopy(v)
	}
	f.mu.Unlock()
	return Object{"images": []any{}}, nil
}
func (f *fakeController) LLMWire() (Object, error) {
	if f.wireErr != nil {
		return nil, f.wireErr
	}
	return Object{"data": []any{Object{"id": "garuda/glm-4", "object": "model"}}}, nil
}
func (f *fakeController) Cancelled(token string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelled[token] != nil
}
func (f *fakeController) Released() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.releaseCalls...)
}

func serverRequest(t *testing.T, s *controllerServer, method, target, remote, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := plainRequest(method, target, body, remote)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	return rec
}

func plainRequest(method, target, body string, remote string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.RemoteAddr = remote
	return r
}

func decodeResponse(t *testing.T, body string) Object {
	t.Helper()
	raw, err := decode([]byte(body))
	if err != nil {
		t.Fatalf("response is not JSON: %v %s", err, body)
	}
	return raw
}

func TestHealthAndMethods(t *testing.T) {
	s := &controllerServer{ctrl: newFakeController()}
	rec := serverRequest(t, s, "GET", "/health", "127.0.0.1:5", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("health failed: %d %s", rec.Code, rec.Body.String())
	}
	rec = serverRequest(t, s, "POST", "/control/status", "127.0.0.1:5", "{}")
	if rec.Code != 405 {
		t.Fatalf("control read must reject POST: %d", rec.Code)
	}
	rec = serverRequest(t, s, "GET", "/control/load", "127.0.0.1:5", "")
	if rec.Code != 405 {
		t.Fatalf("control write must reject GET: %d", rec.Code)
	}
	rec = serverRequest(t, s, "POST", "/control/placement", "10.0.0.9:5", "{}")
	if rec.Code != 403 {
		t.Fatalf("remote control write must be refused before body reads: %d", rec.Code)
	}
	rec = serverRequest(t, s, "GET", "/v1/responses", "127.0.0.1:5", "")
	if rec.Code != 404 {
		t.Fatalf("responses must stay unsupported: %d", rec.Code)
	}
	rec = serverRequest(t, s, "POST", "/v1/embeddings", "127.0.0.1:5", "{}")
	if rec.Code != 404 {
		t.Fatalf("embeddings must stay unsupported: %d", rec.Code)
	}
	rec = serverRequest(t, s, "DELETE", "/control/leases", "127.0.0.1:5", "")
	if rec.Code != 405 {
		t.Fatalf("unexpected method must fail: %d", rec.Code)
	}
	// /control/fence is the one control path with both verbs: DELETE (or
	// anything else) is still refused.
	rec = serverRequest(t, s, "DELETE", "/control/fence", "127.0.0.1:5", "")
	if rec.Code != 405 {
		t.Fatalf("fence must reject unexpected methods: %d", rec.Code)
	}
}

func TestControlLoopbackOnly(t *testing.T) {
	s := &controllerServer{ctrl: newFakeController()}
	rec := serverRequest(t, s, "GET", "/control/status", "10.0.0.9:5", "")
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "administrator SSH") {
		t.Fatalf("control must be loopback only: %d %s", rec.Code, rec.Body.String())
	}
	rec = serverRequest(t, s, "POST", "/control/load", "10.0.0.9:5", `{"model":"glm"}`)
	if rec.Code != 403 {
		t.Fatalf("control write from remote peer must fail: %d", rec.Code)
	}
	// X-Forwarded-For must never grant administrator identity.
	r := plainRequest("GET", "/control/status", "", "10.0.0.9:5")
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Fatalf("forwarded header must not grant access: %d", rec.Code)
	}
}

func TestControlFenceAndPlan(t *testing.T) {
	fake := newFakeController()
	s := &controllerServer{ctrl: fake}
	// The read-only GET is the real contract installFenceState depends on:
	// the exact manual flag, no body, loopback only.
	rec := serverRequest(t, s, "GET", "/control/fence", "127.0.0.1:5", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"fenced":false`) {
		t.Fatalf("fence read failed: %d %s", rec.Code, rec.Body.String())
	}
	if len(fake.Released()) != 0 {
		t.Fatal("a fence read must never touch admissions")
	}
	rec = serverRequest(t, s, "POST", "/control/fence", "127.0.0.1:5", `{"enabled":true}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"fenced":true`) {
		t.Fatalf("fence failed: %d %s", rec.Code, rec.Body.String())
	}
	if !fake.Fenced() {
		t.Fatal("fence did not reach the controller")
	}
	// The GET must observe the manual flag after the POST.
	rec = serverRequest(t, s, "GET", "/control/fence", "127.0.0.1:5", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"fenced":true`) {
		t.Fatalf("fence read after write failed: %d %s", rec.Code, rec.Body.String())
	}
	rec = serverRequest(t, s, "POST", "/control/fence", "127.0.0.1:5", `{"enabled":"yes"}`)
	if rec.Code != 400 {
		t.Fatalf("non-bool fence must fail: %d", rec.Code)
	}
	// The read-only fence state stays under the operator loopback ACL.
	rec = serverRequest(t, s, "GET", "/control/fence", "10.0.0.9:5", "")
	if rec.Code != 403 {
		t.Fatalf("remote fence read must be refused: %d", rec.Code)
	}
	rec = serverRequest(t, s, "POST", "/control/plan", "127.0.0.1:5", `{"model":"glm","action":"load"}`)
	if rec.Code != 200 || fake.plans != 1 {
		t.Fatalf("plan failed: %d", rec.Code)
	}
}

func TestControlStateErrorsAreEnvelopes(t *testing.T) {
	fake := newFakeController()
	fake.snapshotErr = errors.New("device query failed")
	s := &controllerServer{ctrl: fake}
	rec := serverRequest(t, s, "GET", "/control/status", "127.0.0.1:5", "")
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "device query failed") {
		t.Fatalf("snapshot failure must be an envelope: %d %s", rec.Code, rec.Body.String())
	}
	fake.refreshErr = errors.New("cluster file unreadable")
	rec = serverRequest(t, s, "GET", "/control/config", "127.0.0.1:5", "")
	if rec.Code != 503 {
		t.Fatalf("refresh failure must be an envelope: %d", rec.Code)
	}
	rec = serverRequest(t, s, "GET", "/sparky/catalog", "127.0.0.1:5", "")
	if rec.Code != 503 {
		t.Fatalf("catalog refresh failure must be an envelope: %d", rec.Code)
	}
	rec = serverRequest(t, s, "GET", "/v1/image-models", "127.0.0.1:5", "")
	if rec.Code != 503 {
		t.Fatalf("image model refresh failure must be an envelope: %d", rec.Code)
	}
}

func TestModelsListingErrorsAreNot200(t *testing.T) {
	fake := newFakeController()
	fake.wireErr = errors.New("manager listing unavailable")
	s := &controllerServer{ctrl: fake}
	rec := serverRequest(t, s, "GET", "/v1/models", "127.0.0.1:5", "")
	if rec.Code != 503 {
		t.Fatalf("failed listing must not be HTTP 200: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatal("an empty success list must not be returned on failure")
	}
}

func TestJobSourceMustMatchDevice(t *testing.T) {
	s := &controllerServer{ctrl: newFakeController()}
	rec := serverRequest(t, s, "POST", "/jobs/acquire", "10.0.0.9:5", `{"node":"spark02","prompt_id":"p1"}`)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "Job source does not match") {
		t.Fatalf("mismatched job source must fail: %d %s", rec.Code, rec.Body.String())
	}
	rec = serverRequest(t, s, "POST", "/jobs/acquire", "127.0.0.1:5", `{"node":"spark02","prompt_id":"p1"}`)
	if rec.Code != 403 {
		t.Fatalf("loopback is not a valid spark02 source: %d", rec.Code)
	}
	rec = serverRequest(t, s, "POST", "/jobs/acquire", "10.0.0.2:5", `{"node":"spark02","prompt_id":"p1"}`)
	if rec.Code != 200 {
		t.Fatalf("matching direct link must pass: %d", rec.Code)
	}
	rec = serverRequest(t, s, "POST", "/jobs/acquire", "127.0.0.1:5", `{"node":"spark01","prompt_id":"p1"}`)
	if rec.Code != 200 {
		t.Fatalf("loopback acquire for the head node must pass: %d", rec.Code)
	}
	rec = serverRequest(t, s, "POST", "/jobs/acquire", "10.0.0.2:5", `{"node":"spark09","prompt_id":"p1"}`)
	if rec.Code != 400 {
		t.Fatalf("unknown device must fail: %d", rec.Code)
	}
	rec = serverRequest(t, s, "POST", "/jobs/release", "10.0.0.2:5", `{"node":"spark02","token":"t"}`)
	if rec.Code != 409 {
		t.Fatalf("lease of another workload must fail: %d", rec.Code)
	}
	rec = serverRequest(t, s, "POST", "/jobs/release", "127.0.0.1:5", `{"node":"spark01","token":"t"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "released") {
		t.Fatalf("valid release must pass: %d %s", rec.Code, rec.Body.String())
	}
}

func TestInvalidAndOversizeJSON(t *testing.T) {
	s := &controllerServer{ctrl: newFakeController()}
	rec := serverRequest(t, s, "POST", "/control/load", "127.0.0.1:5", `{invalid`)
	if rec.Code != 400 {
		t.Fatalf("invalid JSON must fail: %d", rec.Code)
	}
	rec = serverRequest(t, s, "POST", "/control/plan", "127.0.0.1:5", `[1,2]`)
	if rec.Code != 400 {
		t.Fatalf("non-object JSON must fail: %d", rec.Code)
	}
	oversize := fmt.Sprintf(`{"pad":"%s"}`, strings.Repeat("x", 129<<20))
	r := plainRequest("POST", "/control/plan", oversize, "127.0.0.1:5")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 400 {
		t.Fatalf("oversize body must fail: %d", rec.Code)
	}
}

func TestModelsPrivacyAndOverlay(t *testing.T) {
	s := &controllerServer{ctrl: newFakeController()}
	prevHook := catalogHook
	catalogHook = func(Object) Object {
		return Object{"data": []any{Object{"id": "garuda/glm-4", "capability": "text", "status": "ready"}}}
	}
	defer func() { catalogHook = prevHook }()

	rec := serverRequest(t, s, "GET", "/sparky/catalog", "127.0.0.1:5", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"data"`) {
		t.Fatalf("catalog failed: %d %s", rec.Code, rec.Body.String())
	}
	rec = serverRequest(t, s, "GET", "/v1/models", "127.0.0.1:5", "")
	if rec.Code != 200 {
		t.Fatalf("models failed: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"id":"garuda/glm-4"`) {
		t.Fatalf("wire listing must be preserved: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"id":"flux"`) {
		t.Fatalf("image models must be appended: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "backend_url") || strings.Contains(rec.Body.String(), "api_key") {
		t.Fatalf("no backend URLs or tokens may appear: %s", rec.Body.String())
	}
	// Capability and status metadata from the central catalog must overlay the
	// wire row without touching the id.
	wire := decodeResponse(t, rec.Body.String())
	row := obj(arr(wire["data"])[0])
	if row["capability"] != "text" || row["status"] != "ready" || str(row["id"]) != "garuda/glm-4" {
		t.Fatalf("catalog overlay missing: %v", row)
	}
}

func TestInferenceStreamingAndReleaseOnce(t *testing.T) {
	fake := newFakeController()
	s := &controllerServer{ctrl: fake}

	// Upstream streams one SSE chunk, then holds until cancellation.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`data: {"chunk":1}` + "\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer upstream.Close()
	fake.inferOrigin = upstream.URL
	api := httptest.NewServer(s)
	defer func() {
		api.CloseClientConnections()
		api.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", api.URL+"/v1/chat/completions", strings.NewReader(`{"model":"glm"}`))
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("inference request failed: %v", err)
	}
	// The first chunk must arrive before the upstream finishes: real streaming.
	buf := make([]byte, 4096)
	n, err := resp.Body.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), `"chunk":1`) {
		t.Fatalf("first streamed chunk missing: %q %v", string(buf[:n]), err)
	}
	cancel() // client disconnects mid-stream
	drain := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(resp.Body)
		drain <- err
		resp.Body.Close()
	}()
	select {
	case <-drain:
	case <-time.After(5 * time.Second):
		t.Fatal("the response must end promptly after the client disconnects")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(fake.Released()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	released := fake.Released()
	if len(released) != 1 || released[0] != "tok-1" {
		t.Fatalf("admission must be released exactly once: %v", released)
	}
}

func TestInferenceRequiresLoopbackAndModel(t *testing.T) {
	fake := newFakeController()
	s := &controllerServer{ctrl: fake}
	rec := serverRequest(t, s, "POST", "/v1/chat/completions", "10.0.0.9:5", `{"model":"glm"}`)
	if rec.Code != 403 {
		t.Fatalf("remote inference must be refused: %d", rec.Code)
	}
	rec = serverRequest(t, s, "POST", "/v1/completions", "127.0.0.1:5", `{}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "model is required") {
		t.Fatalf("missing model must fail: %d %s", rec.Code, rec.Body.String())
	}
	rec = serverRequest(t, s, "POST", "/v1/chat/completions", "127.0.0.1:5", `{"model":"glm"}`)
	if rec.Code != 409 {
		t.Fatalf("unavailable origin must produce an error envelope: %d %s", rec.Code, rec.Body.String())
	}
	released := fake.Released()
	if len(released) != 1 {
		t.Fatalf("failed admission must still release once: %v", released)
	}
}

func TestInferenceAdmissionErrorEnvelope(t *testing.T) {
	fake := newFakeController()
	fake.llmErr = errors.New("all devices are busy")
	s := &controllerServer{ctrl: fake}
	rec := serverRequest(t, s, "POST", "/v1/completions", "127.0.0.1:5", `{"model":"glm"}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "all devices are busy") {
		t.Fatalf("admission failure must be an envelope: %d %s", rec.Code, rec.Body.String())
	}
	if len(fake.Released()) != 0 {
		t.Fatal("no token is released when admission itself failed")
	}
}

func TestInferenceQueuedCancellationOnDisconnect(t *testing.T) {
	fake := newFakeController()
	fake.admitDone = make(chan struct{}) // admission waits until the client is gone
	s := &controllerServer{ctrl: fake}
	api := httptest.NewServer(s)
	defer api.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", api.URL+"/v1/chat/completions", strings.NewReader(`{"model":"glm"}`))
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel() // disconnect while queued
	}()
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("queued request must fail once the client disconnects")
	}
	// The disconnect cancellation reaches the queued admission
	// asynchronously; wait on the recorded context with a bounded timeout.
	deadline := time.Now().Add(3 * time.Second)
	for {
		queued := fake.QueuedContext()
		if queued != nil && queued.Err() != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the disconnect must cancel the queued admission context")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(fake.Released()) != 0 {
		t.Fatal("no token exists yet, so nothing may be released")
	}
}

func TestInferenceForcedCancelBeforeHeaders(t *testing.T) {
	fake := newFakeController()
	s := &controllerServer{ctrl: fake}
	gate := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(gate)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer upstream.Close()
	fake.inferOrigin = upstream.URL
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- serverRequest(t, s, "POST", "/v1/completions", "127.0.0.1:5", `{"model":"glm"}`)
	}()
	<-gate
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !fake.Cancelled("tok-1") {
		time.Sleep(5 * time.Millisecond)
	}
	if !fake.Cancelled("tok-1") {
		t.Fatal("forced cancellation must be registered before the response headers")
	}
	fake.mu.Lock()
	cancel := fake.cancelled["tok-1"]
	fake.mu.Unlock()
	cancel() // coordinator-forced cancellation (e.g. model stop) during prefill
	rec := <-done
	if rec == nil {
		t.Fatal("no response recorded")
	}
	if len(fake.Released()) != 1 {
		t.Fatalf("release must happen exactly once after cancellation: %v", fake.Released())
	}
}

func TestHopHeadersStrippedUpstream(t *testing.T) {
	fake := newFakeController()
	s := &controllerServer{ctrl: fake}
	var seen http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	fake.inferOrigin = upstream.URL
	r := plainRequest("POST", "/v1/chat/completions", `{"model":"glm"}`, "127.0.0.1:5")
	r.Header.Set("Connection", "keep-alive, X-Private")
	r.Header.Set("X-Private", "leak")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("inference failed: %d", rec.Code)
	}
	if seen.Get("Connection") != "" || seen.Get("X-Private") != "" {
		t.Fatalf("hop headers and connection-token headers must be stripped: %v", seen)
	}
}

func TestPanicBeforeCommitIsEnvelopeAfterCommitAborts(t *testing.T) {
	fake := newFakeController()
	s := &controllerServer{ctrl: fake}

	// Before commit: a panicking controller yields the safe generic envelope.
	var generateCalls int
	original := fake.generated
	_ = original
	fake.generatePanic = true
	rec := serverRequest(t, s, "POST", "/v1/images/generations", "127.0.0.1:5", `{"prompt":"x"}`)
	if rec.Code != 503 || strings.Contains(rec.Body.String(), "x") {
		t.Fatalf("uncommitted panic must be a generic 503 envelope: %d %s", rec.Code, rec.Body.String())
	}
	fake.generatePanic = false
	_ = generateCalls

	// After commit: an upstream failure mid-stream must not append an
	// envelope to the already started SSE body.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`data: {"chunk":1}` + "\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic("upstream exploded mid-stream")
	}))
	defer upstream.Close()
	fake.inferOrigin = upstream.URL
	api := httptest.NewServer(s)
	defer func() {
		api.CloseClientConnections()
		api.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", api.URL+"/v1/chat/completions", strings.NewReader(`{"model":"glm"}`))
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("streamed request failed: %v", err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		// A mid-stream upstream failure aborts the connection; that is fine
		// as long as no JSON envelope was appended before the abort.
		if !strings.Contains(strings.ToLower(readErr.Error()), "eof") {
			t.Fatalf("the aborted stream failed unexpectedly: %v", readErr)
		}
	} else if strings.Contains(string(body), `"error"`) || strings.Contains(string(body), "service journal") {
		t.Fatalf("no envelope may be appended after the stream committed: %q", string(body))
	}
}

func TestServeControllerDefaultAndInvalidPorts(t *testing.T) {
	prevFactory := CoordinatorFactory
	defer func() { CoordinatorFactory = prevFactory }()

	// An unset controller_port must use the 8020 default and get past port
	// validation; the spark01 loopback self-collision then fails the bind.
	fake := newFakeController()
	CoordinatorFactory = func(string) (Controller, error) { return fake, nil }
	delete(fake.config, "controller_port")
	fake.config["nodes"] = Object{"spark01": Object{"direct_ip": "127.0.0.1"}}
	done := make(chan error, 1)
	go func() { done <- ServeController("/cluster/config/models.json") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the loopback self-collision on the default port must fail startup")
		}
		if strings.Contains(err.Error(), "controller port") {
			t.Fatalf("an unset port must default to 8020, not be rejected: %v", err)
		}
		if !strings.Contains(err.Error(), "bind") {
			t.Fatalf("the error must be the bind collision: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeController must return promptly on a bind failure")
	}

	// An explicitly invalid port must still fail clearly.
	fake = newFakeController()
	fake.config["controller_port"] = 99999
	fake.config["nodes"] = Object{"spark01": Object{"direct_ip": "127.0.0.1"}}
	done = make(chan error, 1)
	CoordinatorFactory = func(string) (Controller, error) { return fake, nil }
	go func() { done <- ServeController("/cluster/config/models.json") }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "controller port") {
			t.Fatalf("an invalid explicit port must fail clearly: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeController must return promptly on an invalid port")
	}
}

func TestServeControllerBindFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port

	// The Spark 1 direct link is not bindable in tests, so the loopback bind
	// collision alone must fail the whole startup before any goroutine runs.
	fake := newFakeController()
	fake.config["controller_port"] = float64(port)
	fake.config["nodes"] = Object{"spark01": Object{"direct_ip": "127.0.0.1"}}
	prevFactory := CoordinatorFactory
	CoordinatorFactory = func(string) (Controller, error) { return fake, nil }
	defer func() { CoordinatorFactory = prevFactory }()

	done := make(chan error, 1)
	go func() { done <- ServeController("/cluster/config/models.json") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a port collision must fail ServeController, not look healthy")
		}
		if !strings.Contains(err.Error(), "bind") {
			t.Fatalf("the error must mention the bind: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeController must return promptly on a bind failure")
	}
}

func TestServeControllerNoFactory(t *testing.T) {
	prevFactory := CoordinatorFactory
	CoordinatorFactory = nil
	defer func() { CoordinatorFactory = prevFactory }()
	if err := ServeController("/x"); err == nil {
		t.Fatal("missing controller implementation must be reported")
	}
}

func TestNumericSpellingPreserved(t *testing.T) {
	fake := newFakeController()
	s := &controllerServer{ctrl: fake}
	rec := serverRequest(t, s, "POST", "/v1/images/generations", "127.0.0.1:5",
		`{"seed":18446744073709551615,"prompt":"x"}`)
	if rec.Code != 200 {
		t.Fatalf("image generation failed: %d %s", rec.Code, rec.Body.String())
	}
	fake.mu.Lock()
	seed := fake.generated["seed"]
	fake.mu.Unlock()
	number, isNumber := seed.(json.Number)
	if !isNumber || number.String() != "18446744073709551615" {
		t.Fatalf("the uint64 seed must survive exactly: %v (%T)", seed, seed)
	}
}

func TestBodyFraming(t *testing.T) {
	fake := newFakeController()
	s := &controllerServer{ctrl: fake}
	rec := serverRequest(t, s, "POST", "/control/plan", "127.0.0.1:5", `{"model":"glm"} {"model":"h"}`)
	if rec.Code != 400 {
		t.Fatalf("concatenated JSON must be rejected: %d", rec.Code)
	}
	rec = serverRequest(t, s, "POST", "/control/plan", "127.0.0.1:5", `{"model":"glm"}`)
	if rec.Code != 200 {
		t.Fatalf("truncated body must fail: %d %s", rec.Code, rec.Body.String())
	}
	rec = serverRequest(t, s, "POST", "/control/plan", "127.0.0.1:5", "{\"model\":\"glm\"}   \n\t")
	if rec.Code != 200 {
		t.Fatalf("trailing whitespace must be accepted: %d %s", rec.Code, rec.Body.String())
	}
	rec = serverRequest(t, s, "POST", "/control/plan", "127.0.0.1:5", `{"model":"glm"`)
	if rec.Code != 400 {
		t.Fatalf("truncated JSON must be rejected: %d", rec.Code)
	}
}

func TestInferenceAndJobsRequirePOST(t *testing.T) {
	s := &controllerServer{ctrl: newFakeController()}
	for _, path := range []string{"/v1/chat/completions", "/v1/completions", "/jobs/acquire", "/jobs/release", "/jobs/cancel"} {
		rec := serverRequest(t, s, "GET", path, "127.0.0.1:5", "")
		if rec.Code != 405 {
			t.Fatalf("GET %s must be 405: %d", path, rec.Code)
		}
		rec = serverRequest(t, s, "DELETE", path, "127.0.0.1:5", "")
		if rec.Code != 405 {
			t.Fatalf("DELETE %s must be 405: %d", path, rec.Code)
		}
	}
}

func TestJobsAndControlInputValidation(t *testing.T) {
	s := &controllerServer{ctrl: newFakeController()}
	if rec := serverRequest(t, s, "POST", "/jobs/acquire", "10.0.0.2:5", `{"node":"spark02"}`); rec.Code != 400 {
		t.Fatalf("missing prompt_id must fail: %d", rec.Code)
	}
	if rec := serverRequest(t, s, "POST", "/jobs/acquire", "10.0.0.2:5", `{"node":"spark02","prompt_id":"p","cpu_only":"yes"}`); rec.Code != 400 {
		t.Fatalf("non-bool cpu_only must fail: %d", rec.Code)
	}
	if rec := serverRequest(t, s, "POST", "/jobs/release", "10.0.0.2:5", `{"node":"spark02"}`); rec.Code != 400 {
		t.Fatalf("missing token must fail: %d", rec.Code)
	}
	if rec := serverRequest(t, s, "POST", "/control/plan", "127.0.0.1:5", `{}`); rec.Code != 400 {
		t.Fatalf("plan without model must fail: %d", rec.Code)
	}
	if rec := serverRequest(t, s, "POST", "/control/placement", "127.0.0.1:5", `{"model":"glm"}`); rec.Code != 400 {
		t.Fatalf("placement without on must fail: %d", rec.Code)
	}
	if rec := serverRequest(t, s, "POST", "/control/run", "127.0.0.1:5", `{"workflow":5}`); rec.Code != 400 {
		t.Fatalf("run with a non-object workflow must fail: %d", rec.Code)
	}
}

func TestProxyTargetsConfiguredOriginExactly(t *testing.T) {
	fake := newFakeController()
	s := &controllerServer{ctrl: fake}
	var seenURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenURL = r.URL.String()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	fake.inferOrigin = upstream.URL
	r := plainRequest("POST", "/v1/chat/completions?source=client", `{"model":"glm"}`, "127.0.0.1:5")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("inference with a valid origin must pass: %d %s", rec.Code, rec.Body.String())
	}
	if seenURL != "/v1/chat/completions" {
		t.Fatalf("the client query must be dropped and the path kept: %q", seenURL)
	}
	// A configured origin with userinfo, a path or a query is refused.
	for _, bad := range []string{"http://user:pass@127.0.0.1:1", upstream.URL + "/path", upstream.URL + "?q=1", "https://" + strings.TrimPrefix(upstream.URL, "http://")} {
		fake.inferOrigin = bad
		rec = serverRequest(t, s, "POST", "/v1/chat/completions", "127.0.0.1:5", `{"model":"glm"}`)
		if rec.Code != 409 {
			t.Fatalf("origin %q must be refused: %d", bad, rec.Code)
		}
	}
}
