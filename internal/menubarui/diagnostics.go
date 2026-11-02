package menubarui

import (
	"fmt"
	"strings"

	"sparky/internal/menubar"
)

// Diagnostics is a short, plain report users can copy. It never includes the
// client config contents, secrets, or raw stderr.
type Diagnostics struct {
	Version     string
	CLIPath     string
	CLIError    string
	Prefs       Prefs
	Login       bool
	Snapshot    menubar.Snapshot
	LastRefresh string
}

// DiagnosticsText renders the diagnostics report.
func DiagnosticsText(d Diagnostics) string {
	var b strings.Builder
	b.WriteString("Sparky menu bar " + d.Version + "\n")
	if d.CLIPath != "" {
		b.WriteString("CLI: " + d.CLIPath + "\n")
	} else {
		msg := d.CLIError
		if msg == "" {
			msg = ErrCLINotFound.Error()
		}
		b.WriteString("CLI: unavailable (" + msg + ")\n")
	}
	fmt.Fprintf(&b, "Start at login: %s\n", yesNo(d.Login))
	fmt.Fprintf(&b, "Show title: %s\n", yesNo(d.Prefs.ShowTitle))
	b.WriteString("Status: " + StatusLine(d.Snapshot) + "\n")
	b.WriteString(FreshnessLine(d.Snapshot) + "\n")
	if d.Snapshot.Error != "" {
		b.WriteString("Last error: " + FriendlyError(fmt.Errorf("%s", d.Snapshot.Error)) + "\n")
	}
	if d.Snapshot.LoadError != "" {
		b.WriteString("Last load error: " + FriendlyError(fmt.Errorf("%s", d.Snapshot.LoadError)) + "\n")
	}
	if d.LastRefresh != "" {
		b.WriteString("Refresh: " + d.LastRefresh + "\n")
	}
	for _, dev := range d.Snapshot.Devices {
		b.WriteString(NodeMemory(dev) + "\n")
	}
	for _, m := range LoadedModels(d.Snapshot) {
		b.WriteString("Loaded: " + ModelLine(m, d.Snapshot) + "\n")
	}
	return b.String()
}

func yesNo(v bool) string {
	if v {
		return "on"
	}
	return "off"
}
