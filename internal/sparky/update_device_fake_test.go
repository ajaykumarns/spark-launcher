package sparky

// Command and file-protocol fakes for the device transaction fixture. The
// command fake understands only the pinned argv shapes the transaction can
// produce: the staged/installed "--version" probes, one staged installer
// invocation, the head controller MainPID read, and the single head
// controller restart. Everything else is recorded as UNEXPECTED and refused.

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// devSplit separates one dispatched argv into (node, argv).
func devSplit(args []string) (string, []string) {
	if len(args) == 7 && args[0] == "ssh" && args[1] == "-o" && args[3] == "-o" && args[5] == "spark02" {
		remote := strings.NewReplacer("'", "", `"`, "").Replace(args[6])
		return args[5], strings.Fields(remote)
	}
	return "spark01", args
}

// command is the updateRun fake.
func (w *deviceWorld) command(_ context.Context, args []string, _ []byte) (CommandResult, error) {
	node, argv := devSplit(args)
	joined := strings.Join(argv, " ")
	switch {
	case len(argv) == 2 && argv[1] == "--version":
		w.record("version", node, argv[0])
		return CommandResult{Out: w.versionLine(node, argv[0]) + "\n"}, nil
	case len(argv) == 5 && argv[1] == "--source" && argv[3] == "--target":
		return w.installer(node, argv)
	case len(argv) == 6 && argv[0] == "systemctl" && argv[1] == "show" && argv[3] == "-p" && argv[4] == "MainPID":
		if argv[2] != updateDeviceControllerUnit {
			w.record("unexpected", node, joined)
			return CommandResult{Code: 1}, nil
		}
		w.record("pid", node, argv[2])
		return CommandResult{Out: strconv.Itoa(devPID) + "\n"}, nil
	case len(argv) == 5 && argv[0] == "sudo" && argv[1] == "-n" && argv[2] == "systemctl" && argv[3] == "restart":
		if argv[4] != updateDeviceControllerUnit || node != "spark01" {
			w.record("unexpected", node, joined)
			return CommandResult{Code: 1}, nil
		}
		if w.restartFail {
			w.record("restart", node, argv[4]+"=failed")
			return CommandResult{Code: 1}, nil
		}
		w.restarts++
		// The restarted controller runs exactly what is on disk now.
		if file := w.lookup("spark01", devTargetPath()); file != nil {
			w.runningSHA = sha256Hex(file.data)
		}
		w.record("restart", node, argv[4])
		if w.afterRestart != nil {
			w.afterRestart()
		}
		return CommandResult{}, nil
	default:
		w.record("unexpected", node, joined)
		return CommandResult{Code: 127}, nil
	}
}

// versionLine answers one exact binary on one node.
func (w *deviceWorld) versionLine(node, binary string) string {
	if binary == devTargetPath() {
		file := w.lookup(node, binary)
		if file == nil {
			return "sparky 0.0.0 (missing)"
		}
		if sha256Hex(file.data) == w.cliSHA {
			return w.stagedVersion
		}
		return w.installedOld
	}
	for _, stage := range []string{str(w.spec["head_stage"]), str(w.spec["worker_stage"])} {
		if binary == filepath.Join(stage, "bin", "sparky") {
			return w.stagedVersion
		}
	}
	return "sparky 0.0.0 (unknown)"
}

// installer is the staged sparky-install fake: it only ever copies a file the
// fixture knows about, records a real backup, and prints the digest it wrote.
func (w *deviceWorld) installer(node string, argv []string) (CommandResult, error) {
	installer, source, target := argv[0], argv[2], argv[4]
	want := filepath.Join(w.stageOf(node), "bin", "sparky-install")
	if installer != want {
		w.record("unexpected", node, "installer "+installer)
		return CommandResult{Code: 1}, nil
	}
	from := w.lookup(node, source)
	if from == nil {
		w.record("unexpected", node, "install missing source "+source)
		return CommandResult{Code: 1}, nil
	}
	backup := ""
	if prior := w.lookup(node, target); prior != nil {
		backup = target + ".bak-" + sha256Hex(prior.data)[:8]
		w.files2(node, backup, prior.data, 0o600)
	}
	if strings.Contains(source, ".bak-") {
		if w.restoreFail == node {
			w.record("restore", node, "failed")
			return CommandResult{Code: 1}, nil
		}
		w.record("restore", node, source+" -> "+target)
	} else {
		if w.installerFail == node {
			w.record("install", node, "failed")
			return CommandResult{Code: 1}, nil
		}
		w.installs = append(w.installs, fmt.Sprintf("%s %s -> %s", node, source, target))
		w.record("install", node, source+" -> "+target)
		if w.afterInstall != nil {
			w.afterInstall(node)
		}
	}
	w.files2(node, target, from.data, 0o755)
	out := fmt.Sprintf(`{"installed":%q,"backup":%q,"sha256":%q,"format":"elf"}`, target, backup, sha256Hex(from.data))
	result := CommandResult{Out: out}
	if w.afterWrite != nil && !strings.Contains(source, ".bak-") {
		w.afterWrite(node, &result)
	}
	return result, nil
}

func (w *deviceWorld) stageOf(node string) string {
	if node == "spark02" {
		return str(w.spec["worker_stage"])
	}
	return str(w.spec["head_stage"])
}

// devAccess is the updateFileAccess fake over the in-memory node files.
type devAccess struct{ w *deviceWorld }

func (a devAccess) inspect(_ context.Context, node, path string) (updateFileSnapshot, error) {
	if a.w.inspectFault != nil {
		if err := a.w.inspectFault(node, path); err != nil {
			return updateFileSnapshot{}, err
		}
	}
	file := a.w.lookup(node, path)
	if file == nil {
		return updateFileSnapshot{}, nil
	}
	return updateFileSnapshot{exists: true, mode: file.mode, sha: sha256Hex(file.data), data: append([]byte(nil), file.data...)}, nil
}

func (a devAccess) backup(_ context.Context, node, source, target, expected string) error {
	file := a.w.lookup(node, source)
	if file == nil || sha256Hex(file.data) != expected {
		return fail("backup source changed")
	}
	if a.w.lookup(node, target) != nil {
		return fail("backup already exists")
	}
	a.w.files2(node, target, append([]byte(nil), file.data...), 0o600)
	return nil
}

func (a devAccess) remove(_ context.Context, node, path, expected string) error {
	file := a.w.lookup(node, path)
	if file == nil {
		return fail("%s is not present on %s", filepath.Base(path), node)
	}
	if sha256Hex(file.data) != expected {
		return fail("%s on %s is not the recorded payload", filepath.Base(path), node)
	}
	a.w.removes = append(a.w.removes, fmt.Sprintf("%s %s@%s", node, path, expected[:8]))
	delete(a.w.files, devKey(node, path))
	return nil
}

// devRuntime is the read-only updateRuntimeSurface fake.
type devRuntime struct{ w *deviceWorld }

func (r devRuntime) legacyActivity(context.Context) (map[string]int, error) {
	out := map[string]int{}
	for k, v := range r.w.activity {
		out[k] = v
	}
	return out, nil
}

func (r devRuntime) nativeOwnership(context.Context) (Object, error) {
	return copyObject(r.w.records), nil
}

func (r devRuntime) snapshot(_ context.Context, _ bool) (Object, error) {
	return copyObject(r.w.snapshot), nil
}
