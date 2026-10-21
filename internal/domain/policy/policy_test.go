package policy

import (
	"errors"
	"testing"
	"time"
)

func TestPolicyLifecycle(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

	t.Run("validation errors on New", func(t *testing.T) {
		_, err := New(NewInput{TenantID: "t1", Name: "pol", Rego: "package p"})
		if !errors.Is(err, ErrInvalidID) {
			t.Fatalf("expected ErrInvalidID, got %v", err)
		}

		_, err = New(NewInput{ID: "p1", Name: "pol", Rego: "package p"})
		if !errors.Is(err, ErrInvalidTenant) {
			t.Fatalf("expected ErrInvalidTenant, got %v", err)
		}

		_, err = New(NewInput{ID: "p1", TenantID: "t1", Name: "   ", Rego: "package p"})
		if !errors.Is(err, ErrInvalidName) {
			t.Fatalf("expected ErrInvalidName, got %v", err)
		}

		_, err = New(NewInput{ID: "p1", TenantID: "t1", Name: "pol", Rego: "   "})
		if !errors.Is(err, ErrInvalidRego) {
			t.Fatalf("expected ErrInvalidRego, got %v", err)
		}
	})

	t.Run("create update and rehydrate", func(t *testing.T) {
		p, err := New(NewInput{
			ID:          "p1",
			TenantID:    "tenant-1",
			Name:        "  Prod Guardrails  ",
			Description: "  Require admin for prod  ",
			Rego:        "package infraguy\ndefault allow = false\n",
			Now:         now,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p.ID() != "p1" || p.TenantID() != "tenant-1" || p.Name() != "Prod Guardrails" || p.Description() != "Require admin for prod" {
			t.Fatalf("unexpected policy fields: %+v", p.Snapshot())
		}

		later := now.Add(5 * time.Minute)
		if err := p.Update("  Updated Name  ", "  Updated Desc  ", "package infraguy\ndefault allow = true\n", later); err != nil {
			t.Fatalf("unexpected update error: %v", err)
		}
		snap := p.Snapshot()
		if snap.Name != "Updated Name" || snap.Description != "Updated Desc" || !snap.UpdatedAt.Equal(later) {
			t.Fatalf("unexpected updated snapshot: %+v", snap)
		}

		rehydrated := Rehydrate(snap)
		if rehydrated.Snapshot() != snap {
			t.Fatalf("rehydrate mismatch: %+v != %+v", rehydrated.Snapshot(), snap)
		}

		if err := p.Update("", "desc", "package p", later); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("expected ErrInvalidName on empty update name, got %v", err)
		}
		if err := p.Update("valid", "desc", "   ", later); !errors.Is(err, ErrInvalidRego) {
			t.Fatalf("expected ErrInvalidRego on empty update rego, got %v", err)
		}
	})
}
