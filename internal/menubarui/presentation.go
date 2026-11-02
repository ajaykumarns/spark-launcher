package menubarui

import (
	"strings"
	"time"
)

// PickerCaptionText picks the short line above the model chooser. It always
// shows an explicit loading, empty, or error state instead of a blank list, and
// prefers the error over a stale list because a failed refresh is actionable.
func PickerCaptionText(hasCLI bool, busy bool, loaded bool, choices int, err string) string {
	err = strings.TrimSpace(err)
	switch {
	case !hasCLI:
		return "Sparky CLI not available"
	case err != "":
		return "Model list unavailable: " + err
	case busy && choices == 0:
		return "Loading model list…"
	case loaded && choices == 0:
		return "No loadable models in the CLI catalog"
	case choices == 0:
		return "Waiting for the model list"
	}
	return "Run through the Sparky CLI"
}

// MergeAcceptedLoad overlays an accepted LoadTracker on a snapshot so the panel
// reflects a click immediately, before the backend goroutine updates
// Snapshot.Loading. When the tracker says a load is running the snapshot is
// marked Loading and any previous load error is cleared, so Load stays disabled
// through the acceptance gap and progress text appears at once. A tracker that
// is not running leaves the snapshot untouched; the controller remains
// authoritative for final readiness.
func MergeAcceptedLoad(s Snapshot, running bool) Snapshot {
	if !running {
		return s
	}
	s.Loading = true
	if s.Error == "" {
		s.LoadError = ""
	}
	return s
}

// LoadProgressText is the footer line while an accepted load runs. It always
// includes the tracker's real elapsed time and names the target model, but it
// never invents stages or percentages: readiness stays the controller's call.
// A message already supplied by the backend is kept ahead of the generic
// wording so real reported detail is not discarded.
func LoadProgressText(s Snapshot, model string, elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	name := shortName(ModelStatus{ID: model, Name: model})
	target := "model"
	if name != "" {
		target = name
	}
	base := "Loading " + target
	if msg := firstLine(s.LoadMessage); msg != "" {
		base = msg
	}
	return base + " · " + elapsed.Round(time.Second).String() + " elapsed"
}

// firstLine keeps footer text on one line, even if a backend message carries a
// wrapped or multi-line detail.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
