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

func TestParsePlanSummaryImport(t *testing.T) {
	got := ParsePlanSummary("Plan: 17 to import, 0 to add, 19 to change, 0 to destroy.\n")
	if !got.Present || got.Import != 17 || got.Add != 0 || got.Change != 19 || got.Destroy != 0 {
		t.Fatalf("%#v", got)
	}
	if got.NoChanges() {
		t.Fatal("imports are changes")
	}
	only := ParsePlanSummary("Plan: 2 to import, 0 to add, 0 to change, 0 to destroy.")
	if !only.Present || only.Import != 2 || only.NoChanges() {
		t.Fatalf("import-only: %#v", only)
	}
	plain := ParsePlanSummary("Plan: 12 to add, 0 to change, 1 to destroy.\n")
	if !plain.Present || plain.Import != 0 || plain.Add != 12 || plain.Change != 0 || plain.Destroy != 1 {
		t.Fatalf("line without import clause: %#v", plain)
	}
	if ParsePlanSummary("Plan: 0 to add, 0 to change, 0 to destroy.\n").NoChanges() != true {
		t.Fatal("all-zero line must stay no-changes")
	}
	partial := ParsePlanSummary("Plan: 17 to import, 0 to add")
	if partial.Present || partial.Import != 0 || partial.NoChanges() {
		t.Fatalf("partial line must not match: %#v", partial)
	}
	first := ParsePlanSummary("Plan: 1 to import, 0 to add, 0 to change, 0 to destroy.\nPlan: 9 to import, 0 to add, 0 to change, 0 to destroy.\n")
	if first.Import != 1 {
		t.Fatalf("first summary line wins, got %#v", first)
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
