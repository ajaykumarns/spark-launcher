package sparky

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- fakes ---

type fakeJournal struct {
	files     []fakeWrite
	units     []fakeUnit
	rollbacks int
}

type fakeWrite struct {
	node, path string
	data       []byte
	mode       os.FileMode
}

type fakeUnit struct {
	node, path, text string
}

func (f *fakeJournal) WriteFile(_ context.Context, node, path string, data []byte, mode os.FileMode) error {
	f.files = append(f.files, fakeWrite{node, path, data, mode})
	return nil
}
func (f *fakeJournal) WriteUnit(_ context.Context, node, path, text string) error {
	f.units = append(f.units, fakeUnit{node, path, text})
	return nil
}
func (f *fakeJournal) Rollback() error { f.rollbacks++; return nil }

// wrote finds a journaled write by destination, whether it went through
// WriteFile or WriteUnit — units and files share the same transaction.
func (f *fakeJournal) wrote(path string) (fakeWrite, bool) {
	for _, w := range f.files {
		if w.path == path {
			return w, true
		}
	}
	for _, u := range f.units {
		if u.path == path {
			return fakeWrite{node: u.node, path: u.path, data: []byte(u.text)}, true
		}
	}
	return fakeWrite{}, false
}

func (f *fakeJournal) unitText(path string) (string, bool) {
	for _, u := range f.units {
		if u.path == path {
			return u.text, true
		}
	}
	return "", false
}

type fakeInstallRuntime struct {
	before, after Object
	unitCalls     []string
	snapshots     int
}

func (f *fakeInstallRuntime) snapshot(_ context.Context, _ bool) (Object, error) {
	f.snapshots++
	if f.snapshots == 1 || f.after == nil {
		return copyObject(f.before), nil
	}
	return copyObject(f.after), nil
}
func (f *fakeInstallRuntime) unit(_ context.Context, node, action, unit string) error {
	f.unitCalls = append(f.unitCalls, node+"|"+action+"|"+unit)
	return nil
}

// installFixture wires all seams to fakes and restores them on cleanup.
type installFixture struct {
	journal       *fakeJournal
	runtime       *fakeInstallRuntime
	commands      []string
	fences        []bool
	fenceURL      []string
	fenceReads    int
	priorFenced   bool
	gateways      int
	gatewayUndos  int
	order         []string // event log: idle:<url>, restart:<node>, gateway, drain
	qwen          int
	failGate      bool
	failDrain     bool
	gatewayResult Object
	// guardCalls counts installGuardState probes per ComfyUI URL, so the
	// default fake can model an absent guard that appears after the restart.
	guardCalls map[string]int
	// probeFailUnit, when set, makes the unit-state probe report an
	// unknown state for exactly that unit, refusing before mutations.
	probeFailUnit string
}

func installFixtureTest(t *testing.T, mutate func(*installFixture)) (*installFixture, string) {
	t.Helper()
	f := &installFixture{
		journal:    &fakeJournal{},
		runtime:    &fakeInstallRuntime{before: installBaseline()},
		guardCalls: map[string]int{},
	}
	if mutate != nil {
		mutate(f)
	}
	stage := installStage(t)
	restoreJournal := installNewJournal
	restorePrepare := installPrepareQwen
	restoreGateway := installGateway
	restoreRuntime := installRuntime
	restoreRun := installRun
	restoreFence := installFence
	restoreFenceState := installFenceState
	restoreWait := installWaitHTTP
	restoreIdle := installIdle
	restoreGuard := installGuardState
	restoreDrain := installDrainActivity
	restoreUnitState := installUnitState
	t.Cleanup(func() {
		installNewJournal, installPrepareQwen, installGateway, installRuntime = restoreJournal, restorePrepare, restoreGateway, restoreRuntime
		installRun, installFence, installFenceState, installWaitHTTP = restoreRun, restoreFence, restoreFenceState, restoreWait
		installIdle, installGuardState, installDrainActivity = restoreIdle, restoreGuard, restoreDrain
		installUnitState = restoreUnitState
	})
	installNewJournal = func(Object, string) installationJournal { return f.journal }
	installPrepareQwen = func(context.Context, Object, string, installationJournal) error { f.qwen++; return nil }
	installGateway = func(Object, int, string) (Object, func() error, error) {
		if f.failGate {
			return nil, nil, fail("gateway down")
		}
		f.gateways++
		f.order = append(f.order, "gateway")
		result := f.gatewayResult
		if result == nil {
			result = Object{"catalog_enabled": true}
		}
		return result, func() error { f.gatewayUndos++; return nil }, nil
	}
	installRuntime = func(Object, string) installationRuntime { return f.runtime }
	installRun = func(_ context.Context, args []string, _ []byte) (CommandResult, error) {
		f.commands = append(f.commands, strings.Join(args, " "))
		return CommandResult{}, nil
	}
	installFence = func(_ context.Context, port int, enabled bool) error {
		f.fences = append(f.fences, enabled)
		f.fenceURL = append(f.fenceURL, "http://127.0.0.1:"+strconv_Itoa(port))
		return nil
	}
	installFenceState = func(_ context.Context, _ int) (bool, error) {
		f.fenceReads++
		return f.priorFenced, nil
	}
	installWaitHTTP = func(_ context.Context, _ string, _ time.Duration) (Object, error) {
		return Object{}, nil
	}
	installIdle = func(_ context.Context, url string) error {
		f.order = append(f.order, "idle:"+url)
		return nil
	}
	// The controller unit is treated as previously absent, so the restore
	// path exercises the stop and disable restoration explicitly. The
	// probe refuses with an unknown state when the fixture targets one.
	installUnitState = func(_ context.Context, unit string) (bool, bool, bool, error) {
		if unit == f.probeFailUnit {
			return false, false, false, fail("The %s unit state is unknown.", unit)
		}
		return false, false, false, nil
	}
	// The guard state is absent until the first restart has happened, then
	// it reports the accepted version for the confirm step. The probe is
	// counted per URL: the first read (the guard check) reports absent, the
	// second (after the restart, the confirm step) reports version 1.
	installGuardState = func(_ context.Context, url string) (bool, error) {
		f.guardCalls[url]++
		return f.guardCalls[url] > 1, nil
	}
	installDrainActivity = func(_ context.Context, _ installationRuntime) error {
		if f.failDrain {
			return fail("drain stuck")
		}
		f.order = append(f.order, "drain")
		return nil
	}
	return f, stage
}

func strconv_Itoa(n int) string { return intText(n) }

// installBaseline is a plausible device baseline: one loaded LLM plus the
// protected Parakeet containers. Every unit the installer may consult has a
// known state; an unknown state must abort before any mutation.
func installBaseline() Object {
	return Object{
		"nodes": Object{
			"spark01": Object{
				"containers": Object{},
				"units": Object{
					"spark-imagegen.service": "active",
					"spark-glm.service":      "active",
					"spark-glm-0.service":    "inactive",
					"spark-glm-1.service":    "inactive",
				},
			},
			"spark02": Object{
				"containers": Object{
					"parakeet-engine":  Object{"running": true, "id": "abc123", "started_at": "2026-01-01T00:00:00Z"},
					"parakeet-gateway": Object{"running": true, "id": "def456", "started_at": "2026-01-01T00:00:01Z"},
				},
				"units": Object{
					"spark-imagegen.service": "active",
					"spark-glm-0.service":    "inactive",
					"spark-glm-1.service":    "inactive",
				},
			},
		},
		"models": Object{
			"glm-5.3-flash": Object{"kind": "llm", "loaded": true, "nodes": []any{"spark01"}},
			"parakeet":      Object{"kind": "audio", "loaded": true, "nodes": []any{"spark02"}},
		},
	}
}

// installStage creates a real staged models.json fixture.
func installStage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	raw, err := json.MarshalIndent(planCluster(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "models.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The DEFAULT installFenceState and installFence seams hit the REAL
// controller route contract: GET /control/fence reads the manual flag and
// POST toggles it, both under the loopback operator ACL. No seam is
// overridden here — the actual HTTP behavior of the server is the contract.
func TestInstallFenceSeamsAgainstRealController(t *testing.T) {
	s := &controllerServer{ctrl: newFakeController()}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: s}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	port := listener.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// GET false: an unfenced controller reads back exactly false.
	prior, err := installFenceState(ctx, port)
	if err != nil || prior {
		t.Fatalf("initial fence state: %v %v", prior, err)
	}
	// POST true, then GET true: the read observes the manual flag.
	if err := installFence(ctx, port, true); err != nil {
		t.Fatalf("fence engage: %v", err)
	}
	prior, err = installFenceState(ctx, port)
	if err != nil || !prior {
		t.Fatalf("fenced state after engage: %v %v", prior, err)
	}
	// POST false restores the exact prior state.
	if err := installFence(ctx, port, false); err != nil {
		t.Fatalf("fence lift: %v", err)
	}
	prior, err = installFenceState(ctx, port)
	if err != nil || prior {
		t.Fatalf("fenced state after lift: %v %v", prior, err)
	}
}

func TestInstallHeadSuccessShape(t *testing.T) {
	f, stage := installFixtureTest(t, nil)
	result, err := InstallHead(stage)
	if err != nil {
		t.Fatalf("InstallHead: %v", err)
	}
	if str(result["status"]) != "installed" ||
		str(result["config_path"]) != "/srv/spark-cluster/config/models.json" ||
		!boolean(result["parakeet_preserved"]) ||
		!boolean(result["manager_not_restarted"]) ||
		!boolean(result["comfy_guards_installed"]) {
		t.Fatalf("result shape: %v", result)
	}
	if _, ok := result["gateway"]; !ok {
		t.Fatal("result must carry the actual gateway result")
	}
	// Node payloads: metadata, plugin, guard, drop-in for both nodes.
	for _, node := range []string{"spark01", "spark02"} {
		for _, want := range []string{
			"/srv/spark-cluster/controller/node-" + node + ".json",
			"/srv/spark-cluster/controller/comfy-" + node + ".json",
			"/srv/image-gen/ComfyUI/custom_nodes/SparkyGuard/__init__.py",
			"/etc/systemd/system/spark-imagegen.service.d/sparky.conf",
			"/etc/systemd/system/sparky-controller.service",
			"/etc/systemd/system/sparky-model@.service",
		} {
			if _, ok := f.journal.wrote(want); !ok {
				t.Fatalf("missing write %s", want)
			}
		}
		// Worker payloads must be routed to the worker, head payloads local.
		if w, ok := f.journal.wrote("/srv/spark-cluster/controller/node-" + node + ".json"); ok && (node == "spark02") != (w.node == "spark02") {
			t.Fatalf("wrong route for %s: %q", node, w.node)
		}
	}
	// The native model template is inactive: no [Install]/WantedBy.
	if text, ok := f.journal.unitText("/etc/systemd/system/sparky-model@.service"); !ok || strings.Contains(text, "WantedBy") {
		t.Fatal("model template must not be enabled")
	}
	// The manager hook is the real compatibility target, written executable.
	w, ok := f.journal.wrote("/srv/spark-cluster/manager/cluster-control.sh")
	if !ok || w.mode != 0o755 || !strings.Contains(string(w.data), "supervise") {
		t.Fatalf("manager hook wrong: %+v", w)
	}
	// Daemon reload on both nodes and a single enable of the controller only.
	joined := strings.Join(f.commands, ";")
	if strings.Count(joined, "daemon-reload") != 2 {
		t.Fatalf("expected two daemon-reloads: %v", f.commands)
	}
	enables := 0
	for _, c := range f.commands {
		if strings.Contains(c, "enable --now") {
			enables++
			if !strings.Contains(c, "sparky-controller.service") {
				t.Fatalf("other unit enabled: %s", c)
			}
		}
		if strings.Contains(c, "systemctl start") || strings.Contains(c, "systemctl stop") ||
			strings.Contains(c, "systemctl disable") {
			t.Fatalf("success must never start, stop or disable a unit: %s", c)
		}
	}
	if enables != 1 {
		t.Fatalf("controller enable wrong: %v", f.commands)
	}
	// Parity record written from the actual baseline for the loaded LLM only.
	w, ok = f.journal.wrote("/srv/spark-cluster/controller/running-glm-5.3-flash.json")
	if !ok {
		t.Fatal("missing parity record")
	}
	var record Object
	if err := json.Unmarshal(w.data, &record); err != nil {
		t.Fatal(err)
	}
	if !sameStrings(stringsOf(record["nodes"]), []string{"spark01"}) ||
		str(record["placement"]) != "spark01" ||
		str(obj(record["units"])["spark01"]) != "spark-glm.service" {
		t.Fatalf("parity record adopted from the actual baseline: %v", record)
	}
	if _, ok := f.journal.wrote("/srv/spark-cluster/controller/running-parakeet.json"); ok {
		t.Fatal("audio models must never get parity records")
	}
	// The installation result is journaled with the public source fields.
	if w, ok := f.journal.wrote("/srv/spark-cluster/controller/installation-result.json"); !ok {
		t.Fatal("missing installation result file")
	} else if !strings.Contains(string(w.data), "parakeet_preserved") {
		t.Fatal("result file lost public fields")
	}
	// Qwen preparation ran once, inactive.
	if f.qwen != 1 {
		t.Fatalf("qwen preparation calls: %d", f.qwen)
	}
	// Fence on then off, gateway exactly once.
	if len(f.fences) != 2 || !f.fences[0] || f.fences[1] {
		t.Fatalf("fence sequence: %v", f.fences)
	}
	if f.gateways != 1 {
		t.Fatalf("gateway calls: %d", f.gateways)
	}
}

func TestInstallHeadIdleBeforeRestart(t *testing.T) {
	f, stage := installFixtureTest(t, nil)
	if _, err := InstallHead(stage); err != nil {
		t.Fatal(err)
	}
	// Guard absent: each node drains (idle, three confirmed samples) before
	// its restart, and only restart actions may touch comfy units.
	idles := 0
	for _, e := range f.order {
		if strings.HasPrefix(e, "idle:") {
			idles++
		}
	}
	if idles != 2 {
		t.Fatalf("idle confirmations: %v", f.order)
	}
	if len(f.runtime.unitCalls) != 2 {
		t.Fatalf("unit calls: %v", f.runtime.unitCalls)
	}
	for _, call := range f.runtime.unitCalls {
		parts := strings.Split(call, "|")
		if parts[1] != "restart" {
			t.Fatalf("non-restart unit action: %s", call)
		}
		if !strings.HasSuffix(parts[2], "spark-imagegen.service") {
			t.Fatalf("unexpected unit restart: %s", call)
		}
	}
}

func TestInstallHeadGuardPresentSkipsRestart(t *testing.T) {
	f, stage := installFixtureTest(t, nil)
	installGuardState = func(_ context.Context, _ string) (bool, error) { return true, nil }
	if _, err := InstallHead(stage); err != nil {
		t.Fatal(err)
	}
	if len(f.runtime.unitCalls) != 0 { // only gateway + drain events allowed
		t.Fatalf("guarded nodes must not be restarted: %v", f.runtime.unitCalls)
	}
	for _, e := range f.order {
		if e != "gateway" && e != "drain" {
			t.Fatalf("guarded nodes must not drain or restart: %v", f.order)
		}
	}
}

// A valid desired state is an inactive worker engine: its files and drop-in
// are installed for a future start, but it is never waited on, restarted or
// enabled. Only the active head engine drains and restarts.
func TestInstallHeadInactiveWorkerUntouched(t *testing.T) {
	f, stage := installFixtureTest(t, nil)
	obj(obj(obj(f.runtime.before)["nodes"])["spark02"])["units"] = Object{
		"spark-imagegen.service": "inactive",
		"spark-glm-0.service":    "inactive",
		"spark-glm-1.service":    "inactive",
	}
	if _, err := InstallHead(stage); err != nil {
		t.Fatal(err)
	}
	idles := 0
	for _, e := range f.order {
		if strings.HasPrefix(e, "idle:") {
			idles++
		}
	}
	if idles != 1 {
		t.Fatalf("only the active engine may drain: %v", f.order)
	}
	if len(f.runtime.unitCalls) != 1 || !strings.HasPrefix(f.runtime.unitCalls[0], "spark01|restart|") {
		t.Fatalf("unit calls: %v", f.runtime.unitCalls)
	}
	for _, c := range f.commands {
		if strings.Contains(c, "spark-imagegen") && strings.Contains(c, "start") {
			t.Fatalf("inactive engine was started: %s", c)
		}
	}
}

// A pre-existing manual fence must survive the whole installation: the
// fence is engaged on top and restored to fenced, never blindly lifted.
func TestInstallHeadPreviousFenceStaysFenced(t *testing.T) {
	f, stage := installFixtureTest(t, func(f *installFixture) { f.priorFenced = true })
	if _, err := InstallHead(stage); err != nil {
		t.Fatal(err)
	}
	if len(f.fences) != 2 || !f.fences[0] || !f.fences[1] {
		t.Fatalf("fence sequence must be on then prior(true): %v", f.fences)
	}
}

// A failure after the gateway connected must run the real gateway undo in
// addition to the file rollback, and restore the prior fence state.
func TestInstallHeadLateFailureRunsGatewayUndo(t *testing.T) {
	f, stage := installFixtureTest(t, func(f *installFixture) { f.failDrain = true })
	_, err := InstallHead(stage)
	if err == nil {
		t.Fatal("a late failure must fail the installation")
	}
	if f.journal.rollbacks != 1 {
		t.Fatalf("rollbacks: %d", f.journal.rollbacks)
	}
	if f.gatewayUndos != 1 {
		t.Fatalf("gateway undos: %d", f.gatewayUndos)
	}
	if len(f.fences) != 2 || !f.fences[0] || f.fences[1] {
		t.Fatalf("fence sequence: %v", f.fences)
	}
	// Service state restoration is attempted even on the failure path.
	joined := strings.Join(f.commands, ";")
	if !strings.Contains(joined, "systemctl stop") || !strings.Contains(joined, "daemon-reload") {
		t.Fatalf("service state was not restored: %v", f.commands)
	}
}

// An existing running record is preserved exactly; the installer never
// overwrites it from desired placement.
func TestInstallHeadPreservesExistingRunningRecord(t *testing.T) {
	f, _ := installFixtureTest(t, nil)
	// A real installed central configuration in a temporary root, with a
	// pre-existing running record that must survive byte-for-byte.
	root := t.TempDir()
	cfg := planCluster()
	cfg["root"] = root
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.MkdirAll(filepath.Join(root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "controller"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config", "models.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"nodes":["spark02"],"units":{"spark02":"legacy.service"},"placement":"spark02"}` + "\n")
	recordPath := filepath.Join(root, "controller", "running-glm-5.3-flash.json")
	if err := os.WriteFile(recordPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	staged := planCluster()
	sraw, _ := json.MarshalIndent(staged, "", "  ")
	if err := os.WriteFile(filepath.Join(stage, "models.json"), sraw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallHead(stage); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(original) {
		t.Fatalf("the running record was overwritten: %s", current)
	}
	for _, w := range f.journal.files {
		if w.path == recordPath {
			t.Fatal("an existing running record must never be rewritten")
		}
	}
}

// An ambiguous actual placement (no recipe matches the baseline's active
// units) must abort with no record written.
func TestInstallHeadAmbiguousPlacementFails(t *testing.T) {
	f, stage := installFixtureTest(t, nil)
	obj(obj(obj(f.runtime.before)["nodes"])["spark01"])["units"] = Object{
		"spark-imagegen.service": "active",
		"spark-glm.service":      "inactive", // loaded but its unit is not the active one
		"spark-glm-0.service":    "inactive",
		"spark-glm-1.service":    "inactive",
	}
	_, err := InstallHead(stage)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous placement must fail clearly: %v", err)
	}
	if _, ok := f.journal.wrote("/srv/spark-cluster/controller/running-glm-5.3-flash.json"); ok {
		t.Fatal("no record may be fabricated for an ambiguous placement")
	}
	if f.journal.rollbacks != 1 {
		t.Fatalf("rollbacks: %d", f.journal.rollbacks)
	}
}

// An unknown ComfyUI unit state aborts before any mutation. The head is
// probed through the real probe seam; a worker's proof comes from its own
// per-node baseline entry — the head is never asked about worker units.
func TestInstallHeadUnknownComfyStateAborts(t *testing.T) {
	f, stage := installFixtureTest(t, func(f *installFixture) {
		f.probeFailUnit = "spark-imagegen.service"
	})
	_, err := InstallHead(stage)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown unit state must abort clearly: %v", err)
	}
	if len(f.journal.files) != 0 || len(f.journal.units) != 0 {
		t.Fatal("no mutation may precede a known unit state")
	}
}

// A worker whose own baseline ComfyUI entry is unknown must abort before
// any mutation: the head's probe never substitutes for per-node proof.
func TestInstallHeadUnknownWorkerComfyBaselineAborts(t *testing.T) {
	f, stage := installFixtureTest(t, func(f *installFixture) {
		units := obj(obj(obj(f.runtime.before)["nodes"])["spark02"])["units"].(Object)
		delete(units, "spark-imagegen.service")
	})
	_, err := InstallHead(stage)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown worker baseline must abort clearly: %v", err)
	}
	if len(f.journal.files) != 0 || len(f.journal.units) != 0 {
		t.Fatal("no mutation may precede a known baseline")
	}
	if f.journal.rollbacks != 0 {
		t.Fatal("nothing to roll back")
	}
}

func TestInstallHeadLateFailureRollsBackAndUnfences(t *testing.T) {
	f, stage := installFixtureTest(t, func(f *installFixture) { f.failGate = true })
	_, err := InstallHead(stage)
	if err == nil {
		t.Fatal("gateway failure must fail the installation")
	}
	if f.journal.rollbacks != 1 {
		t.Fatalf("rollbacks: %d", f.journal.rollbacks)
	}
	// The fence was engaged and lifted again with a fresh context.
	if len(f.fences) != 2 || !f.fences[0] || f.fences[1] {
		t.Fatalf("fence sequence: %v", f.fences)
	}
}

func TestInstallHeadUnknownBaselineFailsBeforeMutations(t *testing.T) {
	f, stage := installFixtureTest(t, func(f *installFixture) {
		f.runtime.before = Object{"nodes": Object{}, "models": Object{}}
	})
	_, err := InstallHead(stage)
	if err == nil || !strings.Contains(err.Error(), "baseline") {
		t.Fatalf("unknown baseline must fail clearly: %v", err)
	}
	if len(f.journal.files) != 0 || len(f.journal.units) != 0 {
		t.Fatal("no mutation may precede a known baseline")
	}
	if f.journal.rollbacks != 0 {
		t.Fatal("nothing to roll back")
	}
}

func TestInstallHeadPortVariant(t *testing.T) {
	f, stage := installFixtureTest(t, nil)
	stage = t.TempDir()
	cfg := planCluster()
	cfg["controller_port"] = json.Number("9443")
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(stage, "models.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallHead(stage); err != nil {
		t.Fatal(err)
	}
	for _, url := range f.fenceURL {
		if url != "http://127.0.0.1:9443" {
			t.Fatalf("fence URL: %s", url)
		}
	}
	if text, ok := f.journal.unitText("/etc/systemd/system/sparky-controller.service"); !ok ||
		!strings.Contains(text, "config/models.json") {
		t.Fatal("controller unit lost its config path")
	}
	for _, w := range f.journal.files {
		if strings.HasPrefix(w.path, "/srv/spark-cluster/controller/comfy-") {
			var plugin Object
			if err := json.Unmarshal(w.data, &plugin); err != nil {
				t.Fatal(err)
			}
			if str(plugin["node"]) == "spark01" && str(plugin["controller"]) != "http://127.0.0.1:9443" {
				t.Fatalf("head plugin controller: %v", plugin["controller"])
			}
		}
	}
}

func TestInstallHeadProtectedIdentityMismatch(t *testing.T) {
	f, stage := installFixtureTest(t, nil)
	f.runtime.after = installBaseline()
	obj(obj(obj(obj(f.runtime.after)["nodes"])["spark02"])["containers"])["parakeet-engine"] =
		Object{"running": true, "id": "moved", "started_at": "2026-01-01T00:00:00Z"}
	_, err := InstallHead(stage)
	if err == nil || !strings.Contains(err.Error(), "Parakeet") {
		t.Fatalf("identity change must fail loudly: %v", err)
	}
	// The fence was lifted; the completed installation is reported, not hidden.
	if f.fences[len(f.fences)-1] {
		t.Fatal("fence must be lifted after a protected identity change")
	}
}

func TestInstallHeadPreservesExistingCentralConfig(t *testing.T) {
	f, _ := installFixtureTest(t, nil)
	// The staged root points at a temp dir so the central-config precedence
	// is exercised without touching the real cluster location.
	root := t.TempDir()
	cfg := planCluster()
	cfg["root"] = root
	cfg["controller_port"] = json.Number("9001")
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.MkdirAll(filepath.Join(root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config", "models.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	staged := planCluster()
	staged["root"] = root // the stage must point at the real central location
	staged["controller_port"] = json.Number("8020")
	sraw, _ := json.MarshalIndent(staged, "", "  ")
	if err := os.WriteFile(filepath.Join(stage, "models.json"), sraw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallHead(stage); err != nil {
		t.Fatal(err)
	}
	// The fence used the central port, not the staged 8020.
	for _, url := range f.fenceURL {
		if url != "http://127.0.0.1:9001" {
			t.Fatalf("central config did not take precedence: %s", url)
		}
	}
	// The central config file was never rewritten by the installer.
	if _, ok := f.journal.wrote(filepath.Join(root, "config", "models.json")); ok {
		t.Fatal("existing central config must not be rewritten")
	}
}

func TestInstallHeadRootMismatchAborts(t *testing.T) {
	_, _ = installFixtureTest(t, nil)
	root := t.TempDir()
	cfg := planCluster()
	cfg["root"] = "/elsewhere"
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.MkdirAll(filepath.Join(root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config", "models.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	staged := planCluster()
	staged["root"] = root // configPath must resolve to the written central config
	sraw, _ := json.MarshalIndent(staged, "", "  ")
	if err := os.WriteFile(filepath.Join(stage, "models.json"), sraw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallHead(stage); err == nil || !strings.Contains(err.Error(), "differs from the staged root") {
		t.Fatalf("root mismatch must abort: %v", err)
	}
}
