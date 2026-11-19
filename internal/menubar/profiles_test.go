package menubar

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestProfileCommandsRequireReviewedPlan(t *testing.T) {
	f := newFakeCLI()
	f.queue("profile", okJSON(t, object{"profiles": []object{
		{"name": "daily", "capabilities": []object{{"name": "chat"}, {"name": "ocr"}}},
		{"name": "broken", "error": "Invalid recipe"},
	}}), nil)
	f.queue("profile", okJSON(t, object{"profile": "daily", "status": "planned", "plan_id": "approved",
		"steps": []object{{"action": "start", "workload": "glm", "nodes": []string{"spark01"}}},
	}), nil)
	f.queue("profile", okJSON(t, object{"profile": "daily", "status": "applied"}), nil)
	ctrl := newController("/sparky", f.runner())
	ctx := context.Background()
	choices, err := ctrl.Profiles(ctx)
	if err != nil || len(choices) != 1 || choices[0].Name != "daily · chat, ocr" {
		t.Fatalf("profile choices = %v, %v", choices, err)
	}
	if err := ctrl.ApplyProfile(ctx, "daily", ""); err == nil {
		t.Fatal("apply without a reviewed plan must fail")
	}
	plan, err := ctrl.PlanProfile(ctx, "daily")
	if err != nil || !strings.Contains(plan.Details, "start glm on spark01") {
		t.Fatalf("profile plan = %v, %v", plan, err)
	}
	if err := ctrl.ApplyProfile(ctx, "daily", plan.ID); err != nil {
		t.Fatal(err)
	}
	calls := f.callsFor("profile")
	want := []string{"--json", "profile", "apply", "--expect-plan", "approved", "--", "daily"}
	if len(calls) != 3 || !slices.Equal(calls[2], want) {
		t.Fatalf("wrong CLI calls: %v", calls)
	}
}
