package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"infra-guy/internal/domain/policy"
	"infra-guy/internal/domain/project"
)

func TestPolicyRepoCRUDAndProjectApprovalPolicy(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	policies := NewPolicyRepo(pool)
	projects := NewProjectRepo(pool)

	now := time.Unix(1_700_000_000, 0).UTC()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenantID := "ten-pol-" + suffix
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, github_account_id, account_type, created_at, updated_at)
		VALUES ($1, 'acme', $2, 'organization', now(), now())`, tenantID, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}

	pol, err := policy.New(policy.NewInput{
		ID:          policy.ID("pol-1-" + suffix),
		TenantID:    tenantID,
		Name:        "Prod Approvers",
		Description: "Require john for prod",
		Rego:        "package infraguy\ndefault allow = true\n",
		Now:         now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := policies.Insert(ctx, pol); err != nil {
		t.Fatal(err)
	}

	dup, _ := policy.New(policy.NewInput{
		ID:       policy.ID("pol-dup-" + suffix),
		TenantID: tenantID,
		Name:     "Prod Approvers",
		Rego:     "package infraguy\ndefault allow = false\n",
		Now:      now,
	})
	if err := policies.Insert(ctx, dup); !errors.Is(err, policy.ErrDuplicateName) {
		t.Fatalf("expected ErrDuplicateName, got %v", err)
	}

	got, err := policies.Get(ctx, pol.ID())
	if err != nil || got == nil {
		t.Fatalf("get policy: %v %#v", err, got)
	}
	if got.Name() != "Prod Approvers" || got.Description() != "Require john for prod" {
		t.Fatalf("unexpected policy: %+v", got.Snapshot())
	}

	if err := pol.Update("Prod Approvers v2", "Updated", "package infraguy\ndefault allow = false\n", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := policies.Update(ctx, pol); err != nil {
		t.Fatal(err)
	}

	listed, err := policies.ListByTenant(ctx, tenantID)
	if err != nil || len(listed) != 1 || listed[0].Name() != "Prod Approvers v2" {
		t.Fatalf("ListByTenant: %v %#v", err, listed)
	}

	repo, _ := project.ParseSourceRepo("bklebasa/infra")
	p, _ := project.New(project.ID("proj-pol-"+suffix), "infra", repo, now)
	_ = p.SetPlacement(tenantID, 100)
	attachTestFolder(t, ctx, pool, p)
	p.SetStateToken("tok")
	p.SetPolicyIDs([]string{string(pol.ID())})
	if err := projects.Save(ctx, p); err != nil {
		t.Fatal(err)
	}

	gotProj, err := projects.Get(ctx, p.Snapshot().ID)
	ids := []string{}
	if gotProj != nil {
		ids = gotProj.Snapshot().PolicyIDs
	}
	if err != nil || gotProj == nil || len(ids) != 1 || ids[0] != string(pol.ID()) {
		t.Fatalf("expected project PolicyIDs=%q, got %#v (err=%v)", pol.ID(), ids, err)
	}

	// Deleting the policy removes the project_policies row.
	if err := policies.Delete(ctx, pol.ID()); err != nil {
		t.Fatal(err)
	}
	gotProjAfter, err := projects.Get(ctx, p.Snapshot().ID)
	var afterIDs []string
	if gotProjAfter != nil {
		afterIDs = gotProjAfter.Snapshot().PolicyIDs
	}
	if err != nil || gotProjAfter == nil || len(afterIDs) != 0 {
		t.Fatalf("expected no PolicyIDs after policy delete, got %#v (err=%v)", afterIDs, err)
	}
}
