package sparky

// Runtime tests: the installed manager inflight wire contract and the
// semantic legacy-sync comparison. All effects are local httptest servers
// and temp files; no real manager, SSH, Docker or systemctl is touched.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// inflightServer serves a bounded initial SSE body on /api/events.
func inflightServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/events" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func newTestRuntime(t *testing.T, managerURL string) *Runtime {
	t.Helper()
	root := t.TempDir()
	cfg := legacyClusterConfig(root, managerURL)
	rt := newRuntime(cfg, filepath.Join(root, "models.json"))
	return rt
}

func TestInflightSnapshotOmittedRequestsIsValid(t *testing.T) {
	// The installed schema uses requests,omitempty: an otherwise valid
	// inflight snapshot with NO requests key is a valid empty snapshot.
	s := inflightServer(t, "data: {\"type\":\"inflight\",\"data\":{\"operation\":\"snapshot\"}}\n\n")
	rt := newTestRuntime(t, s.URL)
	rows, err := rt.legacyRequests(context.Background())
	if err != nil {
		t.Fatalf("an omitted requests field on a valid snapshot must be empty, not unknown: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected zero requests, got %v", rows)
	}
}

func TestInflightDataStringPayloadAccepted(t *testing.T) {
	// Live /api/events sends data as a STRING containing the JSON object.
	s := inflightServer(t, "data: {\"type\":\"inflight\",\"data\":\"{\\\"operation\\\":\\\"snapshot\\\",\\\"requests\\\":[{\\\"id\\\":7,\\\"model\\\":\\\"glm\\\"}]}\"}\n\n")
	rt := newTestRuntime(t, s.URL)
	rows, err := rt.legacyRequests(context.Background())
	if err != nil {
		t.Fatalf("a string data payload must be decoded: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected one request, got %v", rows)
	}
	counts, err := rt.legacyActivity(context.Background())
	if err != nil || counts["glm"] != 1 {
		t.Fatalf("model fallback failed: %v %v", counts, err)
	}
}

func TestInflightSnapshotWithRequestsAccepted(t *testing.T) {
	s := inflightServer(t, "data: {\"type\":\"inflight\",\"data\":{\"operation\":\"snapshot\",\"requests\":[{\"id\":3,\"model_id\":\"glm\"}]}}\n\n")
	rt := newTestRuntime(t, s.URL)
	rows, err := rt.legacyRequests(context.Background())
	if err != nil {
		t.Fatalf("a valid requests list must be returned: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the snapshot request, got %v", rows)
	}
	counts, err := rt.legacyActivity(context.Background())
	if err != nil || counts["glm"] != 1 {
		t.Fatalf("model_id fallback failed: %v %v", counts, err)
	}
}

func TestInflightBadShapesAreUnknown(t *testing.T) {
	cases := map[string]string{
		"requests null":      "data: {\"type\":\"inflight\",\"data\":{\"operation\":\"snapshot\",\"requests\":null}}\n\n",
		"requests wrongtype": "data: {\"type\":\"inflight\",\"data\":{\"operation\":\"snapshot\",\"requests\":\"none\"}}\n\n",
		"malformed event":    "data: {\"type\":\"inflight\"\n\n",
		"data wrongtype":     "data: {\"type\":\"inflight\",\"data\":42}\n\n",
		"data bad string":    "data: {\"type\":\"inflight\",\"data\":\"{not json}\"}\n\n",
	}
	for name, body := range cases {
		s := inflightServer(t, body)
		rt := newTestRuntime(t, s.URL)
		if _, err := rt.legacyRequests(context.Background()); err == nil {
			t.Fatalf("%s must be unknown, never idle", name)
		}
		if _, err := rt.legacyActivity(context.Background()); err == nil {
			t.Fatalf("%s must never be treated as idle by activity", name)
		}
	}
}

func TestInflightNoSnapshotIsUnknown(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 300; i++ {
		b.WriteString("data: {\"type\":\"log\",\"data\":{\"line\":\"x\"}}\n\n")
	}
	s := inflightServer(t, b.String())
	rt := newTestRuntime(t, s.URL)
	if _, err := rt.legacyRequests(context.Background()); err == nil {
		t.Fatal("a bounded scan without a snapshot must be unknown")
	}
}

func TestCancelLegacyContract(t *testing.T) {
	var mu = make(chan int, 1) // serialized by the test's single call
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/events":
			_, _ = w.Write([]byte("data: {\"type\":\"inflight\",\"data\":{\"operation\":\"snapshot\",\"requests\":[{\"id\":7,\"model\":\"glm\"}]}}\n\n"))
		case r.URL.Path == "/api/inflight/7/cancel":
			mu <- 1
			w.WriteHeader(http.StatusNotFound) // completed race: tolerated
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(s.Close)
	rt := newTestRuntime(t, s.URL)
	if err := rt.cancelLegacy(context.Background(), []string{"glm"}); err != nil {
		t.Fatalf("a 404 cancel completion race must be tolerated: %v", err)
	}
	if <-mu != 1 {
		t.Fatal("the cancel endpoint was never reached")
	}
	// An invalid request id is never used in a URL.
	bad := inflightServer(t, "data: {\"type\":\"inflight\",\"data\":{\"operation\":\"snapshot\",\"requests\":[{\"id\":\"../x\",\"model\":\"glm\"}]}}\n\n")
	rt2 := newTestRuntime(t, bad.URL)
	if err := rt2.cancelLegacy(context.Background(), []string{"glm"}); err == nil {
		t.Fatal("an invalid manager request id must be rejected")
	}
	// Unrelated models are never cancelled.
	quiet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/events" {
			_, _ = w.Write([]byte("data: {\"type\":\"inflight\",\"data\":{\"operation\":\"snapshot\",\"requests\":[{\"id\":7,\"model\":\"other\"}]}}\n\n"))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(quiet.Close)
	rt3 := newTestRuntime(t, quiet.URL)
	if err := rt3.cancelLegacy(context.Background(), []string{"glm"}); err != nil {
		t.Fatalf("an unrelated model must not be cancelled: %v", err)
	}
}

func TestNativeNeverCallsManager(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.Close)
	root := t.TempDir()
	cfg := legacyClusterConfig(root, s.URL)
	cfg["lifecycle"] = "native"
	rt := newRuntime(cfg, filepath.Join(root, "models.json"))
	if _, err := rt.legacyActivity(context.Background()); err != nil {
		t.Fatalf("native activity must be empty: %v", err)
	}
	if err := rt.cancelLegacy(context.Background(), []string{"glm"}); err != nil {
		t.Fatalf("native cancel must be a no-op: %v", err)
	}
	if err := rt.syncLegacy(context.Background()); err != nil {
		t.Fatalf("native sync must be a no-op: %v", err)
	}
	if calls != 0 {
		t.Fatalf("native mode called the manager %d times", calls)
	}
}

func TestSyncEqualSemanticComparison(t *testing.T) {
	root := t.TempDir()
	cfg := legacyClusterConfig(root, "http://127.0.0.1:9")
	// The desired update: proxy, ttl 0, concurrency and context applied.
	updated := copyObject(Object{"globalTTL": 0, "models": Object{
		"glm": Object{"proxy": "http://127.0.0.1:8001", "concurrencyLimit": 4, "ttl": 0,
			"capabilities": Object{"context": 262144}}},
		"routing": Object{"router": Object{"use": "matrix", "settings": Object{"matrix": Object{"sets": Object{"placement_1": "glm"}}}}}})
	// Already synced, with json.Number spellings and a different set label.
	config := copyObject(Object{"globalTTL": json.Number("0"), "models": Object{
		"glm": Object{"proxy": "http://127.0.0.1:8001", "concurrencyLimit": json.Number("4"), "ttl": json.Number("0"),
			"unrelated": "kept", "capabilities": Object{"context": json.Number("262144")}}},
		"routing": Object{"router": Object{"use": "matrix", "settings": Object{"matrix": Object{"sets": Object{"group_9": "glm"}}}}}})
	if !syncEqual(config, updated, cfg) {
		t.Fatal("an unchanged placement/TTL/concurrency/context must be a semantic no-op despite labels and numeric spelling")
	}
	if str(obj(obj(config["models"])["glm"])["unrelated"]) != "kept" {
		t.Fatal("unrelated manager fields must be preserved by the comparison")
	}
	// A changed context is a real change.
	obj(obj(config["models"])["glm"])["capabilities"] = Object{"context": 131072}
	if syncEqual(config, updated, cfg) {
		t.Fatal("a changed context must trigger a reload")
	}
	// A matrix membership change is a real change.
	changed := copyObject(config)
	changed["routing"] = Object{"router": Object{"use": "matrix", "settings": Object{"matrix": Object{"sets": Object{"placement_1": "other"}}}}}
	if syncEqual(changed, updated, cfg) {
		t.Fatal("a changed matrix membership must trigger a reload")
	}
}

// legacyClusterConfig builds the minimum valid legacy cluster used by the
// runtime and lifecycle tests.
func legacyClusterConfig(root, managerURL string) Object {
	return Object{
		"version": 1, "root": root, "controller_port": 8020, "lifecycle": "legacy",
		"manager_url": managerURL,
		"nodes": Object{
			"spark01": Object{"direct_ip": "10.0.0.1"},
			"spark02": Object{"direct_ip": "10.0.0.2", "ssh": "spark02"},
		},
		"policy": Object{"interrupt_active_jobs": false, "protected_models": []any{},
			"comfyui_exclusive_with_llms": true},
		"models": Object{
			"glm": Object{"kind": "llm", "name": "GLM", "run_on": "spark01", "keep_loaded": false, "reasoning": false, "aliases": []any{},
				"settings": Object{"spark01": Object{"backend_url": "http://127.0.0.1:8001", "units": Object{"spark01": "glm.service"},
					"context_tokens": 262144, "concurrency": 4, "output_tokens": 8192}}},
			"comfyui": Object{"kind": "comfyui", "name": "ComfyUI", "run_on": "spark02",
				"keep_loaded": false, "reasoning": false, "aliases": []any{},
				"settings": Object{"spark02": Object{}}},
		},
	}
}

func TestLegacyRequestsDeadlineBounded(t *testing.T) {
	// A hung manager must never block the drain: the request itself is
	// bounded by the client timeout seam.
	block := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() { close(block); s.Close() })
	rt := newTestRuntime(t, s.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := rt.legacyActivity(ctx); err == nil {
		t.Fatal("a hung manager activity read must be an error, never idle")
	} else if !strings.Contains(err.Error(), "Cannot confirm") {
		t.Fatalf("unexpected error text: %v", err)
	}
	_ = fmt.Sprint()
}
