package sparky

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func schedulerTask(index int, name string, nodes ...string) profileStartTask {
	return profileStartTask{index: index, step: profileStep("start", name, nodes), nodes: nodes}
}

func nextProfileStart(t *testing.T, started <-chan string) string {
	t.Helper()
	select {
	case name := <-started:
		return name
	case <-time.After(3 * time.Second):
		t.Fatal("an independent profile start did not run")
		return ""
	}
}

func TestProfileSchedulerAdvancesIndependentDevicesAndJoinsBoth(t *testing.T) {
	tasks := []profileStartTask{
		schedulerTask(0, "a1", "spark01"), schedulerTask(1, "a2", "spark01"),
		schedulerTask(2, "b1", "spark02"), schedulerTask(3, "both", "spark01", "spark02"),
		schedulerTask(4, "b2", "spark02"), schedulerTask(5, "a3", "spark01"),
	}
	release := map[string]chan struct{}{}
	for _, task := range tasks {
		release[str(task.step["workload"])] = make(chan struct{})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan string, len(tasks))
	var mu sync.Mutex
	using := map[string]string{}
	type result struct {
		completed []any
		err       error
	}
	done := make(chan result, 1)
	go func() {
		completed, err := scheduleProfileStarts(ctx, tasks, func(ctx context.Context, task profileStartTask) ([]any, error) {
			name := str(task.step["workload"])
			mu.Lock()
			for _, node := range task.nodes {
				if previous := using[node]; previous != "" {
					t.Errorf("%s overlaps %s on %s", name, previous, node)
				}
				using[node] = name
			}
			mu.Unlock()
			started <- name
			select {
			case <-release[name]:
			case <-ctx.Done():
			}
			mu.Lock()
			for _, node := range task.nodes {
				delete(using, node)
			}
			mu.Unlock()
			return []any{task.step}, ctx.Err()
		})
		done <- result{completed, err}
	}()
	first := []string{nextProfileStart(t, started), nextProfileStart(t, started)}
	slices.Sort(first)
	if !slices.Equal(first, []string{"a1", "b1"}) {
		t.Fatalf("first starts = %v", first)
	}
	close(release["a1"])
	if name := nextProfileStart(t, started); name != "a2" {
		t.Fatalf("free lane did not advance: %s", name)
	}
	close(release["a2"])
	select {
	case name := <-started:
		t.Fatalf("two-device start passed a busy device: %s", name)
	case <-time.After(20 * time.Millisecond):
	}
	close(release["b1"])
	if name := nextProfileStart(t, started); name != "both" {
		t.Fatalf("expected both-device barrier: %s", name)
	}
	close(release["both"])
	last := []string{nextProfileStart(t, started), nextProfileStart(t, started)}
	slices.Sort(last)
	if !slices.Equal(last, []string{"a3", "b2"}) {
		t.Fatalf("starts after both-device model = %v", last)
	}
	close(release["a3"])
	close(release["b2"])
	got := <-done
	if got.err != nil || len(got.completed) != len(tasks) {
		t.Fatalf("scheduler result: %v %v", got.completed, got.err)
	}
	for i, raw := range got.completed {
		if str(obj(raw)["workload"]) != str(tasks[i].step["workload"]) {
			t.Fatal("completed effects lost deterministic plan order")
		}
	}
}

func TestProfileSchedulerFailureJoinsActiveStartsAndStopsNewWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tasks := []profileStartTask{schedulerTask(0, "fail", "spark01"), schedulerTask(1, "pending", "spark01"), schedulerTask(2, "other", "spark02")}
	started := make(chan string, 3)
	failNow, finishOther := make(chan struct{}), make(chan struct{})
	done := make(chan []any, 1)
	go func() {
		completed, err := scheduleProfileStarts(ctx, tasks, func(ctx context.Context, task profileStartTask) ([]any, error) {
			name := str(task.step["workload"])
			started <- name
			if name == "fail" {
				select {
				case <-failNow:
				case <-ctx.Done():
				}
				return []any{profileStep("stop", "peer", []string{"spark01"})}, errors.New("start failed")
			}
			if name == "other" {
				select {
				case <-finishOther:
				case <-ctx.Done():
				}
			}
			return []any{task.step}, nil
		})
		if err == nil || !strings.Contains(err.Error(), "start failed") {
			t.Errorf("lost first failure: %v", err)
		}
		done <- completed
	}()
	first := []string{nextProfileStart(t, started), nextProfileStart(t, started)}
	slices.Sort(first)
	if !slices.Equal(first, []string{"fail", "other"}) {
		t.Fatalf("initial starts: %v", first)
	}
	close(failNow)
	select {
	case <-done:
		t.Fatal("returned before the independent start finished")
	case name := <-started:
		t.Fatalf("scheduled work after failure: %s", name)
	case <-time.After(20 * time.Millisecond):
	}
	close(finishOther)
	completed := <-done
	if len(completed) != 2 || str(obj(completed[0])["workload"]) != "peer" || str(obj(completed[1])["workload"]) != "other" {
		t.Fatalf("lost completed effects on failure: %v", completed)
	}
	select {
	case name := <-started:
		t.Fatalf("unexpected start after failure: %s", name)
	default:
	}
}

func TestProfileSchedulerCancellationJoinsWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan string, 3)
	done := make(chan error, 1)
	go func() {
		_, err := scheduleProfileStarts(ctx, []profileStartTask{schedulerTask(0, "a", "spark01"), schedulerTask(1, "b", "spark02"), schedulerTask(2, "pending", "spark01")}, func(ctx context.Context, task profileStartTask) ([]any, error) {
			started <- str(task.step["workload"])
			<-ctx.Done()
			return nil, ctx.Err()
		})
		done <- err
	}()
	nextProfileStart(t, started)
	nextProfileStart(t, started)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation result: %v", err)
	}
	select {
	case name := <-started:
		t.Fatalf("queued task started after cancellation: %s", name)
	default:
	}
}

func TestProfileStartResourcesIncludeConditionalPeerPlacements(t *testing.T) {
	env := newIntegrationEnv(t)
	env.writeCentralPlacements()
	c, err := newCoordinator(env.path, env.runtime())
	if err != nil {
		t.Fatal(err)
	}
	step := profileStep("start", "new", []string{"spark01"})
	step["stop_if_low_memory"] = []any{profileStep("stop", "glm", []string{"spark01"})}
	tasks, err := c.profileStartTasks(c.cfg(), []any{step})
	if err != nil || len(tasks) != 1 || !sameStrings(tasks[0].nodes, []string{"spark01", "spark02"}) {
		t.Fatalf("conditional stop must reserve the peer's saved both-device placement: %v %v", tasks, err)
	}
}

func TestProfileStartsReachRealLifecycleAndServiceAdaptersInParallel(t *testing.T) {
	for _, otherKind := range []string{"model", "audio"} {
		t.Run(otherKind, func(t *testing.T) {
			env := newIntegrationEnv(t)
			env.writeCentralPlacements()
			cfg, err := readCluster(env.path)
			if err != nil {
				t.Fatal(err)
			}
			var profile string
			if otherKind == "model" {
				peer := copyObject(obj(obj(cfg["models"])["glm"]))
				recipe := copyObject(obj(obj(peer["settings"])["spark01"]))
				recipe["units"] = Object{"spark02": "peer.service"}
				recipe["parameter_bindings"] = Object{"spark02": obj(recipe["parameter_bindings"])["spark01"]}
				recipe["startup_order"] = []any{"spark02"}
				peer["run_on"], peer["settings"] = "spark02", Object{"spark02": recipe}
				obj(cfg["models"])["peer"] = peer
				profile = "      chat:\n        - glm: spark01\n        - peer: spark02\n"
			} else {
				profile = "      chat:\n        - glm: spark01\n      transcription:\n        - parakeet: spark02\n"
			}
			if err := writeCluster(env.path, cfg); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(filepath.Dir(env.path), "profiles.yaml"), []byte("version: 1\nprofiles:\n  parallel:\n    capabilities:\n"+profile), 0600); err != nil {
				t.Fatal(err)
			}
			rt := env.runtime()
			probe := func(meta Object) (Object, error) {
				state := env.probe(meta)
				state["containers"] = Object{"parakeet-asr": Object{"running": false}}
				return state, nil
			}
			rt.ProbeNode = probe
			started := make(chan string, 2)
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseAll()
			base := rt.Run
			rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
				command := strings.Join(args, " ")
				isModelStart := strings.HasPrefix(command, "sudo -n systemctl start --no-block sparky-model@")
				isAudioStart := args[0] == "ssh" && strings.Contains(args[len(args)-1], "'docker' 'start' 'parakeet-asr'")
				if isModelStart || isAudioStart {
					started <- command
					select {
					case <-release:
					case <-ctx.Done():
					}
					return CommandResult{}, errors.New("test transport refused startup")
				}
				if args[0] == "ssh" && strings.Contains(args[len(args)-1], "probe --file") {
					var meta Object
					if err := json.Unmarshal(input, &meta); err != nil {
						return CommandResult{}, err
					}
					state, err := probe(meta)
					return CommandResult{Out: string(jsonBytes(state))}, err
				}
				return base(ctx, args, input)
			}
			c, err := newCoordinator(env.path, rt)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan Object, 1)
			go func() {
				result, err := c.Profile("parallel", true)
				if err != nil {
					t.Errorf("apply error: %v", err)
				}
				done <- result
			}()
			nextProfileStart(t, started)
			nextProfileStart(t, started)
			progress := c.ProfileProgress("")
			if active := arr(progress["active_steps"]); len(active) != 2 || num(progress["step"]) != 0 {
				t.Fatalf("parallel starts were hidden by scalar progress: %v", progress)
			}
			// Both real adapters reached their start commands before either
			// returned. Release the deliberate failures and join the apply.
			releaseAll()
			result := <-done
			if str(result["status"]) != "failed" || len(arr(c.ProfileProgress("")["active_steps"])) != 0 {
				t.Fatalf("failed parallel start did not finish cleanly: %v", result)
			}
			if state := c.Admission(); len(arr(state["active"])) != 0 || len(arr(state["waiting"])) != 0 {
				t.Fatalf("failed parallel startup retained a device reservation: %v", state)
			}
			if _, err := os.Stat(profileStatePath(cfg)); !os.IsNotExist(err) {
				t.Fatal("failed starts saved a successful profile")
			}
		})
	}
}

func TestProfileSchedulerRechecksMemoryAfterSameDevicePredecessor(t *testing.T) {
	env := newIntegrationEnv(t)
	rt := env.runtime()
	base := rt.Run
	var mu sync.Mutex
	available := map[string]float64{"spark01": 100, "spark02": 100}
	probe := func(meta Object) (Object, error) {
		state := env.probe(meta)
		mu.Lock()
		free := available[str(meta["name"])]
		mu.Unlock()
		state["memory_bytes"] = Object{"MemAvailable": free * gib}
		return state, nil
	}
	// inspectNode identifies the node through its configuration, not a
	// synthesized aggregate memory value.
	rt.ProbeNode = func(meta Object) (Object, error) { meta["name"] = "spark01"; return probe(meta) }
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		if args[0] == "ssh" && strings.Contains(args[len(args)-1], "probe --file") {
			var meta Object
			if err := json.Unmarshal(input, &meta); err != nil {
				return CommandResult{}, err
			}
			meta["name"] = "spark02"
			state, err := probe(meta)
			return CommandResult{Out: string(jsonBytes(state))}, err
		}
		return base(ctx, args, input)
	}
	tasks := []profileStartTask{schedulerTask(0, "first", "spark01"), schedulerTask(1, "too-large", "spark01"), schedulerTask(2, "independent", "spark02")}
	for _, task := range tasks {
		task.step["minimum_available_gib"] = Object{task.nodes[0]: 80}
	}
	otherStarted := make(chan struct{})
	var starts []string
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	completed, err := scheduleProfileStarts(ctx, tasks, func(ctx context.Context, task profileStartTask) ([]any, error) {
		if err := checkProfileStartMemory(ctx, rt, task.step); err != nil {
			return nil, err
		}
		name := str(task.step["workload"])
		mu.Lock()
		starts = append(starts, name)
		available[task.nodes[0]] = 20
		mu.Unlock()
		if name == "independent" {
			close(otherStarted)
		}
		if name == "first" {
			select {
			case <-otherStarted:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return []any{task.step}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "spark01 has 20.0 GiB") || len(completed) != 2 || slices.Contains(starts, "too-large") {
		t.Fatalf("stale or pooled memory allowed a same-device start: starts=%v completed=%v err=%v", starts, completed, err)
	}
}

func TestProfileServiceStartUsesItsConfiguredMemoryFloor(t *testing.T) {
	env := newIntegrationEnv(t)
	cfg, err := readCluster(env.path)
	if err != nil {
		t.Fatal(err)
	}
	obj(obj(obj(obj(cfg["models"])["parakeet"])["settings"])["spark02"])["minimum_available_gib"] = Object{"spark02": 30}
	if err := writeCluster(env.path, cfg); err != nil {
		t.Fatal(err)
	}
	rt := env.runtime()
	base := rt.Run
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		if args[0] == "ssh" && strings.Contains(args[len(args)-1], "probe --file") {
			var meta Object
			if err := json.Unmarshal(input, &meta); err != nil {
				return CommandResult{}, err
			}
			state := env.probe(meta)
			state["memory_bytes"] = Object{"MemAvailable": 5 * gib}
			return CommandResult{Out: string(jsonBytes(state))}, nil
		}
		if strings.Contains(strings.Join(args, " "), "docker") {
			t.Error("service command ran below its configured memory floor")
			return CommandResult{}, errors.New("unexpected Docker command")
		}
		return base(ctx, args, input)
	}
	c, err := newCoordinator(env.path, rt)
	if err != nil {
		t.Fatal(err)
	}
	step := profileStep("start", "parakeet", []string{"spark02"})
	step["service_kind"] = "audio"
	step["containers"] = []any{"parakeet-asr"}
	completed, _, err := c.applyProfileSteps(context.Background(), cfg, []any{step}, func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "spark02 has 5.0 GiB available; parakeet needs 30.0 GiB") || len(completed) != 0 {
		t.Fatalf("service ignored its startup memory floor: %v %v", completed, err)
	}
}
