package sparky

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestUpdateDevicePrivateSignalsRestoreTransaction(t *testing.T) {
	if kind := os.Getenv("SPARKY_UPDATE_SIGNAL_CHILD"); kind != "" {
		mainDirs(t)
		w := newDeviceWorld(t)
		w.install()
		original := updateRun
		updateRun = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
			result, err := original(ctx, args, input)
			node, argv := devSplit(args)
			if node == "spark01" && len(argv) == 5 && argv[1] == "--source" && argv[2] == w.stagePath(node, "bin/sparky") && err == nil {
				signal := syscall.SIGHUP
				if kind == "term" {
					signal = syscall.SIGTERM
				}
				if kind == "interrupt" {
					signal = syscall.SIGINT
				}
				if err := syscall.Kill(os.Getpid(), signal); err != nil {
					t.Fatal(err)
				}
				select {
				case <-ctx.Done():
					return result, ctx.Err()
				case <-time.After(2 * time.Second):
					t.Fatal("signal did not cancel transaction")
				}
			}
			return result, err
		}
		code, out, errout := runMain([]string{"node", "self-update", "--root", w.root, "--payload", "-"}, string(jsonBytes(w.spec)))
		body, err := decode([]byte(out))
		if err != nil || code != 1 || errout != "" || !boolean(obj(body["rollback"])["complete"]) || !boolean(obj(body["fence"])["restored"]) {
			t.Fatalf("signal result %d %s %s %v", code, out, errout, err)
		}
		for _, node := range updateDeviceNodeNames {
			if devInstalledDigest(t, w, node) != devOld() {
				t.Fatal("signal left changed binary")
			}
		}
		fmt.Print(out)
		os.Exit(code)
	}
	for _, kind := range []string{"hup", "term", "interrupt"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUpdateDevicePrivateSignalsRestoreTransaction$")
			command.Env = append(os.Environ(), "SPARKY_UPDATE_SIGNAL_CHILD="+kind, "TMPDIR="+t.TempDir())
			output, err := command.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 1 {
				t.Fatalf("child: %v %s", err, output)
			}
			body, err := decode(output)
			if err != nil || str(body["status"]) != "error" {
				t.Fatalf("invalid signal report: %s %v", output, err)
			}
		})
	}
}

func TestUpdateDeviceProcessDigestIsSingleSHA256(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux /proc executable identity")
	}
	child := exec.Command("/bin/sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/exe", child.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := updateDeviceReadExe(context.Background(), child.Process.Pid)
	if err != nil || digest != sha256Hex(raw) {
		t.Fatalf("digest=%s error=%v", digest, err)
	}
}
