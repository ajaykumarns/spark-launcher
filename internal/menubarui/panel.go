package menubarui

import (
	"fmt"
	"strings"
	"time"

	"sparky/internal/menubar"
)

// PanelView is the plain, pre-formatted content of the native popover panel.
// The AppKit bridge only lays out and draws these strings; all decisions about
// what to show happen here so they are portable and testable without cgo.
type PanelView struct {
	HeaderTitle   string   // "Sparky"
	HeaderState   string   // "Ready" / "Idle" / "Loading" / "Unreachable" / "Failed"
	ModelCaption  string   // "Language model"
	ModelName     string   // strong model name, or "No model loaded"/"Unknown"
	ModelLocation string   // "Ready on Spark 1 + Spark 2"
	ExtraModels   []string // additional loaded LLMs, one short line each

	MemoryCaption string // "Device memory · in use / total"
	MemoryRows    []PanelPair
	RequestsRow   PanelPair

	LoadRowLabel    string // "Load model…"
	RefreshRowLabel string // "Refresh status"
	Footer          string

	LoadResult string // short root-level load result, "" when clean

	// Picker content for the model chooser.
	PickerCaption string
	Choices       []string // display labels, same order as ChoiceIDs
	ChoiceIDs     []string // IDs passed verbatim to the CLI, never shell text
	Picking       bool

	// Settings affordances.
	ShowTitle      bool
	StartAtLogin   bool
	HasCLI         bool
	LoadEnabled    bool
	RefreshEnabled bool
}

// PanelPair is a left/right aligned row of label and value.
type PanelPair struct {
	Label string
	Value string
}

// BuildPanel derives the panel presentation from a snapshot. It renders real
// data only; nothing here is a hardcoded sample.
func BuildPanel(s menubar.Snapshot, prefs Prefs, choices []Choice, picking bool, hasCLI bool) PanelView {
	v := PanelView{
		HeaderTitle:     "Sparky",
		ModelCaption:    "Language model",
		MemoryCaption:   "Device memory · in use / total",
		LoadRowLabel:    "Load model…",
		RefreshRowLabel: "Refresh status",
		PickerCaption:   "Run through the Sparky CLI",
		HasCLI:          hasCLI,
		ShowTitle:       prefs.ShowTitle,
		Picking:         picking,
	}
	connected := panelConnected(s)
	transitioning := panelTransitioning(s)
	v.HeaderState = panelState(s, connected)
	// A new GUI load is unsafe while our own load runs, and also while an
	// observed controller transition (starting/loading/stopping/draining) is
	// in progress, since the inventory is mid-change.
	v.LoadEnabled = hasCLI && connected && !s.Loading && !transitioning
	v.RefreshEnabled = hasCLI

	if connected {
		loaded := LoadedModels(s)
		if len(loaded) == 0 {
			v.ModelName = "No model loaded"
			v.ModelLocation = "Nothing is loaded on the Sparks"
		} else {
			primary := loaded[0]
			v.ModelName = modelDisplayName(primary)
			v.ModelLocation = locationLine(primary, s)
			// Show every other loaded LLM. Its state word comes from
			// StateLabel; the header carries the worst state across all rows.
			for _, m := range loaded[1:] {
				v.ExtraModels = append(v.ExtraModels,
					modelDisplayName(m)+" · "+StateLabel(m, s)+" · "+placementLabel(m))
			}
		}
	} else if len(LoadedModels(s)) > 0 {
		// Cached identity from the last good status is still useful when the
		// Sparks cannot be reached; mark it plainly as last known.
		v.ModelName = "Last known: " + modelDisplayName(LoadedModels(s)[0])
		v.ModelLocation = "Current model state is unavailable"
	} else {
		v.ModelName = "Unknown"
		v.ModelLocation = "Current model state is unavailable"
	}

	v.MemoryRows = memoryRows(s, connected)
	v.RequestsRow = PanelPair{Label: requestsLabel(s), Value: requestsValue(s, connected)}

	v.Footer = footerLine(s)
	v.LoadResult = LoadResultLine(s)

	for _, c := range CatalogChoices(choices) {
		v.Choices = append(v.Choices, ChoiceDisplayName(c))
		v.ChoiceIDs = append(v.ChoiceIDs, c.ID)
	}
	return v
}

// ChoiceDisplayName is the picker label for a choice: the friendly name alone
// when distinct, otherwise the canonical ID, so picker rows stay compact.
func ChoiceDisplayName(c Choice) string {
	if c.Name != "" && c.Name != c.ID {
		return c.Name
	}
	return c.ID
}

// panelConnected reports whether the snapshot reflects a usable CLI
// connection: at least one successful status with no unrecovered refresh
// failure. A load can still be in flight or have failed; that does not make
// the controller offline, so the safe load path stays available.
func panelConnected(s menubar.Snapshot) bool {
	return s.Error == "" && !s.UpdatedAt.IsZero()
}

// panelState is the header state word, chosen by this explicit priority:
//
//  1. our own load in flight            -> Loading
//  2. an actual refresh error           -> Unreachable
//  3. no successful status yet          -> Unknown (first read still pending)
//  4. any loaded model Failed           -> Failed
//  5. any loaded model in a transition  -> Loading (starting/loading) or
//     Stopping (stopping/draining); these are progress, never Unknown
//  6. every loaded model verified ready -> Ready
//  7. nothing loaded yet on a fresh read-> Idle
//  8. anything else (unknown/unverified)-> Unknown
//
// All loaded models are scanned and the worst state wins, so an early
// Unknown row can never hide a later Failed, Loading or Stopping row.
func panelState(s menubar.Snapshot, connected bool) string {
	if s.Loading {
		return "Loading"
	}
	if s.Error != "" {
		return "Unreachable"
	}
	if !connected {
		// UpdatedAt is zero and no error exists: the first read is pending.
		return "Unknown"
	}
	loaded := LoadedModels(s)
	if len(loaded) == 0 {
		return "Idle"
	}
	worst := 0
	for _, m := range loaded {
		if p := modelPriority(m, s); p > worst {
			worst = p
		}
	}
	switch worst {
	case priorityFailed:
		return "Failed"
	case priorityStarting:
		return "Loading"
	case priorityStopping:
		return "Stopping"
	case priorityReady:
		return "Ready"
	}
	return "Unknown"
}

// Priorities for the header scan. Numerically larger is shown first, so an
// Unknown/unverified row outranks Ready but stays below real progress and
// failure. This keeps verified readiness strict: Ready is chosen only when
// every loaded model scored priorityReady.
const (
	priorityReady = iota + 1
	priorityUnknown
	priorityStarting
	priorityStopping
	priorityFailed
)

// statePriority maps a StateLabel word to the header priority. Controller
// transition tokens are surfaced as progress, not as unknown states.
func statePriority(label string) int {
	label = strings.TrimSpace(label)
	switch label {
	case "Ready":
		return priorityReady
	// Native inventory reports loaded=true as a reservation while the
	// workload is still loading. Never Ready, but it is progress.
	case "Loading", "Loaded":
		return priorityStarting
	case "Failed":
		return priorityFailed
	}
	// Unknown/unverified states (including "Unknown (loaded)", "Starting")
	// must outrank Ready so a Ready + Unknown pair never claims Ready.
	return priorityUnknown
}

// transitionWords are the controller transition tokens the native inventory
// can report while a workload is reserved but not yet ready or fully gone.
var transitionWords = map[string]bool{
	"starting": true, "queued": true, "loading": true,
	"stopping": true, "draining": true,
}

// modelPriority derives the header priority from both the mapped StateLabel
// word and the raw controller token. The raw token matters because StateLabel
// title-cases unrecognised tokens (e.g. "starting" -> "Starting"), and the
// token is the authoritative source for transition progress.
func modelPriority(m menubar.ModelStatus, s menubar.Snapshot) int {
	p := statePriority(StateLabel(m, s))
	token := strings.ToLower(strings.TrimSpace(m.State))
	if !transitionWords[token] {
		return p
	}
	tp := priorityStarting
	if token == "stopping" || token == "draining" {
		tp = priorityStopping
	}
	if tp > p {
		return tp
	}
	return p
}

// panelTransitioning reports whether any loaded LLM is in an observed
// controller transition, either starting/loading or stopping/draining.
func panelTransitioning(s menubar.Snapshot) bool {
	for _, m := range LoadedModels(s) {
		p := modelPriority(m, s)
		if p == priorityStarting || p == priorityStopping {
			return true
		}
	}
	return false
}

func modelDisplayName(m menubar.ModelStatus) string {
	if m.Name != "" {
		return m.Name
	}
	if m.ID != "" {
		return m.ID
	}
	return "Unknown model"
}

// locationLine is the small secondary line under the model name, mirroring the
// approved reference: state word plus placement. It uses the model's own
// state so a cached line stays informative when the controller is stale.
func locationLine(m menubar.ModelStatus, s menubar.Snapshot) string {
	local := s
	local.Error = ""
	state := StateLabel(m, local)
	place := placementLabel(m)
	if place == "" {
		return state
	}
	return state + " on " + place
}

// placementLabel turns backend node IDs (spark01/spark02) into the reference's
// "Spark 1 + Spark 2" wording without losing unknown placement.
func placementLabel(m menubar.ModelStatus) string {
	if len(m.Nodes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(m.Nodes))
	for _, n := range m.Nodes {
		parts = append(parts, displayNode(n))
	}
	return strings.Join(parts, " + ")
}

func displayNode(id string) string {
	switch id {
	case "spark01":
		return "Spark 1"
	case "spark02":
		return "Spark 2"
	}
	if id == "" {
		return "unknown node"
	}
	return id
}

// memoryRows always yields stable Spark 1 and Spark 2 slots plus any extra
// devices reported, in deterministic order. Unknown readings stay words,
// never zero, so the panel is useful before first contact.
func memoryRows(s menubar.Snapshot, connected bool) []PanelPair {
	order := []string{"spark01", "spark02"}
	byID := map[string]menubar.DeviceStatus{}
	var extra []string
	seen := map[string]bool{}
	for _, d := range s.Devices {
		if d.ID == "" || seen[d.ID] {
			continue
		}
		seen[d.ID] = true
		byID[d.ID] = d
	}
	for id := range byID {
		if id != "spark01" && id != "spark02" {
			extra = append(extra, id)
		}
	}
	// Sort extras deterministically without importing sort twice.
	for i := 1; i < len(extra); i++ {
		for j := i; j > 0 && extra[j] < extra[j-1]; j-- {
			extra[j], extra[j-1] = extra[j-1], extra[j]
		}
	}
	order = append(order, extra...)

	rows := make([]PanelPair, 0, len(order))
	for _, id := range order {
		d, ok := byID[id]
		if !ok {
			rows = append(rows, PanelPair{Label: displayNode(id), Value: "Unavailable"})
			continue
		}
		rows = append(rows, PanelPair{Label: displayNode(id), Value: memoryValue(d, connected)})
	}
	return rows
}

// memoryValue renders "in use / total GiB" with a single unit, computing in
// use as total minus available. Unknown readings say Unavailable.
func memoryValue(d menubar.DeviceStatus, connected bool) string {
	if !connected {
		return "Unavailable"
	}
	if d.TotalBytes == nil || d.AvailableBytes == nil {
		return "Unavailable"
	}
	total, avail := *d.TotalBytes, *d.AvailableBytes
	if avail > total {
		return "Unavailable"
	}
	inUse := float64(total-avail) / gib
	totalGiB := float64(total) / gib
	return fmt.Sprintf("%.1f / %.1f GiB", inUse, totalGiB)
}

// requestsLabel makes an aggregate explicit and keeps the single-model label
// exactly "Active requests".
func requestsLabel(s menubar.Snapshot) string {
	n := len(LoadedModels(s))
	if n > 1 {
		return fmt.Sprintf("Active requests · %d models", n)
	}
	return "Active requests"
}

// requestsValue sums known request counts across loaded LLMs. Any unknown
// count makes the aggregate Unknown so a partial known subset is never shown
// as a complete total. Zero is valid only after a fresh, verified, empty
// loaded set; before any successful status or when offline it is Unknown.
func requestsValue(s menubar.Snapshot, connected bool) string {
	if s.Loading {
		return "Unknown"
	}
	if !connected {
		return "Unknown"
	}
	loaded := LoadedModels(s)
	if len(loaded) == 0 {
		return "0"
	}
	total := 0
	for _, m := range loaded {
		if m.ActiveRequests == nil {
			return "Unknown"
		}
		total += *m.ActiveRequests
	}
	return fmt.Sprintf("%d", total)
}

// footerLine mirrors the reference footer: load progress while loading,
// preserved last-contact age when stale, otherwise the last update age.
func footerLine(s menubar.Snapshot) string {
	if s.Loading {
		if s.LoadMessage != "" {
			return s.LoadMessage
		}
		return "Load in progress · memory from last sample"
	}
	if s.UpdatedAt.IsZero() {
		if s.Error != "" {
			return "Last contact: unavailable"
		}
		return "Waiting for first update"
	}
	age := HumanAge(time.Since(s.UpdatedAt))
	if s.Error != "" {
		return "Last contact " + age
	}
	return "Updated " + age
}
