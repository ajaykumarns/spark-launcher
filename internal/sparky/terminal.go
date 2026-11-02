package sparky

// Human terminal rendering. JSON output never enters this file; the CLI
// prints encoded objects directly and bypasses terminalRender entirely.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// terminalOptions configures one render pass.
type terminalOptions struct {
	Color string // "auto", "always", "never" ("" means auto)
	Width int    // terminal width in columns; <=0 means 100
	TTY   bool   // whether stdout is a terminal (used for auto mode)
}

const ansiReset = "\x1b[0m"

var ansiColors = map[string]string{
	"red":           "\x1b[31;1m",
	"green":         "\x1b[32;1m",
	"yellow":        "\x1b[33;1m",
	"cyan":          "\x1b[36;1m",
	"bright_black":  "\x1b[90m",
	"bright_red":    "\x1b[91;1m",
	"bright_green":  "\x1b[92;1m",
	"bright_yellow": "\x1b[93;1m",
	"bright_cyan":   "\x1b[96;1m",
	"dim":           "\x1b[2m",
	"bold":          "\x1b[1m",
}

// clean removes control characters and non-printable runes so model names,
// user strings or server data can never inject terminal escape sequences.
// Every rune that is not printable per unicode.IsPrint (including ASCII and
// C1 controls such as U+009B, and line separators U+2028/U+2029) is replaced
// with a space, matching Python's str.isprintable sanitization. Newlines and
// tabs therefore do not survive clean; views that need line breaks must split
// trusted text into lines first and clean each line (see cleanLines).
func clean(value any) string {
	s := ""
	switch v := value.(type) {
	case string:
		s = v
	case nil:
		return ""
	default:
		s = fmt.Sprintf("%v", v)
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return b.String()
}

// cleanLines sanitizes multi-line trusted text, preserving the intended line
// structure. Each line is cleaned independently, so a newline in data can
// never smuggle an unsanitized segment through as part of another line.
func cleanLines(s string) []string {
	parts := strings.Split(s, "\n")
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = clean(p)
	}
	return out
}

// useColor resolves the color mode. Explicit always/never wins; auto is
// disabled by NO_COLOR or TERM=dumb, forced on by FORCE_COLOR, else by TTY.
func useColor(opts terminalOptions) bool {
	mode := opts.Color
	if mode != "auto" && mode != "always" && mode != "never" {
		mode = "auto"
	}
	if mode != "auto" {
		return mode == "always"
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if lookupEnv("TERM") == "dumb" {
		return false
	}
	if lookupEnv("FORCE_COLOR") != "" && lookupEnv("FORCE_COLOR") != "0" {
		return true
	}
	return opts.TTY
}

// paint applies a space-separated style list such as "bold bright_cyan".
func paint(enabled bool, style, text string) string {
	if !enabled || style == "" {
		return text
	}
	var b strings.Builder
	applied := false
	for _, name := range strings.Fields(style) {
		if code, ok := ansiColors[name]; ok {
			b.WriteString(code)
			applied = true
		}
	}
	if !applied {
		return text
	}
	return b.String() + text + ansiReset
}

// stateStyle maps a state reading to a color class.
func stateStyle(value string) string {
	v := strings.ToLower(value)
	for _, word := range []string{"unavailable", "failed", "error", "unknown"} {
		if strings.Contains(v, word) {
			return "bright_red"
		}
	}
	for _, word := range []string{"waiting", "paused", "prepared", "not verified", "queued"} {
		if strings.Contains(v, word) && !strings.HasPrefix(v, "idle") {
			return "bright_yellow"
		}
	}
	switch v {
	case "stopped", "disabled", "-", "on demand", "not finished":
		return "dim"
	}
	switch {
	case v == "ready" || v == "running" || v == "ok" || v == "enabled" || v == "yes" || strings.HasPrefix(v, "executing"):
		return "bright_green"
	case strings.HasPrefix(v, "idle"):
		return "bright_cyan"
	}
	return ""
}

// ---- width-aware plain tables ----

// wrapText folds text to at most width runes per line, breaking on spaces
// when possible and on a rune boundary otherwise. Width is counted in runes
// consistently with visualLen (minus ANSI spans), so multi-byte text is
// never split inside a UTF-8 sequence and no character is dropped.
func wrapText(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	var lines []string
	for _, para := range strings.Split(s, "\n") {
		runes := []rune(para)
		for len(runes) > width {
			cut := -1
			for i := width; i > 0; i-- {
				if runes[i-1] == ' ' {
					cut = i - 1
					break
				}
			}
			if cut <= 0 {
				cut = width
			}
			lines = append(lines, string(runes[:cut]))
			rest := runes[cut:]
			for len(rest) > 0 && rest[0] == ' ' {
				rest = rest[1:]
			}
			runes = rest
		}
		lines = append(lines, string(runes))
	}
	return lines
}

type styledRow []string

// table renders headers/rows as an ASCII grid. Rows with fewer cells are
// padded; extra cells are kept as continuation text on the first column.
func table(headers []string, rows []styledRow, title string, width int, color bool) string {
	for i := range headers {
		headers[i] = clean(headers[i])
	}
	for i, row := range rows {
		cells := make([]string, len(row))
		for j, cell := range row {
			cells[j] = clean(cell)
		}
		rows[i] = cells
	}
	if width <= 0 {
		width = 100
	}
	if width < 60 {
		// Stacked labeled cards keep every value on narrow screens.
		cards := make([]string, 0, len(rows))
		for _, row := range rows {
			cards = append(cards, keyValues(orDefault(title, "Details"), labelCells(headers, row), "cyan", width, color))
		}
		return strings.Join(cards, "\n")
	}
	// Column widths.
	count := len(headers)
	for _, row := range rows {
		if len(row) > count {
			count = len(row)
		}
	}
	widths := make([]int, count)
	for i, h := range headers {
		widths[i] = visualLen(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if visualLen(cell) > widths[i] {
				widths[i] = visualLen(cell)
			}
		}
	}
	// Budget: shrink widest columns until the grid fits (never below 3).
	const padding, overhead = 3, 4 // " x " per column plus border pipe ends
	for sum(widths)+padding*count+overhead > width {
		widest := 0
		for i, w := range widths {
			if w > widths[widest] {
				widest = i
			}
		}
		if widths[widest] <= 3 {
			break
		}
		widths[widest]--
	}
	// Numeric columns are right-aligned.
	right := make([]bool, count)
	for i, h := range headers {
		for _, word := range []string{"CONTEXT", "OUTPUT", "CALLS", "SLOTS", "SEQUENCES", "RAM", "BUDGET"} {
			if strings.Contains(h, word) {
				right[i] = true
			}
		}
	}
	wrap := make([][][]string, len(rows))
	rowHeights := make([]int, len(rows))
	for r := range rows {
		row := rows[r]
		cells := make([][]string, count)
		height := 1
		for i := 0; i < count; i++ {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			lines := wrapText(cell, widths[i])
			if len(lines) > height {
				height = len(lines)
			}
			cells[i] = lines
		}
		wrap[r] = cells
		rowHeights[r] = height
	}
	var b strings.Builder
	divider := gridLine(widths, count)
	writeLine := func(cells [][]string, height int, firstBold bool) {
		for line := 0; line < height; line++ {
			b.WriteString("|")
			for i := 0; i < count; i++ {
				text := ""
				if line < len(cells[i]) {
					text = cells[i][line]
				}
				pad := widths[i] - visualLen(text)
				style := ""
				if i == 0 && firstBold {
					style = "bold"
				} else if i > 0 && line == 0 && line < len(cells[i]) {
					style = stateStyle(text)
				}
				text = paint(color, style, text)
				if right[i] && line >= len(cells[i])-1 && line < height {
					b.WriteString(strings.Repeat(" ", pad+1) + text + " |")
				} else {
					b.WriteString(" " + text + strings.Repeat(" ", pad+1) + "|")
				}
			}
			b.WriteString("\n")
		}
	}
	if title != "" {
		b.WriteString(paint(color, "bright_cyan", title) + "\n")
	}
	b.WriteString(divider)
	headCells := make([][]string, count)
	for i, h := range headers {
		headCells[i] = []string{paint(color, "bright_cyan", h)}
	}
	writeLine(headCells, 1, false)
	b.WriteString(divider)
	for r := range rows {
		writeLine(wrap[r], rowHeights[r], true)
		if len(rows) > 1 && r < len(rows)-1 && (headers[0] == "STEP" || headers[0] == "TIME (LOCAL)") {
			b.WriteString(divider)
		}
	}
	b.WriteString(strings.TrimSuffix(divider, "\n"))
	return b.String()
}

func orDefault(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}

func labelCells(headers, row []string) [][2]string {
	out := make([][2]string, 0, len(headers))
	for i, h := range headers {
		v := ""
		if i < len(row) {
			v = row[i]
		}
		out = append(out, [2]string{h, v})
	}
	return out
}

func gridLine(widths []int, count int) string {
	var b strings.Builder
	b.WriteString("+")
	for i := 0; i < count; i++ {
		b.WriteString(strings.Repeat("-", widths[i]+2) + "+")
	}
	b.WriteString("\n")
	return b.String()
}

func sum(xs []int) int {
	out := 0
	for _, x := range xs {
		out += x
	}
	return out
}

// visualLen ignores ANSI sequences so colored cells still pad correctly.
func visualLen(s string) int {
	n := 0
	inEscape := false
	for _, r := range s {
		if inEscape {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		if r == 0x1b {
			inEscape = true
			continue
		}
		n++
	}
	return n
}

// ---- cards, notices, help ----

func keyValues(title string, rows [][2]string, tone string, width int, color bool) string {
	var b strings.Builder
	inner := width - 4
	if inner < 8 {
		inner = 8
	}
	label := 0
	for _, kv := range rows {
		if l := visualLen(kv[0]) + 1; l > label {
			label = l
		}
	}
	// A long label must never force the card wider than the screen; such
	// rows fall back to a label line with the value on following lines.
	valueWidth := inner - label - 3
	if valueWidth < 8 {
		valueWidth = 0 // stacked mode for every row
	}
	if title != "" {
		b.WriteString(paint(color, "bold "+tone, title) + "\n")
	}
	b.WriteString("+" + strings.Repeat("-", inner+2) + "+\n")
	for _, kv := range rows {
		labelText := clean(kv[0]) + ":"
		value := clean(kv[1])
		if valueWidth > 0 {
			lines := wrapText(value, valueWidth)
			for i, line := range lines {
				l := labelText
				if i > 0 {
					l = ""
				}
				b.WriteString("| " + paint(color, "dim", padEnd(l, label+1)) + paint(color, stateStyle(line), line) + strings.Repeat(" ", maxInt(0, valueWidth-visualLen(line))+1) + "|\n")
			}
			continue
		}
		b.WriteString("| " + paint(color, "dim", labelText) + strings.Repeat(" ", maxInt(0, inner-visualLen(labelText))+1) + "|\n")
		for _, line := range wrapText(value, inner-2) {
			b.WriteString("|   " + paint(color, stateStyle(line), line) + strings.Repeat(" ", maxInt(0, inner-visualLen(line)-2)+1) + "|\n")
		}
	}
	b.WriteString("+" + strings.Repeat("-", inner+2) + "+")
	return b.String()
}

func padEnd(s string, width int) string {
	pad := width - visualLen(s)
	if pad <= 0 {
		return s
	}
	return s + strings.Repeat(" ", pad)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func heading(title string, color bool, width int) string {
	// Headings wrap so long titles respect narrow screens.
	var b strings.Builder
	for _, line := range wrapText(clean(title), width) {
		b.WriteString(paint(color, "bright_cyan", line) + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func note(message string, tone string, color bool, width int) string {
	// Notes wrap to the requested width so narrow screens keep every word.
	var b strings.Builder
	for _, line := range wrapText(clean(message), width) {
		b.WriteString(paint(color, tone, line) + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func noticeView(title, message, tone string, color bool, width int) string {
	var b strings.Builder
	inner := width - 4
	if inner < 8 {
		inner = 8
	}
	// Split intended lines BEFORE cleaning so newlines are preserved while
	// control characters inside each line are still removed.
	lines := cleanLines(message)
	for _, line := range lines {
		for _, wrapped := range wrapText(line, inner) {
			if l := visualLen(wrapped); l > inner {
				inner = l
			}
		}
	}
	b.WriteString("+" + strings.Repeat("-", inner+2) + "+\n")
	b.WriteString("| " + paint(color, "bold "+tone, padEnd(clean(title), inner)) + " |\n")
	for _, line := range lines {
		for _, wrapped := range wrapText(line, inner) {
			b.WriteString("| " + paint(color, stateStyle(wrapped), wrapped) + strings.Repeat(" ", maxInt(0, inner-visualLen(wrapped))+1) + "|\n")
		}
	}
	b.WriteString("+" + strings.Repeat("-", inner+2) + "+")
	return b.String()
}

func helpView(text string, color bool, width int) string {
	var b strings.Builder
	inner := width - 4
	if inner < 8 {
		inner = 8
	}
	// Intended line structure is fixed first; each line is then cleaned.
	body := cleanLines(strings.TrimRight(text, " \t"))
	for _, line := range body {
		for _, wrapped := range wrapText(line, inner) {
			if l := visualLen(wrapped); l > inner {
				inner = l
			}
		}
	}
	b.WriteString("+" + strings.Repeat("-", inner+2) + "+\n")
	b.WriteString("| " + paint(color, "bold bright_cyan", padEnd("sparky", inner)) + " |\n")
	for _, line := range body {
		for _, wrapped := range wrapText(line, inner) {
			shown := wrapped
			if color {
				shown = colorizeHelp(wrapped)
			}
			b.WriteString("| " + shown + strings.Repeat(" ", maxInt(0, inner-visualLen(wrapped))+1) + "|\n")
		}
	}
	b.WriteString("+" + strings.Repeat("-", inner+2) + "+")
	return b.String()
}

// colorizeHelp highlights the sparky word and --flags in the body text.
func colorizeHelp(line string) string {
	fields := strings.Fields(line)
	for i, f := range fields {
		switch {
		case f == "sparky":
			fields[i] = paint(true, "bright_cyan", f)
		case strings.HasPrefix(f, "--") && len(f) > 3 && f[2] >= 'a' && f[2] <= 'z':
			fields[i] = paint(true, "bright_green", f)
		}
	}
	return strings.Join(fields, " ")
}

// ---- value formatting helpers ----

func memory(value any) string {
	if value == nil || !integer(value) {
		return "unknown"
	}
	return fmt.Sprintf("%.1f GiB", num(value)/1024/1024/1024)
}

func isZero(value any) bool {
	return integer(value) && num(value) == 0
}

func count(value any) string {
	if !integer(value) {
		return "unknown"
	}
	return comma(int64(num(value)))
}

func comma(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	return strings.Join(parts, ",")
}

func allocation(value any) string {
	if value == nil {
		return "unknown"
	}
	if !integer(value) {
		return "unknown"
	}
	n := num(value)
	if n < 1024*1024*1024 {
		return fmt.Sprintf("%.0f MiB", n/1024/1024)
	}
	return fmt.Sprintf("%.1f GiB", n/1024/1024/1024)
}

var kindLabels = Object{"llm": "LLM", "audio": "audio", "comfyui": "ComfyUI", "comfy_model": "image"}

func kindLabel(kind string) string { return str(value(kindLabels, kind, kind)) }

func displayModel(model Object) string {
	aliases := stringsOf(model["aliases"])
	if len(aliases) > 0 {
		return aliases[0]
	}
	id := str(model["id"])
	if i := strings.Index(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// lookupEnv reads an environment variable without os import cycle concerns.
func lookupEnv(key string) string { return envDefault(key, "") }

// hasEnv reports whether the variable exists at all (NO_COLOR semantics).
func hasEnv(key string) bool { _, ok := os.LookupEnv(key); return ok }

// parseTimestamp formats an ISO timestamp in the local zone.
func parseTimestamp(s string) (string, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "", err
	}
	return t.Local().Format("2006-01-02 15:04 MST"), nil
}

// ---- views ----

func formatModelsView(catalog Object, model Object, width int, color bool) string {
	if model != nil {
		aliases := stringsOf(model["aliases"])
		if len(aliases) == 0 {
			aliases = []string{"none"}
		}
		nodes := stringsOf(model["nodes"])
		if len(nodes) == 0 {
			nodes = []string{"automatic routing"}
		}
		inputs := stringsOf(model["inputs"])
		if len(inputs) == 0 {
			inputs = []string{"text"}
		}
		name := str(model["name"])
		if name == "" {
			name = str(model["id"])
		}
		return keyValues(name, [][2]string{
			{"Model ID", str(model["id"])},
			{"Aliases", strings.Join(aliases, ", ")},
			{"Context", count(model["context_tokens"]) + " tokens"},
			{"Output budget", count(model["output_tokens"]) + " tokens"},
			{"Configured devices", strings.Join(nodes, ", ")},
			{"Input", strings.Join(inputs, ", ")},
		}, "cyan", width, color) + "\n" + note("This shows configuration. It does not load a model.", "dim", color, width)
	}
	rows := []styledRow{}
	for _, item := range arr(catalog["models"]) {
		m := obj(item)
		nodes := stringsOf(m["nodes"])
		if len(nodes) == 0 {
			nodes = []string{"automatic"}
		}
		inputs := stringsOf(m["inputs"])
		if len(inputs) == 0 {
			inputs = []string{"text"}
		}
		rows = append(rows, styledRow{displayModel(m), count(m["context_tokens"]), count(m["output_tokens"]), strings.Join(nodes, ", "), strings.Join(inputs, ", ")})
	}
	lines := []string{
		table([]string{"MODEL", "CONTEXT", "OUTPUT", "RUNS ON", "INPUT"}, rows, "Available models", width, color), "",
		"Context and output are token counts. These are configured routes, not running models.",
		"Use sparky models NAME for details. Use sparky status for running models.",
	}
	if boolean(catalog["offline"]) {
		lines = append(lines, "Cached data from "+clean(value(catalog, "fetched_at", "unknown time"))+". Current server state was not checked.")
	}
	for _, service := range arr(catalog["audio_services"]) {
		lines = append(lines, "", formatSpeechService(obj(service), width, color))
	}
	return strings.Join(lines, "\n")
}

func formatSpeechService(service Object, width int, color bool) string {
	speechURL := str(service["speech_url"])
	if speechURL == "" {
		speechURL = strings.TrimRight(str(service["api_base"]), "/") + "/audio/speech"
	}
	apiKey := "see service settings"
	if v, ok := service["api_key_required"]; ok && !boolean(v) {
		apiKey = "not required"
	}
	voices := stringsOf(service["voices"])
	formats := stringsOf(service["formats"])
	device := str(service["device"])
	if device == "" {
		device = "unknown"
	}
	return keyValues("Speech generation", [][2]string{
		{"Model", str(service["model"])},
		{"Default voice", str(service["default_voice"])},
		{"API base", str(service["api_base"])},
		{"Speech URL", speechURL},
		{"Runs on", strings.Join(stringsOf(service["nodes"]), ", ")},
		{"Device", device},
		{"API key", apiKey},
		{"Voices", strings.Join(voices, ", ")},
		{"Formats", strings.Join(formats, ", ")},
	}, "cyan", width, color)
}

func formatInventory(data Object, width int, color bool) string {
	rows := []styledRow{}
	for _, name := range keys(obj(data["models"])) {
		state := obj(obj(data["models"])[name])
		kind := str(state["kind"])
		status := str(value(state, "state", "unknown"))
		if kind == "comfy_model" {
			status = "on demand"
		}
		rows = append(rows, styledRow{name, orDefault(kindLabel(kind), kind), status, str(value(state, "desired", "unknown"))})
	}
	return strings.Join([]string{
		table([]string{"MODEL / SERVICE", "TYPE", "STATE", "CONFIGURED ON"}, rows, "Installed models and services", width, color), "",
		"Image weights load when a ComfyUI workflow runs. Their memory state is not inferred from installed files.",
		"Use sparky status to see the actual devices in use.",
	}, "\n")
}

func formatPlan(data Object, width int, color bool) string {
	title := "Plan: " + str(data["action"]) + " " + str(data["workload"]) + " on " + strings.Join(stringsOf(data["nodes"]), ", ")
	lines := []string{heading(title, color, width), note("No services were changed.", "dim", color, width)}
	if wait := stringsOf(data["wait_for"]); len(wait) > 0 {
		lines = append(lines, "Wait for active work: "+clean(strings.Join(wait, ", ")))
	}
	descriptions := map[string]func(Object, string) string{
		"unload": func(step Object, name string) string { return "Unload " + name + " after its active requests finish." },
		"keep_running": func(step Object, name string) string {
			return "Keep " + name + " running. No load is needed."
		},
		"release_idle_comfy_cache": func(step Object, name string) string {
			return "Release idle ComfyUI model memory on " + str(step["node"]) + ". Keep the service running."
		},
		"restart_for_placement": func(step Object, name string) string {
			return "Move " + name + " from " + strings.Join(stringsOf(step["from"]), ", ") + " to " + strings.Join(stringsOf(step["to"]), ", ") + " after active work finishes."
		},
		"load": func(step Object, name string) string {
			return "Load " + name + " on " + strings.Join(stringsOf(step["nodes"]), ", ") + "."
		},
		"ensure_running": func(Object, string) string { return "" },
		"ensure_comfyui": func(step Object, name string) string {
			return "Make ComfyUI available on " + strings.Join(stringsOf(step["nodes"]), ", ") + ". Weights load when a workflow runs."
		},
	}
	steps := []styledRow{}
	for i, item := range arr(data["steps"]) {
		step := obj(item)
		action := str(step["action"])
		name := str(value(step, "workload", data["workload"]))
		detail := ""
		if fn, ok := descriptions[action]; ok {
			detail = fn(step, name)
		}
		if action == "ensure_running" {
			detail = "Keep " + name + " running. Start it only if it is stopped."
		}
		if detail == "" {
			detail = strings.ReplaceAll(action, "_", " ")
		}
		steps = append(steps, styledRow{strconv.Itoa(i + 1), strings.ReplaceAll(action, "_", " "), detail})
		for _, node := range keys(obj(step["minimum_available_gib"])) {
			v := obj(step["minimum_available_gib"])[node]
			steps = append(steps, styledRow{"", "memory check", node + ": " + intText(v) + " GiB available"})
		}
	}
	lines = append(lines, table([]string{"STEP", "ACTION", "DETAIL"}, steps, "", width, color))
	if protected := stringsOf(data["protected"]); len(protected) > 0 {
		lines = append(lines, note("Kept loaded: "+strings.Join(protected, ", ")+".", "bright_green", color, width))
	}
	if str(data["recipe_validation"]) == "prepared_not_live_tested" {
		lines = append(lines, noticeView("Unverified placement", "This placement recipe is prepared. A live model load has not been verified.", "bright_yellow", color, width))
	}
	lines = append(lines, note("Active jobs will not be interrupted.", "dim", color, width))
	return strings.Join(lines, "\n")
}

func formatConfigView(data Object, width int, color bool) string {
	config := obj(data["config"])
	var lines []string
	if _, has := data["model"]; !has {
		lines = append(lines, "Authoritative file on Spark 1: "+clean(data["path"]))
		rows := []styledRow{}
		for _, name := range keys(obj(config["models"])) {
			model := obj(obj(config["models"])[name])
			setting := obj(obj(model["settings"])[str(model["run_on"])])
			context := "-"
			if str(model["kind"]) == "llm" {
				context = count(setting["context_tokens"])
			}
			keep := "no"
			if boolean(model["keep_loaded"]) {
				keep = "yes"
			}
			rows = append(rows, styledRow{name, orDefault(kindLabel(str(model["kind"])), str(model["kind"])), str(model["run_on"]), context, keep})
		}
		lines = append(lines, table([]string{"MODEL / SERVICE", "TYPE", "RUN ON", "CONTEXT", "KEEP LOADED"}, rows, "Central model settings", width, color), "",
			"Use sparky config show NAME for placement settings.",
			"Use sparky config placement NAME spark01|spark02|both to change placement.",
			"Use sparky load NAME to apply the selected placement.")
		return strings.Join(lines, "\n")
	}
	name := str(data["model"])
	model := obj(value(config, name, config))
	aliases := stringsOf(model["aliases"])
	if len(aliases) == 0 {
		aliases = []string{"none"}
	}
	lines = append(lines, clean(orDefault(str(model["name"]), name)), "Model ID: "+clean(name),
		"Selected placement: "+clean(model["run_on"]),
		"Aliases: "+strings.Join(aliases, ", "))
	keep := "no"
	if boolean(model["keep_loaded"]) {
		keep = "yes"
	}
	lines = append(lines, "Keep loaded: "+keep)
	if containers := stringsOf(obj(model["monitoring"])["containers"]); len(containers) > 0 {
		lines = append(lines, "Memory containers: "+clean(strings.Join(containers, ", ")))
	}
	if str(model["kind"]) == "audio" {
		if tts := obj(model["tts"]); tts != nil {
			tts["nodes"] = anyStrings([]string{str(model["run_on"])})
			lines = append(lines, formatSpeechService(tts, width, color))
			return strings.Join(lines, "\n")
		}
	}
	rows := []styledRow{}
	for _, placement := range keys(obj(model["settings"])) {
		setting := obj(obj(model["settings"])[placement])
		parameters := obj(setting["parameters"])
		enabled := true
		if v, ok := setting["enabled"]; ok {
			enabled = boolean(v)
		}
		context := "-"
		if str(model["kind"]) == "llm" {
			context = count(setting["context_tokens"])
		}
		slots := str(value(setting, "concurrency", "-"))
		if integer(setting["concurrency"]) {
			slots = intText(setting["concurrency"])
		}
		sequences := str(value(parameters, "max_sequences", "-"))
		if integer(parameters["max_sequences"]) {
			sequences = intText(parameters["max_sequences"])
		}
		gpuBudget := "-"
		if _, ok := parameters["gpu_memory_utilization"]; ok {
			gpuBudget = fmt.Sprintf("%.0f%%", num(parameters["gpu_memory_utilization"])*100)
		}
		state := "enabled"
		if !enabled {
			state = "disabled"
		}
		rows = append(rows, styledRow{placement, context, slots, sequences, gpuBudget, state})
		if str(setting["recipe_validation"]) == "prepared_not_live_tested" {
			lines = append(lines, clean(placement)+": recipe prepared; live model load not verified.")
		}
		if !enabled {
			lines = append(lines, placement+": "+clean(value(setting, "reason", "disabled")))
		}
	}
	lines = append(lines, table([]string{"PLACEMENT", "CONTEXT", "SLOTS", "SEQUENCES", "GPU BUDGET", "STATE"}, rows, "Supported placements", width, color),
		"Request slots are manager admissions. Engine sequences are the model batch limit.",
		"Changes apply on a future load. Use sparky status for the actual running placement.")
	return strings.Join(lines, "\n")
}

func orList(xs []string, d string) string {
	if len(xs) == 0 {
		return d
	}
	return strings.Join(xs, ", ")
}

func formatResultView(action string, data Object, color bool, width int) string {
	var text string
	switch action {
	case "validate":
		text = "Configuration is valid. " + intText(data["model_count"]) + " model and service entries.\nFile on Spark 1: " + clean(data["path"])
	case "placement":
		text = "Selected placement for " + clean(data["workload"]) + ": " + str(data["run_on"]) + ".\nRunning services were not moved. Apply it with sparky load " + clean(data["workload"]) + "."
	case "run":
		text = "ComfyUI job submitted on " + clean(data["node"]) + ".\nJob ID: " + clean(data["prompt_id"]) + "\nSubmission is complete. Execution may wait for a free device.\nUse sparky status to check queues."
	case "load", "unload":
		label := "Unloaded"
		if action == "load" {
			label = "Ready"
		}
		text = label + ": " + clean(data["workload"]) + "."
		if note := str(data["note"]); note != "" {
			text += "\n" + clean(note)
		}
	case "gateway":
		text = "Gateway protocol routes are ready. Existing model and audio services were kept."
	default:
		text = clean(value(data, "detail", "Done."))
	}
	titles := Object{"validate": "Configuration valid", "placement": "Placement saved", "run": "Workflow submitted",
		"load": "Model ready", "unload": "Model unloaded", "gateway": "Gateway ready"}
	return noticeView(orDefault(str(titles[action]), "Result"), text, "bright_green", color, width)
}

func formatDevices(data Object, width int, color bool) string {
	var cards []string
	for _, nodeName := range keys(obj(data["devices"])) {
		state := obj(obj(data["devices"])[nodeName])
		if err := str(state["error"]); err != "" {
			cards = append(cards, noticeView(nodeName+": unavailable", err, "bright_red", color, width))
			continue
		}
		mem := obj(state["memory_bytes"])
		disk := obj(state["disk_bytes"])
		rows := [][2]string{
			{"Available RAM", memory(mem["MemAvailable"]) + " / " + memory(mem["MemTotal"])},
			{"Free disk", memory(disk["free"]) + " / " + memory(disk["total"])},
		}
		for _, gpu := range arr(state["gpu"]) {
			parts := strings.Split(clean(gpu), ",")
			if len(parts) == 4 {
				for i := range parts {
					parts[i] = strings.TrimSpace(parts[i])
				}
				rows = append(rows, [2]string{"GPU", parts[0]}, [2]string{"GPU allocation", parts[2] + " / " + parts[3] + " MiB"}, [2]string{"Temperature", parts[1] + " C"})
			}
		}
		containers := stringsOf(state["running_containers"])
		if len(containers) == 0 {
			containers = []string{"none reported"}
		}
		rows = append(rows, [2]string{"Containers", strings.Join(containers, ", ")})
		hostname := str(value(state, "hostname", "unknown"))
		cards = append(cards, keyValues(nodeName+" ("+hostname+")", rows, "cyan", width, color))
		for _, warning := range stringsOf(state["warnings"]) {
			cards = append(cards, noticeView("Device warning", warning, "bright_yellow", color, width))
		}
	}
	cards = append(cards, note("CPU and GPU share memory on each Spark. Do not add the RAM and GPU figures together.", "dim", color, width))
	return strings.Join(cards, "\n")
}

func formatLaunch(plan Object, width int, color bool) string {
	model := obj(plan["model"])
	title := "Launch preview"
	if boolean(plan["prepared"]) {
		title = "Settings prepared"
	}
	rows := [][2]string{
		{"Harness", str(plan["harness"])},
		{"Model", str(model["id"])},
		{"Context", count(model["context_tokens"]) + " tokens"},
		{"Gateway", str(plan["gateway"])},
		{"Project", str(plan["project"])},
		{"Executable", str(plan["binary"])},
	}
	for _, path := range stringsOf(plan["generated_files"]) {
		rows = append(rows, [2]string{"Generated file", path})
	}
	if extra := stringsOf(plan["extra_arguments"]); len(extra) > 0 {
		rows = append(rows, [2]string{"Native arguments", strings.Join(extra, " ")})
	}
	noteText := "Launch preview. No settings were changed."
	if boolean(plan["prepared"]) {
		noteText = "No harness was started."
	}
	lines := []string{keyValues(title, rows, "cyan", width, color), note(noteText, "dim", color, width)}
	if boolean(plan["offline"]) {
		lines = append(lines, note("Using cached model metadata. The server was not checked.", "bright_yellow", color, width))
	}
	if boolean(plan["prepared"]) && str(plan["harness"]) == "hermes" {
		lines = append(lines, note("Only the existing Hermes Spark provider was updated.", "dim", color, width))
	}
	return strings.Join(lines, "\n")
}

func formatLogs(data Object, width int, color bool) string {
	rows := []styledRow{}
	for _, item := range arr(data["records"]) {
		record := obj(item)
		when := "unknown"
		if started := str(record["started_at"]); started != "" {
			if t, err := parseTimestamp(started); err == nil {
				when = t
			}
		}
		status := "not finished"
		if _, ok := record["exit_code"]; ok {
			if num(record["exit_code"]) == 0 {
				status = "ok"
			} else {
				status = "exit " + intText(record["exit_code"])
			}
		}
		model := str(value(record, "model", "?"))
		model = strings.TrimPrefix(model, "garuda/")
		rows = append(rows, styledRow{when, str(value(record, "harness", "?")), model, status, str(value(record, "project", "?"))})
	}
	lines := []string{note("Local harness launches. LLM call history stays in the gateway.", "dim", color, width)}
	if len(rows) > 0 {
		lines = append(lines, table([]string{"TIME (LOCAL)", "HARNESS", "MODEL", "RESULT", "PROJECT"}, rows, "Recent harness launches", width, color))
	} else {
		lines = append(lines, note("No local harness launches recorded. Start with sparky claude --model qwen.", "dim", color, width))
	}
	if skipped := len(arr(data["skipped_files"])); skipped > 0 {
		lines = append(lines, fmt.Sprintf("Skipped %d unreadable launch records.", skipped))
	}
	return strings.Join(lines, "\n")
}

func formatDoctor(data Object, width int, color bool) string {
	rows := []styledRow{}
	for _, item := range arr(data["checks"]) {
		check := obj(item)
		result := "failed"
		if boolean(check["ok"]) {
			result = "ok"
		}
		rows = append(rows, styledRow{str(check["name"]), result, str(check["message"])})
	}
	return table([]string{"CHECK", "RESULT", "DETAIL"}, rows, "Connection checks", width, color) + "\n" + note("No inference was run.", "dim", color, width)
}

func formatOCR(result Object, color bool, width int) string {
	// Every untrusted value is cleaned and every line is wrapped to the
	// requested width, including the long installation-log path.
	raw := [][2]string{
		{"OCR service", clean(result["state"])},
		{"Installed", yesNo(result["installed"])},
	}
	if image := clean(result["image"]); image != "" {
		raw = append(raw, [2]string{"Image", image})
	}
	if _, ok := result["running"]; ok {
		raw = append(raw, [2]string{"API ready", yesNo(result["api_ready"])})
	}
	if revision := clean(result["revision"]); revision != "" {
		raw = append(raw, [2]string{"Source revision", revision})
	}
	if log := clean(result["log"]); log != "" {
		raw = append(raw, [2]string{"Installation log on Spark01", log})
	}
	var b strings.Builder
	for _, kv := range raw {
		label := kv[0] + ":"
		// Long labels wrap on their own lines so the value still fits.
		if visualLen(label)+10 > width {
			for _, line := range wrapText(label, width) {
				b.WriteString(paint(color, "dim", line) + "\n")
			}
			for _, line := range wrapText(kv[1], width) {
				b.WriteString(paint(color, stateStyle(line), line) + "\n")
			}
			continue
		}
		lines := wrapText(kv[1], width-visualLen(label)-1)
		for i, line := range lines {
			if i == 0 {
				b.WriteString(paint(color, "dim", label+" ") + paint(color, stateStyle(line), line) + "\n")
			} else {
				b.WriteString(strings.Repeat(" ", visualLen(label)+1) + paint(color, stateStyle(line), line) + "\n")
			}
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func yesNo(v any) string {
	if boolean(v) {
		return "yes"
	}
	return "no"
}

// decodeJSONObject parses a JSON string into an Object with json.Number
// semantics; useful for fixtures and the CLI glue layer.
func decodeJSONObject(s string) Object {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var o Object
	_ = dec.Decode(&o)
	return o
}

// ---- status view (status.py) ----

func fallbackWorkloads(data Object, node string) []Object {
	rows := []Object{}
	for _, name := range keys(obj(data["models"])) {
		model := obj(obj(data["models"])[name])
		kind := str(model["kind"])
		if kind != "llm" && kind != "audio" {
			continue
		}
		if !contains(stringsOf(model["nodes"]), node) {
			continue
		}
		rows = append(rows, Object{"id": name, "name": str(value(model, "display_name", name)), "kind": kind,
			"state": str(value(model, "state", "unknown")), "context_tokens": model["context_tokens"],
			"context_source": model["context_source"], "gpu_bytes": nil, "host_pss_bytes": nil})
	}
	comfy := obj(obj(obj(data["comfyui"])[node]))
	if boolean(comfy["reachable"]) {
		phase := str(value(comfy, "phase", "running"))
		rows = append(rows, Object{"id": "comfyui", "name": "ComfyUI", "kind": "comfyui",
			"state": fmt.Sprintf("%s (%d queued)", phase, int(num(comfy["pending"]))), "gpu_bytes": nil, "host_pss_bytes": nil})
	}
	return rows
}

func formatStatus(data Object, width int, color bool) string {
	deviceRows := []styledRow{}
	nodes := obj(data["nodes"])
	for _, node := range keys(nodes) {
		state := obj(nodes[node])
		mem := obj(state["memory_bytes"])
		total, available := mem["MemTotal"], mem["MemAvailable"]
		used := any(nil)
		if integer(total) && integer(available) {
			used = num(total) - num(available)
		}
		deviceRows = append(deviceRows, styledRow{node, memory(used), memory(available), memory(total)})
	}
	lines := []string{heading("Spark cluster status", color, width), "",
		table([]string{"DEVICE", "IN USE", "AVAILABLE RAM", "TOTAL RAM"}, deviceRows, "Device memory", width, color), ""}
	configuredContext := false
	for _, node := range keys(nodes) {
		sample := obj(obj(obj(data["memory_usage"])[node]))
		workloads := []Object{}
		for _, item := range arr(sample["workloads"]) {
			workloads = append(workloads, obj(item))
		}
		known := map[string]bool{}
		for _, w := range workloads {
			known[str(w["id"])] = true
		}
		for _, w := range fallbackWorkloads(data, node) {
			if !known[str(w["id"])] {
				workloads = append(workloads, w)
			}
		}
		order := Object{"llm": 0, "audio": 1, "comfyui": 2}
		sort.SliceStable(workloads, func(i, j int) bool {
			rank := func(w Object) int {
				if str(w["id"]) == "system" {
					return 4
				}
				if v, ok := order[str(w["kind"])]; ok {
					return int(num(v))
				}
				return 3
			}
			if rank(workloads[i]) != rank(workloads[j]) {
				return rank(workloads[i]) < rank(workloads[j])
			}
			return str(workloads[i]["name"]) < str(workloads[j]["name"])
		})
		rows := []styledRow{}
		for _, w := range workloads {
			limit := w["context_tokens"]
			context := "-"
			if !isZero(limit) && limit != nil {
				context = count(limit)
			} else if str(w["kind"]) == "llm" {
				context = "unknown"
			}
			if context != "-" && context != "unknown" && str(w["context_source"]) == "configured" {
				context += "*"
				configuredContext = true
			}
			rows = append(rows, styledRow{str(w["name"]), str(value(w, "state", "running")), context, allocation(w["gpu_bytes"]), allocation(w["host_pss_bytes"])})
		}
		if len(rows) > 0 {
			lines = append(lines, table([]string{"WORKLOAD", "STATE", "CONTEXT", "GPU RAM", "HOST RAM"}, rows, "Running workloads / "+clean(node), width, color), "")
		} else {
			lines = append(lines, note(node+": no running workloads reported.", "dim", color, width))
		}
		for _, warning := range stringsOf(sample["warnings"]) {
			lines = append(lines, note(warning, "bright_yellow", color, width))
		}
	}
	stopped := []string{}
	for _, name := range keys(obj(data["models"])) {
		state := obj(obj(data["models"])[name])
		kind := str(state["kind"])
		if (kind == "llm" || kind == "audio") && str(state["state"]) == "stopped" {
			stopped = append(stopped, name)
		}
	}
	if len(stopped) > 0 {
		lines = append(lines, note("Stopped models: "+strings.Join(stopped, ", ")+".", "dim", color, width))
	}
	admission := obj(data["admission"])
	waiting := []string{}
	for _, item := range arr(admission["waiting"]) {
		waiting = append(waiting, str(obj(item)["workload"]))
	}
	if len(waiting) > 0 {
		lines = append(lines, note("Waiting: "+strings.Join(waiting, ", "), "bright_yellow", color, width))
	} else {
		lines = append(lines, "Device requests waiting: 0.")
	}
	if boolean(admission["config_transition"]) {
		lines = append(lines, note("New work is paused for a configuration change.", "bright_yellow", color, width))
	}
	protected := []string{}
	for _, name := range keys(obj(data["models"])) {
		if boolean(obj(obj(data["models"])[name])["keep_loaded"]) {
			protected = append(protected, name)
		}
	}
	if len(protected) > 0 {
		lines = append(lines, note("Kept loaded: "+strings.Join(protected, ", ")+".", "bright_green", color, width))
	}
	if configuredContext {
		lines = append(lines, note("* Context uses configuration; the running backend did not report its limit.", "bright_yellow", color, width))
	}
	lines = append(lines,
		note("GPU RAM includes allocated model weights and cache. Host RAM counts shared pages proportionally.", "dim", color, width),
		note("GPU and host figures can overlap in shared physical RAM. Do not add them.", "dim", color, width),
		note("Use sparky status --json for full details.", "dim", color, width))
	return strings.Join(lines, "\n")
}

func objOrEmpty(v any) Object {
	if o, ok := v.(Object); ok && o != nil {
		return o
	}
	return Object{}
}

// ---- integration entry point ----

func terminalRender(kind string, data any, options terminalOptions) string {
	color := useColor(options)
	width := options.Width
	if width <= 0 {
		width = 100
	}
	switch kind {
	case "models":
		return formatModelsView(objOrEmpty(data), nil, width, color)
	case "model":
		return formatModelsView(nil, objOrEmpty(data), width, color)
	case "speech":
		return formatSpeechService(objOrEmpty(data), width, color)
	case "inventory":
		return formatInventory(objOrEmpty(data), width, color)
	case "plan":
		return formatPlan(objOrEmpty(data), width, color)
	case "config":
		return formatConfigView(objOrEmpty(data), width, color)
	case "result":
		return formatResultView(str(objOrEmpty(data)["action"]), objOrEmpty(data), color, width)
	case "devices":
		return formatDevices(objOrEmpty(data), width, color)
	case "launch":
		return formatLaunch(objOrEmpty(data), width, color)
	case "logs":
		return formatLogs(objOrEmpty(data), width, color)
	case "doctor":
		return formatDoctor(objOrEmpty(data), width, color)
	case "status":
		return formatStatus(objOrEmpty(data), width, color)
	case "ocr":
		return formatOCR(objOrEmpty(data), color, width)
	case "notice":
		o := objOrEmpty(data)
		return noticeView(str(value(o, "title", "Notice")), str(value(o, "message", "")), orDefault(str(o["tone"]), "bright_green"), color, width)
	case "help":
		return helpView(str(objOrEmpty(data)["text"]), color, width)
	}
	return ""
}
