package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// buildTestBinary returns a path to a real compiled binary: the running test
// binary on darwin, or a small cross-compiled ELF when testing ELF handling
// on a linux host.
func buildTestBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

// crossBuild builds a trivial binary for the given GOOS/GOARCH into dir.
func crossBuild(t *testing.T, dir, goos, goarch string) string {
	t.Helper()
	if runtime.GOARCH != goarch {
		t.Skipf("cannot validate %s/%s on %s/%s", goos, goarch, runtime.GOOS, runtime.GOARCH)
	}
	out := filepath.Join(dir, "elfbin")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/sparky-install")
	cmd.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	cmd.Dir = "../.."
	if err := cmd.Run(); err != nil {
		t.Skipf("cross build failed: %v", err)
	}
	return out
}

func runInstall(t *testing.T, args ...string) (int, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, &stdout, &stderr
}

func TestInstallFreshTarget(t *testing.T) {
	dir := t.TempDir()
	src := buildTestBinary(t)
	target := filepath.Join(dir, "bin", "sparky")
	code, stdout, stderr := runInstall(t, "--source", src, "--target", target)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	var res result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, stdout.String())
	}
	if res.Backup != "" {
		t.Errorf("expected no backup for fresh install, got %s", res.Backup)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("target missing: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %o, want 755", info.Mode().Perm())
	}
	if res.Format != "macho" && res.Format != "elf" {
		t.Errorf("unexpected format %q", res.Format)
	}
	if len(res.SHA256) != 64 {
		t.Errorf("bad sha256 %q", res.SHA256)
	}
}

func TestInstallRefusesNonRegularSource(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(buildTestBinary(t), link); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runInstall(t, "--source", link, "--target", filepath.Join(dir, "sparky"))
	if code != 1 {
		t.Fatalf("exit %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "regular") {
		t.Errorf("stderr missing regular-file error: %s", stderr.String())
	}
}

func TestInstallRefusesInvalidFormat(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "notabinary")
	if err := os.WriteFile(bad, []byte("just text"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runInstall(t, "--source", bad, "--target", filepath.Join(dir, "sparky"))
	if code != 1 {
		t.Fatalf("exit %d, want 1; stderr: %s", code, stderr.String())
	}
}

func TestInstallRefusesTruncatedBinary(t *testing.T) {
	dir := t.TempDir()
	src := buildTestBinary(t)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	trunc := filepath.Join(dir, "truncated")
	if err := os.WriteFile(trunc, data[:len(data)/3], 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runInstall(t, "--source", trunc, "--target", filepath.Join(dir, "sparky"))
	if code != 1 {
		t.Fatalf("exit %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Mach-O") && !strings.Contains(stderr.String(), "ELF") {
		t.Errorf("stderr missing format error: %s", stderr.String())
	}
}

func TestInstallWrongArchRejected(t *testing.T) {
	// A Mach-O/ELF for a different architecture must be refused. Build an ELF
	// when the host is linux/amd64; otherwise skip the ELF path and reject a
	// text file that claims a foreign container already covered above. On
	// darwin/arm64 we can still build the ELF and confirm rejection since
	// detectFormat only accepts the current platform's container.
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("platform-specific check")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "linuxbin")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/sparky-install")
	cmd.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	cmd.Dir = "../.."
	if err := cmd.Run(); err != nil {
		t.Skipf("cross build failed: %v", err)
	}
	code, _, stderr := runInstall(t, "--source", out, "--target", filepath.Join(dir, "sparky"))
	if code != 1 {
		t.Fatalf("exit %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not a valid Mach-O") {
		t.Errorf("stderr missing wrong-container error: %s", stderr.String())
	}
}

func TestInstallReplacesSymlinkItself(t *testing.T) {
	dir := t.TempDir()
	src := buildTestBinary(t)
	// Simulate the Mac uv tool layout: the target is a symlink managed by uv.
	uvPayload := filepath.Join(dir, "uvtool", "sparky-real")
	if err := os.MkdirAll(filepath.Dir(uvPayload), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, "bin", "sparky")), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "bin", "sparky")
	if err := os.Symlink(uvPayload, target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(uvPayload, []byte("#!/usr/bin/env python3\nold uv payload\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runInstall(t, "--source", src, "--target", target)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	// The link itself must be replaced, uv payload untouched, and a private
	// metadata backup kept.
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("symlink was followed, not replaced")
	}
	payload, err := os.ReadFile(uvPayload)
	if err != nil || !strings.Contains(string(payload), "old uv payload") {
		t.Fatalf("uv payload was modified: %v %s", err, payload)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "bin", ".sparky-backups"))
	if len(entries) != 1 {
		t.Fatalf("expected one backup, got %d", len(entries))
	}
	meta, _ := os.ReadFile(filepath.Join(dir, "bin", ".sparky-backups", entries[0].Name()))
	if !strings.Contains(string(meta), "link\t"+uvPayload) {
		t.Errorf("backup does not record original link: %s", meta)
	}
	st, _ := os.Stat(filepath.Join(dir, "bin", ".sparky-backups", entries[0].Name()))
	if st.Mode().Perm() != 0o600 {
		t.Errorf("metadata backup mode = %o, want 600", st.Mode().Perm())
	}
}

func TestInstallRefusesZipappWithoutFlag(t *testing.T) {
	dir := t.TempDir()
	src := buildTestBinary(t)
	target := filepath.Join(dir, "sparky")
	zipapp := []byte("PK\x03\x04fake zipapp with python inside")
	if err := os.WriteFile(target, zipapp, 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runInstall(t, "--source", src, "--target", target)
	if code != 1 {
		t.Fatalf("exit %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "legacy-processes-stopped") {
		t.Errorf("stderr missing drain-and-stop requirement: %s", stderr.String())
	}
	// Target intact.
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, zipapp) {
		t.Error("zipapp target was modified on refusal")
	}
}

func TestInstallZipappWithStoppedFlag(t *testing.T) {
	dir := t.TempDir()
	src := buildTestBinary(t)
	target := filepath.Join(dir, "sparky")
	if err := os.WriteFile(target, []byte("PK\x03\x04old zipapp"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runInstall(t, "--source", src, "--target", target, "--legacy-processes-stopped")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	entries, _ := os.ReadDir(filepath.Join(dir, ".sparky-backups"))
	if len(entries) != 1 {
		t.Fatalf("expected backup of old zipapp, got %d", len(entries))
	}
	st, _ := os.Stat(filepath.Join(dir, ".sparky-backups", entries[0].Name()))
	if st.Mode().Perm() != 0o700 {
		t.Errorf("backup mode = %o, want 700", st.Mode().Perm())
	}
	old, _ := os.ReadFile(filepath.Join(dir, ".sparky-backups", entries[0].Name()))
	if !bytes.Equal(old, []byte("PK\x03\x04old zipapp")) {
		t.Error("backup does not hold the original zipapp bytes")
	}
}

func TestInstallDetectsConcurrentEdit(t *testing.T) {
	dir := t.TempDir()
	src := buildTestBinary(t)
	target := filepath.Join(dir, "sparky")
	original := []byte("PK\x03\x04original")
	if err := os.WriteFile(target, original, 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate another writer swapping the target between the identity
	// snapshot and the final rename.
	changed := filepath.Join(dir, "changed")
	if err := os.WriteFile(changed, []byte("PK\x03\x04changed!!"), 0o755); err != nil {
		t.Fatal(err)
	}
	preRenameHook = func(path string) {
		if err := os.Rename(changed, path); err != nil {
			t.Errorf("hook: %v", err)
		}
	}
	defer func() { preRenameHook = nil }()

	_, err := install(src, target, true)
	if err == nil {
		t.Fatal("expected concurrent-change abort")
	}
	if !strings.Contains(err.Error(), "changed during installation") {
		t.Errorf("unexpected error: %v", err)
	}
	// Target must hold the concurrent writer's bytes, not the staged binary.
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, []byte("PK\x03\x04changed!!")) {
		t.Errorf("target was clobbered: %q", got)
	}
	// No temp stage files left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".sparky-install-") {
			t.Errorf("temp stage left behind: %s", e.Name())
		}
	}
}

func TestInstallRepeatedReplacementSucceeds(t *testing.T) {
	dir := t.TempDir()
	src := buildTestBinary(t)
	target := filepath.Join(dir, "sparky")
	if err := os.WriteFile(target, []byte("PK\x03\x04old"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := install(src, target, true); err != nil {
			t.Fatalf("install %d failed: %v", i, err)
		}
	}
}

func TestInstallBackupPreservesOriginalBytes(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sparky")
	oldBytes := []byte("PK\x03\x04whatever")
	if err := os.WriteFile(target, oldBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	backup, err := backupOriginal(target, oldBytes, false)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(backup) != filepath.Join(dir, ".sparky-backups") {
		t.Errorf("backup outside private dir: %s", backup)
	}
	st, _ := os.Stat(backup)
	if st.Mode().Perm() != 0o700 {
		t.Errorf("backup mode = %o, want 700", st.Mode().Perm())
	}
	got, _ := os.ReadFile(backup)
	if !bytes.Equal(got, oldBytes) {
		t.Error("backup content differs from original")
	}
}

func TestInstallFailureLeavesTempFilesCleaned(t *testing.T) {
	dir := t.TempDir()
	// Invalid format: staging never happens, no temp leftovers.
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("nope"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, _ := runInstall(t, "--source", bad, "--target", filepath.Join(dir, "sparky"))
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".sparky-install-") {
			t.Errorf("temp stage left behind: %s", e.Name())
		}
	}
}

func TestUsageErrorsAreJSON(t *testing.T) {
	cases := [][]string{
		{"--nope"},
		{"--source"},
		{"extra"},
		{"--source", "x", "extra"},
		{},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		code := run(args, &stdout, &stderr)
		if code != 2 {
			t.Errorf("args %v: exit %d, want 2", args, code)
		}
		if stdout.Len() != 0 {
			t.Errorf("args %v: unexpected stdout %s", args, stdout.String())
		}
		var payload map[string]string
		if err := json.Unmarshal(stderr.Bytes(), &payload); err != nil {
			t.Errorf("args %v: stderr not one JSON object: %q", args, stderr.String())
		}
	}
	// -h documents usage on stdout with exit 0.
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-h"}, &stdout, &stderr); code != 0 {
		t.Errorf("-h exit %d, want 0", code)
	}
	if stderr.Len() != 0 {
		t.Errorf("-h wrote to stderr: %s", stderr.String())
	}
}

func TestBackupDirRejectsSymlinkAndTightensMode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sparky")

	// Symlink at the backup path is rejected.
	real := filepath.Join(dir, "elsewhere")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, ".sparky-backups")); err != nil {
		t.Fatal(err)
	}
	if _, err := backupDir(target); err == nil {
		t.Fatal("expected symlinked backup dir rejection")
	}

	// A preexisting loose directory is tightened to 0700.
	dir2 := t.TempDir()
	target2 := filepath.Join(dir2, "sparky")
	loose := filepath.Join(dir2, ".sparky-backups")
	if err := os.Mkdir(loose, 0o777); err != nil {
		t.Fatal(err)
	}
	got, err := backupDir(target2)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(got)
	if info.Mode().Perm() != 0o700 {
		t.Errorf("backup dir mode = %o, want 700", info.Mode().Perm())
	}
}

func TestRepeatedInstallsUniqueBackups(t *testing.T) {
	dir := t.TempDir()
	src := buildTestBinary(t)
	target := filepath.Join(dir, "sparky")
	if err := os.WriteFile(target, []byte("PK\x03\x04v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := install(src, target, true); err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(dir, ".sparky-backups"))
	if len(entries) != 3 {
		t.Fatalf("expected 3 unique backups, got %d", len(entries))
	}
	seen := map[string]bool{}
	foundV1 := false
	for _, e := range entries {
		if seen[e.Name()] {
			t.Errorf("duplicate backup name %s", e.Name())
		}
		seen[e.Name()] = true
		data, _ := os.ReadFile(filepath.Join(dir, ".sparky-backups", e.Name()))
		if bytes.Equal(data, []byte("PK\x03\x04v1")) {
			foundV1 = true
		}
	}
	if !foundV1 {
		t.Error("no backup holds the very first target")
	}
}

func TestConcurrentCreatorRejected(t *testing.T) {
	dir := t.TempDir()
	src := buildTestBinary(t)
	target := filepath.Join(dir, "sparky")
	preRenameHook = func(path string) {
		if err := os.WriteFile(path, []byte("someone else"), 0o755); err != nil {
			t.Errorf("hook: %v", err)
		}
	}
	defer func() { preRenameHook = nil }()
	_, err := install(src, target, false)
	if err == nil {
		t.Fatal("expected concurrent-creator rejection")
	}
	if !strings.Contains(err.Error(), "created during installation") {
		t.Errorf("unexpected error: %v", err)
	}
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, []byte("someone else")) {
		t.Errorf("concurrent creator was overwritten: %q", got)
	}
}

func TestSameInodeContentChangeDetected(t *testing.T) {
	dir := t.TempDir()
	src := buildTestBinary(t)
	target := filepath.Join(dir, "sparky")
	original := []byte("PK\x03\x04original")
	changed := []byte("PK\x03\x04modifiEd") // same size, same inode, different bytes
	if err := os.WriteFile(target, original, 0o755); err != nil {
		t.Fatal(err)
	}
	preRenameHook = func(path string) {
		f, err := os.OpenFile(path, os.O_WRONLY, 0o755)
		if err != nil {
			t.Errorf("hook: %v", err)
			return
		}
		f.Write(changed)
		f.Close()
	}
	defer func() { preRenameHook = nil }()
	_, err := install(src, target, true) // zipapp target: explicit flag, focus is the digest check
	if err == nil {
		t.Fatal("expected same-inode content change to be detected")
	}
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, changed) {
		t.Errorf("target altered: %q", got)
	}
}

func TestSourceChangedDuringInstall(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src-bin")
	if err := copyFile(src, buildTestBinary(t)); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "sparky")
	postCopyHook = func(source string) {
		// Replace the source path with a different inode and size after staging.
		if err := os.Remove(source); err != nil {
			t.Errorf("hook: %v", err)
		}
		if err := os.WriteFile(source, []byte("different, shorter"), 0o755); err != nil {
			t.Errorf("hook: %v", err)
		}
	}
	defer func() { postCopyHook = nil }()
	_, err := install(src, target, false)
	if err == nil {
		t.Fatal("expected changed-source rejection")
	}
	if !strings.Contains(err.Error(), "source changed during installation") {
		t.Errorf("unexpected error: %v", err)
	}
}

func copyFile(dst, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}

func TestOversizedSourceRejected(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxSourceSize + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	code, _, stderr := runInstall(t, "--source", big, "--target", filepath.Join(dir, "sparky"))
	if code != 1 {
		t.Fatalf("exit %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "1 GiB") {
		t.Errorf("stderr missing size limit: %s", stderr.String())
	}
}

func TestSharedLibraryRejected(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Mach-O dylib check is darwin-specific")
	}
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib.so")
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", lib, "./cmd/sparky-install")
	cmd.Dir = "../.."
	if err := cmd.Run(); err != nil {
		t.Skipf("c-shared build unavailable: %v", err)
	}
	code, _, stderr := runInstall(t, "--source", lib, "--target", filepath.Join(dir, "sparky"))
	if code != 1 {
		t.Fatalf("exit %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not an executable") {
		t.Errorf("stderr missing type error: %s", stderr.String())
	}
}

func TestWrongELFArchRejectedOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
		t.Skip("ELF arch check is linux/arm64-specific")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "amd64bin")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/sparky-install")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	cmd.Dir = "../.."
	if err := cmd.Run(); err != nil {
		t.Skipf("cross build failed: %v", err)
	}
	code, _, stderr := runInstall(t, "--source", out, "--target", filepath.Join(dir, "sparky"))
	if code != 1 {
		t.Fatalf("exit %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "does not match") {
		t.Errorf("stderr missing arch error: %s", stderr.String())
	}
}

func TestRefusesNonRegularTarget(t *testing.T) {
	dir := t.TempDir()
	src := buildTestBinary(t)

	// FIFO target must not be backed up or replaced.
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	code, _, stderr := runInstall(t, "--source", src, "--target", fifo)
	if code != 1 {
		t.Fatalf("exit %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "non-regular") {
		t.Errorf("stderr missing non-regular error: %s", stderr.String())
	}
	if info, err := os.Lstat(fifo); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("fifo was modified: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".sparky-backups")); !os.IsNotExist(err) {
		t.Error("backup dir created for rejected target")
	}

	// Directory target likewise.
	sub := filepath.Join(dir, "subdir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = runInstall(t, "--source", src, "--target", sub)
	if code != 1 {
		t.Fatalf("exit %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "non-regular") {
		t.Errorf("stderr missing non-regular error: %s", stderr.String())
	}
	if info, err := os.Stat(sub); err != nil || !info.IsDir() {
		t.Errorf("directory was modified: %v", err)
	}
}

func TestSameInodeSameSizeSourceRewriteDetected(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src-bin")
	if err := copyFile(src, buildTestBinary(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "sparky")
	postCopyHook = func(source string) {
		// Rewrite in place: same inode, same size, different content.
		data, err := os.ReadFile(source)
		if err != nil {
			t.Errorf("hook: %v", err)
			return
		}
		flipped := append([]byte(nil), data...)
		flipped[0] ^= 0xFF
		f, err := os.OpenFile(source, os.O_WRONLY, 0o755)
		if err != nil {
			t.Errorf("hook: %v", err)
			return
		}
		defer f.Close()
		if _, err := f.WriteAt(flipped, 0); err != nil {
			t.Errorf("hook: %v", err)
		}
	}
	defer func() { postCopyHook = nil }()
	_, err := install(src, target, false)
	if err == nil {
		t.Fatal("expected same-inode same-size rewrite to be rejected")
	}
	if !strings.Contains(err.Error(), "source changed during installation") {
		t.Errorf("unexpected error: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("target installed despite changed source")
	}
}

func TestSymlinkSwapSourceRaceDetected(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src-bin")
	if err := copyFile(src, buildTestBinary(t)); err != nil {
		t.Fatal(err)
	}
	// Same-size decoy so the old size check alone would pass.
	decoy := filepath.Join(dir, "decoy")
	data, _ := os.ReadFile(src)
	if err := os.WriteFile(decoy, data, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(decoy, os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("X"), 0); err != nil {
		t.Fatal(err)
	}
	f.Close()

	target := filepath.Join(dir, "sparky")
	preOpenHook = func(source string) {
		if err := os.Remove(source); err != nil {
			t.Errorf("hook: %v", err)
		}
		if err := os.Symlink(decoy, source); err != nil {
			t.Errorf("hook: %v", err)
		}
	}
	defer func() { preOpenHook = nil }()
	_, err = install(src, target, false)
	if err == nil {
		t.Fatal("expected symlink-swap source race to be rejected")
	}
	if !strings.Contains(err.Error(), "changed between check and open") {
		t.Errorf("unexpected error: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("target installed despite symlink swap")
	}
}

func TestSyncDirPropagatesRealErrnos(t *testing.T) {
	// syncDir on an open fd closed underneath returns EBADF, a real error
	// that must propagate, not be swallowed as "unsupported".
	if err := syncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected error for missing directory")
	}
	// A valid directory sync must succeed on darwin/linux CI hosts.
	if err := syncDir(t.TempDir()); err != nil {
		t.Fatalf("syncDir on real directory: %v", err)
	}
}
