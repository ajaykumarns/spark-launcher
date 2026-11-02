package sparky

// Native durable ownership inventory tests. Everything is local temp files:
// records are created with the real BeginOperation/TransitionState helpers,
// the runner is a loud refusal that fails the test on ANY command, and no
// SSH, systemd, docker, manager or backend is ever touched.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func inventoryRunner(t *testing.T) Runner {
	return func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		t.Errorf("unexpected command in ownership inventory: %v", args)
		return CommandResult{}, fail("no commands allowed")
	}
}

func inventoryConfig(root string, keep bool) Object {
	c := baseConfig()
	c["root"] = root
	c["lifecycle"] = "native"
	m := llmModel("both")
	m["keep_loaded"] = keep
	c["models"] = Object{"glm": m}
	return c
}

func inventoryEnv(t *testing.T, keep bool) (*Runtime, string) {
	t.Helper()
	root := t.TempDir()
	rt := newRuntime(inventoryConfig(root, keep), filepath.Join(root, "models.json"))
	rt.Run = inventoryRunner(t)
	return rt, root
}

// inventoryBegin writes a real queued record for glm on both nodes.
func inventoryBegin(t *testing.T, rt *Runtime) Object {
	t.Helper()
	cfg := rt.config()
	m := obj(obj(cfg["models"])["glm"])
	s := obj(obj(m["settings"])["both"])
	rec, err := rt.BeginOperation("glm", m, s, placements["both"], "both")
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func inventoryTo(t *testing.T, rt *Runtime, rec Object, state string) {
	t.Helper()
	if err := rt.TransitionState("glm", str(rec["generation"]), state); err != nil {
		t.Fatalf("transition to %s: %v", state, err)
	}
}

func TestNativeOwnershipMissingDirectoryIsEmpty(t *testing.T) {
	rt, _ := inventoryEnv(t, true)
	out, err := rt.nativeOwnership(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("expected empty inventory, got %v", out)
	}
}

func TestNativeOwnershipQueuedReservesNodesBeforeAnyStart(t *testing.T) {
	rt, _ := inventoryEnv(t, true)
	inventoryBegin(t, rt)
	out, err := rt.nativeOwnership(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	row := obj(out["glm"])
	if str(row["kind"]) != "llm" || str(row["state"]) != StateQueued {
		t.Fatalf("unexpected row: %v", row)
	}
	if !boolean(row["loaded"]) || !boolean(row["reserved"]) {
		t.Fatalf("queued record must hold occupancy: %v", row)
	}
	if got := stringsOf(row["nodes"]); !sameStrings(got, []string{"spark01", "spark02"}) {
		t.Fatalf("queued record must reserve both recorded nodes, got %v", got)
	}
	if str(row["desired"]) != "both" || !boolean(row["keep_loaded"]) || row["active_requests"] != 0 {
		t.Fatalf("unexpected policy fields: %v", row)
	}
}

func TestNativeOwnershipAllActiveStatesHoldNodes(t *testing.T) {
	for _, state := range []string{StateStarting, StateReady, StateStopping, StateFailed} {
		rt, _ := inventoryEnv(t, true)
		rec := inventoryBegin(t, rt)
		inventoryTo(t, rt, rec, StateStarting)
		if state != StateStarting {
			inventoryTo(t, rt, rec, state)
		}
		out, err := rt.nativeOwnership(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		row := obj(out["glm"])
		if str(row["state"]) != state || !boolean(row["loaded"]) ||
			!sameStrings(stringsOf(row["nodes"]), []string{"spark01", "spark02"}) {
			t.Fatalf("%s record must hold its recorded nodes, got %v", state, row)
		}
	}
}

func TestNativeOwnershipStoppedReleasesReservation(t *testing.T) {
	rt, _ := inventoryEnv(t, true)
	rec := inventoryBegin(t, rt)
	inventoryTo(t, rt, rec, StateStarting)
	inventoryTo(t, rt, rec, StateStopping)
	inventoryTo(t, rt, rec, StateStopped)
	out, err := rt.nativeOwnership(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	row := obj(out["glm"])
	if boolean(row["loaded"]) || boolean(row["reserved"]) || len(stringsOf(row["nodes"])) != 0 {
		t.Fatalf("stopped record must release its reservation: %v", row)
	}
	if str(row["state"]) != StateStopped {
		t.Fatalf("unexpected state: %v", row)
	}
}

func TestNativeOwnershipRemovedModelStillHoldsRecordedNodes(t *testing.T) {
	rt, _ := inventoryEnv(t, true)
	inventoryBegin(t, rt)
	// The model is deleted from the current config entirely.
	c := rt.config()
	delete(obj(c["models"]), "glm")
	rt.configure(c)
	out, err := rt.nativeOwnership(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	row := obj(out["glm"])
	if !boolean(row["loaded"]) || !sameStrings(stringsOf(row["nodes"]), []string{"spark01", "spark02"}) {
		t.Fatalf("removed model must keep its recorded occupancy: %v", row)
	}
	if row["desired"] != nil {
		t.Fatalf("removed model must have a null desired placement: %v", row)
	}
	// The frozen snapshot policy applies when no current policy exists.
	if !boolean(row["keep_loaded"]) {
		t.Fatalf("frozen keep_loaded must survive config removal: %v", row)
	}
}

func TestNativeOwnershipPlacementEditAndCurrentPolicyWin(t *testing.T) {
	rt, _ := inventoryEnv(t, true)
	rec := inventoryBegin(t, rt)
	// The current policy is keep_loaded=false and the placement was edited
	// to a single node: desired follows the edit, occupancy follows the
	// frozen record, and the current keep_loaded wins over the frozen one.
	c := rt.config()
	m := obj(obj(c["models"])["glm"])
	m["keep_loaded"] = false
	m["run_on"] = "spark01"
	delete(obj(m["settings"]), "spark02")
	obj(m["settings"])["spark01"] = obj(obj(m["settings"])["both"])
	rt.configure(c)
	out, err := rt.nativeOwnership(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	row := obj(out["glm"])
	if str(row["desired"]) != "spark01" || boolean(row["keep_loaded"]) {
		t.Fatalf("current policy and placement must win: %v", row)
	}
	if !boolean(row["loaded"]) || !sameStrings(stringsOf(row["nodes"]), []string{"spark01", "spark02"}) {
		t.Fatalf("edited placement must not free the recorded nodes: %v", row)
	}
	_ = rec
}

func TestNativeOwnershipCorruptRecordFails(t *testing.T) {
	rt, root := inventoryEnv(t, true)
	if err := os.MkdirAll(filepath.Join(root, "controller/lifecycle"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "controller/lifecycle/glm.json"), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := rt.nativeOwnership(context.Background())
	if err == nil {
		t.Fatal("corrupt record must error, never report a free inventory")
	}
}

func TestNativeOwnershipTruncatedRecordFails(t *testing.T) {
	rt, root := inventoryEnv(t, true)
	rec := inventoryBegin(t, rt)
	path := filepath.Join(root, "controller/lifecycle/glm.json")
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, full[:len(full)/2], 0600); err != nil {
		t.Fatal(err)
	}
	_ = rec
	if _, err := rt.nativeOwnership(context.Background()); err == nil {
		t.Fatal("truncated record must error")
	}
}

func TestNativeOwnershipSymlinkRecordFails(t *testing.T) {
	rt, root := inventoryEnv(t, true)
	dir := filepath.Join(root, "controller/lifecycle")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "elsewhere.json")
	if err := os.WriteFile(real, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "glm.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.nativeOwnership(context.Background()); err == nil {
		t.Fatal("symlinked record must error")
	}
}

func TestNativeOwnershipInvalidRecordNameFails(t *testing.T) {
	rt, root := inventoryEnv(t, true)
	dir := filepath.Join(root, "controller/lifecycle")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad name.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.nativeOwnership(context.Background()); err == nil {
		t.Fatal("invalid record filename must error")
	}
}

func TestNativeOwnershipNameMismatchFails(t *testing.T) {
	rt, root := inventoryEnv(t, true)
	rec := inventoryBegin(t, rt)
	// The record content names a different model than its file: strict
	// reading must refuse the mismatch even though the file name itself
	// is a valid canonical name.
	bad := copyObject(rec)
	bad["name"] = "other"
	if err := writeObject(filepath.Join(root, "controller/lifecycle/glm.json"), bad); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.nativeOwnership(context.Background()); err == nil {
		t.Fatal("record whose name does not match its file must error")
	}
}

func TestNativeOwnershipMissingConfigSnapshotFails(t *testing.T) {
	rt, root := inventoryEnv(t, true)
	rec := inventoryBegin(t, rt)
	delete(rec, "config")
	if err := writeObject(filepath.Join(root, "controller/lifecycle/glm.json"), rec); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.nativeOwnership(context.Background()); err == nil {
		t.Fatal("record without a config snapshot must error")
	}
}

func TestNativeOwnershipRootMismatchFails(t *testing.T) {
	rt, root := inventoryEnv(t, true)
	rec := inventoryBegin(t, rt)
	cfg := obj(rec["config"])
	cfg["root"] = "/somewhere/else"
	if err := writeObject(filepath.Join(root, "controller/lifecycle/glm.json"), rec); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.nativeOwnership(context.Background()); err == nil {
		t.Fatal("record written for a different root must error")
	}
}

func TestNativeOwnershipUnknownNodeFails(t *testing.T) {
	rt, root := inventoryEnv(t, true)
	rec := inventoryBegin(t, rt)
	rec["nodes"] = anyStrings([]string{"spark01", "spark999"})
	if err := writeObject(filepath.Join(root, "controller/lifecycle/glm.json"), rec); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.nativeOwnership(context.Background()); err == nil {
		t.Fatal("record holding an unknown node must error")
	}
}

func TestNativeOwnershipCallerMutationCannotChangeFilesOrFutureCalls(t *testing.T) {
	rt, _ := inventoryEnv(t, true)
	inventoryBegin(t, rt)
	first, err := rt.nativeOwnership(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	obj(first["glm"])["nodes"] = []string{"hacked"}
	first["glm"] = nil
	second, err := rt.nativeOwnership(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sameStrings(stringsOf(obj(second["glm"])["nodes"]), []string{"spark01", "spark02"}) {
		t.Fatalf("future calls must not observe caller mutation: %v", second)
	}
}

func TestNativeOwnershipHonorsCanceledContext(t *testing.T) {
	rt, _ := inventoryEnv(t, true)
	inventoryBegin(t, rt)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := rt.nativeOwnership(ctx)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context must be honored, got %v", err)
	}
}
