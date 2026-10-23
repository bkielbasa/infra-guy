package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrLocked       = errors.New("state already locked")
	ErrLockMismatch = errors.New("lock id mismatch")
	ErrInvalidState = errors.New("invalid terraform state JSON")
)

type Metadata struct {
	Exists    bool
	Serial    *int64
	Size      int
	Locked    bool
	UpdatedAt time.Time
}

type Snapshot struct {
	ProjectID      string
	Data           []byte
	LockID         string
	LockInfo       []byte
	Serial         *int64
	UpdatedAt      time.Time
	ChangedOutputs map[string]bool
}

type State struct {
	s Snapshot
}

func New(projectID string) *State { return &State{s: Snapshot{ProjectID: projectID}} }

func cloneInt64(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneBoolMap(m map[string]bool) map[string]bool {
	if m == nil {
		return nil
	}
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneSnapshot(s Snapshot) Snapshot {
	out := s
	out.Data = append([]byte(nil), s.Data...)
	out.LockInfo = append([]byte(nil), s.LockInfo...)
	out.Serial = cloneInt64(s.Serial)
	out.ChangedOutputs = cloneBoolMap(s.ChangedOutputs)
	return out
}

func Restore(s Snapshot) *State { return &State{s: cloneSnapshot(s)} }

func (s *State) Snapshot() Snapshot { return cloneSnapshot(s.s) }

func (s *State) Empty() bool { return len(s.s.Data) == 0 }

func (s Snapshot) Metadata() Metadata {
	return Metadata{
		Exists:    len(s.Data) > 0,
		Serial:    cloneInt64(s.Serial),
		Size:      len(s.Data),
		Locked:    s.LockID != "",
		UpdatedAt: s.UpdatedAt,
	}
}

func (s *State) Put(data []byte, lockID, _ string, now time.Time) error {
	if s.s.LockID != "" && lockID != s.s.LockID {
		return ErrLockMismatch
	}
	s.s.ChangedOutputs = diffOutputs(s.s.Data, data)
	s.s.Data = append([]byte(nil), data...)
	s.s.Serial = readSerial(data)
	s.s.UpdatedAt = now
	return nil
}

func (s *State) Import(data []byte, now time.Time) error {
	if s.s.LockID != "" {
		return ErrLocked
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return ErrInvalidState
	}
	s.s.ChangedOutputs = diffOutputs(s.s.Data, data)
	s.s.Data = append([]byte(nil), data...)
	s.s.Serial = readSerial(data)
	s.s.UpdatedAt = now
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

type OutputValue struct {
	Raw       json.RawMessage
	EnvString string
	Sensitive bool
}

func ExtractOutputs(data []byte) (map[string]OutputValue, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]OutputValue{}, nil
	}
	var payload struct {
		Outputs map[string]struct {
			Value     json.RawMessage `json:"value"`
			Sensitive bool            `json:"sensitive"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	out := make(map[string]OutputValue, len(payload.Outputs))
	for k, v := range payload.Outputs {
		raw := bytes.TrimSpace(v.Value)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			continue
		}
		var compacted bytes.Buffer
		if err := json.Compact(&compacted, raw); err != nil {
			compacted.Reset()
			compacted.Write(raw)
		}
		compactBytes := compacted.Bytes()
		var strVal string
		if err := json.Unmarshal(compactBytes, &strVal); err != nil {
			strVal = string(compactBytes)
		}
		out[k] = OutputValue{
			Raw:       append(json.RawMessage(nil), compactBytes...),
			EnvString: strVal,
			Sensitive: v.Sensitive,
		}
	}
	return out, nil
}

func (s Snapshot) Outputs() map[string]OutputValue {
	outs, err := ExtractOutputs(s.Data)
	if err != nil {
		return map[string]OutputValue{}
	}
	return outs
}

func diffOutputs(prevData, nextData []byte) map[string]bool {
	curr, err := ExtractOutputs(nextData)
	if err != nil {
		curr = map[string]OutputValue{}
	}
	changed := make(map[string]bool, len(curr))
	if len(bytes.TrimSpace(prevData)) == 0 {
		for k := range curr {
			changed[k] = true
		}
		return changed
	}
	prev, err := ExtractOutputs(prevData)
	if err != nil {
		for k := range curr {
			changed[k] = true
		}
		return changed
	}
	for k, cv := range curr {
		pv, ok := prev[k]
		if !ok || !bytes.Equal(cv.Raw, pv.Raw) {
			changed[k] = true
		}
	}
	for k := range prev {
		if _, ok := curr[k]; !ok {
			changed[k] = true
		}
	}
	return changed
}

func (s Snapshot) ChangedOutputsSincePrevious() map[string]bool {
	if s.ChangedOutputs != nil {
		return cloneBoolMap(s.ChangedOutputs)
	}
	curr := s.Outputs()
	changed := make(map[string]bool, len(curr))
	for k := range curr {
		changed[k] = true
	}
	return changed
}
