package audit_test

import (
	"context"
	"testing"
	"time"

	"infra-guy/internal/domain/audit"
)

func TestHashChainAndTamperDetection(t *testing.T) {
	ctx := context.Background()
	repo := audit.NewMemoryRepository()
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

	s1, err := repo.Append(ctx, audit.Snapshot{
		ID:           "aud-1",
		TenantID:     "tenant-a",
		ActorID:      "u-1",
		ActorLogin:   "alice",
		ActorRole:    "admin",
		Action:       "workspace.created",
		ResourceType: "workspace",
		ResourceID:   "proj-1",
		ResourceName: "prod-vpc",
		Summary:      "Created workspace prod-vpc",
		Metadata:     map[string]any{"provider": "opentofu", "auto_approve": false},
		CreatedAt:    now,
	})
	if err != nil {
		t.Fatalf("append 1 failed: %v", err)
	}
	if s1.PrevHash != "" {
		t.Fatalf("expected empty prev_hash on first entry, got %q", s1.PrevHash)
	}
	if s1.EntryHash == "" {
		t.Fatalf("expected non-empty entry_hash")
	}
	if !audit.VerifyEntry(s1) {
		t.Fatalf("expected entry 1 to verify")
	}

	s2, err := repo.Append(ctx, audit.Snapshot{
		ID:           "aud-2",
		TenantID:     "tenant-a",
		ActorID:      "u-2",
		ActorLogin:   "bob",
		ActorRole:    "member",
		Action:       "run.confirmed",
		ResourceType: "run",
		ResourceID:   "run-1",
		ResourceName: "prod-vpc",
		Summary:      "Confirmed apply for run run-1 on workspace prod-vpc",
		Metadata:     map[string]any{"status": "applying"},
		CreatedAt:    now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("append 2 failed: %v", err)
	}
	if s2.PrevHash != s1.EntryHash {
		t.Fatalf("expected s2.PrevHash (%q) == s1.EntryHash (%q)", s2.PrevHash, s1.EntryHash)
	}

	// Verify chain in newest-first order (as returned by ListByTenant)
	entries, err := repo.ListByTenant(ctx, "tenant-a", audit.Filter{})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if !audit.VerifyChain(entries, true) {
		t.Fatalf("expected chain to verify")
	}

	// Tamper with ActorLogin on entry 1 and verify detection
	tampered := make([]audit.Snapshot, len(entries))
	copy(tampered, entries)
	tampered[1].ActorLogin = "attacker"
	if audit.VerifyChain(tampered, true) {
		t.Fatalf("expected tamper detection when entry field is modified")
	}

	// Filtering tests
	filtered, err := repo.ListByTenant(ctx, "tenant-a", audit.Filter{
		Action: "run.confirmed",
	})
	if err != nil {
		t.Fatalf("filtered list failed: %v", err)
	}
	if len(filtered) != 1 || filtered[0].ID != s2.ID {
		t.Fatalf("expected 1 filtered entry matching run.confirmed, got %+v", filtered)
	}

	// Time window filter
	future := now.Add(1 * time.Hour)
	emptyList, err := repo.ListByTenant(ctx, "tenant-a", audit.Filter{
		Since: future,
	})
	if err != nil {
		t.Fatalf("future filter failed: %v", err)
	}
	if len(emptyList) != 0 {
		t.Fatalf("expected 0 entries after future timestamp, got %d", len(emptyList))
	}
}
