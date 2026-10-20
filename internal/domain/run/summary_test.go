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

func TestNoChangesSentences(t *testing.T) {
	if !ParsePlanSummary("Plan: 0 to add, 0 to change, 0 to destroy.\n").NoChanges() {
		t.Fatal("zero plan line must be no-changes")
	}
	sentence := ParsePlanSummary("\nNo changes. Your infrastructure matches the configuration.\n\nTerraform has compared your real infrastructure against your configuration\nand found no differences, so no changes are needed.\n")
	if !sentence.NoChanges() || sentence.Present {
		t.Fatalf("no-changes sentence: %#v", sentence)
	}
	noSummary := ParsePlanSummary("Terraform will perform the following actions:\n\n  # null_resource.x will be created\n")
	if noSummary.NoChanges() || noSummary.Present {
		t.Fatalf("plan without a summary line must not be no-changes: %#v", noSummary)
	}
}
