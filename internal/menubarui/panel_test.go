package menubarui

import (
	"strings"
	"testing"
	"time"

	"sparky/internal/menubar"
)

func ptrU64(v uint64) *uint64 { return &v }
func ptrInt(v int) *int       { return &v }

func sampleSnapshot() menubar.Snapshot {
	return menubar.Snapshot{
		UpdatedAt: time.Now().Add(-10 * time.Second),
		Models: []menubar.ModelStatus{
			{ID: "glm-5.3-flash", Name: "GLM-5.3-flash", State: "ready", Loaded: true,
				Nodes: []string{"spark01", "spark02"}, ActiveRequests: ptrInt(0)},
		},
		Devices: []menubar.DeviceStatus{
			{ID: "spark01", TotalBytes: ptrU64(128 * gib), AvailableBytes: ptrU64(80 * gib)},
			{ID: "spark02", TotalBytes: ptrU64(128 * gib), AvailableBytes: ptrU64(70 * gib)},
		},
	}
}

func TestPanelReadyMatchesReferenceHierarchy(t *testing.T) {
	v := BuildPanel(sampleSnapshot(), Prefs{}, nil, false, true)
	if v.HeaderTitle != "Sparky" || v.HeaderState != "Ready" {
		t.Fatalf("header = %q/%q", v.HeaderTitle, v.HeaderState)
	}
	if v.ModelName != "GLM-5.3-flash" {
		t.Fatalf("model = %q", v.ModelName)
	}
	if v.ModelLocation != "Ready on Spark 1 + Spark 2" {
		t.Fatalf("location = %q", v.ModelLocation)
	}
	if len(v.MemoryRows) != 2 {
		t.Fatalf("memory rows = %v", v.MemoryRows)
	}
	if v.MemoryRows[0].Label != "Spark 1" || v.MemoryRows[1].Label != "Spark 2" {
		t.Fatalf("memory labels = %v", v.MemoryRows)
	}
	if v.MemoryRows[0].Value != "48.0 / 128.0 GiB" {
		t.Fatalf("memory value = %q", v.MemoryRows[0].Value)
	}
	if strings.Count(v.MemoryRows[0].Value, "GiB") != 1 {
		t.Fatalf("memory value must carry one unit: %q", v.MemoryRows[0].Value)
	}
	if v.MemoryRows[1].Value != "58.0 / 128.0 GiB" {
		t.Fatalf("spark 2 memory = %q", v.MemoryRows[1].Value)
	}
	if v.RequestsRow.Label != "Active requests" || v.RequestsRow.Value != "0" {
		t.Fatalf("requests = %+v", v.RequestsRow)
	}
	if v.LoadRowLabel != "Load model…" || v.RefreshRowLabel != "Refresh status" {
		t.Fatalf("rows = %q/%q", v.LoadRowLabel, v.RefreshRowLabel)
	}
	if !strings.HasPrefix(v.Footer, "Updated ") {
		t.Fatalf("footer = %q", v.Footer)
	}
	if !v.LoadEnabled || !v.RefreshEnabled {
		t.Fatalf("enabled = %v/%v", v.LoadEnabled, v.RefreshEnabled)
	}
}

func TestPanelHeaderOnlyReadyWhenEveryLoaderVerified(t *testing.T) {
	cases := []struct {
		name  string
		state menubar.ModelStatus
		want  string
	}{
		{"verified ready", menubar.ModelStatus{Loaded: true, State: "ready"}, "Ready"},
		// "loaded" is a reservation during loading, never a readiness proof.
		{"generic loaded", menubar.ModelStatus{Loaded: true, State: "loaded"}, "Loading"},
		{"missing state", menubar.ModelStatus{Loaded: true}, "Unknown"},
		{"unrecognised state", menubar.ModelStatus{Loaded: true, State: "weird"}, "Unknown"},
		{"failed", menubar.ModelStatus{Loaded: true, State: "failed"}, "Failed"},
		{"controller loading", menubar.ModelStatus{Loaded: true, State: "loading"}, "Loading"},
		{"starting", menubar.ModelStatus{Loaded: true, State: "starting"}, "Loading"},
		{"stopping", menubar.ModelStatus{Loaded: true, State: "stopping"}, "Stopping"},
		{"stopped token", menubar.ModelStatus{Loaded: true, State: "stopped"}, "Unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := menubar.Snapshot{UpdatedAt: time.Now(), Models: []menubar.ModelStatus{tc.state}}
			if got := BuildPanel(s, Prefs{}, nil, false, true).HeaderState; got != tc.want {
				t.Fatalf("header = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPanelIdleWhenFreshButNothingLoaded(t *testing.T) {
	s := menubar.Snapshot{UpdatedAt: time.Now()}
	v := BuildPanel(s, Prefs{}, nil, false, true)
	if v.HeaderState != "Idle" {
		t.Fatalf("header = %q, want Idle", v.HeaderState)
	}
	if v.ModelName != "No model loaded" {
		t.Fatalf("model = %q", v.ModelName)
	}
	if v.ModelLocation != "Nothing is loaded on the Sparks" {
		t.Fatalf("location = %q", v.ModelLocation)
	}
	if !v.LoadEnabled {
		t.Fatal("safe load path must be enabled when connected and idle")
	}
	if v.RequestsRow.Value != "0" {
		t.Fatalf("requests = %q, want 0 after a fresh verified empty set", v.RequestsRow.Value)
	}
}

func TestPanelBeforeFirstStatusIsUnknownNotIdleOrZero(t *testing.T) {
	v := BuildPanel(menubar.Snapshot{}, Prefs{}, nil, false, true)
	if v.HeaderState != "Unknown" {
		t.Fatalf("header = %q; first pending read is not a failure", v.HeaderState)
	}
	if v.ModelName != "Unknown" {
		t.Fatalf("model = %q; must not claim no model loaded", v.ModelName)
	}
	if v.RequestsRow.Value != "Unknown" {
		t.Fatalf("requests = %q; must be Unknown before first contact", v.RequestsRow.Value)
	}
	if v.LoadEnabled {
		t.Fatal("load must stay disabled before a fresh status")
	}
	if !strings.Contains(v.Footer, "Waiting") {
		t.Fatalf("footer = %q", v.Footer)
	}
}

func TestPanelLoadEnabledIndependentOfHeaderState(t *testing.T) {
	s := sampleSnapshot()
	s.Models[0].State = "" // unknown readiness, but CLI connected
	v := BuildPanel(s, Prefs{}, nil, false, true)
	if v.HeaderState != "Unknown" {
		t.Fatalf("header = %q", v.HeaderState)
	}
	if !v.LoadEnabled {
		t.Fatal("safe load path must be enabled for a connected controller with unknown readiness")
	}

	s.LoadError = "model rejected"
	if v := BuildPanel(s, Prefs{}, nil, false, true); !v.LoadEnabled {
		t.Fatal("a historical load failure must not disable loading")
	}

	s.Loading = true
	if v := BuildPanel(s, Prefs{}, nil, false, true); v.LoadEnabled {
		t.Fatal("load must be disabled while a load is in flight")
	}

	s = sampleSnapshot()
	s.Error = "offline"
	if v := BuildPanel(s, Prefs{}, nil, false, true); v.LoadEnabled {
		t.Fatal("load must be disabled when unreachable")
	}
	if v := BuildPanel(menubar.Snapshot{UpdatedAt: time.Now()}, Prefs{}, nil, false, false); v.LoadEnabled || v.RefreshEnabled {
		t.Fatal("load/refresh must be disabled without a CLI")
	}
}

func TestPanelShowsMultipleLoadedModels(t *testing.T) {
	s := sampleSnapshot()
	s.Models = append(s.Models, menubar.ModelStatus{
		ID: "qwen3.8", Name: "Qwen3.8-Flash-Next", State: "ready", Loaded: true,
		Nodes: []string{"spark01"}, ActiveRequests: ptrInt(2),
	})
	v := BuildPanel(s, Prefs{}, nil, false, true)
	if v.ModelName != "GLM-5.3-flash" {
		t.Fatalf("primary model = %q (must be actual loaded set, not default)", v.ModelName)
	}
	if len(v.ExtraModels) != 1 || !strings.HasPrefix(v.ExtraModels[0], "Qwen3.8-Flash-Next") {
		t.Fatalf("extra models = %v", v.ExtraModels)
	}
	if v.RequestsRow.Value != "2" {
		t.Fatalf("requests = %q", v.RequestsRow.Value)
	}
	if v.RequestsRow.Label != "Active requests · 2 models" {
		t.Fatalf("aggregate label = %q", v.RequestsRow.Label)
	}
}

func TestPanelAggregateRequestsUnknownIsNotPartialTotal(t *testing.T) {
	s := sampleSnapshot()
	s.Models = append(s.Models, menubar.ModelStatus{
		ID: "qwen3.8", Name: "Qwen3.8-Flash-Next", State: "ready", Loaded: true,
		Nodes: []string{"spark02"}, ActiveRequests: nil,
	})
	v := BuildPanel(s, Prefs{}, nil, false, true)
	if v.RequestsRow.Value != "Unknown" {
		t.Fatalf("partial known subset must not be shown as a total: %q", v.RequestsRow.Value)
	}

	// Unknown primary with known secondary is also Unknown.
	s = sampleSnapshot()
	s.Models[0].ActiveRequests = nil
	s.Models = append(s.Models, menubar.ModelStatus{
		ID: "qwen3.8", State: "ready", Loaded: true, Nodes: []string{"spark02"}, ActiveRequests: ptrInt(3),
	})
	if got := BuildPanel(s, Prefs{}, nil, false, true).RequestsRow.Value; got != "Unknown" {
		t.Fatalf("aggregate with any unknown = %q", got)
	}
}

func TestPanelUnknownVsZeroRequests(t *testing.T) {
	s := sampleSnapshot()
	s.Models[0].ActiveRequests = nil
	v := BuildPanel(s, Prefs{}, nil, false, true)
	if v.RequestsRow.Value != "Unknown" {
		t.Fatalf("unknown requests rendered as %q", v.RequestsRow.Value)
	}
	if v.RequestsRow.Label != "Active requests" {
		t.Fatalf("single-model label = %q", v.RequestsRow.Label)
	}
	s.Models[0].ActiveRequests = ptrInt(0)
	if got := BuildPanel(s, Prefs{}, nil, false, true).RequestsRow.Value; got != "0" {
		t.Fatalf("zero requests rendered as %q", got)
	}

	// Stale cached data keeps request uncertainty rather than a known total.
	s.Error = "network unreachable"
	if got := BuildPanel(s, Prefs{}, nil, false, true).RequestsRow.Value; got != "Unknown" {
		t.Fatalf("offline requests = %q", got)
	}
}

func TestPanelUnknownMemoryStaysWord(t *testing.T) {
	s := sampleSnapshot()
	s.Devices[0].AvailableBytes = nil
	v := BuildPanel(s, Prefs{}, nil, false, true)
	if v.MemoryRows[0].Value != "Unavailable" {
		t.Fatalf("unknown memory = %q", v.MemoryRows[0].Value)
	}
	if strings.Contains(v.MemoryRows[0].Value, "0") {
		t.Fatalf("unknown memory must not look like zero: %q", v.MemoryRows[0].Value)
	}
}

func TestPanelMemoryStableSlotsBeforeFirstContact(t *testing.T) {
	v := BuildPanel(menubar.Snapshot{}, Prefs{}, nil, false, true)
	if len(v.MemoryRows) != 2 {
		t.Fatalf("memory rows = %v", v.MemoryRows)
	}
	if v.MemoryRows[0].Label != "Spark 1" || v.MemoryRows[1].Label != "Spark 2" {
		t.Fatalf("labels = %v", v.MemoryRows)
	}
	for _, row := range v.MemoryRows {
		if row.Value != "Unavailable" || strings.Contains(row.Value, "0.0") {
			t.Fatalf("placeholder memory = %q", row.Value)
		}
	}
	if v.LoadEnabled {
		t.Fatal("load must stay disabled while the first read is pending")
	}
}

func TestPanelPendingThenActualErrorIsUnreachable(t *testing.T) {
	// No successful status yet, but a real refresh error has occurred.
	s := menubar.Snapshot{Error: "cannot reach the Sparks"}
	if got := BuildPanel(s, Prefs{}, nil, false, true).HeaderState; got != "Unreachable" {
		t.Fatalf("header = %q, want Unreachable once a real error exists", got)
	}
}

func TestPanelTransitionStatesAreProgressNotUnknown(t *testing.T) {
	cases := []struct {
		state     string
		wantState string
	}{
		{"starting", "Loading"},
		{"queued", "Loading"},
		{"loading", "Loading"},
		{"stopping", "Stopping"},
		{"draining", "Stopping"},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			s := sampleSnapshot()
			s.Models[0].State = tc.state // loaded=true reservation during transition
			v := BuildPanel(s, Prefs{}, nil, false, true)
			if v.HeaderState != tc.wantState {
				t.Fatalf("header = %q, want %q", v.HeaderState, tc.wantState)
			}
			if v.HeaderState == "Ready" {
				t.Fatal("a transition must never claim Ready")
			}
			if v.LoadEnabled {
				t.Fatal("no new GUI load during an observed transition")
			}
			// Real identity and placement stay visible.
			if v.ModelName != "GLM-5.3-flash" {
				t.Fatalf("model = %q", v.ModelName)
			}
			if !strings.Contains(v.ModelLocation, "Spark 1 + Spark 2") {
				t.Fatalf("location = %q", v.ModelLocation)
			}
			// No invented percentage or elapsed time for external work.
			if strings.Contains(v.ModelLocation, "%") || strings.Contains(v.ModelLocation, "elapsed") {
				t.Fatalf("location invents progress: %q", v.ModelLocation)
			}
		})
	}
}

func TestPanelHeaderScanFindsWorstStateAcrossModels(t *testing.T) {
	cases := []struct {
		name  string
		extra menubar.ModelStatus
		want  string
	}{
		{
			name:  "unknown row does not hide later failed",
			extra: menubar.ModelStatus{ID: "b", Name: "B", Loaded: true, Nodes: []string{"spark02"}, State: "failed"},
			want:  "Failed",
		},
		{
			name:  "unknown row does not hide later starting",
			extra: menubar.ModelStatus{ID: "b", Name: "B", Loaded: true, State: "starting"},
			want:  "Loading",
		},
		{
			name:  "unknown row does not hide later stopping",
			extra: menubar.ModelStatus{ID: "b", Name: "B", Loaded: true, State: "stopping"},
			want:  "Stopping",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := sampleSnapshot()
			s.Models[0].State = "" // unknown/unverified first row
			s.Models = append(s.Models, tc.extra)
			if got := BuildPanel(s, Prefs{}, nil, false, true).HeaderState; got != tc.want {
				t.Fatalf("header = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPanelReadyPlusUnknownNeverClaimsReady(t *testing.T) {
	ready := menubar.ModelStatus{ID: "a", Name: "A", Loaded: true, Nodes: []string{"spark01"}, State: "ready"}
	unknown := menubar.ModelStatus{ID: "b", Name: "B", Loaded: true, Nodes: []string{"spark02"}}
	cases := []struct {
		name   string
		models []menubar.ModelStatus
		want   string
	}{
		{"ready then unknown", []menubar.ModelStatus{ready, unknown}, "Unknown"},
		{"unknown then ready", []menubar.ModelStatus{unknown, ready}, "Unknown"},
		{"unknown + ready + failed", []menubar.ModelStatus{
			unknown, ready,
			{ID: "c", Name: "C", Loaded: true, State: "failed"},
		}, "Failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := menubar.Snapshot{UpdatedAt: time.Now(), Models: tc.models}
			if got := BuildPanel(s, Prefs{}, nil, false, true).HeaderState; got != tc.want {
				t.Fatalf("header = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPanelFailedOutranksTransitionAndReady(t *testing.T) {
	s := sampleSnapshot() // row 0 is verified Ready
	s.Models = append(s.Models,
		menubar.ModelStatus{ID: "b", Name: "B", Loaded: true, State: "loading", ActiveRequests: ptrInt(0)},
		menubar.ModelStatus{ID: "c", Name: "C", Loaded: true, State: "failed", ActiveRequests: ptrInt(0)},
	)
	v := BuildPanel(s, Prefs{}, nil, false, true)
	if v.HeaderState != "Failed" {
		t.Fatalf("header = %q; Failed must outrank Loading and Ready", v.HeaderState)
	}
}

func TestPanelMemoryOnlyOneDeviceKeepsStableOtherSlot(t *testing.T) {
	s := sampleSnapshot()
	s.Devices = s.Devices[:1]
	v := BuildPanel(s, Prefs{}, nil, false, true)
	if v.MemoryRows[0].Value != "48.0 / 128.0 GiB" {
		t.Fatalf("spark 1 = %q", v.MemoryRows[0].Value)
	}
	if v.MemoryRows[1].Label != "Spark 2" || v.MemoryRows[1].Value != "Unavailable" {
		t.Fatalf("missing spark 2 = %+v", v.MemoryRows[1])
	}
}

func TestPanelUnreachableKeepsLastKnownIdentity(t *testing.T) {
	s := sampleSnapshot()
	s.Error = "network unreachable"
	v := BuildPanel(s, Prefs{}, nil, false, true)
	if v.HeaderState != "Unreachable" {
		t.Fatalf("header = %q", v.HeaderState)
	}
	if v.ModelName != "Last known: GLM-5.3-flash" {
		t.Fatalf("model = %q", v.ModelName)
	}
	if v.ModelLocation != "Current model state is unavailable" {
		t.Fatalf("location = %q", v.ModelLocation)
	}
	if !strings.HasPrefix(v.Footer, "Last contact ") {
		t.Fatalf("footer = %q", v.Footer)
	}
	if v.LoadEnabled {
		t.Fatal("load must be disabled when unreachable")
	}
	for _, row := range v.MemoryRows {
		if row.Value != "Unavailable" {
			t.Fatalf("stale memory must be unavailable: %+v", row)
		}
	}
	if v.RequestsRow.Value != "Unknown" {
		t.Fatalf("stale requests = %q", v.RequestsRow.Value)
	}
}

func TestPanelUnreachableWithoutCachedStatusDoesNotClaimNoModel(t *testing.T) {
	s := menubar.Snapshot{Error: "cannot reach the Sparks"}
	v := BuildPanel(s, Prefs{}, nil, false, true)
	if v.ModelName != "Unknown" || strings.Contains(v.ModelName, "No model") {
		t.Fatalf("model = %q", v.ModelName)
	}
	if v.ModelLocation != "Current model state is unavailable" {
		t.Fatalf("location = %q", v.ModelLocation)
	}
	if v.RequestsRow.Value != "Unknown" {
		t.Fatalf("requests = %q", v.RequestsRow.Value)
	}
}

func TestPanelLoading(t *testing.T) {
	s := sampleSnapshot()
	s.Loading = true
	s.LoadMessage = "Loading GLM-5.3-flash..."
	v := BuildPanel(s, Prefs{}, nil, false, true)
	if v.HeaderState != "Loading" {
		t.Fatalf("header = %q", v.HeaderState)
	}
	if v.LoadEnabled {
		t.Fatal("load must be disabled while loading")
	}
	if v.Footer != "Loading GLM-5.3-flash..." {
		t.Fatalf("footer = %q", v.Footer)
	}
	if v.RequestsRow.Value != "Unknown" {
		t.Fatalf("in-flight requests = %q", v.RequestsRow.Value)
	}

	s.LoadMessage = ""
	v = BuildPanel(s, Prefs{}, nil, false, true)
	if v.Footer != "Load in progress · memory from last sample" {
		t.Fatalf("default loading footer = %q", v.Footer)
	}
}

func TestPanelLoadResultVisibleAndNotCurrentModelFailure(t *testing.T) {
	s := sampleSnapshot()
	s.LoadError = "model rejected"
	v := BuildPanel(s, Prefs{}, nil, false, true)
	if v.LoadResult != "Last load failed — see diagnostics" {
		t.Fatalf("load result = %q", v.LoadResult)
	}
	if v.ModelLocation != "Ready on Spark 1 + Spark 2" {
		t.Fatalf("current model location must stay ready: %q", v.ModelLocation)
	}
	if v.HeaderState != "Ready" {
		t.Fatalf("header must reflect current readiness: %q", v.HeaderState)
	}
	if !v.LoadEnabled {
		t.Fatal("a healthy loaded model with a historical failure can still load")
	}
}

func TestPanelReadinessUnknownResultPreserved(t *testing.T) {
	s := sampleSnapshot()
	s.LoadError = "The load reported ready, but readiness could not be confirmed: timeout"
	s.LoadMessage = "GLM-5.3-flash: readiness not confirmed. Refresh in a moment."
	v := BuildPanel(s, Prefs{}, nil, false, true)
	if v.LoadResult != "Load readiness not confirmed — see diagnostics" {
		t.Fatalf("load result = %q", v.LoadResult)
	}
	if !v.LoadEnabled {
		t.Fatal("a readiness-unknown result must not block the safe load path")
	}
	if v.ModelLocation != "Ready on Spark 1 + Spark 2" {
		t.Fatalf("healthy loaded model must not be relabelled: %q", v.ModelLocation)
	}
}

func TestPanelPickerChoicesCarryIDs(t *testing.T) {
	choices := []Choice{
		{ID: "qwen3.8", Name: "Qwen3.8-Flash-Next"},
		{ID: "glm-5.3-flash", Name: "GLM-5.3-flash"},
		{ID: "spark-auto", Name: "spark-auto"},
	}
	v := BuildPanel(sampleSnapshot(), Prefs{ShowTitle: true}, choices, true, true)
	if !v.Picking {
		t.Fatal("picking flag lost")
	}
	if len(v.Choices) != 2 || len(v.ChoiceIDs) != 2 {
		t.Fatalf("choices = %v ids = %v", v.Choices, v.ChoiceIDs)
	}
	if v.ChoiceIDs[0] != "glm-5.3-flash" || v.ChoiceIDs[1] != "qwen3.8" {
		t.Fatalf("ids = %v", v.ChoiceIDs)
	}
	if v.Choices[0] != "GLM-5.3-flash" {
		t.Fatalf("labels = %v", v.Choices)
	}
	if !v.ShowTitle {
		t.Fatal("show-title pref lost")
	}
}

func TestPanelNoCLI(t *testing.T) {
	v := BuildPanel(menubar.Snapshot{}, Prefs{}, nil, false, false)
	if v.HasCLI {
		t.Fatal("HasCLI must be false")
	}
	if v.LoadEnabled || v.RefreshEnabled {
		t.Fatal("actions must be disabled without a CLI")
	}
	if v.ModelName != "Unknown" {
		t.Fatalf("model = %q", v.ModelName)
	}
}
