package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"infra-guy/internal/domain/dependency"
	"infra-guy/internal/domain/project"
)

func TestDependencyRepoCRUDAndMappings(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	deps := NewDependencyRepo(pool)
	projects := NewProjectRepo(pool)

	now := time.Unix(1_700_000_000, 0).UTC()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenantID := "ten-dep-" + suffix
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, github_account_id, account_type, created_at, updated_at)
		VALUES ($1, 'acme', $2, 'organization', now(), now())`, tenantID, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}

	repo, _ := project.ParseSourceRepo("bklebasa/infra")
	upstream, _ := project.New(project.ID("proj-up-"+suffix), "network", repo, now)
	_ = upstream.SetPlacement(tenantID, 100)
	attachTestFolder(t, ctx, pool, upstream)
	upstream.SetStateToken("tok-up")
	if err := projects.Save(ctx, upstream); err != nil {
		t.Fatal(err)
	}

	downstream, _ := project.New(project.ID("proj-down-"+suffix), "app", repo, now)
	_ = downstream.SetPlacement(tenantID, 101)
	attachTestFolder(t, ctx, pool, downstream)
	downstream.SetStateToken("tok-down")
	if err := projects.Save(ctx, downstream); err != nil {
		t.Fatal(err)
	}

	depID := dependency.ID("dep-1-" + suffix)
	d, err := dependency.New(depID, tenantID, downstream.Snapshot().ID, upstream.Snapshot().ID, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.Create(ctx, d); err != nil {
		t.Fatal(err)
	}

	m1, err := dependency.NewMapping("map-1-"+suffix, "vpc_id", "network_vpc_id", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.UpsertMapping(ctx, depID, m1); err != nil {
		t.Fatal(err)
	}

	// Upsert with same input_name updates output_name
	m1Updated, err := dependency.NewMapping("map-2-"+suffix, "main_vpc_id", "network_vpc_id", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.UpsertMapping(ctx, depID, m1Updated); err != nil {
		t.Fatal(err)
	}

	if err := deps.SetTriggerAlways(ctx, depID, true); err != nil {
		t.Fatal(err)
	}

	got, err := deps.Get(ctx, depID)
	if err != nil || got == nil {
		t.Fatalf("Get: err=%v got=%#v", err, got)
	}
	snap := got.Snapshot()
	if !snap.TriggerAlways || len(snap.Mappings) != 1 || snap.Mappings[0].OutputName != "main_vpc_id" {
		t.Fatalf("unexpected dependency snapshot: %+v", snap)
	}

	byProject, err := deps.ListByProject(ctx, downstream.Snapshot().ID)
	if err != nil || len(byProject) != 1 || len(byProject[0].Snapshot().Mappings) != 1 {
		t.Fatalf("ListByProject: err=%v got=%#v", err, byProject)
	}

	byUpstream, err := deps.ListDownstream(ctx, upstream.Snapshot().ID)
	if err != nil || len(byUpstream) != 1 || byUpstream[0].Snapshot().ID != depID {
		t.Fatalf("ListDownstream: err=%v got=%#v", err, byUpstream)
	}

	byTenant, err := deps.ListByTenant(ctx, tenantID)
	if err != nil || len(byTenant) != 1 || byTenant[0].Snapshot().ID != depID {
		t.Fatalf("ListByTenant: err=%v got=%#v", err, byTenant)
	}

	if err := deps.DeleteMapping(ctx, depID, snap.Mappings[0].ID); err != nil {
		t.Fatal(err)
	}
	afterMapDel, err := deps.Get(ctx, depID)
	if err != nil || afterMapDel == nil || len(afterMapDel.Snapshot().Mappings) != 0 {
		t.Fatalf("after DeleteMapping: err=%v got=%#v", err, afterMapDel)
	}

	if err := deps.Delete(ctx, depID); err != nil {
		t.Fatal(err)
	}
	deleted, err := deps.Get(ctx, depID)
	if err != nil || deleted != nil {
		t.Fatalf("after Delete: err=%v got=%#v", err, deleted)
	}
}
