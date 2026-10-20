package run

import "testing"

func TestParsePlanSummary(t *testing.T) {
	got := ParsePlanSummary("Plan: 12 to add, 0 to change, 1 to destroy.\n")
	if !got.Present || got.Add != 12 || got.Change != 0 || got.Destroy != 1 {
		t.Fatalf("%#v", got)
	}
	if got.NoChanges() {
		t.Fatal("changes are not no-changes")
	}
	none := ParsePlanSummary("No changes. Your infrastructure matches the configuration.\n\nPlan: 0 to add, 0 to change, 0 to destroy.\n")
	if !none.NoChanges() {
		t.Fatalf("%#v", none)
	}
	missing := ParsePlanSummary("Error: boom")
	if missing.Present || missing.NoChanges() {
		t.Fatalf("missing line must not count as no changes: %#v", missing)
	}
}
