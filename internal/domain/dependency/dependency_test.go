package dependency

import (
	"errors"
	"testing"
	"time"
)

func TestNewDependencyValidation(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	if _, err := New("dep-1", "ten-1", "proj-a", "proj-a", false, now); !errors.Is(err, ErrSelfDependency) {
		t.Fatalf("expected ErrSelfDependency, got %v", err)
	}
	dep, err := New("dep-1", "ten-1", "proj-b", "proj-a", true, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dep.Snapshot().ProjectID != "proj-b" || dep.Snapshot().DependsOnProjectID != "proj-a" || !dep.Snapshot().TriggerAlways {
		t.Fatalf("unexpected snapshot: %+v", dep.Snapshot())
	}
}

func TestNewMappingAndEnvKey(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	m1, err := NewMapping("m-1", "vpc_id", "", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m1.InputName != "vpc_id" || m1.EnvKey() != "TF_VAR_vpc_id" {
		t.Fatalf("expected default input_name=vpc_id and TF_VAR_vpc_id, got %+v (%s)", m1, m1.EnvKey())
	}

	m2, err := NewMapping("m-2", "vpc_id", "TF_VAR_network_vpc_id", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m2.InputName != "network_vpc_id" || m2.EnvKey() != "TF_VAR_network_vpc_id" {
		t.Fatalf("expected normalized input_name=network_vpc_id and TF_VAR_network_vpc_id, got %+v (%s)", m2, m2.EnvKey())
	}

	if _, err := NewMapping("m-3", "bad name!", "vpc_id", now); err == nil {
		t.Fatal("expected error for invalid output_name")
	}
}

func TestWouldCreateCycle(t *testing.T) {
	edges := []Edge{
		{ProjectID: "proj-b", DependsOnProjectID: "proj-a"},
		{ProjectID: "proj-c", DependsOnProjectID: "proj-b"},
	}
	if WouldCreateCycle(edges, "proj-d", "proj-c") {
		t.Fatal("proj-d -> proj-c should not create a cycle")
	}
	if !WouldCreateCycle(edges, "proj-a", "proj-c") {
		t.Fatal("proj-a -> proj-c should create a cycle (c -> b -> a -> c)")
	}
	if !WouldCreateCycle(edges, "proj-a", "proj-a") {
		t.Fatal("self edge should be detected as cycle")
	}
}
