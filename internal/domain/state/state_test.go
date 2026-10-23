package state

import (
	"bytes"
	"testing"
	"time"
)

func TestPutWithoutLockUpdatesStateAndMetadata(t *testing.T) {
	s := New("p1")
	if s.Snapshot().Metadata().Exists {
		t.Fatal("expected Exists=false on empty state")
	}
	now := time.Unix(5, 0).UTC()
	body := []byte(`{"version":4,"serial":7}`)
	if err := s.Put(body, "", "run-1", now); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if snap.Serial == nil || *snap.Serial != 7 || !snap.UpdatedAt.Equal(now) {
		t.Fatalf("%#v", snap)
	}
	meta := snap.Metadata()
	if !meta.Exists || meta.Serial == nil || *meta.Serial != 7 || meta.Size != len(body) || meta.Locked || !meta.UpdatedAt.Equal(now) {
		t.Fatalf("unexpected metadata: %#v", meta)
	}
	if s.Empty() {
		t.Fatal("empty after put")
	}
}

func TestPutRequiresMatchingLock(t *testing.T) {
	s := New("p1")
	now := time.Unix(5, 0).UTC()
	info := []byte(`{"ID":"lock-1"}`)
	if _, err := s.Lock(info); err != nil {
		t.Fatal(err)
	}
	if err := s.Put([]byte(`{"serial":1}`), "", "run-1", now); err != ErrLockMismatch {
		t.Fatalf("missing id: %v", err)
	}
	if err := s.Put([]byte(`{"serial":1}`), "other", "run-1", now); err != ErrLockMismatch {
		t.Fatalf("wrong id: %v", err)
	}
	if err := s.Put([]byte(`{"serial":1}`), "lock-1", "run-1", now); err != nil {
		t.Fatal(err)
	}
}

func TestLockConflictReturnsExisting(t *testing.T) {
	s := New("p1")
	first := []byte(`{"ID":"lock-1"}`)
	if _, err := s.Lock(first); err != nil {
		t.Fatal(err)
	}
	existing, err := s.Lock([]byte(`{"ID":"lock-2"}`))
	if err != ErrLocked || !bytes.Equal(existing, first) {
		t.Fatalf("%v %s", err, existing)
	}
	if err := s.Unlock("nope"); err != ErrLockMismatch {
		t.Fatalf("unlock: %v", err)
	}
	if err := s.Unlock("lock-1"); err != nil {
		t.Fatal(err)
	}
	s.ForceUnlock()
	if s.Snapshot().LockID != "" {
		t.Fatal("force unlock left a lock")
	}
}

func TestSnapshotIsolatedFromCallerMutation(t *testing.T) {
	s := New("p1")
	body := []byte(`{"version":4,"serial":7,"outputs":{"k":{"value":"v"}}}`)
	if err := s.Put(body, "", "run-1", time.Unix(5, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	snap.Data[0] = 'X'
	*snap.Serial = 99
	snap.ChangedOutputs["k"] = false

	again := s.Snapshot()
	if again.Data[0] != '{' {
		t.Fatalf("data mutated: %q", again.Data)
	}
	if *again.Serial != 7 {
		t.Fatalf("serial mutated: %d", *again.Serial)
	}
	if !again.ChangedOutputs["k"] {
		t.Fatalf("changed outputs mutated: %+v", again.ChangedOutputs)
	}
}

func TestImportState(t *testing.T) {
	s := New("p1")
	now := time.Unix(10, 0).UTC()

	// Invalid JSON or non-object JSON rejected
	for _, bad := range [][]byte{nil, []byte(""), []byte("not-json"), []byte("[]"), []byte("123"), []byte("null")} {
		if err := s.Import(bad, now); err != ErrInvalidState {
			t.Fatalf("Import(%q) = %v, want ErrInvalidState", bad, err)
		}
	}

	// Valid state JSON imports cleanly and computes changed outputs
	v1 := []byte(`{"version":4,"serial":5,"outputs":{"vpc_id":{"value":"vpc-1"}}}`)
	if err := s.Import(v1, now); err != nil {
		t.Fatalf("Import v1: %v", err)
	}
	snap := s.Snapshot()
	if snap.Serial == nil || *snap.Serial != 5 || !snap.UpdatedAt.Equal(now) {
		t.Fatalf("unexpected snapshot after import: %#v", snap)
	}
	if !snap.ChangedOutputsSincePrevious()["vpc_id"] {
		t.Fatalf("expected vpc_id marked changed after import: %+v", snap.ChangedOutputsSincePrevious())
	}

	// Importing while locked fails with ErrLocked
	if _, err := s.Lock([]byte(`{"ID":"lock-1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Import(v1, now); err != ErrLocked {
		t.Fatalf("Import while locked = %v, want ErrLocked", err)
	}
}

func TestMissingSerial(t *testing.T) {
	s := New("p1")
	if err := s.Put([]byte(`{"version":4}`), "", "run-1", time.Unix(1, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Serial != nil {
		t.Fatal("expected empty serial")
	}
}

func TestExtractOutputsAndChangedOutputs(t *testing.T) {
	tfstateV1 := []byte(`{
		"version": 4,
		"serial": 1,
		"outputs": {
			"vpc_id": {
				"value": "vpc-12345",
				"type": "string",
				"sensitive": false
			},
			"subnet_ids": {
				"value": ["sub-1", "sub-2"],
				"type": ["tuple", ["string", "string"]],
				"sensitive": false
			},
			"db_password": {
				"value": "s3cr3t",
				"type": "string",
				"sensitive": true
			},
			"empty_val": {
				"value": null,
				"type": "string"
			}
		}
	}`)

	outs, err := ExtractOutputs(tfstateV1)
	if err != nil {
		t.Fatalf("ExtractOutputs failed: %v", err)
	}
	if len(outs) != 3 {
		t.Fatalf("expected 3 non-null outputs, got %d: %+v", len(outs), outs)
	}
	if outs["vpc_id"].EnvString != "vpc-12345" || outs["vpc_id"].Sensitive {
		t.Fatalf("unexpected vpc_id output: %+v", outs["vpc_id"])
	}
	if outs["subnet_ids"].EnvString != `["sub-1","sub-2"]` {
		t.Fatalf("unexpected subnet_ids EnvString: %q", outs["subnet_ids"].EnvString)
	}
	if outs["db_password"].EnvString != "s3cr3t" || !outs["db_password"].Sensitive {
		t.Fatalf("unexpected db_password output: %+v", outs["db_password"])
	}

	st := New("proj-1")
	now := time.Unix(100, 0).UTC()
	if err := st.Put(tfstateV1, "", "run-1", now); err != nil {
		t.Fatal(err)
	}
	changed1 := st.Snapshot().ChangedOutputsSincePrevious()
	if !changed1["vpc_id"] || !changed1["subnet_ids"] || !changed1["db_password"] {
		t.Fatalf("expected all outputs marked changed on first state, got %+v", changed1)
	}

	tfstateV2 := []byte(`{
		"version": 4,
		"serial": 2,
		"outputs": {
			"vpc_id": {
				"value": "vpc-12345",
				"type": "string"
			},
			"subnet_ids": {
				"value": ["sub-1", "sub-2", "sub-3"],
				"type": ["tuple", ["string", "string", "string"]]
			}
		}
	}`)
	if err := st.Put(tfstateV2, "", "run-2", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	changed2 := st.Snapshot().ChangedOutputsSincePrevious()
	if changed2["vpc_id"] {
		t.Fatalf("expected vpc_id unchanged, got %+v", changed2)
	}
	if !changed2["subnet_ids"] {
		t.Fatalf("expected subnet_ids changed, got %+v", changed2)
	}
	if !changed2["db_password"] {
		t.Fatalf("expected removed output db_password marked changed, got %+v", changed2)
	}
}
