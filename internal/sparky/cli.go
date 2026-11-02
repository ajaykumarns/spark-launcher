package sparky

// Pure CLI argument parsing (stage 1 of the public CLI port of
// src/spark_launcher/cli.py). This file performs no I/O: it never reads
// configuration, opens processes, touches the network or writes files. The
// only non-parser helper used is checkHarnessPassthrough from harness.go,
// which is a pure argument validator.
//
// Stage 2 (remote SSH client) and stage 3 (Main/dispatch) are implemented
// separately on top of the cliInvocation struct defined here.

import (
	"fmt"
	"strconv"
	"strings"
)

// cliVersion matches the Python package version.
const cliVersion = "0.6.2"

// cliInvocation is the structured result of parsing one command line.
type cliInvocation struct {
	// Command is the main command word ("status", "claude", "node", ...).
	// It is empty only when no command was given (help/version/no args).
	Command string
	// Sub is the nested action for commands with a required sub-action
	// ("ocr", "config", "admin").
	Sub string
	// Positional holds the positional arguments in order, including the
	// sub-command word itself as the first element for ocr, config, and
	// admin (Sub is the first positional, not separate from it).
	Positional []string
	// Values holds string option values keyed without the leading "--"
	// (for example Values["model"] for --model). Defaults are applied.
	// The alias -m is stored under "model".
	Values map[string]string
	// Flags records every option that appeared on the command line,
	// boolean and valued, keyed by canonical name without the leading
	// "--" (so -m records "model").
	Flags map[string]bool
	// JSON reports whether --json appeared before the first "--".
	JSON bool
	// Color is the last valid --color value before the first "--", or ""
	// when the option was not given. Every occurrence is validated.
	Color string
	// Help reports -h/--help; Version reports --version. Stage 3 prints
	// the corresponding text and exits 0.
	Help    bool
	Version bool
	// Native holds the words after the first "--" for a harness command,
	// verbatim, including a literal second "--" prompt delimiter. Empty
	// for every other command.
	Native []string
	// NodeArgs holds the raw words after "node" with only the global
	// output options (--json, --color) removed; words after a "--" are
	// preserved verbatim. They are handed to the private node dispatcher.
	NodeArgs []string
	// Limit is the validated positive --limit for the logs command.
	Limit int
}

// usageError is a command syntax error. Its exit code is 2, distinct from
// runtime failures (1) and interruption (130).
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }
func (e *usageError) ExitCode() int { return 2 }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// cliUsageSuffix is appended to syntax errors so the message stays
// actionable without Go flag-package chatter.
const cliUsageSuffix = ". Run sparky --help for usage."

// cliHarnessCommands are the four harnesses, in display order.
var cliHarnessCommands = []string{"claude", "hermes", "codex", "opencode"}

// cliCommandList is every top-level command word.
var cliCommandList = append([]string{
	"status", "models", "devices", "ocr", "plan", "load", "unload",
	"image-profile", "config", "run", "sync", "logs", "doctor", "ui",
	"init", "admin", "node",
}, cliHarnessCommands...)

// cliSpecs describes the accepted flags and positionals per command. Every
// command also accepts the global output options --json and --color before,
// after, or nested under it. Sub-command-specific restrictions (ocr flags
// with install, admin target with ssh, config sub-grammar) are enforced in
// cliPostValidate.
type cliSpec struct {
	posNames    []string
	posMin      int
	posMax      int
	posChoices  map[int][]string
	strFlags    []string
	boolFlags   []string
	flagChoices map[string][]string
	required    []string
	defaults    map[string]string
}

var (
	cliOCRActions     = []string{"install", "status", "start", "stop"}
	cliPlacements     = []string{"spark01", "spark02", "both"}
	cliWorkflowNodes  = []string{"spark01", "spark02"}
	cliConfigActions  = []string{"show", "validate", "placement"}
	cliAdminActions   = []string{"ssh", "enable-harnesses"}
	cliProfileModes   = []string{"show", "shared", "dedicated"}
	cliColorChoices   = []string{"auto", "always", "never"}
	cliPlanActions    = []string{"load", "unload"}
	cliGlobalStrFlags = []string{"--color"}
	cliGlobalBools    = []string{"--json"}
)

func harnessSpec() *cliSpec {
	return &cliSpec{
		posMax:    0,
		strFlags:  []string{"--model", "-m", "--project"},
		boolFlags: []string{"--offline", "--dry-run", "--prepare"},
		defaults:  map[string]string{"project": "."},
	}
}

var cliSpecs = map[string]*cliSpec{
	"status":  {posMax: 0},
	"models":  {posMax: 1, posNames: []string{"model"}, boolFlags: []string{"--offline", "--installed"}},
	"devices": {posMax: 0},
	"ocr": {posMin: 1, posMax: 1, posNames: []string{"action"},
		posChoices: map[int][]string{0: cliOCRActions},
		strFlags:   []string{"--repo", "--ref"}, boolFlags: []string{"--rebuild"}},
	"plan": {posMin: 2, posMax: 2, posNames: []string{"action", "model"},
		posChoices:  map[int][]string{0: cliPlanActions},
		strFlags:    []string{"--on"},
		flagChoices: map[string][]string{"--on": cliPlacements}},
	"load": {posMin: 1, posMax: 1, posNames: []string{"model"},
		strFlags:    []string{"--on"},
		flagChoices: map[string][]string{"--on": cliPlacements},
		boolFlags:   []string{"--force"}},
	"unload": {posMin: 1, posMax: 1, posNames: []string{"model"}, boolFlags: []string{"--force"}},
	"image-profile": {posMax: 1, posNames: []string{"mode"},
		posChoices: map[int][]string{0: cliProfileModes},
		boolFlags:  []string{"--dry-run"},
		defaults:   map[string]string{"mode": "show"}},
	// config sub-grammar: show [MODEL], validate (no extra), placement MODEL ON.
	"config": {posMin: 1, posMax: 3, posNames: []string{"action", "model", "on"},
		posChoices: map[int][]string{0: cliConfigActions}},
	"run": {posMin: 2, posMax: 2, posNames: []string{"engine", "workflow"},
		posChoices:  map[int][]string{0: []string{"comfyui"}},
		strFlags:    []string{"--on"},
		flagChoices: map[string][]string{"--on": cliWorkflowNodes}},
	"sync":   {posMax: 0},
	"logs":   {posMax: 0, strFlags: []string{"--limit"}},
	"doctor": {posMax: 1, posNames: []string{"harness"}, posChoices: map[int][]string{0: cliHarnessCommands}},
	"ui":     {posMax: 0},
	"init": {posMax: 0,
		strFlags: []string{"--gateway", "--prefix", "--admin-host", "--worker-host", "--remote-root", "--admin-url"},
		required: []string{"--gateway"},
		defaults: map[string]string{"prefix": "garuda/", "admin-host": "", "worker-host": "spark02", "remote-root": "", "admin-url": ""}},
	// admin sub-grammar: ssh [spark01|spark02], enable-harnesses (no target).
	"admin": {posMin: 1, posMax: 2, posNames: []string{"action", "target"},
		posChoices: map[int][]string{0: cliAdminActions}},
	"node":     {}, // parsed by the private node dispatcher
	"claude":   harnessSpec(),
	"hermes":   harnessSpec(),
	"codex":    harnessSpec(),
	"opencode": harnessSpec(),
}

// cliHelpText is the concise top-level usage shown by --help and with no
// arguments.
const cliHelpText = `Manage models on two Sparks and launch coding tools.

Usage:
  sparky COMMAND [options]
  sparky HARNESS [--model NAME] [-- native arguments]
  sparky --json COMMAND        Print one JSON result for scripts

Commands:
  status           Memory, running models, and queued work
  models [NAME]    Model names and limits; --installed lists installed services
  devices          Memory, disk, GPU, and containers on each Spark
  ocr ACTION       OCR service: install|status|start|stop
  plan ACTION NAME Preview load/unload steps without changing services
  load NAME        Load a model after active work finishes (--on NODE for one load, --force cancels conflicts)
  unload NAME      Unload a model after its active work finishes
  image-profile    Shared or dedicated image memory limits
  config ACTION    show [MODEL] | validate | placement MODEL ON
  run comfyui FILE Submit a ComfyUI API workflow JSON file
  sync             Refresh the Hermes Spark provider from the live model list
  logs             Recent local harness launches (--limit N)
  doctor [HARNESS] Check gateway access and harness installation; no inference
  ui               Open the gateway UI (--json prints the URL)
  init             Create the client connection file (--gateway REQUIRED)
  admin ACTION     ssh [spark01|spark02] | enable-harnesses
  HARNESS          claude, hermes, codex, or opencode

Examples:
  sparky status
  sparky models
  sparky plan load glm
  sparky load glm --on spark01
  sparky claude --model qwen -- --continue
  sparky status --json

Native harness arguments go after --, for example:
  sparky claude --model qwen -- --continue`

// cliParse parses a full argument vector into a cliInvocation. It is pure:
// the only effect is argument validation, and the only cross-module call is
// the pure checkHarnessPassthrough guard for harness native arguments.
//
// Token roles drive every decision: an option value is never mistaken for a
// flag or command, so opaque values such as a project path named "claude"
// stay values. Option scanning stops at the first "--": for a harness
// command everything after it is native arguments (kept verbatim, including
// a second literal "--"); for any other command it ends the options and the
// remaining words are positionals, which are still validated.
func cliParse(args []string) (*cliInvocation, error) {
	// On parse errors the partially filled invocation is still returned so
	// the dispatcher can honor the requested output mode by token role
	// (--json seen before failing, the private node stream, --color).
	inv := &cliInvocation{Values: map[string]string{}, Flags: map[string]bool{}}

	// Global zone: only global output options may appear before the
	// command word.
	index := 0
	for ; index < len(args); index++ {
		w := args[index]
		if !strings.HasPrefix(w, "-") || w == "-" {
			break // command token (or an error below for "--")
		}
		switch {
		case w == "--":
			return inv, usagef("Choose a command%s", cliUsageSuffix)
		case w == "--json":
			inv.JSON = true
		case w == "--version":
			inv.Version = true
		case w == "--help", w == "-h":
			inv.Help = true
		case strings.HasPrefix(w, "--help="), strings.HasPrefix(w, "-h="):
			return inv, usagef("Option --help does not take a value%s", cliUsageSuffix)
		case w == "--color":
			v, err := cliColorArgument(args, &index)
			if err != nil {
				return inv, err
			}
			inv.Color = v
		case strings.HasPrefix(w, "--color="):
			v := strings.TrimPrefix(w, "--color=")
			if !contains(cliColorChoices, v) {
				return inv, usagef("Option --color must be one of %s%s", strings.Join(cliColorChoices, ", "), cliUsageSuffix)
			}
			inv.Color = v
		case strings.HasPrefix(w, "--json="):
			return inv, usagef("Option --json does not take a value%s", cliUsageSuffix)
		default:
			return inv, usagef("Unknown option: %s%s", w, cliUsageSuffix)
		}
	}
	if index == len(args) {
		// No command word. No arguments, --help, or --version select
		// the corresponding text (stage 3 prints and exits 0); a lone
		// global option such as --json is a usage error.
		if len(args) == 0 {
			inv.Help = true
		}
		if !inv.Help && !inv.Version {
			return inv, usagef("Choose a command%s", cliUsageSuffix)
		}
		return inv, nil
	}

	command := args[index]
	if !contains(cliCommandList, command) {
		hint := ""
		if closest := clientClosestMatch(command, cliCommandList); closest != "" {
			hint = " Did you mean " + closest + "?"
		}
		return inv, usagef("Unknown command: %s.%s Run sparky --help for usage.", command, hint)
	}
	inv.Command = command

	// Locate the first "--" after the command; it delimits native words
	// for a harness and ends options for every other command.
	delim := -1
	for i := index + 1; i < len(args); i++ {
		if args[i] == "--" {
			delim = i
			break
		}
	}

	if command == "node" {
		// Private protocol words stay raw; only global output options
		// are removed, and never beyond a literal "--".
		stripped, err := cliStripGlobalOutput(inv, args[index+1:])
		if err != nil {
			return inv, err
		}
		inv.NodeArgs = stripped
		return inv, nil
	}

	before, tail := args[index+1:], []string{}
	if delim > index {
		before, tail = args[index+1:delim], args[delim+1:]
	}
	// Parse the pre-delimiter option zone first, then append the tail
	// words, and only afterwards validate counts, choices, required
	// options, defaults and sub-grammars. This makes "load -- glm" and
	// "load glm" identical: the delimiter only changes where words come
	// from, never how they are classified.
	if err := cliParseInto(inv, command, before); err != nil {
		return inv, err
	}
	if contains(cliHarnessCommands, command) {
		// Native words are attached exactly, verbatim; a second "--"
		// inside them stays a literal prompt delimiter.
		inv.Native = append([]string{}, tail...)
		if err := checkHarnessPassthrough(command, inv.Native); err != nil {
			return inv, err
		}
	} else {
		// After --, remaining words are plain positionals.
		for _, w := range tail {
			if err := cliAddPositional(inv, w); err != nil {
				return inv, err
			}
		}
	}
	if err := cliFinalize(inv, command); err != nil {
		return inv, err
	}
	return inv, nil
}

// cliFinalize applies defaults and validates everything that depends on
// the complete word list: minimum positionals, required options, the logs
// limit, sub-command derivation, and the cross-flag grammar rules. A help
// request is never rejected solely for missing arguments.
func cliFinalize(inv *cliInvocation, command string) error {
	sp := cliSpecs[command]
	for key, d := range sp.defaults {
		if _, ok := inv.Values[key]; !ok {
			inv.Values[key] = d
		}
	}
	if !inv.Help {
		if len(inv.Positional) < sp.posMin {
			return usagef("Missing argument: %s%s", sp.posNames[len(inv.Positional)], cliUsageSuffix)
		}
		for _, name := range sp.required {
			if !inv.Flags[name[2:]] {
				return usagef("Option %s is required%s", name, cliUsageSuffix)
			}
		}
	}
	if command == "logs" {
		raw, ok := inv.Values["limit"]
		switch {
		case !ok:
			inv.Limit = 10
		default:
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				return usagef("Option --limit needs a positive whole number%s", cliUsageSuffix)
			}
			inv.Limit = n
		}
	}
	if len(inv.Positional) > 0 && contains([]string{"ocr", "config", "admin"}, command) {
		inv.Sub = inv.Positional[0]
	}
	return cliPostValidate(inv, command)
}

// cliColorArgument consumes one "--color VALUE" pair (the index is advanced
// past the value) and validates the choice.
func cliColorArgument(args []string, index *int) (string, error) {
	if *index+1 >= len(args) {
		return "", usagef("Option --color needs a value%s", cliUsageSuffix)
	}
	*index++
	v := args[*index]
	if !contains(cliColorChoices, v) {
		return "", usagef("Option --color must be one of %s%s", strings.Join(cliColorChoices, ", "), cliUsageSuffix)
	}
	return v, nil
}

// cliAddPositional appends one positional word with count and enum checks.
func cliAddPositional(inv *cliInvocation, w string) error {
	sp := cliSpecs[inv.Command]
	if len(inv.Positional) >= sp.posMax {
		return usagef("Unexpected argument: %s%s", w, cliUsageSuffix)
	}
	if choices, ok := sp.posChoices[len(inv.Positional)]; ok && !contains(choices, w) {
		return usagef("Argument %s must be one of %s%s", sp.posNames[len(inv.Positional)], strings.Join(choices, ", "), cliUsageSuffix)
	}
	inv.Positional = append(inv.Positional, w)
	return nil
}

// cliParseInto parses a command's own option zone into the invocation.
// It performs no validation beyond per-token checks (unknown options,
// invalid choices, missing values); defaults, counts, and sub-grammar
// rules are applied later by cliFinalize once the tail is included.
func cliParseInto(inv *cliInvocation, command string, words []string) error {
	sp := cliSpecs[command]
	for i := 0; i < len(words); i++ {
		w := words[i]
		if !strings.HasPrefix(w, "-") || w == "-" {
			if err := cliAddPositional(inv, w); err != nil {
				return err
			}
			continue
		}
		name, inline, hasInline := strings.Cut(w, "=")
		canonical := ""
		switch {
		case name == "--help" || name == "-h":
			if hasInline {
				return usagef("Option --help does not take a value%s", cliUsageSuffix)
			}
			inv.Help = true
			continue
		case name == "--json":
			if hasInline {
				return usagef("Option --json does not take a value%s", cliUsageSuffix)
			}
			inv.JSON = true
			continue
		case name == "--color":
			if hasInline {
				v := inline
				if !contains(cliColorChoices, v) {
					return usagef("Option --color must be one of %s%s", strings.Join(cliColorChoices, ", "), cliUsageSuffix)
				}
				inv.Color = v
				continue
			}
			v, err := cliColorArgument(words, &i)
			if err != nil {
				return err
			}
			inv.Color = v
			continue
		case contains(sp.boolFlags, name):
			if hasInline {
				return usagef("Option %s does not take a value%s", name, cliUsageSuffix)
			}
			canonical = name[2:]
		case contains(sp.strFlags, name):
			if !hasInline {
				if i+1 >= len(words) {
					return usagef("Option %s needs a value%s", name, cliUsageSuffix)
				}
				i++
				inline = words[i]
			}
			if choices, ok := sp.flagChoices[name]; ok && !contains(choices, inline) {
				return usagef("Option %s must be one of %s%s", name, strings.Join(choices, ", "), cliUsageSuffix)
			}
			if name == "-m" {
				name = "--model"
			}
			inv.Values[name[2:]] = inline
			canonical = name[2:]
		default:
			return usagef("Unknown option: %s%s", name, cliUsageSuffix)
		}
		inv.Flags[canonical] = true
	}
	return nil
}

// cliPostValidate enforces sub-command grammars and cross-flag rules that a
// flat per-command spec cannot express. A help request is never rejected
// solely for missing required arguments.
func cliPostValidate(inv *cliInvocation, command string) error {
	pos := inv.Positional
	switch command {
	case "models":
		if inv.Flags["installed"] && (inv.Flags["offline"] || len(pos) > 0) {
			return usagef("--installed cannot be combined with --offline or a model name. Use config show NAME for installed settings.")
		}
	case "config":
		if inv.Help {
			return nil
		}
		switch pos[0] {
		case "show":
			if len(pos) > 2 {
				return usagef("Unexpected argument: %s%s", pos[2], cliUsageSuffix)
			}
		case "validate":
			if len(pos) > 1 {
				return usagef("Unexpected argument: %s%s", pos[1], cliUsageSuffix)
			}
		case "placement":
			if len(pos) < 3 {
				return usagef("Missing argument: %s%s", posNamesFor(command)[len(pos)], cliUsageSuffix)
			}
			if !contains(cliPlacements, pos[2]) {
				return usagef("Argument on must be one of %s%s", strings.Join(cliPlacements, ", "), cliUsageSuffix)
			}
		}
	case "ocr":
		if inv.Sub != "install" {
			for _, name := range []string{"repo", "ref", "rebuild"} {
				if inv.Flags[name] {
					return usagef("Option --%s is only valid with 'sparky ocr install'.", name)
				}
			}
		}
	case "admin":
		if inv.Help {
			return nil
		}
		if pos[0] == "ssh" {
			if len(pos) > 2 {
				return usagef("Unexpected argument: %s%s", pos[2], cliUsageSuffix)
			}
			if len(pos) == 2 && !contains(cliPlacements[:2], pos[1]) {
				return usagef("Argument target must be one of %s%s", strings.Join(cliPlacements[:2], ", "), cliUsageSuffix)
			}
		} else if len(pos) > 1 {
			return usagef("Unexpected argument: %s%s", pos[1], cliUsageSuffix)
		}
	case "claude", "hermes", "codex", "opencode":
		if inv.Flags["dry-run"] && inv.Flags["prepare"] {
			return usagef("--dry-run and --prepare cannot be used together.")
		}
		if !inv.Help && inv.JSON && !inv.Flags["dry-run"] && !inv.Flags["prepare"] {
			return usagef("Use --json with --dry-run or --prepare. Pass native harness options after --.")
		}
	}
	return nil
}

// posNamesFor returns the positional names of a command's spec.
func posNamesFor(command string) []string { return cliSpecs[command].posNames }

// cliNodeBoolFlags are the boolean option names used by node actions.
// Every other option word (known value flags such as --model, --file,
// --payload, --root, --dir, --action, and unknown private ones, so the
// dispatcher can report strict errors itself) takes a value.
var cliNodeBoolFlags = map[string]bool{"force": true}

// cliStripGlobalOutput removes only the global output options from the
// node words; everything after a literal "--" stays verbatim so no
// dispatcher value is ever consumed. Occurrences are validated. --json
// remains a sparky output switch for the whole word list (until "--") and
// is stripped wherever it is not a flag value; --color and --help are only
// ours in the leading option zone, and after the first dispatcher word
// they belong to the dispatcher and pass through verbatim.
//
// An option word is only treated as a global output option when it really
// plays that role. A token that follows a value flag (known ones such as
// --model/--file/--payload/--root/--dir/--action, and unknown private
// ones, so the dispatcher can report strict errors itself) is preserved
// verbatim as that flag's value, even if it is spelled --json or --color.
// Known node booleans like --force never consume a following token.
func cliStripGlobalOutput(inv *cliInvocation, words []string) ([]string, error) {
	out := []string{}
	leading := true
	for i := 0; i < len(words); i++ {
		w := words[i]
		if w == "--" {
			return append(out, words[i:]...), nil
		}
		if !strings.HasPrefix(w, "-") || w == "-" {
			out = append(out, w)
			leading = false
			continue
		}
		name, inline, hasInline := strings.Cut(w, "=")
		switch name {
		case "--json":
			if hasInline {
				return nil, usagef("Option --json does not take a value%s", cliUsageSuffix)
			}
			inv.JSON = true
			continue
		case "--help", "-h":
			if !leading {
				// The dispatcher owns --help after its action word.
				out = append(out, w)
				continue
			}
			if hasInline {
				return nil, usagef("Option --help does not take a value%s", cliUsageSuffix)
			}
			inv.Help = true
			continue
		case "--color":
			if !leading {
				// The dispatcher owns --color after its action word.
				out = append(out, w)
				continue
			}
			if hasInline {
				if !contains(cliColorChoices, inline) {
					return nil, usagef("Option --color must be one of %s%s", strings.Join(cliColorChoices, ", "), cliUsageSuffix)
				}
				inv.Color = inline
				continue
			}
			v, err := cliColorArgument(words, &i)
			if err != nil {
				return nil, err
			}
			inv.Color = v
			continue
		}
		// A private node option: keep it, and keep its opaque value with
		// it so globals are never stripped from a value position.
		out = append(out, w)
		if !hasInline && i+1 < len(words) && !cliNodeBoolFlags[name[2:]] {
			out = append(out, words[i+1])
			i++
		}
		leading = false
	}
	return out, nil
}
