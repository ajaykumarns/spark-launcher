//go:build darwin && cgo

package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa
#include <stdlib.h>
#include <string.h>
#include "panel.h"
*/
import "C"

import (
	"unsafe"
)

// cstr returns a C string for s. Callers must free it with C.free.
func cstr(s string) *C.char {
	if s == "" {
		return nil
	}
	return C.CString(s)
}

// freeC frees a slice of C strings allocated by cstrSlice.
func freeC(list []*C.char) {
	for _, p := range list {
		if p != nil {
			C.free(unsafe.Pointer(p))
		}
	}
}

// cstrSlice converts Go strings to C string arrays for the bridge. The caller
// must free the returned pointers. An empty slice yields (nil, 0).
func cstrSlice(in []string) ([]*C.char, C.int) {
	if len(in) == 0 {
		return nil, 0
	}
	out := make([]*C.char, len(in))
	for i, s := range in {
		out[i] = cstr(s)
	}
	return out, C.int(len(in))
}

// cArray returns a **char pointer into list's backing array. list must stay
// referenced (in scope) across the C call; callers do so directly.
func cArray(list []*C.char) **C.char {
	if len(list) == 0 {
		return nil
	}
	return (**C.char)(unsafe.Pointer(&list[0]))
}

// The Go caller must be on the locked main OS thread (see main_darwin.go).
func panelInit()     { C.sparky_init() }
func panelRun()      { C.sparky_run() } // same locked main OS thread
func bridgeQuitApp() { C.sparky_quit() }

// panelSetContent pushes the pre-formatted panel strings to AppKit. The C side
// copies them synchronously, so Go memory is not retained.
func panelSetContent(v panelStrings) {
	ht := cstr(v.HeaderTitle)
	hs := cstr(v.HeaderState)
	mc := cstr(v.ModelCaption)
	mn := cstr(v.ModelName)
	ml := cstr(v.ModelLocation)
	mm := cstr(v.MemoryCaption)
	rl := cstr(v.RefreshLabel)
	ll := cstr(v.LoadLabel)
	ft := cstr(v.Footer)
	lr := cstr(v.LoadResult)
	defer freeC([]*C.char{ht, hs, mc, mn, ml, mm, rl, ll, ft, lr})
	C.sparky_set_panel(ht, hs, mc, mn, ml, mm, rl, ll, ft, lr)
}

func panelSetExtraModels(lines []string) {
	list, n := cstrSlice(lines)
	defer freeC(list)
	C.sparky_set_extra_models(cArray(list), n)
}

func panelSetMemoryRows(pairs []panelPair) {
	if len(pairs) == 0 {
		C.sparky_set_memory_rows(nil, nil, 0)
		return
	}
	labels := make([]*C.char, len(pairs))
	values := make([]*C.char, len(pairs))
	for i, p := range pairs {
		labels[i] = cstr(p.Label)
		values[i] = cstr(p.Value)
	}
	defer freeC(labels)
	defer freeC(values)
	C.sparky_set_memory_rows(cArray(labels), cArray(values), C.int(len(pairs)))
}

func panelSetRequests(label, value string) {
	lab := cstr(label)
	val := cstr(value)
	defer freeC([]*C.char{lab, val})
	C.sparky_set_requests(lab, val)
}

func panelSetPicker(picking bool, caption string, labels, ids []string) {
	cap := cstr(caption)
	defer freeC([]*C.char{cap})
	lab, n := cstrSlice(labels)
	defer freeC(lab)
	idv, _ := cstrSlice(ids)
	defer freeC(idv)
	p := C.int(0)
	if picking {
		p = 1
	}
	C.sparky_set_picker(p, cap, cArray(lab), cArray(idv), n)
}

func panelSetEnabled(loadEnabled, refreshEnabled, hasCLI bool) {
	b := func(v bool) C.int {
		if v {
			return 1
		}
		return 0
	}
	C.sparky_set_enabled(b(loadEnabled), b(refreshEnabled), b(hasCLI))
}

func panelSetSettings(showTitle, loginEnabled bool) {
	b := func(v bool) C.int {
		if v {
			return 1
		}
		return 0
	}
	C.sparky_set_settings(b(showTitle), b(loginEnabled))
}

func panelSetIcon(state int, title string) {
	t := cstr(title)
	defer freeC([]*C.char{t})
	C.sparky_set_icon_state(C.int(state), t)
}
