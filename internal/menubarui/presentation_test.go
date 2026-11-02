package menubarui

import (
	"strings"
	"testing"
	"time"

	"sparky/internal/menubar"
)

func TestPickerCaptionExplicitStates(t *testing.T) {
	cases := []struct {
		name    string
		hasCLI  bool
		busy    bool
		loaded  bool
		choices int
		err     string
		want    string
	}{
		{"no cli", false, false, false, 0, "", "Sparky CLI not available"},
		{"error wins over list", true, false, true, 3, "status failed",
			"Model list unavailable: status failed"},
		{"loading", true, true, false, 0, "", "Loading model list…"},
		{"empty catalog", true, false, true, 0, "", "No loadable models in the CLI catalog"},
		{"waiting", true, false, false, 0, "", "Waiting for the model list"},
		{"ready", true, false, true, 2, "", "Run through the Sparky CLI"},
		{"refetch keeps list", true, true, true, 2, "", "Run through the Sparky CLI"},
	}
	for _, c := range cases {
		got := PickerCaptionText(c.hasCLI, c.busy, c.loaded, c.choices, c.err)
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
		if strings.Contains(got, "\n") {
			t.Errorf("%s: caption must be one short line", c.name)
		}
	}
}

func TestMergeAcceptedLoadClosesAcceptanceGap(t *testing.T) {
	ready := Snapshot{
		UpdatedAt: time.Now(),
		LoadError: "previous load failed",
		Models:    []menubar.ModelStatus{{ID: "glm", Name: "GLM", Loaded: true, State: "ready"}},
	}

	before := BuildPanel(ready, Prefs{}, nil, false, true)
	if !before.LoadEnabled {
		t.Fatal("load should be enabled before a click")
	}

	merged := MergeAcceptedLoad(ready, true)
	during := BuildPanel(merged, Prefs{}, nil, false, true)
	if during.LoadEnabled {
		t.Fatal("load must stay disabled while the accepted load runs")
	}
	if during.HeaderState != "Loading" {
		t.Fatalf("header state = %q, want Loading", during.HeaderState)
	}
	if merged.LoadError != "" {
		t.Fatalf("accepted load must clear the previous load error, got %q", merged.LoadError)
	}
	if !merged.UpdatedAt.Equal(ready.UpdatedAt) {
		t.Fatal("merge must preserve last successful data")
	}
	if ready.Loading {
		t.Fatal("merge must not mutate the caller's snapshot")
	}

	// A tracker that has finished does not keep Load disabled: the controller
	// snapshot stays authoritative for final readiness.
	after := BuildPanel(MergeAcceptedLoad(ready, false), Prefs{}, nil, false, true)
	if !after.LoadEnabled {
		t.Fatal("finished tracker must not leave Load disabled")
	}
}

func TestMergeAcceptedLoadPreservesUnreachableError(t *testing.T) {
	s := Snapshot{Error: "connection refused"}
	got := MergeAcceptedLoad(s, true)
	if got.Error == "" {
		t.Fatal("merge must keep a real connection error")
	}
	if !got.Loading {
		t.Fatal("merge must mark the accepted load as running")
	}
}

func TestMergeAcceptedLoadIdleIsNoop(t *testing.T) {
	s := Snapshot{UpdatedAt: time.Now(),
		Models: []menubar.ModelStatus{{ID: "glm", Loaded: true, State: "ready"}}}
	got := MergeAcceptedLoad(s, false)
	if got.Loading || got.LoadError != s.LoadError || !got.UpdatedAt.Equal(s.UpdatedAt) {
		t.Fatal("idle merge must leave the snapshot unchanged")
	}
}

func TestLoadProgressTextAlwaysShowsRealElapsed(t *testing.T) {
	// The backend fills LoadMessage immediately; elapsed must still appear.
	s := Snapshot{Loading: true, LoadMessage: "Loading GLM-5.3-flash…"}
	got := LoadProgressText(s, "glm-5.3-flash", 95*time.Second)
	if !strings.Contains(got, "1m35s elapsed") {
		t.Fatalf("elapsed missing: %q", got)
	}
	if !strings.Contains(got, "Loading GLM-5.3-flash…") {
		t.Fatalf("backend message must be preserved: %q", got)
	}
	if strings.Contains(got, "%") {
		t.Fatalf("no invented percentage: %q", got)
	}
	if strings.Contains(strings.ToLower(got), "ready") {
		t.Fatalf("progress must not claim Ready: %q", got)
	}
}

func TestLoadProgressTextGenericTargetAndFloor(t *testing.T) {
	got := LoadProgressText(Snapshot{Loading: true}, "org/model-deepseek", -3*time.Second)
	if !strings.HasPrefix(got, "Loading model-deepseek · ") {
		t.Fatalf("want generic target wording, got %q", got)
	}
	if !strings.HasSuffix(got, "0s elapsed") {
		t.Fatalf("negative elapsed must floor at 0s, got %q", got)
	}
	empty := LoadProgressText(Snapshot{Loading: true}, "", 2*time.Second)
	if !strings.HasPrefix(empty, "Loading model · ") {
		t.Fatalf("empty model id needs neutral noun, got %q", empty)
	}
}

func TestLoadProgressTextKeepsSingleLine(t *testing.T) {
	got := LoadProgressText(Snapshot{Loading: true, LoadMessage: "line one\nline two"},
		"glm", 61*time.Second)
	if strings.Contains(got, "\n") {
		t.Fatalf("footer must stay one line: %q", got)
	}
}
