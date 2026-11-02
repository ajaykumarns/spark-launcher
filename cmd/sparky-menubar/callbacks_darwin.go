//go:build darwin && cgo

package main

/*
#include <stdlib.h>
// cgo generates the extern declarations for the exported Go functions; adding
// them here would conflict with the generated prototypes.
*/
import "C"

import "unsafe"

// The panel app instance, set once before the AppKit run loop starts.
var activeApp *menuApp

// Every callback below is invoked from AppKit's main thread. None of them
// waits on AppKit or does remote work inline; each hands work to Go state or
// background goroutines, so the event loop never blocks.

//export goPanelOpened
func goPanelOpened() {
	if activeApp != nil {
		activeApp.panelOpened()
	}
}

//export goRefresh
func goRefresh() {
	if activeApp != nil {
		activeApp.panelRefresh()
	}
}

//export goPickerOpen
func goPickerOpen() {
	if activeApp != nil {
		activeApp.panelPickerOpen()
	}
}

//export goPickerClose
func goPickerClose() {
	if activeApp != nil {
		activeApp.panelPickerClose()
	}
}

//export goChoiceClick
func goChoiceClick(modelID *C.char) {
	if activeApp == nil || modelID == nil {
		return
	}
	// The ID is passed verbatim to the existing CLI argument array; never
	// interpreted as shell text.
	id := C.GoString(modelID)
	if id == "" {
		return
	}
	activeApp.panelChoose(id)
}

//export goToggleTitle
func goToggleTitle() {
	if activeApp != nil {
		activeApp.panelToggleTitle()
	}
}

//export goToggleLogin
func goToggleLogin() {
	if activeApp != nil {
		activeApp.panelToggleLogin()
	}
}

//export goCopyDiagnostics
func goCopyDiagnostics() {
	if activeApp != nil {
		activeApp.panelCopyDiagnostics()
	}
}

//export goQuit
func goQuit() {
	if activeApp != nil {
		activeApp.panelQuit()
	}
}

// Keep unsafe referenced for builds where only the C bridge uses pointers.
var _ = unsafe.Pointer(nil)
