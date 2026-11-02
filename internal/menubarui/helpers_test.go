package menubarui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeExec(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveCLIPrefersBundle(t *testing.T) {
	dataDir := t.TempDir()
	bundle := filepath.Join(dataDir, "Resources")
	override := filepath.Join(dataDir, "override-sparky")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	bundled := writeExec(t, bundle, "sparky")
	writeExec(t, dataDir, "override-sparky")

	got, err := ResolveCLI(CLIOptions{Override: override, BundleDir: bundle})
	if err != nil {
		t.Fatal(err)
	}
	if got != bundled {
		t.Fatalf("want bundled %q, got %q", bundled, got)
	}
}

func TestResolveCLIOverride(t *testing.T) {
	dataDir := t.TempDir()
	override := writeExec(t, dataDir, "sparky")
	got, err := ResolveCLI(CLIOptions{Override: override})
	if err != nil {
		t.Fatal(err)
	}
	if got != override {
		t.Fatalf("want %q, got %q", override, got)
	}
}

func TestResolveCLIExplicitOverrideBeatsBundle(t *testing.T) {
	dataDir := t.TempDir()
	bundle := filepath.Join(dataDir, "Resources")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExec(t, bundle, "sparky")
	override := writeExec(t, dataDir, "override-sparky")
	got, err := ResolveCLI(CLIOptions{Override: override, Explicit: true, BundleDir: bundle})
	if err != nil {
		t.Fatal(err)
	}
	if got != override {
		t.Fatalf("explicit --cli must beat bundle: got %q", got)
	}
}

func TestResolveCLIExplicitOverrideFailsClosed(t *testing.T) {
	dataDir := t.TempDir()
	// A real fallback install exists, but an invalid explicit override must not
	// fall back to it.
	fallback := writeExec(t, dataDir, "sparky")
	t.Setenv("SPARKY_MENUBAR_INSTALL_DIRS", dataDir)
	bad := filepath.Join(dataDir, "not-exec")
	if err := os.WriteFile(bad, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveCLI(CLIOptions{Override: bad, Explicit: true}); err == nil {
		t.Fatal("invalid explicit override must fail closed")
	}
	if _, err := ResolveCLI(CLIOptions{Override: filepath.Join(dataDir, "missing"), Explicit: true}); err == nil {
		t.Fatal("missing explicit override must fail closed")
	}
	if _, err := ResolveCLI(CLIOptions{Explicit: true}); err == nil {
		t.Fatal("--cli without a path must fail")
	}
	_ = fallback
}

func TestResolveCLIRejectsRelativeOverride(t *testing.T) {
	_, err := ResolveCLI(CLIOptions{Override: "sparky"})
	if err == nil {
		t.Fatal("expected relative override error")
	}
}

func TestResolveCLINotFound(t *testing.T) {
	t.Setenv("SPARKY_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("SPARKY_MENUBAR_INSTALL_DIRS", t.TempDir())
	_, err := ResolveCLI(CLIOptions{})
	if err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestResolveCLIFromConfigBinaries(t *testing.T) {
	dataDir := t.TempDir()
	bin := writeExec(t, dataDir, "sparky")
	cfg := filepath.Join(dataDir, "client.json")
	b, _ := json.Marshal(map[string]any{"binaries": map[string]string{"sparky": bin}})
	if err := os.WriteFile(cfg, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPARKY_CONFIG", cfg)
	t.Setenv("SPARKY_MENUBAR_INSTALL_DIRS", t.TempDir())
	got, err := ResolveCLI(CLIOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got != bin {
		t.Fatalf("want %q, got %q", bin, got)
	}
}

func TestResolveCLIIgnoresNonExecutable(t *testing.T) {
	dataDir := t.TempDir()
	p := filepath.Join(dataDir, "sparky")
	if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPARKY_CONFIG", filepath.Join(dataDir, "missing.json"))
	t.Setenv("SPARKY_MENUBAR_INSTALL_DIRS", t.TempDir())
	if _, err := ResolveCLI(CLIOptions{Override: p}); err == nil {
		t.Fatal("expected non-executable override to fail")
	}
}

func TestPrefsDefaultsOptOut(t *testing.T) {
	t.Setenv("SPARKY_MENUBAR_PREFS", filepath.Join(t.TempDir(), "prefs.json"))
	if LoadPrefs().StartAtLogin {
		t.Fatal("start at login must default off")
	}
}

func TestPrefsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	t.Setenv("SPARKY_MENUBAR_PREFS", path)
	want := Prefs{StartAtLogin: true, ShowTitle: true, CLIOverride: "/opt/sparky"}
	if err := SavePrefs(want); err != nil {
		t.Fatal(err)
	}
	got := LoadPrefs()
	if got != want {
		t.Fatalf("want %+v, got %+v", want, got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("prefs must be owner-only, got %v", info.Mode().Perm())
	}
}

func TestPrefsBrokenFileFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPARKY_MENUBAR_PREFS", path)
	if got := LoadPrefs(); got != DefaultPrefs() {
		t.Fatalf("want defaults, got %+v", got)
	}
}

func TestLockSingleInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "menubar.lock")
	l, err := acquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLock(path); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second lock = %v, want ErrAlreadyRunning", err)
	}
	// Releasing leaves the inode in place; a fresh instance can lock it again.
	l.Release()
	l2, err := acquireLock(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lock file must remain after release: %v", err)
	}
	l2.Release()
}

func TestLockConcurrentProcessExclusion(t *testing.T) {
	if os.Getenv("MENUBAR_LOCK_HELPER") == "1" {
		l, err := acquireLock(os.Getenv("MENUBAR_LOCK_PATH"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		fmt.Println("locked")
		// Hold until the parent kills us (exercises crash release).
		time.Sleep(30 * time.Second)
		l.Release()
		return
	}
	path := filepath.Join(t.TempDir(), "menubar.lock")
	cmd := exec.Command(os.Args[0], "-test.run=TestLockConcurrentProcessExclusion")
	cmd.Env = append(os.Environ(), "MENUBAR_LOCK_HELPER=1", "MENUBAR_LOCK_PATH="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	if _, err := out.Read(buf); err != nil {
		cmd.Process.Kill()
		t.Fatal(err)
	}
	// The helper process holds the lock; this process must be refused.
	if _, err := acquireLock(path); !errors.Is(err, ErrAlreadyRunning) {
		cmd.Process.Kill()
		t.Fatalf("cross-process lock = %v, want ErrAlreadyRunning", err)
	}
	// Kill the helper without releasing: the kernel drops the flock, so a new
	// instance can acquire it (crash release).
	cmd.Process.Kill()
	_ = cmd.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for {
		l, err := acquireLock(path)
		if err == nil {
			l.Release()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock not released after crash: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLoginPlistEscapes(t *testing.T) {
	s := loginPlist("/Apps/A & B/Sparky")
	if !strings.Contains(s, "/Apps/A &amp; B/Sparky") {
		t.Fatalf("plist not escaped: %s", s)
	}
	if !strings.Contains(s, "RunAtLoad") {
		t.Fatal("plist missing RunAtLoad")
	}
}
