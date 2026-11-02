package menubarui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"sparky/internal/menubar"
)

const gib = 1024 * 1024 * 1024

// FormatGiB renders a byte count as GiB with one decimal, or "unknown".
func FormatGiB(b *uint64) string {
	if b == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.1f GiB", float64(*b)/gib)
}

// InUseGiB computes total minus available. It returns "" when either reading
// is unknown rather than treating unknown as zero.
func InUseGiB(d menubar.DeviceStatus) string {
	if d.TotalBytes == nil || d.AvailableBytes == nil {
		return ""
	}
	total, avail := *d.TotalBytes, *d.AvailableBytes
	if avail > total {
		return ""
	}
	return fmt.Sprintf("%.1f GiB", float64(total-avail)/gib)
}

// NodeMemory is one line of per-device memory for the status section.
func NodeMemory(d menubar.DeviceStatus) string {
	if d.TotalBytes == nil || d.AvailableBytes == nil {
		return fmt.Sprintf("%s: memory unknown", displayID(d.ID))
	}
	inUse := InUseGiB(d)
	if inUse == "" {
		return fmt.Sprintf("%s: memory unknown", displayID(d.ID))
	}
	return fmt.Sprintf("%s: %s in use of %s, %s available",
		displayID(d.ID), inUse, FormatGiB(d.TotalBytes), FormatGiB(d.AvailableBytes))
}

// RequestsLabel preserves unknown request counts instead of showing zero.
func RequestsLabel(m menubar.ModelStatus) string {
	if m.ActiveRequests == nil {
		return "requests unknown"
	}
	n := *m.ActiveRequests
	if n == 1 {
		return "1 request"
	}
	return fmt.Sprintf("%d requests", n)
}

// ModelsTitle is the header for the loaded-model section.
func ModelsTitle(s menubar.Snapshot) string {
	n := len(LoadedModels(s))
	if n == 0 {
		return "Loaded models"
	}
	return fmt.Sprintf("Loaded models (%d)", n)
}

// ModelRows returns one visible line per loaded LLM. When none are loaded it
// returns a single explicit "No model loaded" or "Unknown" line so the menu
// never silently omits the main requirement.
func ModelRows(s menubar.Snapshot) []string {
	loaded := LoadedModels(s)
	if len(loaded) == 0 {
		if s.UpdatedAt.IsZero() && s.Error == "" {
			return []string{"Unknown — no data yet"}
		}
		return []string{"No model loaded"}
	}
	rows := make([]string, 0, len(loaded))
	for _, m := range loaded {
		rows = append(rows, ModelLine(m, s))
	}
	return rows
}

// ModelLine is the menu line for one loaded model.
func ModelLine(m menubar.ModelStatus, snap menubar.Snapshot) string {
	name := m.Name
	if name == "" {
		name = m.ID
	}
	parts := []string{name, StateLabel(m, snap)}
	if len(m.Nodes) > 0 {
		parts = append(parts, strings.Join(m.Nodes, ", "))
	}
	parts = append(parts, RequestsLabel(m))
	return strings.Join(parts, " · ")
}

// StatusLine summarises the current snapshot in one short plain sentence.
func StatusLine(s menubar.Snapshot) string {
	switch {
	case s.Loading:
		if s.LoadMessage != "" {
			return s.LoadMessage
		}
		return "Loading a model…"
	case s.Error != "":
		return "Controller unreachable"
	case s.UpdatedAt.IsZero():
		return "No successful status yet"
	}
	loaded := LoadedModels(s)
	if len(loaded) == 0 {
		return "No model loaded"
	}
	return fmt.Sprintf("%d model%s loaded", len(loaded), plural(len(loaded)))
}

// FreshnessLine renders the last-successful update time and age. With cached
// data it is preserved even when the latest refresh failed, so a stale menu
// still shows when it was last good. Without any successful status it says so
// plainly instead of implying a cached sample exists.
func FreshnessLine(s menubar.Snapshot) string {
	if s.UpdatedAt.IsZero() {
		return "No successful status yet"
	}
	line := "Last update: " + s.UpdatedAt.Local().Format("15:04") +
		" (" + HumanAge(time.Since(s.UpdatedAt)) + ")"
	if s.Error != "" {
		line += " — stale"
	}
	return line
}

// LoadResultLine is the short root-level result shown after a load outcome
// that needs attention, so a failure is not hidden inside the Load submenu.
// It returns "" when there is nothing to show.
//
// It never claims a historical failure is the current model's state: a failure
// line says "Last load failed" (or "readiness not confirmed") and points at
// diagnostics. The currently loaded model rows and their state stay visible
// separately and are never relabelled as failed by this line.
func LoadResultLine(s menubar.Snapshot) string {
	if s.LoadError == "" {
		return ""
	}
	// A load still running has its own elapsed line; don't stack a stale
	// error on top of an in-progress load.
	if s.Loading {
		return ""
	}
	msg := strings.ToLower(s.LoadError + " " + s.LoadMessage)
	if strings.Contains(msg, "readiness not confirmed") ||
		strings.Contains(msg, "could not be confirmed") {
		return "Load readiness not confirmed — see diagnostics"
	}
	// Historical wording: the loaded model rows above keep showing the
	// actual current state; this line only reports the last load attempt.
	return "Last load failed — see diagnostics"
}

// HumanAge renders a duration as a short age.
func HumanAge(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%d h ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%d d ago", int(d/(24*time.Hour)))
}

// ElapsedLabel renders load elapsed time without implying progress percent.
func ElapsedLabel(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("Loading… %s elapsed", d.Round(time.Second).String())
}

// IconTitle is the optional short tray title.
// Loading shows "…" and a stale/error state shows "!". When the user enabled
// the title, a single loaded model shows its short name and several loaded
// models show a count. Recovery clears the glyph automatically.
func IconTitle(s menubar.Snapshot, showTitle bool) string {
	if s.Loading {
		return "…"
	}
	if s.Error != "" {
		return "!"
	}
	if !showTitle {
		return ""
	}
	loaded := LoadedModels(s)
	switch len(loaded) {
	case 0:
		return ""
	case 1:
		return shortName(loaded[0])
	default:
		return fmt.Sprintf("%d models", len(loaded))
	}
}

// shortName is the last path segment of a model ID, or the display name.
func shortName(m menubar.ModelStatus) string {
	name := m.Name
	if name == "" {
		name = m.ID
	}
	if i := strings.LastIndexByte(name, '/'); i >= 0 && i+1 < len(name) {
		name = name[i+1:]
	}
	if len(name) > 24 {
		name = name[:24] + "…"
	}
	return name
}

// Tooltip is the icon hover text.
func Tooltip(s menubar.Snapshot, version string) string {
	line := StatusLine(s)
	if version != "" {
		line += " (Sparky " + version + ")"
	}
	return line
}

// CatalogChoices returns a stable, copy-safe sorted list without synthetic
// entries; the backend already omits spark-auto, but this is defensive.
func CatalogChoices(in []menubar.Choice) []menubar.Choice {
	out := make([]menubar.Choice, 0, len(in))
	for _, c := range in {
		if c.ID == "" || strings.Contains(c.ID, "spark-auto") {
			continue
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ChoiceLabel is the menu label for a catalog entry.
func ChoiceLabel(c menubar.Choice) string {
	name := c.Name
	if name == "" || name == c.ID {
		return c.ID
	}
	return name + " (" + c.ID + ")"
}

// FriendlyError turns an error into short plain text without secrets or raw
// multi-line stderr dumps.
func FriendlyError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = strings.TrimSpace(msg[:i])
	}
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	if msg == "" {
		return "Something went wrong"
	}
	return msg
}

func displayID(id string) string {
	if id == "" {
		return "node"
	}
	return id
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
