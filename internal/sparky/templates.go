package sparky

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
)

// This file provides PURE template helpers for the Sparky native systemd
// units, the ComfyUI drop-in, and the legacy-manager transitional shim. They
// accept every input (user, helper, config, root, node, plugin paths) as
// explicit arguments and never read live cluster, node, or runtime state.
// The native lifecycle itself lives in the core implementation; generating
// and placing units is the caller's (device InstallHead) responsibility.

//go:embed assets/comfy_guard.py
var comfyGuardAsset []byte

// ComfyGuardScript returns a defensive copy of the engine-owned
// comfy_guard.py embedded asset. This Python hook runs inside ComfyUI (an
// accepted exception); all Sparky decisions and CLI remain Go.
func ComfyGuardScript() []byte {
	out := make([]byte, len(comfyGuardAsset))
	copy(out, comfyGuardAsset)
	return out
}

// systemdExecValue escapes a value for use as an argument in an
// ExecStart=/ExecStop=/ExecStopPost= directive. For the executable itself
// the caller prefixes ":" which disables systemd environment-variable
// substitution for the whole command line, so dollar signs stay literal.
// Percent signs are doubled because Exec lines still expand unit
// specifiers: a user path must never be able to introduce %i, %h, etc.
// Backslashes and double quotes are C-style escaped inside the quoted
// path, per systemd.service Command lines.
func systemdExecValue(s string) (string, error) {
	if err := validateUnitText(s); err != nil {
		return "", err
	}
	r := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		`%`, `%%`,
	)
	return `"` + r.Replace(s) + `"`, nil
}

// systemdBarePathValue renders a path for directives parsed as a single
// unquoted WHOLE value (WorkingDirectory=, EnvironmentFile=). Real
// `systemd-analyze verify` runs show these parsers do NOT strip quotes: a
// quoted path is taken literally (leading `"` makes it relative) and
// silently ignored. The value is therefore rendered unquoted. These
// directives expand %-specifiers, so a literal "%" is doubled to "%%";
// "$" is not expanded and stays literal. A path containing a double quote
// or backslash cannot be represented faithfully in this unquoted whole-
// value form (quoting would be taken literally), so it is rejected
// explicitly rather than rendered wrong. Spaces are preserved literally.
func systemdBarePathValue(s string) (string, error) {
	if strings.ContainsAny(s, `"\`) {
		return "", fail("path contains a quote or backslash that cannot be represented in a systemd directive parsed as a whole unquoted value")
	}
	if err := validateUnitText(s); err != nil {
		return "", err
	}
	return strings.ReplaceAll(s, `%`, `%%`), nil
}

// systemdWorkingDirectoryValue renders a WorkingDirectory= value. See
// systemdBarePathValue: the whole value is the path, unquoted.
func systemdWorkingDirectoryValue(s string) (string, error) {
	return systemdBarePathValue(s)
}

// systemdPathValue escapes a value for use in directives parsed as a
// word list with quote support (EnvironmentFile=... in other unit kinds)
// or as a quoted whole-assignment (Environment=). Those parsers DO strip
// matching quotes, so the value is rendered double-quoted with backslashes
// and inner double quotes C-escaped. Both expand %-specifiers, so a
// literal "%" is doubled to "%%"; "$" is left literal. Note directives
// parsed as a whole unquoted value (WorkingDirectory=, EnvironmentFile=)
// have different semantics and must use systemdBarePathValue instead.
func systemdPathValue(s string) (string, error) {
	if err := validateUnitText(s); err != nil {
		return "", err
	}
	r := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		`%`, `%%`,
	)
	return `"` + r.Replace(s) + `"`, nil
}

// validateAbsolutePath rejects empty and relative paths consistently across
// every template input; errors never echo the raw input.
func validateAbsolutePath(kind, p string) error {
	if p == "" {
		return fail("%s must not be empty", kind)
	}
	if !filepath.IsAbs(p) {
		return fail("%s must be an absolute path", kind)
	}
	return validateUnitText(p)
}

// validateServiceUser accepts a valid local service-user token: a name of
// letters, digits, "_", ".", "-" (not starting with "-" or ".") with an
// optional trailing "$", or a numeric UID. Anything else — whitespace,
// newlines, specifiers, control characters, injection attempts — is
// rejected without echoing the input.
func validateServiceUser(user string) error {
	if user == "" {
		return fail("service user must not be empty")
	}
	if isNumericUID(user) {
		return nil
	}
	for i, c := range user {
		valid := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '_' || c == '.' || c == '-' || (c == '$' && i == len(user)-1)
		if !valid {
			return fail("service user contains a character that is invalid in a systemd User= directive")
		}
	}
	if user[0] == '-' || user[0] == '.' {
		return fail("service user must not start with '-' or '.'")
	}
	return nil
}

func isNumericUID(user string) bool {
	if user == "" {
		return false
	}
	for _, c := range user {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// validateUnitText rejects NUL, newlines, and other ASCII control characters
// that systemd unit files cannot represent inside a single directive.
func validateUnitText(s string) error {
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return fail("path contains a control character that is invalid in a systemd unit")
		}
	}
	return nil
}

// validateModelName rejects anything that is not a plain unit-name-safe
// model identifier. Models appear only as systemd %i instances or shell
// arguments, never interpolated into unit text.
func validateModelName(model string) error {
	if model == "" {
		return fail("empty model name")
	}
	for _, c := range model {
		if c < 0x20 || c == 0x7f || strings.ContainsRune("/%\\ \"$'\t\n\r", c) {
			return fail("model name contains a character that is invalid for a systemd instance or shell argument")
		}
	}
	return nil
}

// SparkyControllerUnit renders the Go-controller systemd unit.
//
// Type=simple, User=service user, WorkingDirectory=root,
// EnvironmentFile=ROOT/vllm/api.env (path only, never values),
// ExecStart=HELPER --json node controller --file CONFIG, Restart=on-failure
// with RestartSec=3 and UMask=0077, ordered after network-online. The
// EnvironmentFile is mandatory: the llama-swap manager resolves
// ${env.VLLM_API_KEY} from that file's process environment, and backend
// header reading cannot replace it on a fresh install. [Install]
// WantedBy=multi-user.target makes `systemctl enable --now` work and starts
// the daemon at boot. It deliberately has no dependency on llama-swap or
// garuda-models units: preserving parity startup ordering is the migration
// caller's job.
func SparkyControllerUnit(user, helper, config, root string) (string, error) {
	for name, v := range map[string]string{"user": user, "helper": helper, "config": config, "root": root} {
		if v == "" {
			return "", fail("SparkyControllerUnit: empty %s", name)
		}
	}
	if err := validateServiceUser(user); err != nil {
		return "", err
	}
	if err := validateAbsolutePath("helper", helper); err != nil {
		return "", fail("SparkyControllerUnit helper: %v", err)
	}
	if err := validateAbsolutePath("config", config); err != nil {
		return "", fail("SparkyControllerUnit config: %v", err)
	}
	if err := validateAbsolutePath("root", root); err != nil {
		return "", fail("SparkyControllerUnit root: %v", err)
	}
	helperQ, err := systemdExecValue(helper)
	if err != nil {
		return "", fmt.Errorf("helper: %w", err)
	}
	configQ, err := systemdExecValue(config)
	if err != nil {
		return "", fmt.Errorf("config: %w", err)
	}
	rootQ, err := systemdWorkingDirectoryValue(root)
	if err != nil {
		return "", fail("SparkyControllerUnit root: %v", err)
	}
	envFileQ, err := systemdBarePathValue(filepath.Join(root, "vllm", "api.env"))
	if err != nil {
		return "", fmt.Errorf("environment file: %w", err)
	}
	var b strings.Builder
	b.WriteString("# Generated by Sparky. Do not edit; regenerate instead.\n")
	b.WriteString("[Unit]\n")
	b.WriteString("Description=Sparky controller\n")
	b.WriteString("Wants=network-online.target\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("\n[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("User=" + user + "\n")
	b.WriteString("WorkingDirectory=" + rootQ + "\n")
	b.WriteString("EnvironmentFile=" + envFileQ + "\n")
	b.WriteString("ExecStart=:" + helperQ + " --json node controller --file " + configQ + "\n")
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=3\n")
	b.WriteString("UMask=0077\n")
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")
	return b.String(), nil
}

// SparkyModelUnitTemplate renders the per-model native unit template
// (sparky-model@.service). The literal systemd specifier %i is preserved:
// the instance name is the model. ExecStop performs the native bounded
// stop, ExecStopPost handles abnormal-death cleanup per rank, KillMode=mixed
// and TimeoutStopSec=300 bound each stop independently. Restart=no and no
// [Install]/WantedBy section: units are placed directly by the native
// implementation, never enabled or autoloaded.
func SparkyModelUnitTemplate(user, helper, config string) (string, error) {
	if user == "" {
		return "", fail("SparkyModelUnitTemplate: empty user")
	}
	if err := validateServiceUser(user); err != nil {
		return "", err
	}
	if err := validateAbsolutePath("helper", helper); err != nil {
		return "", fail("SparkyModelUnitTemplate helper: %v", err)
	}
	if err := validateAbsolutePath("config", config); err != nil {
		return "", fail("SparkyModelUnitTemplate config: %v", err)
	}
	helperQ, err := systemdExecValue(helper)
	if err != nil {
		return "", fmt.Errorf("helper: %w", err)
	}
	configQ, err := systemdExecValue(config)
	if err != nil {
		return "", fmt.Errorf("config: %w", err)
	}
	var b strings.Builder
	b.WriteString("# Generated by Sparky. Do not edit; regenerate instead.\n")
	b.WriteString("[Unit]\n")
	b.WriteString("Description=Sparky native model %i\n")
	b.WriteString("\n[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("User=" + user + "\n")
	b.WriteString("ExecStart=:" + helperQ + " --json node supervise --file " + configQ + " --model %i\n")
	b.WriteString("ExecStop=:" + helperQ + " --json node lifecycle --file " + configQ + " --model %i --action stop\n")
	b.WriteString("ExecStopPost=:" + helperQ + " --json node lifecycle --file " + configQ + " --model %i --action cleanup\n")
	b.WriteString("KillMode=mixed\n")
	b.WriteString("TimeoutStopSec=300\n")
	b.WriteString("Restart=no\n")
	return b.String(), nil
}

// ComfyUIDropin renders the ComfyUI service drop-in. It points
// SPARKY_COMFY_CONFIG at the ComfyUI plugin config (read by the embedded
// comfy_guard.py inside the ComfyUI process) and drains the Go node via
// helper --json node drain-comfy in ExecStop: draining must run BEFORE the
// engine is killed, never after. It does not override User= — the original
// ComfyUI service user is preserved. TimeoutStopSec=infinity preserves
// queued and active image work for as long as the drain needs. No LLM/audio
// behavior and no cgroup changes.
func ComfyUIDropin(helper, nodePath, pluginPath string) (string, error) {
	if err := validateAbsolutePath("helper", helper); err != nil {
		return "", fail("ComfyUIDropin helper: %v", err)
	}
	if err := validateAbsolutePath("node path", nodePath); err != nil {
		return "", fail("ComfyUIDropin node path: %v", err)
	}
	if err := validateAbsolutePath("plugin path", pluginPath); err != nil {
		return "", fail("ComfyUIDropin plugin path: %v", err)
	}
	helperQ, err := systemdExecValue(helper)
	if err != nil {
		return "", fmt.Errorf("helper: %w", err)
	}
	nodeQ, err := systemdExecValue(nodePath)
	if err != nil {
		return "", fmt.Errorf("node path: %w", err)
	}
	pluginQ, err := systemdPathValue(pluginPath)
	if err != nil {
		return "", fmt.Errorf("plugin path: %w", err)
	}
	var b strings.Builder
	b.WriteString("# Generated by Sparky. Do not edit; regenerate instead.\n")
	b.WriteString("[Service]\n")
	b.WriteString("Environment=\"SPARKY_COMFY_CONFIG=" + pluginQ[1:] + "\n")
	b.WriteString("ExecStop=:" + helperQ + " --json node drain-comfy --file " + nodeQ + "\n")
	b.WriteString("TimeoutStopSec=infinity\n")
	return b.String(), nil
}

// LegacyModelShim renders the transitional shell shim used by the legacy
// Python manager hook. It validates the argument shape exactly, contains no
// model logic and imports nothing from Python Sparky, and execs the Go
// helper: run -> supervise, stop/cleanup -> lifecycle --action. Unknown
// arguments produce one fixed JSON error on stderr and exit 2. Native mode
// never invokes this shim.
func LegacyModelShim(helper, config string) (string, error) {
	if err := validateAbsolutePath("helper", helper); err != nil {
		return "", fail("LegacyModelShim helper: %v", err)
	}
	if err := validateAbsolutePath("config", config); err != nil {
		return "", fail("LegacyModelShim config: %v", err)
	}
	// Inside the shell script the values are shell-quoted, not systemd-
	// quoted; systemdUnitValue was used only to reject control characters.
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Generated by Sparky transitional shim. No Python Sparky import; no model logic here.\n")
	b.WriteString("SPARKY_HELPER=" + shellQuote(helper) + "\n")
	b.WriteString("SPARKY_CONFIG=" + shellQuote(config) + "\n")
	b.WriteString("if [ \"$#\" -ne 2 ]; then\n")
	b.WriteString("\tprintf '%s\\n' '{\"error\":\"usage: sparky-model {run|stop|cleanup} MODEL\"}' >&2\n")
	b.WriteString("\texit 2\n")
	b.WriteString("fi\n")
	b.WriteString("case \"$1\" in\n")
	b.WriteString("\trun)\n")
	b.WriteString("\t\texec \"$SPARKY_HELPER\" --json node supervise --file \"$SPARKY_CONFIG\" --model \"$2\"\n")
	b.WriteString("\t\t;;\n")
	b.WriteString("\tstop|cleanup)\n")
	b.WriteString("\t\texec \"$SPARKY_HELPER\" --json node lifecycle --file \"$SPARKY_CONFIG\" --model \"$2\" --action \"$1\"\n")
	b.WriteString("\t\t;;\n")
	b.WriteString("\t*)\n")
	b.WriteString("\t\tprintf '%s\\n' '{\"error\":\"unknown action; expected run, stop, or cleanup\"}' >&2\n")
	b.WriteString("\t\texit 2\n")
	b.WriteString("\t\t;;\n")
	b.WriteString("esac\n")
	return b.String(), nil
}
