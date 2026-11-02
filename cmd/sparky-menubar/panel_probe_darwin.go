//go:build darwin && cgo

package main

/*
#include <stdlib.h>
#include "panel.h"
*/
import "C"

import "unsafe"

// The exported Go callbacks in callbacks_darwin.go are part of the same
// package; cgo supplies their extern declarations to panel.m. Keeping this file
// in the app package (rather than a test-only package) lets the native test
// drive the real production picker bridge instead of a reimplementation.
//
// All probe helpers are main-thread-only and return an error code rather than
// dispatching, so tests never wait on an unpumped main queue.

// probeBuildPanel builds the real native menu object graph in
// memory. It creates no status item, no window, shows nothing, and does not
// run NSApp's event loop.
func probeBuildPanel() int { return int(C.sparky_probe_build_panel()) }

// probeChoiceRows reports how many native model choice items exist.
func probeChoiceRows() int { return int(C.sparky_probe_choice_count()) }

// probePickerCaption reports the current catalog status. The C side
// returns a strdup'd copy, which is freed here.
func probePickerCaption() string {
	p := C.sparky_probe_picker_caption()
	if p == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(p))
	return C.GoString(p)
}

func probeLayout(picking bool) string {
	mode := C.int(0)
	if picking {
		mode = 1
	}
	p := C.sparky_probe_layout(mode)
	if p == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(p))
	return C.GoString(p)
}
