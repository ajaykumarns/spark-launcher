//go:build darwin && cgo

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"sparky/internal/menubarui"
)

const version = "0.1.0"

// init locks the goroutine that starts the program to the process main OS
// thread. main() runs on that goroutine, so AppKit initialization and
// [NSApp run] happen on the real main thread with a stable thread-local
// Foundation state, as AppKit requires.
func init() {
	runtime.LockOSThread()
}

func main() {
	opts, err := menubarui.ParseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, menubarui.FriendlyError(err))
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, menubarui.Usage)
		os.Exit(2)
	}
	if opts.Help {
		fmt.Println(menubarui.Usage)
		return
	}
	if opts.Version {
		fmt.Println("sparky-menubar " + version)
		return
	}

	// An explicit --cli flag wins and fails closed; a saved preference is a
	// soft override preferred after the bundle. Normal startup prefers the
	// bundled CLI.
	bundleDir := bundleResourcesDir()
	prefs := menubarui.LoadPrefs()
	softOverride := ""
	if opts.CLIOverride != "" {
		softOverride = opts.CLIOverride
	} else {
		softOverride = prefs.CLIOverride
	}
	cli, cliErr := menubarui.ResolveCLI(menubarui.CLIOptions{
		Override:  softOverride,
		Explicit:  opts.CLIOverride != "",
		BundleDir: bundleDir,
	})
	if cliErr != nil {
		// Keep the app usable enough to show diagnostics and Quit even when
		// the CLI is missing or an override is invalid.
		cli = ""
	}

	lock, lockErr := menubarui.AcquireLock()
	if lockErr != nil {
		fmt.Fprintln(os.Stderr, menubarui.FriendlyError(lockErr))
		os.Exit(1)
	}

	app := &menuApp{
		version: version,
		cli:     cli,
		cliErr:  cliErr,
		prefs:   prefs,
		lock:    lock,
	}
	if cli != "" {
		app.ctrl = menubarui.NewController(cli)
		app.poller = menubarui.NewPoller(app.ctrl, 60*time.Second, 30*time.Second)
	}
	runApp(app)
}

// bundleResourcesDir returns an absolute Resources directory for a bundled
// app, or "" when running the binary directly from dist.
func bundleResourcesDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return ""
	}
	// .../Sparky.app/Contents/MacOS/sparky-menubar -> .../Contents/Resources
	macOS := filepath.Dir(exe)
	if filepath.Base(macOS) != "MacOS" {
		return ""
	}
	contents := filepath.Dir(macOS)
	if filepath.Base(contents) != "Contents" {
		return ""
	}
	return filepath.Join(contents, "Resources")
}
