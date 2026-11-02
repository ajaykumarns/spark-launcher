//go:build darwin && cgo

package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
)

// AppKit view objects must be built on the process main OS thread, exactly like
// main_darwin.go. testing.M.Run executes each test in a separate test goroutine
// that LockOSThread cannot move onto the main thread, so the native cases run
// synchronously inside TestMain on its already-locked main-thread goroutine,
// before the test framework starts. The Test* functions below only report the
// recorded result; they never touch AppKit themselves.
var (
	nativeStartupErr error
	nativeStartupRan bool
	nativeLayoutErr  error
)

// Exercise native model menu construction and replacement without a status
// item, window, NSApp event loop, images, CLI or model calls.
func nativePickerCase() error {
	if code := probeBuildPanel(); code != 0 {
		return fmt.Errorf("headless panel construction failed (code %d)", code)
	}

	labels := []string{"Spark Auto", "DeepSeek V3", "GLM 4.6"}
	ids := []string{"spark-auto", "deepseek-v3", "glm-4.6"}
	panelSetPicker(true, "Choose a model to load", labels, ids)
	if rows := probeChoiceRows(); rows != len(labels) {
		return fmt.Errorf("initial picker rows = %d, want %d", rows, len(labels))
	}
	if caption := probePickerCaption(); caption != "Choose a model to load" {
		return fmt.Errorf("initial picker caption = %q", caption)
	}

	// A changed catalog forces the rebuild path (ClearStack + reinsert), which
	// is where the original crash occurred.
	replacement := []string{"Spark Auto", "Kimi K2"}
	replacementIDs := []string{"spark-auto", "kimi-k2"}
	panelSetPicker(true, "Choose a model to load", replacement, replacementIDs)
	if rows := probeChoiceRows(); rows != len(replacement) {
		return fmt.Errorf("replacement rows = %d, want %d", rows, len(replacement))
	}

	// Identical catalog must not rebuild (keeps keyboard focus); rows stay.
	panelSetPicker(true, "Choose a model to load", replacement, replacementIDs)
	if rows := probeChoiceRows(); rows != len(replacement) {
		return fmt.Errorf("identical-catalog rows = %d, want %d", rows, len(replacement))
	}

	// Empty catalog (loading/empty state) must clear the stack cleanly.
	panelSetPicker(true, "Waiting for the CLI catalog…", nil, nil)
	if rows := probeChoiceRows(); rows != 0 {
		return fmt.Errorf("empty-catalog rows = %d, want 0", rows)
	}
	if caption := probePickerCaption(); !strings.Contains(caption, "catalog") {
		return fmt.Errorf("empty-catalog caption = %q", caption)
	}

	// Return to overview through the real path and confirm nothing threw.
	if code := probeBuildPanel(); code != 0 {
		return fmt.Errorf("probe after overview return failed (code %d)", code)
	}
	panelSetPicker(false, "Choose a model to load", replacement, replacementIDs)
	if rows := probeChoiceRows(); rows != len(replacement) {
		return fmt.Errorf("overview-mode rows = %d, want %d", rows, len(replacement))
	}
	return nil
}

func TestMain(m *testing.M) {
	// Stay on the process main OS thread for the native case, then release it
	// so the ordinary Go tests are free to schedule normally.
	runtime.LockOSThread()
	nativeStartupErr = nativePickerCase()
	nativeLayoutErr = nativeLayoutCase()
	nativeStartupRan = true
	runtime.UnlockOSThread()
	os.Exit(m.Run())
}

func nativeLayoutCase() error {
	panelSetContent(panelStrings{
		HeaderTitle: "Sparky", HeaderState: "Ready", ModelCaption: "Language model",
		ModelName: "deepseek-v4.1-flash-1017", ModelLocation: "Ready on Spark 1 + Spark 2",
		MemoryCaption: "Device memory · in use / total", LoadLabel: "Load model…",
		RefreshLabel: "Refresh status", Footer: "Updated just now",
	})
	panelSetMemoryRows([]panelPair{{"Spark 1", "114.6 / 121.7 GiB"}, {"Spark 2", "115.4 / 121.7 GiB"}})
	panelSetRequests("Active requests", "0")
	if err := probeLayout(false); err != "" {
		return fmt.Errorf("overview: %s", err)
	}
	panelSetPicker(true, "Run through the Sparky CLI", []string{"DeepSeek", strings.Repeat("Long model name ", 8)}, []string{"deepseek", "long"})
	if err := probeLayout(true); err != "" {
		return fmt.Errorf("picker: %s", err)
	}
	return nil
}

func TestNativeMenuLayoutAndTracking(t *testing.T) {
	if nativeLayoutErr != nil {
		t.Fatal(nativeLayoutErr)
	}
}

func TestNativeModelMenuUpdates(t *testing.T) {
	if !nativeStartupRan {
		t.Fatal("native probe case did not run")
	}
	if nativeStartupErr != nil {
		t.Fatalf("native picker regression: %v", nativeStartupErr)
	}
}
