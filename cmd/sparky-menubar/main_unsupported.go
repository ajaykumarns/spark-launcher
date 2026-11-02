//go:build !darwin || !cgo

// Package main builds a plain, dependency-free stub on platforms without the
// native menu. This keeps headless CI and CGO_ENABLED=0 cross builds green;
// the real app is built with: CGO_ENABLED=1 GOOS=darwin go build.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "The Sparky menu bar app needs macOS with cgo.")
	fmt.Fprintln(os.Stderr, "Build it with: CGO_ENABLED=1 GOOS=darwin go build ./cmd/sparky-menubar")
	os.Exit(2)
}
