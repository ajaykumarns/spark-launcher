// Command sparky-install installs the compiled Sparky binary onto a target
// path, replacing an old Python zipapp only after the legacy processes have
// been drained and stopped, or when the operator passes an explicit override.
package main

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

const (
	maxSourceSize = 1 << 30 // 1 GiB
	backupDirMode = 0o700
	backupMode    = 0o700
	metaMode      = 0o600
	binMode       = 0o755
)

// preRenameHook and postCopyHook are nil in production. Tests inject them to
// mutate the source or target at exactly the race windows the checks guard.
var (
	preRenameHook func(target string)
	postCopyHook  func(source string)
	// preOpenHook runs inside stageSource between the initial Lstat and the
	// open of the source; nil in production, used by tests to swap the path
	// into a symlink exactly at that window.
	preOpenHook func(source string)
)

type result struct {
	Installed string `json:"installed"`
	Backup    string `json:"backup"`
	SHA256    string `json:"sha256"`
	Format    string `json:"format"`
}

type installError struct {
	msg string
}

func (e *installError) Error() string { return e.msg }

const usageJSON = `{"error":"usage: sparky-install --source BIN [--target PATH] [--legacy-processes-stopped]"}`

// detectFormat validates that the file at path is an executable binary of a
// type and architecture matching the current GOOS/GOARCH. It accepts only
// real executables: Mach-O MH_EXECUTE and ELF ET_EXEC or ET_DYN with a
// PT_INTERP segment (PIE). Relocatable objects, core files, and plain DSOs
// are rejected. No execution of the artifact happens here.
func detectFormat(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	switch runtime.GOOS {
	case "darwin":
		machoFile, err := macho.NewFile(f)
		if err != nil {
			return "", &installError{msg: "source is not a valid Mach-O binary for this platform: " + err.Error()}
		}
		if machoFile.Type != macho.TypeExec {
			return "", &installError{msg: fmt.Sprintf("source Mach-O type 0x%x is not an executable", uint32(machoFile.Type))}
		}
		want := map[string]macho.Cpu{"amd64": macho.CpuAmd64, "arm64": macho.CpuArm64}[runtime.GOARCH]
		if want == 0 {
			return "", &installError{msg: "unsupported GOARCH: " + runtime.GOARCH}
		}
		if machoFile.Cpu != want {
			return "", &installError{msg: fmt.Sprintf("source Mach-O architecture 0x%x does not match %s", uint32(machoFile.Cpu), runtime.GOARCH)}
		}
		return "macho", nil
	case "linux":
		elfFile, err := elf.NewFile(f)
		if err != nil {
			return "", &installError{msg: "source is not a valid ELF binary for this platform: " + err.Error()}
		}
		switch elfFile.Type {
		case elf.ET_EXEC:
			// Fine: a static or dynamically linked executable.
		case elf.ET_DYN:
			// Only a PIE (with an interpreter) qualifies, not a shared
			// library or relocatable object.
			hasInterp := false
			for _, prog := range elfFile.Progs {
				if prog.Type == elf.PT_INTERP {
					hasInterp = true
					break
				}
			}
			if !hasInterp {
				return "", &installError{msg: "source ELF is a shared object, not an executable or PIE"}
			}
		default:
			return "", &installError{msg: fmt.Sprintf("source ELF type 0x%x is not an executable", uint16(elfFile.Type))}
		}
		if elfFile.Machine.String() != archELF(runtime.GOARCH) {
			return "", &installError{msg: "source ELF architecture " + elfFile.Machine.String() + " does not match " + runtime.GOARCH}
		}
		return "elf", nil
	default:
		return "", &installError{msg: "unsupported platform: " + runtime.GOOS}
	}
}

func archELF(goarch string) string {
	switch goarch {
	case "arm64":
		return "EM_AARCH64"
	case "amd64":
		return "EM_X86_64"
	default:
		return goarch
	}
}

// isZipappOrScript reports whether the target's original bytes look like a
// Python zipapp or a #!python script, which a running legacy controller may
// still have open with cached module offsets.
func isZipappOrScript(head []byte) bool {
	if len(head) >= 4 && string(head[:2]) == "PK" {
		return true
	}
	return strings.Contains(string(head[:min(len(head), 256)]), "python")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// statIdentity captures the full identity of the existing target — metadata
// plus a content digest (or, for symlinks, the referent name) — so a
// concurrent change can be detected just before the final rename.
type statIdentity struct {
	mode   os.FileMode
	size   int64
	inode  uint64
	link   string
	digest string
}

func (a statIdentity) equal(b statIdentity) bool {
	return a.mode == b.mode && a.size == b.size && a.inode == b.inode &&
		a.link == b.link && a.digest == b.digest
}

// identityOf captures the target identity. Symlinks are described by their
// referent name; regular files by their content digest.
func identityOf(path string) (statIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return statIdentity{}, err
	}
	id := statIdentity{mode: info.Mode(), size: info.Size()}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		id.inode = stat.Ino
	}
	if id.mode&os.ModeSymlink != 0 {
		link, err := os.Readlink(path)
		if err != nil {
			return statIdentity{}, err
		}
		id.link = link
		return id, nil
	}
	if !info.Mode().IsRegular() {
		return id, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return statIdentity{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return statIdentity{}, err
	}
	id.digest = hex.EncodeToString(h.Sum(nil))
	return id, nil
}

// backupDir ensures the private backup directory next to the target exists as
// a real directory with mode 0700. An existing symlink or non-directory at
// that path is rejected; an existing directory is tightened to 0700.
func backupDir(target string) (string, error) {
	dir := filepath.Join(filepath.Dir(target), ".sparky-backups")
	info, err := os.Lstat(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}
		if err := os.Mkdir(dir, backupDirMode); err != nil {
			return "", err
		}
		info, err = os.Lstat(dir)
		if err != nil {
			return "", err
		}
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", &installError{msg: "backup path exists but is not a private directory: " + dir}
	}
	if err := os.Chmod(dir, backupDirMode); err != nil {
		return "", err
	}
	return dir, nil
}

// backupOriginal writes a unique, exclusive private backup. Regular targets
// are backed up byte for byte (0700); symlink targets keep a 0600 metadata
// record of the original referent. Backups contain only the previous
// executable or link name, never credentials or session data. The backup is
// read from the same original content captured in identityOf's first pass;
// on any failure the partial backup is removed and nothing is reported.
func backupOriginal(target string, originalBytes []byte, isSymlink bool) (string, error) {
	dir, err := backupDir(target)
	if err != nil {
		return "", err
	}
	var (
		f    *os.File
		err2 error
	)
	if isSymlink {
		f, err2 = os.CreateTemp(dir, "sparky-*.link")
	} else {
		f, err2 = os.CreateTemp(dir, "sparky-*.bak")
	}
	if err2 != nil {
		return "", err2
	}
	name := f.Name()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Chmod(metaMode); err != nil {
		return fail(err)
	}
	if isSymlink {
		link, err := os.Readlink(target)
		if err != nil {
			return fail(err)
		}
		if _, err := f.Write([]byte("link\t" + link + "\n")); err != nil {
			return fail(err)
		}
	} else {
		if _, err := f.Write(originalBytes); err != nil {
			return fail(err)
		}
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	if !isSymlink {
		if err := os.Chmod(name, backupMode); err != nil {
			os.Remove(name)
			return "", err
		}
	}
	return name, nil
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sparky-install", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // no flag-library chatter: one safe JSON error
	fs.Usage = func() {}     // emitted explicitly below, exactly once
	source := fs.String("source", "", "compiled Sparky binary to install")
	target := fs.String("target", filepath.Join(homeDir(), ".local", "bin", "sparky"), "install path")
	legacyStopped := fs.Bool("legacy-processes-stopped", false, "confirm the legacy Python controller has been drained and stopped; required to replace an old Python zipapp")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stdout, usageJSON)
			return 0
		}
		fmt.Fprintln(stderr, usageJSON)
		return 2
	}
	if fs.NArg() != 0 || *source == "" {
		fmt.Fprintln(stderr, usageJSON)
		return 2
	}

	res, err := install(*source, *target, *legacyStopped)
	if err != nil {
		payload, _ := json.Marshal(map[string]string{"error": err.Error()})
		fmt.Fprintf(stderr, "%s\n", payload)
		return 1
	}
	payload, _ := json.Marshal(res)
	fmt.Fprintf(stdout, "%s\n", payload)
	return 0
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}

func install(source, target string, legacyStopped bool) (*result, error) {
	sourcePath, err := filepath.Abs(source)
	if err != nil {
		return nil, err
	}
	targetPath, err := filepath.Abs(target)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), binMode); err != nil {
		return nil, err
	}

	// Capture the original target identity before staging. Errors other than
	// a genuinely absent target abort; permission problems are not treated
	// as absence.
	var original statIdentity
	var originalBytes []byte
	hadTarget := false
	wasSymlink := false
	_, err = os.Lstat(targetPath)
	if err == nil {
		hadTarget = true
		original, err = identityOf(targetPath)
		if err != nil {
			return nil, err
		}
		wasSymlink = original.mode&os.ModeSymlink != 0
		if !wasSymlink && !original.mode.IsRegular() {
			return nil, &installError{msg: "refusing to replace non-regular target at " + targetPath + ": only a regular file or symlink may be installed over"}
		}
		if !wasSymlink {
			// Read the original content once; it feeds both the zipapp
			// check and the backup, so they describe the same snapshot.
			data, err := os.ReadFile(targetPath)
			if err != nil {
				return nil, err
			}
			originalBytes = data
			if isZipappOrScript(data) && !legacyStopped {
				return nil, &installError{msg: "target is an old Python zipapp or script. Drain and stop the legacy Python controller first, then rerun with --legacy-processes-stopped to confirm. Running processes keep lazy-import offsets that replacement would break."}
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	var backup string
	if hadTarget {
		backup, err = backupOriginal(targetPath, originalBytes, wasSymlink)
		if err != nil {
			return nil, err
		}
	}

	// Stage the source into the destination directory. The copy validates
	// the actual staged bytes afterwards, so what is checked is exactly what
	// is renamed.
	stage, err := os.CreateTemp(filepath.Dir(targetPath), ".sparky-install-*")
	if err != nil {
		return nil, err
	}
	stageName := stage.Name()
	stage.Close()
	defer os.Remove(stageName)

	sum, err := stageSource(sourcePath, stageName)
	if err != nil {
		return nil, err
	}

	// Validate the staged artifact itself: format, type, and architecture.
	format, err := detectFormat(stageName)
	if err != nil {
		return nil, err
	}

	if preRenameHook != nil {
		preRenameHook(targetPath)
	}

	// Re-check the target identity right before the atomic rename. A target
	// that was absent must still be absent; a concurrent creator must not be
	// overwritten.
	if hadTarget {
		current, err := identityOf(targetPath)
		if err != nil {
			return nil, &installError{msg: "target changed during installation: " + err.Error()}
		}
		if !current.equal(original) {
			return nil, &installError{msg: "Sparky changed during installation. Retry after the other update finishes."}
		}
	} else if _, err := os.Lstat(targetPath); err == nil {
		return nil, &installError{msg: "Sparky was created during installation. Retry after the other update finishes."}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	if err := os.Rename(stageName, targetPath); err != nil {
		return nil, err
	}

	// The artifact is installed at this point. Durability issues are
	// reported as needing attention, not as an untouched original.
	if err := syncDir(filepath.Dir(targetPath)); err != nil {
		return &result{
			Installed: targetPath,
			Backup:    backup,
			SHA256:    sum,
			Format:    format,
		}, &installError{msg: "installed, but durability could not be confirmed and needs attention: " + err.Error()}
	}

	return &result{
		Installed: targetPath,
		Backup:    backup,
		SHA256:    sum,
		Format:    format,
	}, nil
}

// syncDir makes a directory entry durable. Platforms that do not support
// directory fsync return an accepted errno and are not an error.
func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		var errno syscall.Errno
		if errors.As(err, &errno) {
			switch errno {
			case syscall.EINVAL, syscall.ENOTSUP:
				return nil // documented: directory sync unsupported on this filesystem
			}
			// EBADF, EPERM and anything else are real errors and propagate.
		}
		return err
	}
	return nil
}

// stageSource copies the source into the staging file. The source is opened
// with O_NOFOLLOW semantics (an explicit Lstat plus fd re-check), must be a
// regular file, and is copied with a hard byte bound so a source that grows
// beyond the limit during the copy fails instead of exhausting the disk.
// The digest is computed over the staged bytes and reported as the result.
func stageSource(source, stageName string) (string, error) {
	srcInfo, err := os.Lstat(source)
	if err != nil {
		return "", err
	}
	if srcInfo.Mode()&os.ModeSymlink != 0 || !srcInfo.Mode().IsRegular() {
		return "", &installError{msg: "source must be a regular file, not a symlink or device"}
	}
	var initialDev uint64
	if stat, ok := srcInfo.Sys().(*syscall.Stat_t); ok {
		initialDev = uint64(stat.Dev)
	}
	if preOpenHook != nil {
		preOpenHook(source)
	}
	src, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer src.Close()
	fdInfo, err := src.Stat()
	if err != nil {
		return "", err
	}
	if !fdInfo.Mode().IsRegular() {
		return "", &installError{msg: "source must be a regular file, not a symlink or device"}
	}
	// The open descriptor must refer to the exact inode the path named at
	// the initial Lstat; a path swapped to a different file (e.g. through a
	// symlink race) between the two calls would otherwise be staged.
	if stat, ok := fdInfo.Sys().(*syscall.Stat_t); ok {
		if uint64(stat.Dev) != initialDev || stat.Ino != srcStatIno(srcInfo) {
			return "", &installError{msg: "source path changed between check and open"}
		}
	}
	if fdInfo.Size() > maxSourceSize {
		return "", &installError{msg: "source is larger than the 1 GiB limit"}
	}

	dst, err := os.OpenFile(stageName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, binMode)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	// Copy at most maxSourceSize+1 bytes: an over-limit source fails here
	// even if it grows after the initial stat.
	n, err := io.Copy(dst, io.TeeReader(io.LimitReader(src, maxSourceSize+1), h))
	if err != nil {
		dst.Close()
		return "", err
	}
	if n > maxSourceSize {
		dst.Close()
		return "", &installError{msg: "source is larger than the 1 GiB limit"}
	}
	if err := dst.Chmod(binMode); err != nil {
		dst.Close()
		return "", err
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		return "", err
	}
	if err := dst.Close(); err != nil {
		return "", err
	}

	// Detect a source that was rewritten after the initial stat: the open
	// descriptor still refers to the original inode, so a replaced file
	// would have left us staging stale bytes that no longer match the path.
	if postCopyHook != nil {
		postCopyHook(source)
	}
	finalInfo, err := os.Lstat(source)
	if err != nil {
		return "", err
	}
	changed := finalInfo.Mode() != fdInfo.Mode() || finalInfo.Size() != fdInfo.Size()
	if !changed {
		if stat, ok := fdInfo.Sys().(*syscall.Stat_t); ok {
			if stat2, ok := finalInfo.Sys().(*syscall.Stat_t); ok {
				changed = stat2.Ino != stat.Ino
			}
		}
	}
	if changed {
		return "", &installError{msg: "source changed during installation. Retry after the other update finishes."}
	}
	// Even a same-inode, same-size in-place rewrite must fail: re-read the
	// source from its path and compare the digest against the staged bytes.
	reopen, err := os.Open(source)
	if err != nil {
		return "", err
	}
	reopenInfo, err := reopen.Stat()
	if err != nil {
		reopen.Close()
		return "", err
	}
	if !reopenInfo.Mode().IsRegular() || srcStatIno(reopenInfo) != srcStatIno(fdInfo) {
		reopen.Close()
		return "", &installError{msg: "source changed during installation. Retry after the other update finishes."}
	}
	reopenDigest := sha256.New()
	if _, err := io.Copy(reopenDigest, io.LimitReader(reopen, maxSourceSize+1)); err != nil {
		reopen.Close()
		return "", err
	}
	reopen.Close()
	if !bytes.Equal(reopenDigest.Sum(nil), h.Sum(nil)) {
		return "", &installError{msg: "source changed during installation. Retry after the other update finishes."}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func srcStatIno(info os.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Ino
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
