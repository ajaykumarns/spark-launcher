package menubarui

import (
	"strings"
	"testing"
	"time"

	"sparky/internal/menubar"
)

func TestPanelShowsExternalProfileSwitchInsteadOfOldReadyProfile(t *testing.T) {
	s := Snapshot{ProfileName: "glm-5.3", UpdatedAt: time.Now(),
		Capabilities: []menubar.ProfileCapability{{Name: "chat", State: "available"}},
		Models:       []menubar.ModelStatus{{ID: "glm", State: "ready", Loaded: true}},
		ProfileTransition: &menubar.ProfileTransition{Name: "qwen-next-flash", Status: "running",
			Message: "Start Qwen on spark01", Step: 3, Total: 9, Elapsed: 65 * time.Second, StepElapsed: 5 * time.Second}}
	v := BuildPanel(s, Prefs{}, nil, false, true)
	if v.HeaderState != "Switching profile" || v.LoadEnabled || !strings.Contains(v.ModelName, "qwen-next-flash") || strings.Contains(v.ModelName, "glm") || !strings.Contains(v.ModelLocation, "Step 3/9") || !strings.Contains(v.Footer, "1m5s") || !strings.Contains(v.Footer, "step: 5s") {
		t.Fatalf("panel still shows the old ready profile: %+v", v)
	}
	if got := IconTitle(s, true); got != "Switching: qwen-next-flash" {
		t.Fatalf("icon title hides target profile: %q", got)
	}
	if got := IconTitle(s, false); got != "…" {
		t.Fatalf("icon must indicate a switch even with title disabled: %q", got)
	}
	// A failed refresh must not keep claiming live switching/readiness.
	s.Error = "Connection failed"
	v = BuildPanel(s, Prefs{}, nil, false, true)
	if v.HeaderState != "Unreachable" || !strings.Contains(v.ModelName, "Last switch") || v.LoadEnabled {
		t.Fatalf("stale switch data was presented as live: %+v", v)
	}
	s.Error = ""
	s.ProfileTransition.Status, s.ProfileTransition.Error = "failed", "Qwen did not become ready"
	v = BuildPanel(s, Prefs{}, nil, false, true)
	if v.HeaderState != "Profile switch failed" || v.LoadResult != "Qwen did not become ready" || strings.Contains(v.HeaderState, "Ready") || IconTitle(s, true) != "!" {
		t.Fatalf("failed switch looked ready or kept loading: %+v", v)
	}
	s.ProfileTransition.Status, s.ProfileName = "applied", "qwen-next-flash"
	v = BuildPanel(s, Prefs{}, nil, false, true)
	if v.HeaderState != "Ready" || !strings.Contains(v.ModelName, "qwen-next-flash") || !v.LoadEnabled {
		t.Fatalf("completion did not restore normal profile display: %+v", v)
	}
}

func TestProfileElapsedTimeAdvancesOnlyWhileRunning(t *testing.T) {
	p := &menubar.ProfileTransition{Status: "running", Elapsed: time.Minute, ObservedAt: time.Now().Add(-5 * time.Second)}
	if got := ProfileProgressText(p); !strings.Contains(got, "1m5s") {
		t.Fatalf("elapsed time did not advance between status reads: %q", got)
	}
	p.Status = "failed"
	if got := ProfileProgressText(p); got != "Elapsed: 1m0s" {
		t.Fatalf("finished switch timer kept advancing: %q", got)
	}
}

func TestPollerRefreshesObservedProfileSwitchMoreOften(t *testing.T) {
	f := &fakeCtrl{snap: Snapshot{ProfileTransition: &menubar.ProfileTransition{Status: "running"}}}
	p := NewPoller(f, 15*time.Second, time.Second)
	if got := p.nextDelay(); got != 5*time.Second {
		t.Fatalf("switch refresh delay = %v", got)
	}
	f.snap.ProfileTransition.Status = "applied"
	if got := p.nextDelay(); got != 15*time.Second {
		t.Fatalf("idle refresh delay = %v", got)
	}
}
