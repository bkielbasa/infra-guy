package state

import (
	"bytes"
	"testing"
	"time"
)

func TestPutWithoutLockAppendsHistory(t *testing.T) {
	s := New("p1")
	now := time.Unix(5, 0).UTC()
	body := []byte(`{"version":4,"serial":7}`)
	if err := s.Put(body, "", "run-1", now); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if snap.Serial == nil || *snap.Serial != 7 || len(snap.History) != 1 {
		t.Fatalf("%#v", snap)
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
	body := []byte(`{"version":4,"serial":7}`)
	if err := s.Put(body, "", "run-1", time.Unix(5, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	snap.Data[0] = 'X'
	snap.History[0].Data[0] = 'Y'
	*snap.Serial = 99
	*snap.History[0].Serial = 99

	again := s.Snapshot()
	if again.Data[0] != '{' || again.History[0].Data[0] != '{' {
		t.Fatalf("data mutated: %q %q", again.Data, again.History[0].Data)
	}
	if *again.Serial != 7 || *again.History[0].Serial != 7 {
		t.Fatalf("serial mutated: %d %d", *again.Serial, *again.History[0].Serial)
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
