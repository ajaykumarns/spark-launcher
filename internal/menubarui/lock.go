package menubarui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrAlreadyRunning is returned when another menu bar instance holds the lock.
var ErrAlreadyRunning = errors.New("Sparky menu bar is already running")

// Lock is a single-instance guard backed by an OS advisory lock (flock) on an
// owner-only file. The kernel releases the lock automatically when the
// process exits, so a crash never leaves a stale lock, and the owning process
// keeps the file descriptor open for its whole lifetime. The lock file is
// never unlinked on release: unlinking a live inode would let a second
// process lock a fresh inode while the first still holds the old one.
type Lock struct {
	file *os.File
	path string
}

// AcquireLock takes the per-user single-instance lock. It returns
// ErrAlreadyRunning when another live process holds it.
func AcquireLock() (*Lock, error) {
	return acquireLock(lockPath())
}

func acquireLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrAlreadyRunning
		}
		return nil, err
	}
	// The PID is informational only; exclusion comes from flock.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
	}
	return &Lock{file: f, path: path}, nil
}

// Release drops the advisory lock and closes the descriptor. It leaves the
// lock file in place so the inode identity is stable across instances.
func (l *Lock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}

func lockPath() string {
	if p := os.Getenv("SPARKY_MENUBAR_LOCK"); p != "" {
		return p
	}
	return filepath.Join(stateDir(), "menubar.lock")
}
