// Package menubarui contains the user-facing helpers behind the Sparky macOS
// menu bar app. It deliberately has no GUI or cgo dependency so it builds and
// tests on headless machines; the native command wires it to a small AppKit
// popover bridge that consumes the plain strings this package produces.
package menubarui

import (
	"context"

	"sparky/internal/menubar"
)

// Controller is the narrow backend contract the app consumes. The concrete
// implementation is menubar.Controller; tests use synthetic fakes.
type Controller interface {
	Snapshot() menubar.Snapshot
	Refresh(context.Context) error
	Catalog(context.Context) ([]menubar.Choice, error)
	Load(context.Context, string) error
}

// Re-exported backend types so the app wires directly to the shared API.
type (
	Snapshot     = menubar.Snapshot
	ModelStatus  = menubar.ModelStatus
	DeviceStatus = menubar.DeviceStatus
	Choice       = menubar.Choice
)

// LoadedModels returns only LLMs that are actually loaded, in stable order.
// The app shows real residency, never the configured default.
func LoadedModels(s Snapshot) []menubar.ModelStatus {
	var out []menubar.ModelStatus
	for _, m := range s.Models {
		if m.Loaded {
			out = append(out, m)
		}
	}
	return out
}

// AnyLoading reports whether a load is in progress according to the backend.
func AnyLoading(s Snapshot) bool { return s.Loading }

// StateLabel maps backend state strings to short user-facing words.
func StateLabel(m menubar.ModelStatus, snap Snapshot) string {
	if snap.Error != "" && !snap.Loading {
		return "Unreachable"
	}
	switch m.State {
	case "ready":
		if m.Loaded {
			return "Ready"
		}
		return "Unknown"
	case "loaded":
		// A generic "loaded" token does not prove verified readiness.
		return "Loaded"
	case "loading":
		return "Loading"
	case "failed":
		return "Failed"
	case "stopped", "unloaded", "idle":
		return "Stopped"
	case "":
		if m.Loaded {
			// Present but without a controller state: never claim Ready.
			return "Unknown (loaded)"
		}
		return "Unknown"
	default:
		return title(m.State)
	}
}
