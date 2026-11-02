package menubarui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrCLINotFound is returned when no usable absolute Sparky CLI is available.
var ErrCLINotFound = errors.New("sparky CLI not found; install Sparky or set an absolute override")

// CLIOptions describes where to look for the bundled CLI.
type CLIOptions struct {
	// Override is an explicitly configured absolute CLI path. Never resolved
	// through PATH. A saved preference is a soft override (Explicit=false) that
	// is preferred after the bundle; a --cli flag is Explicit=true and is
	// checked first, failing closed.
	Override string
	// Explicit marks Override as a --cli flag: it takes precedence over the
	// bundle and fails closed when invalid.
	Explicit bool
	// BundleDir is the .app Resources directory; the bundled CLI is preferred.
	BundleDir string
}

// ResolveCLI returns the absolute path of the Sparky CLI to run, or an error.
// A GUI process does not inherit the user's shell PATH, so PATH is never
// consulted.
//
// An explicit override (Explicit=true, from --cli or a saved preference) fails
// closed: it must be an absolute executable file, and if it is not, resolution
// fails instead of silently falling back to a real CLI. Without an explicit
// override the order is bundle, then existing user config binaries, then known
// install locations.
func ResolveCLI(opts CLIOptions) (string, error) {
	if opts.Explicit && opts.Override == "" {
		return "", ErrCLINotFound
	}
	if opts.Override != "" {
		if !filepath.IsAbs(opts.Override) {
			return "", ErrOverrideNotAbsolute
		}
		p, ok := usableExecutable(opts.Override)
		if !ok {
			return "", ErrOverrideNotUsable
		}
		if opts.Explicit {
			return p, nil
		}
	}
	candidates := []string{}
	if opts.BundleDir != "" {
		candidates = append(candidates, filepath.Join(opts.BundleDir, "sparky"))
	}
	if opts.Override != "" {
		candidates = append(candidates, opts.Override)
	}
	if bin := configCLI(); bin != "" {
		candidates = append(candidates, bin)
	}
	candidates = append(candidates, installLocations()...)
	for _, c := range candidates {
		if p, ok := usableExecutable(c); ok {
			return p, nil
		}
	}
	return "", ErrCLINotFound
}

// ErrOverrideNotAbsolute and ErrOverrideNotUsable are fail-closed override
// errors for a developer --cli path.
var (
	ErrOverrideNotAbsolute = errors.New("--cli must be an absolute path")
	ErrOverrideNotUsable   = errors.New("--cli is not an executable file")
)

// installLocations returns candidate CLI paths from conventional directories.
// SPARKY_MENUBAR_INSTALL_DIRS (a path list) isolates tests and developer use
// from real installs on the machine.
var installLocations = func() []string {
	dirs := []string{}
	if override := os.Getenv("SPARKY_MENUBAR_INSTALL_DIRS"); override != "" {
		dirs = filepath.SplitList(override)
	} else {
		dirs = []string{
			filepath.Join(defaultHome(), ".local/bin"),
			"/usr/local/bin",
			"/opt/homebrew/bin",
		}
	}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, filepath.Join(d, "sparky"))
	}
	return out
}

// usableExecutable reports whether path is an absolute executable regular file.
func usableExecutable(path string) (string, bool) {
	if path == "" || !filepath.IsAbs(path) {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return "", false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	return abs, true
}

// configCLI reads the existing user client config's absolute "sparky" binary
// override, if any. A missing or malformed file is simply ignored.
func configCLI() string {
	raw, err := os.ReadFile(configPath())
	if err != nil {
		return ""
	}
	var cfg struct {
		Binaries map[string]string `json:"binaries"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return ""
	}
	bin := strings.TrimSpace(cfg.Binaries["sparky"])
	if bin == "" || !filepath.IsAbs(bin) {
		return ""
	}
	return bin
}

func defaultHome() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return ""
}

// configPath mirrors internal/sparky's client config location and honors
// SPARKY_CONFIG so the app uses the same existing user configuration.
func configPath() string {
	if p := os.Getenv("SPARKY_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(defaultHome(), ".config/sparky/client.json")
}

// stateDir mirrors internal/sparky's state location and honors SPARKY_STATE_DIR.
func stateDir() string {
	if p := os.Getenv("SPARKY_STATE_DIR"); p != "" {
		return p
	}
	return filepath.Join(defaultHome(), ".local/state/sparky")
}
