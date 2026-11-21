package sparky

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestApplyProfileReportsWaitAndFinalState(t *testing.T) {
	env := newIntegrationEnv(t)
	env.writeCentralPlacements()
	env.installFactory()
	t.Cleanup(env.joinSupervisor)
	if err := os.WriteFile(filepath.Join(filepath.Dir(env.path), "profiles.yaml"), []byte("version: 1\nprofiles:\n  empty:\n    capabilities: {}\n  single:\n    capabilities:\n      chat:\n        - glm: spark01\n"), 0600); err != nil {
		t.Fatal(err)
	}
	startReached, releaseStart := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseStart) })
	rt := env.runtime()
	run := rt.Run
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		if strings.Join(args, " ") == "sudo -n systemctl start --no-block sparky-model@glm.service" {
			close(startReached)
			select {
			case <-releaseStart:
			case <-ctx.Done():
				return CommandResult{}, ctx.Err()
			}
		}
		return run(ctx, args, input)
	}
	c, err := newCoordinator(env.path, rt)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.reconfigure.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			c.reconfigure.unlock()
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := c.ApplyProfile("empty", "", "request-one")
		done <- err
	}()
	deadline := time.After(3 * time.Second)
	for str(c.ProfileProgress("request-one")["status"]) != "running" {
		select {
		case <-deadline:
			t.Fatal("apply did not publish progress while waiting for the change lock")
		case <-time.After(time.Millisecond):
		}
	}
	progress := c.ProfileProgress("request-one")
	if str(progress["profile"]) != "empty" || !strings.Contains(str(progress["message"]), "Waiting") {
		t.Fatalf("wrong waiting progress: %v", progress)
	}
	if str(c.ProfileProgress("other-request")["status"]) != "pending" {
		t.Fatal("another client received this request's progress")
	}
	if snapshot, err := c.Snapshot(true); err != nil || str(obj(snapshot["profile_transition"])["status"]) != "running" {
		t.Fatalf("status must publish the active profile switch: %v %v", snapshot, err)
	}
	c.reconfigure.unlock()
	locked = false
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if p := c.ProfileProgress("request-one"); str(p["status"]) != "applied" || str(selectedProfile(c.cfg())["name"]) != "empty" {
		t.Fatalf("successful switch did not finish progress: %v", p)
	}
	if _, err := c.ApplyProfile("missing", "", "request-two"); err == nil {
		t.Fatal("missing profile should fail")
	}
	if p := c.ProfileProgress("request-two"); str(p["status"]) != "failed" || !strings.Contains(str(p["error"]), "Unknown profile") {
		t.Fatalf("early failure left a running switch: %v", p)
	}
	go func() { _, err := c.ApplyProfile("single", "", "request-three"); done <- err }()
	select {
	case <-startReached:
	case <-time.After(3 * time.Second):
		t.Fatal("model start did not reach the transport")
	}
	if p := c.ProfileProgress("request-three"); str(p["status"]) != "running" || num(p["step"]) != 1 || num(p["total"]) != 1 || !strings.Contains(str(p["message"]), "Start glm on spark01") {
		t.Fatalf("controller did not publish the real in-flight model step: %v", p)
	}
	if str(selectedProfile(c.cfg())["name"]) != "empty" {
		t.Fatal("in-flight step changed the saved profile before readiness")
	}
	releaseOnce.Do(func() { close(releaseStart) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if str(c.ProfileProgress("request-three")["status"]) != "applied" {
		t.Fatal("completed model start did not finish progress")
	}
}

func TestProfileProgressStepAndFailureElapsed(t *testing.T) {
	c := &Coordinator{}
	c.beginProfileProgress("qwen27", "id")
	c.profilePhase("Start qwen27 on spark01", 3, 8)
	c.mu.Lock()
	c.profileProgress.started = time.Now().Add(-65 * time.Second)
	c.profileProgress.stepStarted = time.Now().Add(-5 * time.Second)
	c.mu.Unlock()
	p := c.ProfileProgress("id")
	if num(p["step"]) != 3 || num(p["total"]) != 8 || num(p["elapsed_seconds"]) < 65 || num(p["step_elapsed_seconds"]) < 5 {
		t.Fatalf("wrong current step or elapsed time: %v", p)
	}
	c.finishProfileProgress(Object{"status": "failed", "error": "Backend not ready"}, nil)
	c.mu.Lock()
	c.profileProgress.finished = c.profileProgress.started.Add(70 * time.Second)
	c.mu.Unlock()
	if p := c.ProfileProgress("id"); str(p["status"]) != "failed" || num(p["elapsed_seconds"]) != 70 || str(p["error"]) != "Backend not ready" {
		t.Fatalf("terminal progress must freeze elapsed time and retain the failed step: %v", p)
	}
}

func TestProfileProgressKeepsAllParallelStepsAndTheirTimers(t *testing.T) {
	c := &Coordinator{}
	c.beginProfileProgress("parallel", "id")
	c.profileStepPhase("Start Qwen on spark01", 1, 5)
	c.profileStepPhase("Start audio on spark02", 3, 5)
	c.mu.Lock()
	first := c.profileProgress.active[1]
	first.started = time.Now().Add(-20 * time.Second)
	c.profileProgress.active[1] = first
	c.mu.Unlock()
	p := c.ProfileProgress("id")
	active := arr(p["active_steps"])
	if len(active) != 2 || num(p["step"]) != 0 || num(obj(active[0])["step"]) != 1 || num(obj(active[0])["elapsed_seconds"]) < 20 {
		t.Fatalf("parallel progress hid or reset an active step: %v", p)
	}
	c.profileStepDone(3, nil)
	p = c.ProfileProgress("id")
	if len(arr(p["active_steps"])) != 1 || num(p["step"]) != 1 || num(p["step_elapsed_seconds"]) < 20 {
		t.Fatalf("remaining step lost its timer after the other device finished: %v", p)
	}
	c.profileStepDone(1, errors.New("failed"))
	c.finishProfileProgress(Object{"status": "failed", "error": "failed"}, nil)
	if p := c.ProfileProgress("id"); len(arr(p["active_steps"])) != 0 || str(p["status"]) != "failed" {
		t.Fatalf("completed apply kept active steps: %v", p)
	}
}

type profileProgressController struct {
	*fakeController
	state Object
}

func (f *profileProgressController) ProfileProgress(id string) Object {
	if id != "request" {
		return Object{"status": "pending"}
	}
	return f.state
}

func TestProfileProgressEndpointIsLoopbackOnly(t *testing.T) {
	f := &profileProgressController{newFakeController(), Object{"status": "running", "message": "Start Qwen"}}
	h := &controllerServer{ctrl: f}
	for _, tc := range []struct {
		peer, method string
		code         int
	}{
		{"127.0.0.1:1234", "POST", 200},
		{"127.0.0.1:1234", "GET", 405},
		{"100.117.252.74:1234", "POST", 403},
	} {
		r := httptest.NewRequest(tc.method, "/control/profile-progress", strings.NewReader(`{"id":"request"}`))
		r.RemoteAddr = tc.peer
		w := httptest.NewRecorder()
		h.serveControl(w, r, r.URL.Path)
		if w.Code != tc.code {
			t.Fatalf("%s %s: got %d: %s", tc.peer, tc.method, w.Code, w.Body.String())
		}
		if tc.code == http.StatusOK && !strings.Contains(w.Body.String(), "Start Qwen") {
			t.Fatal("progress response lost its step")
		}
	}
}

func TestCLIProfileProgressBeforeCompletionAndJSONSilence(t *testing.T) {
	oldInterval, oldHeartbeat := cliProfilePollInterval, cliProgressInterval
	cliProfilePollInterval, cliProgressInterval = time.Millisecond, 3*time.Millisecond
	defer func() { cliProfilePollInterval, cliProgressInterval = oldInterval, oldHeartbeat }()
	for _, progressEnabled := range []bool{true, false} {
		var mu sync.Mutex
		id := ""
		probed := make(chan struct{}, 1)
		allowFinish := make(chan struct{})
		restore := remoteUseFakeRunner(func(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error) {
			payload, _ := decode(stdin)
			switch {
			case strings.Contains(argv[len(argv)-1], "'profile-apply'"):
				mu.Lock()
				id = str(payload["progress_id"])
				mu.Unlock()
				if progressEnabled {
					select {
					case <-allowFinish:
					case <-ctx.Done():
						return remoteRunResult{}, ctx.Err()
					}
				}
				return remoteRunResult{stdout: []byte(`{"status":"applied"}`)}, nil
			case strings.Contains(argv[len(argv)-1], "'profile-progress'"):
				mu.Lock()
				requestID := id
				mu.Unlock()
				if requestID == "" {
					return remoteRunResult{stdout: []byte(`{"status":"pending"}`)}, nil
				}
				select {
				case probed <- struct{}{}:
				default:
				}
				return remoteRunResult{stdout: jsonBytes(Object{"id": requestID, "status": "running", "active_steps": []any{
					Object{"step": 2, "total": 6, "message": "Start Qwen on spark01", "elapsed_seconds": 4},
					Object{"step": 3, "total": 6, "message": "Start audio on spark02", "elapsed_seconds": 7},
				}})}, nil
			}
			return remoteRunResult{}, errors.New("unexpected remote operation")
		})
		reader, writer := io.Pipe()
		captured := make(chan string, 1)
		go func() { b, _ := io.ReadAll(reader); captured <- string(b) }()
		result := make(chan error, 1)
		go func() {
			_, err := cliApplyProfile(context.Background(), remoteTestConfig(), Object{"name": "qwen"}, progressEnabled, writer)
			result <- err
		}()
		if progressEnabled {
			select {
			case <-probed:
			case <-time.After(3 * time.Second):
				t.Fatal("no progress read occurred while apply was blocked")
			}
			// Let the reporter write at least one current-step line before completion.
			time.Sleep(10 * time.Millisecond)
			close(allowFinish)
		}
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		writer.Close()
		output := <-captured
		if progressEnabled && (!strings.Contains(output, "Applying profile qwen") || !strings.Contains(output, "Step 2/6: Start Qwen") || !strings.Contains(output, "Step 3/6: Start audio") || !strings.Contains(output, "elapsed") || !strings.Contains(output, "step: 4s") || !strings.Contains(output, "step: 7s")) {
			t.Fatalf("missing live progress: %q", output)
		}
		if !progressEnabled && (output != "" || id != "") {
			t.Fatalf("JSON mode emitted progress or changed its payload: %q id=%q", output, id)
		}
		restore()
	}
}

func TestCLIProfileProgressProbeFailureDoesNotCancelApply(t *testing.T) {
	oldInterval := cliProfilePollInterval
	cliProfilePollInterval = time.Millisecond
	defer func() { cliProfilePollInterval = oldInterval }()
	probed := make(chan struct{})
	var once sync.Once
	restore := remoteUseFakeRunner(func(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error) {
		if strings.Contains(argv[len(argv)-1], "'profile-progress'") {
			once.Do(func() { close(probed) })
			return remoteRunResult{}, errors.New("probe failed")
		}
		select {
		case <-probed:
		case <-ctx.Done():
			return remoteRunResult{}, ctx.Err()
		}
		return remoteRunResult{stdout: []byte(`{"status":"applied"}`)}, nil
	})
	defer restore()
	var output bytes.Buffer
	result, err := cliApplyProfile(context.Background(), remoteTestConfig(), Object{"name": "qwen"}, true, &output)
	if err != nil || str(result["status"]) != "applied" {
		t.Fatalf("progress failure cancelled apply: %v %v", result, err)
	}
}
