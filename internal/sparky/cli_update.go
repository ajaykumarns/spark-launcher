package sparky

// Public CLI surface for "sparky update" (CLI-only updater on the Mac).
// Grammar (validated entirely locally, before any configuration or network
// effect): sparky update [--check] [--tag vX.Y.Z] [--json]. There is no
// --force, no targets selector, no repo/model option: the updater always
// updates the Mac CLI and both Sparks from the fixed verified release, and
// the device transaction alone owns fencing, restart and rollback.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// cliUpdateCommand runs one update (or check) and returns the exit code:
// 0 success (including a check that found an update is available), 1
// runtime failure or incomplete check, 2 usage, 130/143 interrupted.
func cliUpdateCommand(ctx context.Context, cfg clientConfig, inv *cliInvocation, out, errout io.Writer) int {
	// Client platform gate and --tag syntax are checked before any effect.
	if err := updateEnsureClientPlatform(); err != nil {
		return cliErrorExit(inv, err, out, errout)
	}
	tag := inv.Values["tag"]
	if err := updateValidateTag(tag); err != nil {
		return cliErrorExit(inv, err, out, errout)
	}
	progress := func(message string) {
		if message != "" && cliProgressOut != nil {
			fmt.Fprintln(cliProgressOut, message)
		}
	}
	res := updateExecute(ctx, cfg, inv.Flags["check"], tag, progress)

	// A partial report survives interruption and failure: emit it first so
	// scripts always see per-target state, then report the outcome.
	if res.hasReport {
		if inv.JSON {
			_, _ = out.Write(jsonBytes(res.report))
		} else {
			fmt.Fprint(out, cliUpdateHuman(inv, res.report))
		}
	}
	if res.canceled {
		// Honor the signal cause (130 vs 143); a plain cancel is still a
		// normal interruption, never a generic failure.
		err := cliInterruptErr(ctx, context.Canceled)
		var typed *cliInterruptError
		if !errors.As(err, &typed) {
			err = &cliInterruptError{}
		}
		return cliErrorExit(inv, err, out, errout)
	}
	if res.err != nil {
		return cliErrorExit(inv, res.err, out, errout)
	}
	if boolean(res.report["ok"]) {
		return 0
	}
	return 1
}

// cliUpdateHuman renders one plain-text summary. It lists every component
// with its observed or resulting version and digest, and never collapses a
// partial result into a generic error.
func cliUpdateHuman(inv *cliInvocation, report Object) string {
	var b strings.Builder
	action := "update"
	if str(report["action"]) != "" {
		action = str(report["action"])
	}
	head := "sparky " + action + " " + str(report["tag"])
	if v := str(report["version"]); v != "" {
		head += " (" + v + ")"
	}
	fmt.Fprintln(&b, head)
	for _, raw := range arr(report["components"]) {
		c := obj(raw)
		line := "  " + str(c["component"])
		for len(line) < 12 {
			line += " "
		}
		line += str(c["status"])
		for len(line) < 26 {
			line += " "
		}
		if v := str(c["version"]); v != "" {
			line += "v" + v
		}
		if s := str(c["sha256"]); s != "" && len(s) == 64 {
			line += "  " + s[:12]
		}
		if bck := str(c["backup"]); bck != "" {
			line += "  backup: " + bck
		}
		if cr, ok := c["controller_restarted"]; ok {
			if boolean(cr) {
				line += "  controller restarted"
			}
		}
		fmt.Fprintln(&b, line)
		if e := str(c["error"]); e != "" {
			fmt.Fprintln(&b, "              "+e)
		}
	}
	if blocked := str(report["blocked"]); blocked != "" {
		fmt.Fprintln(&b, "Blocked: "+blocked)
	}
	if !boolean(report["ok"]) && report["blocked"] == nil {
		fmt.Fprintln(&b, "Update did not complete; see the component states above.")
	}
	return b.String()
}
