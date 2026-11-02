// Package menubar is the portable backend for the native Sparky macOS menu
// bar app. It runs the existing sparky CLI in the background and exposes
// plain data to the native UI: device and model status, the configured model
// catalog, and a single, non-overlapping model load.
//
// The package is pure Go and CGO-free so it stays portable and testable off
// macOS. It never touches the controller, runtime, CLI parser, or device
// configuration, and it never starts a load on its own: only an explicit
// Load call from the UI starts remote work.
//
// Data comes from three read-only CLI invocations:
//
//	sparky --json status          device memory and every model's actual state
//	sparky --json models          the configured catalog (with spark-auto)
//	sparky --json load ID          the requested load and its final result
//
// Unknown or missing readings stay unknown (nil), never zero. Failed
// refreshes keep the last successful snapshot so the menu can show cached
// data while the cluster is unreachable.
package menubar
