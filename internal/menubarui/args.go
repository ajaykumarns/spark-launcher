package menubarui

import (
	"errors"
	"fmt"
)

// Options are the command-line options understood by the menu bar app.
type Options struct {
	CLIOverride string // --cli PATH (explicit, fail-closed)
	Help        bool
	Version     bool
}

// Usage is the short help text.
const Usage = `Usage: sparky-menubar [--cli PATH] [--help] [--version]

  --cli PATH     use this absolute Sparky CLI (fails closed if invalid)
  --help, -h     show this help
  --version      print the app version

The app runs the existing sparky CLI in the background. It never loads a
model by itself, never uses PATH, and stores preferences in the Sparky state
directory.`

// ParseArgs parses the app's arguments without touching the UI.
func ParseArgs(args []string) (Options, error) {
	var o Options
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--help", "-h":
			o.Help = true
		case "--version":
			o.Version = true
		case "--cli":
			if i+1 >= len(args) {
				return o, errors.New("--cli requires a PATH")
			}
			i++
			if args[i] == "" {
				return o, errors.New("--cli requires a non-empty PATH")
			}
			o.CLIOverride = args[i]
		default:
			if len(args[i]) >= 2 && args[i][0] == '-' {
				return o, fmt.Errorf("unknown option %s", args[i])
			}
			return o, fmt.Errorf("unexpected argument %s", args[i])
		}
	}
	return o, nil
}
