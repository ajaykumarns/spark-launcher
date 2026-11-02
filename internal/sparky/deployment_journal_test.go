package sparky

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// journalFailWrites wraps a worker fake so one operation fails without
// touching the real protocol state underneath. The match may inspect argv
// and the decoded metadata.
func journalFailWrites(fake *journalFake, match func([]string, Object) bool) *journalFake {
	fake.mu.Lock()
	prev := fake.script
	fake.script = func(args []string, input []byte) (CommandResult, error) {
		if meta, err := decode(input); err == nil && match(args, meta) {
			return CommandResult{Code: 1}, nil
		}
		return prev(args, input)
	}
	fake.mu.Unlock()
	return fake
}

// journalWorkerInspect reads the fake spark02 worker's isolated protocol
// state for a head path through the real strict inspect protocol on the
// mapped location.
func journalWorkerInspect(t *testing.T, fake *journalFake, path string) Object {
	t.Helper()
	mapped, ok := journalWorkerPath(fake.worker, fake.root, "spark02", path)
	if !ok {
		t.Fatalf("the path %s is not mappable into the worker tree", path)
	}
	res, err := nodeWriteFile(strings.NewReader(`{"operation":"inspect","path":"` + mapped + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	return obj(res)
}

// journalWorkerState reads the fake worker's protocol state for a path.
func journalWorkerState(t *testing.T, fake *journalFake, path string) string {
	t.Helper()
	res := journalWorkerInspect(t, fake, path)
	if res["exists"] != true {
		return ""
	}
	return string(nodeFilePayload(t, res))
}

// journalWorkerMode reads the fake worker's protocol mode for a path.
func journalWorkerMode(t *testing.T, fake *journalFake, path string) string {
	t.Helper()
	return str(journalWorkerInspect(t, fake, path)["mode"])
}

// journalWorkerSeed writes the fake worker's isolated state directly,
// independent of any journal operation.
func journalWorkerSeed(t *testing.T, fake *journalFake, path string, data []byte, mode os.FileMode) {
	t.Helper()
	mapped, ok := journalWorkerPath(fake.worker, fake.root, "spark02", path)
	if !ok {
		t.Fatalf("the path %s is not mappable into the worker tree", path)
	}
	if err := os.MkdirAll(filepath.Dir(mapped), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mapped, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mapped, mode); err != nil {
		t.Fatal(err)
	}
}

// TestJournalRootUnitFailureRestoresBothNodesAndModes covers a late root
// unit failure: every earlier payload on both nodes is restored or removed
// with its exact previous bytes and mode.
func TestJournalRootUnitFailureRestoresBothNodesAndModes(t *testing.T) {
	root := canonicalTempDir(t)
	fake := journalWorkerFake(t, root)
	existing := filepath.Join(root, "extra", "a.conf")
	if err := os.MkdirAll(filepath.Dir(existing), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("old-a"), 0o640); err != nil {
		t.Fatal(err)
	}
	// The worker holds its own independent copy of the same prior state, so
	// both nodes can be asserted separately after the rollback.
	journalWorkerSeed(t, fake, existing, []byte("old-a"), 0o640)
	fresh := filepath.Join(root, "extra", "b.conf")
	journal := NewDeploymentJournal(deploymentConfig(root), filepath.Join(root, "stage"))
	ctx := context.Background()
	if err := journal.WriteFile(ctx, "spark01", existing, []byte("new-a"), 0o600); err != nil {
		t.Fatalf("local overwrite failed: %v", err)
	}
	if err := journal.WriteFile(ctx, "spark01", fresh, []byte("b"), 0o640); err != nil {
		t.Fatalf("local create failed: %v", err)
	}
	if err := journal.WriteFile(ctx, "spark02", existing, []byte("new-a"), 0o600); err != nil {
		t.Fatalf("worker overwrite failed: %v", err)
	}
	if err := journal.WriteFile(ctx, "spark02", fresh, []byte("b"), 0o640); err != nil {
		t.Fatalf("worker create failed: %v", err)
	}
	// The root unit write on the worker fails with zero protocol effect.
	journalFailWrites(fake, func(args []string, meta Object) bool {
		return str(meta["operation"]) == "write" && strings.HasSuffix(str(meta["path"]), ".service") &&
			strings.Contains(strings.Join(args, " "), "ssh")
	})
	unit := "/etc/systemd/system/sparky-journal-test.service"
	if err := journal.WriteUnit(ctx, "spark01", unit, "[Unit]\nDescription=t\n"); err != nil {
		t.Fatalf("local unit write failed: %v", err)
	}
	if err := journal.WriteUnit(ctx, "spark02", unit, "[Unit]\nDescription=t\n"); err == nil {
		t.Fatal("the failing worker unit write must error")
	}
	if err := journal.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	// Both nodes restored, asserted independently. The head: the overwritten
	// file keeps its exact prior bytes and mode; the created file and the
	// successful local unit are gone.
	got, err := os.ReadFile(existing)
	if err != nil || string(got) != "old-a" {
		t.Fatalf("the overwritten file must be restored: %q %v", got, err)
	}
	if info, err := os.Stat(existing); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("the prior mode must be restored: %v %v", info, err)
	}
	if _, err := os.Lstat(fresh); !os.IsNotExist(err) {
		t.Fatalf("%s must be removed by rollback: %v", fresh, err)
	}
	// The worker, independently: the previously existing copy is restored to
	// its exact prior bytes and mode, and the created file and both unit
	// copies are removed.
	if got := journalWorkerState(t, fake, existing); got != "old-a" {
		t.Fatalf("the worker copy must be restored: %q", got)
	}
	if mode := journalWorkerMode(t, fake, existing); mode != "640" {
		t.Fatalf("the worker prior mode must be restored: %q", mode)
	}
	for _, path := range []string{fresh, unit} {
		if snap := journalWorkerState(t, fake, path); snap != "" {
			t.Fatalf("the worker copy of %s must be removed: %q", path, snap)
		}
	}
}

func nodeFilePayload(t *testing.T, result Object) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(str(result["data_base64"]))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestJournalRemoteRootCommandIsAbsoluteHelper asserts the real root worker
// command: ssh with a shell-joined "sudo -n <absolute helper>" command line,
// never an unexpandable literal environment reference.
func TestJournalRemoteRootCommandIsAbsoluteHelper(t *testing.T) {
	root := canonicalTempDir(t)
	fake := journalWorkerFake(t, root)
	journal := NewDeploymentJournal(deploymentConfig(root), filepath.Join(root, "stage"))
	if err := journal.WriteUnit(context.Background(), "spark02",
		"/etc/systemd/system/sparky-cmd-test.service", "[Unit]\nDescription=t\n"); err != nil {
		t.Fatalf("root worker unit write failed: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.calls) == 0 {
		t.Fatal("no command was captured")
	}
	last := fake.calls[len(fake.calls)-1]
	expected := shellJoin([]string{"sudo", "-n", journalHelper(), "--json", "node", "write-file"})
	if !strings.HasSuffix(last, expected) {
		t.Fatalf("root worker command must be the absolute quoted helper:\n got %s\nwant suffix %s", last, expected)
	}
	if strings.Contains(last, "$HOME") || strings.Contains(last, "~/.local") {
		t.Fatalf("root worker command must be absolute: %s", last)
	}
}

// TestJournalConfigCopyIsolatesNodeMetadata asserts a caller mutation of
// node metadata after construction cannot redirect operations.
func TestJournalConfigCopyIsolatesNodeMetadata(t *testing.T) {
	root := canonicalTempDir(t)
	cfg := deploymentConfig(root)
	journal := NewDeploymentJournal(cfg, filepath.Join(root, "stage"))
	obj(obj(cfg["nodes"])["spark02"])["ssh"] = "redirected-host"
	fake := journalWorkerFake(t, root)
	if err := journal.WriteFile(context.Background(), "spark02",
		filepath.Join(root, "copy-test.conf"), []byte("x"), 0o600); err != nil {
		t.Fatalf("worker write failed: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, call := range fake.calls {
		if strings.Contains(call, "redirected-host") {
			t.Fatalf("the redirected host must never be used: %v", fake.calls)
		}
	}
	if len(fake.calls) == 0 || !strings.Contains(fake.calls[0], "spark02") {
		t.Fatalf("the original host must stay pinned: %v", fake.calls)
	}
	if got := journalWorkerState(t, fake, filepath.Join(root, "copy-test.conf")); got != "x" {
		t.Fatalf("worker state: %q", got)
	}
}

// TestJournalRejectsMalformedInspectBeforeWrite covers strict response
// parsing: a malformed inspect is an error and no write is dispatched.
func TestJournalRejectsMalformedInspectBeforeWrite(t *testing.T) {
	root := canonicalTempDir(t)
	fake := journalWorkerFake(t, root)
	fake.mu.Lock()
	prev := fake.script
	fake.script = func(args []string, input []byte) (CommandResult, error) {
		if meta, err := decode(input); err == nil && str(meta["operation"]) == "inspect" {
			return CommandResult{Code: 0, Out: `{"exists":true,"sha256":"nonsense"}`}, nil
		}
		return prev(args, input)
	}
	fake.mu.Unlock()
	journal := NewDeploymentJournal(deploymentConfig(root), filepath.Join(root, "stage"))
	target := filepath.Join(root, "strict.conf")
	if err := journal.WriteFile(context.Background(), "spark02", target, []byte("x"), 0o600); err == nil {
		t.Fatal("a malformed inspect response must be refused")
	}
	if got := journalWorkerState(t, fake, target); got != "" {
		t.Fatalf("no write may follow a malformed inspect: %q", got)
	}
}

// TestJournalPartialRemoteCommitTransportFailure covers the remote committing
// the write and the transport then failing: the registered undo still
// restores the previous state.
func TestJournalPartialRemoteCommitTransportFailure(t *testing.T) {
	root := canonicalTempDir(t)
	fake := journalWorkerFake(t, root)
	target := filepath.Join(root, "extra", "commit.conf")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("prior"), 0o640); err != nil {
		t.Fatal(err)
	}
	// The worker holds its own independent prior copy that the transport
	// failure later leaves committed under "own" until the undo restores it.
	journalWorkerSeed(t, fake, target, []byte("prior"), 0o640)
	// Let the protocol mutate, then report transport failure for exactly
	// one write: the undo dispatch itself must not be sabotaged.
	fake.mu.Lock()
	prev := fake.script
	failed := 0
	fake.script = func(args []string, input []byte) (CommandResult, error) {
		if meta, derr := decode(input); derr == nil && str(meta["operation"]) == "write" &&
			str(meta["path"]) == target {
			failed++
			if failed == 1 {
				return CommandResult{Code: 255}, nil
			}
		}
		return prev(args, input)
	}
	fake.mu.Unlock()
	journal := NewDeploymentJournal(deploymentConfig(root), filepath.Join(root, "stage"))
	if err := journal.WriteFile(context.Background(), "spark02", target, []byte("own"), 0o600); err == nil {
		t.Fatal("the transport failure must surface")
	}
	// The undo must confirm the restore of the exact prior state.
	if err := journal.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := journalWorkerState(t, fake, target); got != "prior" {
		t.Fatalf("worker copy not restored: %q", got)
	}
}

// TestJournalCanceledContextStillRollsBack asserts that a canceled initiating
// context stops new writes but the recorded undos still run with fresh
// contexts.
func TestJournalCanceledContextStillRollsBack(t *testing.T) {
	root := canonicalTempDir(t)
	journalWorkerFake(t, root)
	journal := NewDeploymentJournal(deploymentConfig(root), filepath.Join(root, "stage"))
	target := filepath.Join(root, "cancel.conf")
	if err := journal.WriteFile(context.Background(), "spark01", target, []byte("own"), 0o600); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := journal.WriteFile(ctx, "spark01", filepath.Join(root, "later.conf"), []byte("x"), 0o600); err == nil {
		t.Fatal("a canceled context must refuse new writes")
	}
	if err := journal.Rollback(); err != nil {
		t.Fatalf("rollback after cancel: %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("the undo must still run after cancellation")
	}
	if _, err := os.Lstat(filepath.Join(root, "later.conf")); !os.IsNotExist(err) {
		t.Fatal("the refused write must have no effect")
	}
}

// TestJournalOneUndoFailureDoesNotSkipOthers covers reverse-order full
// attempts: an external edit on one artifact is preserved and reported, and
// the remaining undos still run.
func TestJournalOneUndoFailureDoesNotSkipOthers(t *testing.T) {
	root := canonicalTempDir(t)
	journalWorkerFake(t, root)
	journal := NewDeploymentJournal(deploymentConfig(root), filepath.Join(root, "stage"))
	ctx := context.Background()
	first := filepath.Join(root, "one.conf")
	second := filepath.Join(root, "two.conf")
	if err := journal.WriteFile(ctx, "spark01", first, []byte("own-1"), 0o600); err != nil {
		t.Fatalf("write one: %v", err)
	}
	if err := journal.WriteFile(ctx, "spark01", second, []byte("own-2"), 0o600); err != nil {
		t.Fatalf("write two: %v", err)
	}
	// Externally edit the first file after the write.
	if err := os.WriteFile(first, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := journal.Rollback(); err == nil || !strings.Contains(err.Error(), "one.conf") {
		t.Fatalf("the external edit must be reported: %v", err)
	}
	got, err := os.ReadFile(first)
	if err != nil || string(got) != "foreign" {
		t.Fatalf("the external edit must be preserved: %q %v", got, err)
	}
	if _, err := os.Lstat(second); !os.IsNotExist(err) {
		t.Fatal("the other undo must still run")
	}
}

// TestJournalExternalEditOfExistingFilePreservedAndReported covers an
// externally edited previously-existing file: preserved, reported, and never
// overwritten with the prior bytes.
func TestJournalExternalEditOfExistingFile(t *testing.T) {
	root := canonicalTempDir(t)
	journalWorkerFake(t, root)
	journal := NewDeploymentJournal(deploymentConfig(root), filepath.Join(root, "stage"))
	target := filepath.Join(root, "edit.conf")
	if err := os.WriteFile(target, []byte("prior"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteFile(context.Background(), "spark01", target, []byte("own"), 0o600); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if err := os.WriteFile(target, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := journal.Rollback(); err == nil || !strings.Contains(err.Error(), "edit.conf") {
		t.Fatalf("the external edit must be reported: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "foreign" {
		t.Fatalf("the foreign content must survive: %q", got)
	}
}

// TestJournalExternalDeletionNotResurrected asserts a previously-existing
// file that was externally deleted stays absent and is reported.
func TestJournalExternalDeletionNotResurrected(t *testing.T) {
	root := canonicalTempDir(t)
	journalWorkerFake(t, root)
	journal := NewDeploymentJournal(deploymentConfig(root), filepath.Join(root, "stage"))
	target := filepath.Join(root, "gone.conf")
	if err := os.WriteFile(target, []byte("prior"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteFile(context.Background(), "spark01", target, []byte("own"), 0o600); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := journal.Rollback(); err == nil || !strings.Contains(err.Error(), "gone.conf") {
		t.Fatalf("the external deletion must be reported: %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("absence must be preserved, never resurrected")
	}
}

// TestJournalModeTransitionRestore covers the strict restore rule: a same
// content but changed mode is an external change, while an exact own payload
// and mode is restored to the exact prior bytes and mode.
func TestJournalModeTransitionRestore(t *testing.T) {
	root := canonicalTempDir(t)
	journalWorkerFake(t, root)
	ctx := context.Background()
	// Case a, its own journal: own payload with an externally changed mode
	// is preserved and reported.
	journalA := NewDeploymentJournal(deploymentConfig(root), filepath.Join(root, "stage-a"))
	a := filepath.Join(root, "mode-a.conf")
	if err := os.WriteFile(a, []byte("prior-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := journalA.WriteFile(ctx, "spark01", a, []byte("own-a"), 0o600); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.Chmod(a, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := journalA.Rollback(); err == nil || !strings.Contains(err.Error(), "mode-a.conf") {
		t.Fatalf("an external mode change must be reported: %v", err)
	}
	info, _ := os.Stat(a)
	if info.Mode().Perm() != 0o644 {
		t.Fatal("the external mode must be preserved")
	}
	// Case b, an independent journal: exact own payload and mode is restored
	// to the exact prior bytes and mode, unaffected by case a's outcome.
	journalB := NewDeploymentJournal(deploymentConfig(root), filepath.Join(root, "stage-b"))
	b := filepath.Join(root, "mode-b.conf")
	if err := os.WriteFile(b, []byte("prior-b"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := journalB.WriteFile(ctx, "spark01", b, []byte("own-b"), 0o600); err != nil {
		t.Fatalf("write b: %v", err)
	}
	if err := journalB.Rollback(); err != nil {
		t.Fatalf("rollback b: %v", err)
	}
	got, _ := os.ReadFile(b)
	if string(got) != "prior-b" {
		t.Fatalf("restore bytes: %q", got)
	}
	if info, _ := os.Stat(b); info.Mode().Perm() != 0o640 {
		t.Fatal("restore mode")
	}
}

// TestJournalPerCommandDeadline asserts every dispatched command is bounded:
// the actual context deadline captured inside the fake transport is checked,
// so an inherited shorter deadline wins for every command of the bounded
// write, and the 60 second bound applies otherwise. Each spark02 write
// dispatches exactly one inspect and one write command; the stub answers
// each with the operation's real protocol shape, never a blanket response.
func TestJournalPerCommandDeadline(t *testing.T) {
	root := canonicalTempDir(t)
	var mu sync.Mutex
	var deadlines []time.Time
	prev := deploymentRun
	deploymentRun = func(ctx context.Context, _ []string, input []byte) (CommandResult, error) {
		deadline := time.Time{}
		if d, ok := ctx.Deadline(); ok {
			deadline = d
		}
		mu.Lock()
		deadlines = append(deadlines, deadline)
		mu.Unlock()
		meta, derr := decode(input)
		if derr != nil {
			return CommandResult{Code: 2}, nil
		}
		switch str(meta["operation"]) {
		case "inspect":
			return CommandResult{Code: 0, Out: `{"exists":false}`}, nil
		case "write":
			raw, berr := base64.StdEncoding.DecodeString(str(meta["data_base64"]))
			if berr != nil {
				return CommandResult{Code: 1}, nil
			}
			return CommandResult{Code: 0, Out: `{"status":"written","sha256":"` + sha256Hex(raw) + `"}`}, nil
		}
		return CommandResult{Code: 2}, nil
	}
	t.Cleanup(func() { deploymentRun = prev })
	journal := NewDeploymentJournal(deploymentConfig(root), filepath.Join(root, "stage"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := journal.WriteFile(ctx, "spark02", filepath.Join(root, "d1.conf"), []byte("x"), 0o600); err != nil {
		t.Fatalf("the bounded write must succeed: %v", err)
	}
	if err := journal.WriteFile(context.Background(), "spark02", filepath.Join(root, "d2.conf"), []byte("x"), 0o600); err != nil {
		t.Fatalf("the bounded write must succeed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	// Two writes, each with one inspect and one write command: four bounded
	// dispatches, matching the real transaction flow.
	if len(deadlines) != 4 {
		t.Fatalf("every dispatch must be bounded: %d", len(deadlines))
	}
	now := time.Now()
	for i, deadline := range deadlines {
		if deadline.IsZero() {
			t.Fatalf("command %d ran without any deadline", i)
		}
		d := deadline.Sub(now)
		if i < 2 {
			if d <= 0 || d > 2*time.Second {
				t.Fatalf("the inherited shorter deadline must be honored on command %d: %v", i, d)
			}
			continue
		}
		if d <= 0 || d > deploymentJournalBound {
			t.Fatalf("the default bound must apply on command %d: %v", i, d)
		}
	}
}

// TestJournalInvalidNodeRefused covers the strict node allowlist.
func TestJournalInvalidNodeRefused(t *testing.T) {
	root := canonicalTempDir(t)
	journal := NewDeploymentJournal(deploymentConfig(root), filepath.Join(root, "stage"))
	if err := journal.WriteFile(context.Background(), "spark03", "/tmp/x", []byte("x"), 0o600); err == nil {
		t.Fatal("an invalid node must be refused")
	}
}
