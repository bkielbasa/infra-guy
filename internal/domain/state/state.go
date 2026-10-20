package state

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrLocked       = errors.New("state already locked")
	ErrLockMismatch = errors.New("lock id mismatch")
)

type HistoryEntry struct {
	RunID     string
	Serial    *int64
	Data      []byte
	CreatedAt time.Time
}

type Snapshot struct {
	ProjectID string
	Data      []byte
	LockID    string
	LockInfo  []byte
	Serial    *int64
	UpdatedAt time.Time
	History   []HistoryEntry
}

type State struct {
	s        Snapshot
	restored int
}

func New(projectID string) *State { return &State{s: Snapshot{ProjectID: projectID}} }

func cloneInt64(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneSnapshot(s Snapshot) Snapshot {
	out := s
	out.Data = append([]byte(nil), s.Data...)
	out.LockInfo = append([]byte(nil), s.LockInfo...)
	out.Serial = cloneInt64(s.Serial)
	if len(s.History) == 0 {
		out.History = nil
	} else {
		out.History = make([]HistoryEntry, len(s.History))
		for i, h := range s.History {
			out.History[i] = HistoryEntry{
				RunID:     h.RunID,
				Serial:    cloneInt64(h.Serial),
				Data:      append([]byte(nil), h.Data...),
				CreatedAt: h.CreatedAt,
			}
		}
	}
	return out
}

func Restore(s Snapshot) *State { return &State{s: cloneSnapshot(s), restored: len(s.History)} }

func (s *State) Snapshot() Snapshot { return cloneSnapshot(s.s) }

func (s *State) NewHistory() []HistoryEntry {
	return cloneSnapshot(Snapshot{History: s.s.History[s.restored:]}).History
}

func (s *State) Empty() bool { return len(s.s.Data) == 0 }

func (s *State) Put(data []byte, lockID, runID string, now time.Time) error {
	if s.s.LockID != "" && lockID != s.s.LockID {
		return ErrLockMismatch
	}
	serial := readSerial(data)
	s.s.Data = append([]byte(nil), data...)
	s.s.Serial = serial
	s.s.UpdatedAt = now
	s.s.History = append(s.s.History, HistoryEntry{
		RunID: runID, Serial: serial, Data: append([]byte(nil), data...), CreatedAt: now,
	})
	return nil
}

func (s *State) Lock(info []byte) ([]byte, error) {
	if s.s.LockID != "" {
		return append([]byte(nil), s.s.LockInfo...), ErrLocked
	}
	var payload struct {
		ID string
	}
	if err := json.Unmarshal(info, &payload); err != nil || payload.ID == "" {
		return nil, errors.New("lock info requires ID")
	}
	s.s.LockID = payload.ID
	s.s.LockInfo = append([]byte(nil), info...)
	return nil, nil
}

func (s *State) Unlock(lockID string) error {
	if s.s.LockID == "" || s.s.LockID != lockID {
		return ErrLockMismatch
	}
	s.s.LockID = ""
	s.s.LockInfo = nil
	return nil
}

func (s *State) ForceUnlock() {
	s.s.LockID = ""
	s.s.LockInfo = nil
}

func readSerial(data []byte) *int64 {
	var payload struct {
		Serial *int64 `json:"serial"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil
	}
	return payload.Serial
}
