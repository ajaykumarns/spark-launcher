package menubar

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRefreshProfileTransitionFromAnotherClient(t *testing.T) {
	wire := `{"models":{},"profile":{"name":"glm-5.3"},"profile_transition":{"profile":"qwen-next-flash","status":"running","message":"Start Qwen on spark01","step":2,"total":8,"elapsed_seconds":65,"step_elapsed_seconds":5}}`
	c := newController("/sparky", func(_ context.Context, _ string, args []string) (runResult, error) {
		if strings.Join(args, " ") != "--json status" {
			t.Fatalf("unexpected command: %v", args)
		}
		return runResult{Stdout: []byte(wire)}, nil
	})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := c.Snapshot()
	if !s.SwitchingProfile() || s.Loading || s.ProfileName != "glm-5.3" || s.ProfileTransition.Name != "qwen-next-flash" || s.ProfileTransition.Elapsed != 65*time.Second || s.ProfileTransition.ObservedAt.IsZero() {
		t.Fatalf("external switch was not retained separately from the saved profile: %+v", s)
	}
	s.ProfileTransition.Message = "changed by reader"
	if c.Snapshot().ProfileTransition.Message == "changed by reader" {
		t.Fatal("snapshot shared mutable progress state")
	}
	wire = `{"models":{},"profile":{"name":"qwen-next-flash"},"profile_transition":{"profile":"qwen-next-flash","status":"applied","elapsed_seconds":90}}`
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s = c.Snapshot(); s.SwitchingProfile() || s.ProfileName != "qwen-next-flash" {
		t.Fatalf("successful refresh kept showing the switch: %+v", s)
	}
}
