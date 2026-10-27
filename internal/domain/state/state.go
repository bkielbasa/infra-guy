package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var (
	ErrLocked           = errors.New("state already locked")
	ErrLockMismatch     = errors.New("lock id mismatch")
	ErrInvalidState     = errors.New("invalid terraform state JSON")
	ErrVersionNotFound  = errors.New("state version not found")
)

type Metadata struct {
	Exists        bool
	Serial        *int64
	Size          int
	Locked        bool
	UpdatedAt     time.Time
	ResourceCount int
}

type VersionSummary struct {
	Serial         int64
	Size           int
	RunID          string
	Actor          string
	ResourceCount  int
	ChangedOutputs []string
	CreatedAt      time.Time
}

type Version struct {
	VersionSummary
	Data []byte
}

type OutputDiff struct {
	Name   string
	Action string // "added", "removed", "changed"
	Before string
	After  string
}

type ResourceDiff struct {
	Address    string
	Type       string
	Mode       string
	Provider   string
	Action     string // "added", "removed", "changed"
	BeforeJSON string
	AfterJSON  string
}

type StateDiff struct {
	FromSerial      int64
	ToSerial        int64
	AddedCount      int
	ChangedCount    int
	RemovedCount    int
	OutputChanges   []OutputDiff
	ResourceChanges []ResourceDiff
	UnifiedDiff     string
}

type Snapshot struct {
	ProjectID      string
	Data           []byte
	LockID         string
	LockInfo       []byte
	Serial         *int64
	UpdatedAt      time.Time
	ChangedOutputs map[string]bool
	LastRunID      string
	LastActor      string
	Versions       []Version
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

func cloneVersions(vs []Version) []Version {
	if len(vs) == 0 {
		return nil
	}
	out := make([]Version, len(vs))
	for i, v := range vs {
		out[i] = Version{
			VersionSummary: VersionSummary{
				Serial:         v.Serial,
				Size:           v.Size,
				RunID:          v.RunID,
				Actor:          v.Actor,
				ResourceCount:  v.ResourceCount,
				ChangedOutputs: append([]string(nil), v.ChangedOutputs...),
				CreatedAt:      v.CreatedAt,
			},
			Data: append([]byte(nil), v.Data...),
		}
	}
	return out
}

func cloneSnapshot(s Snapshot) Snapshot {
	out := s
	out.Data = append([]byte(nil), s.Data...)
	out.LockInfo = append([]byte(nil), s.LockInfo...)
	out.Serial = cloneInt64(s.Serial)
	out.ChangedOutputs = cloneBoolMap(s.ChangedOutputs)
	out.Versions = cloneVersions(s.Versions)
	return out
}

func Restore(s Snapshot) *State { return &State{s: cloneSnapshot(s)} }

func (s *State) Snapshot() Snapshot { return cloneSnapshot(s.s) }

func (s *State) Empty() bool { return len(s.s.Data) == 0 }

func (s Snapshot) Metadata() Metadata {
	return Metadata{
		Exists:        len(s.Data) > 0,
		Serial:        cloneInt64(s.Serial),
		Size:          len(s.Data),
		Locked:        s.LockID != "",
		UpdatedAt:     s.UpdatedAt,
		ResourceCount: CountResources(s.Data),
	}
}

func sortedChangedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (s *State) recordVersion(serial int64, data []byte, runID, actor string, changed map[string]bool, now time.Time) {
	v := Version{
		VersionSummary: VersionSummary{
			Serial:         serial,
			Size:           len(data),
			RunID:          runID,
			Actor:          actor,
			ResourceCount:  CountResources(data),
			ChangedOutputs: sortedChangedKeys(changed),
			CreatedAt:      now,
		},
		Data: append([]byte(nil), data...),
	}
	replaced := false
	for i := range s.s.Versions {
		if s.s.Versions[i].Serial == serial {
			s.s.Versions[i] = v
			replaced = true
			break
		}
	}
	if !replaced {
		s.s.Versions = append(s.s.Versions, v)
	}
	sort.Slice(s.s.Versions, func(i, j int) bool {
		return s.s.Versions[i].Serial > s.s.Versions[j].Serial
	})
}

func (s *State) Put(data []byte, lockID, runID string, now time.Time) error {
	if s.s.LockID != "" && lockID != s.s.LockID {
		return ErrLockMismatch
	}
	s.s.ChangedOutputs = diffOutputs(s.s.Data, data)
	s.s.Data = append([]byte(nil), data...)
	s.s.Serial = readSerial(data)
	s.s.UpdatedAt = now
	s.s.LastRunID = runID
	s.s.LastActor = "terraform"
	if s.s.Serial != nil {
		s.recordVersion(*s.s.Serial, s.s.Data, runID, "terraform", s.s.ChangedOutputs, now)
	}
	return nil
}

func (s *State) Import(data []byte, now time.Time) error {
	return s.ImportWithActor(data, "import", now)
}

func (s *State) ImportWithActor(data []byte, actor string, now time.Time) error {
	if s.s.LockID != "" {
		return ErrLocked
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return ErrInvalidState
	}
	if actor == "" {
		actor = "import"
	}
	s.s.ChangedOutputs = diffOutputs(s.s.Data, data)
	s.s.Data = append([]byte(nil), data...)
	s.s.Serial = readSerial(data)
	s.s.UpdatedAt = now
	s.s.LastRunID = ""
	s.s.LastActor = actor
	if s.s.Serial != nil {
		s.recordVersion(*s.s.Serial, s.s.Data, "", actor, s.s.ChangedOutputs, now)
	}
	return nil
}

// Rollback restores targetData as a new state version with an incremented serial
// number (maxKnownSerial + 1), rewriting the "serial" field inside the JSON state
// so Terraform's HTTP state backend accepts subsequent runs without serial regression.
func (s *State) Rollback(targetData []byte, actor string, now time.Time) (int64, error) {
	if s.s.LockID != "" {
		return 0, ErrLocked
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(targetData, &obj); err != nil || obj == nil {
		return 0, ErrInvalidState
	}
	var maxSerial int64
	if s.s.Serial != nil && *s.s.Serial > maxSerial {
		maxSerial = *s.s.Serial
	}
	for _, v := range s.s.Versions {
		if v.Serial > maxSerial {
			maxSerial = v.Serial
		}
	}
	if ts := readSerial(targetData); ts != nil && *ts > maxSerial {
		maxSerial = *ts
	}
	nextSerial := maxSerial + 1
	rolledData, err := rewriteSerial(targetData, nextSerial)
	if err != nil {
		return 0, err
	}
	if actor == "" {
		actor = "rollback"
	}
	s.s.ChangedOutputs = diffOutputs(s.s.Data, rolledData)
	s.s.Data = rolledData
	s.s.Serial = &nextSerial
	s.s.UpdatedAt = now
	s.s.LastRunID = ""
	s.s.LastActor = actor
	s.recordVersion(nextSerial, rolledData, "", actor, s.s.ChangedOutputs, now)
	return nextSerial, nil
}

func rewriteSerial(data []byte, serial int64) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return nil, ErrInvalidState
	}
	sb, err := json.Marshal(serial)
	if err != nil {
		return nil, err
	}
	obj["serial"] = sb
	out, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return nil, err
	}
	return out, nil
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

type parsedResource struct {
	Address  string
	Type     string
	Mode     string
	Provider string
	AttrJSON string
}

type tfStateResourcesPayload struct {
	Resources []struct {
		Module    string `json:"module"`
		Mode      string `json:"mode"`
		Type      string `json:"type"`
		Name      string `json:"name"`
		Provider  string `json:"provider"`
		Instances []struct {
			IndexKey   any             `json:"index_key"`
			Attributes json.RawMessage `json:"attributes"`
		} `json:"instances"`
	} `json:"resources"`
}

// CountResources returns the number of resource instances in a Terraform state JSON payload.
func CountResources(data []byte) int {
	if len(bytes.TrimSpace(data)) == 0 {
		return 0
	}
	var payload tfStateResourcesPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return 0
	}
	total := 0
	for _, r := range payload.Resources {
		if len(r.Instances) == 0 {
			total++
		} else {
			total += len(r.Instances)
		}
	}
	return total
}

func extractResources(data []byte) map[string]parsedResource {
	out := make(map[string]parsedResource)
	if len(bytes.TrimSpace(data)) == 0 {
		return out
	}
	var payload tfStateResourcesPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return out
	}
	for _, r := range payload.Resources {
		base := r.Type + "." + r.Name
		if r.Mode == "data" {
			base = "data." + base
		}
		if r.Module != "" {
			base = r.Module + "." + base
		}
		if len(r.Instances) == 0 {
			out[base] = parsedResource{
				Address:  base,
				Type:     r.Type,
				Mode:     r.Mode,
				Provider: r.Provider,
				AttrJSON: "{}",
			}
			continue
		}
		for idx, inst := range r.Instances {
			addr := base
			if inst.IndexKey != nil {
				switch k := inst.IndexKey.(type) {
				case string:
					addr = fmt.Sprintf(`%s[%q]`, base, k)
				case float64:
					addr = fmt.Sprintf(`%s[%d]`, base, int64(k))
				default:
					addr = fmt.Sprintf(`%s[%v]`, base, k)
				}
			} else if len(r.Instances) > 1 {
				addr = fmt.Sprintf(`%s[%d]`, base, idx)
			}
			attrStr := "{}"
			if len(inst.Attributes) > 0 {
				var pretty bytes.Buffer
				if err := json.Indent(&pretty, inst.Attributes, "", "  "); err == nil {
					attrStr = pretty.String()
				} else {
					attrStr = string(inst.Attributes)
				}
			}
			out[addr] = parsedResource{
				Address:  addr,
				Type:     r.Type,
				Mode:     r.Mode,
				Provider: r.Provider,
				AttrJSON: attrStr,
			}
		}
	}
	return out
}

func formatOutputForDiff(v OutputValue) string {
	if v.Sensitive {
		return "(sensitive)"
	}
	return v.EnvString
}

// DiffStates compares two Terraform state JSON payloads and returns structured
// resource and output diffs along with a unified text diff.
func DiffStates(fromSerial int64, fromData []byte, toSerial int64, toData []byte) StateDiff {
	diff := StateDiff{
		FromSerial: fromSerial,
		ToSerial:   toSerial,
	}

	fromOuts, _ := ExtractOutputs(fromData)
	toOuts, _ := ExtractOutputs(toData)
	outKeys := make(map[string]bool, len(fromOuts)+len(toOuts))
	for k := range fromOuts {
		outKeys[k] = true
	}
	for k := range toOuts {
		outKeys[k] = true
	}
	sortedOutKeys := make([]string, 0, len(outKeys))
	for k := range outKeys {
		sortedOutKeys = append(sortedOutKeys, k)
	}
	sort.Strings(sortedOutKeys)
	for _, k := range sortedOutKeys {
		fo, hasFrom := fromOuts[k]
		to, hasTo := toOuts[k]
		switch {
		case !hasFrom && hasTo:
			diff.OutputChanges = append(diff.OutputChanges, OutputDiff{
				Name:   k,
				Action: "added",
				After:  formatOutputForDiff(to),
			})
		case hasFrom && !hasTo:
			diff.OutputChanges = append(diff.OutputChanges, OutputDiff{
				Name:   k,
				Action: "removed",
				Before: formatOutputForDiff(fo),
			})
		case hasFrom && hasTo && !bytes.Equal(fo.Raw, to.Raw):
			diff.OutputChanges = append(diff.OutputChanges, OutputDiff{
				Name:   k,
				Action: "changed",
				Before: formatOutputForDiff(fo),
				After:  formatOutputForDiff(to),
			})
		}
	}

	fromRes := extractResources(fromData)
	toRes := extractResources(toData)
	resKeys := make(map[string]bool, len(fromRes)+len(toRes))
	for k := range fromRes {
		resKeys[k] = true
	}
	for k := range toRes {
		resKeys[k] = true
	}
	sortedResKeys := make([]string, 0, len(resKeys))
	for k := range resKeys {
		sortedResKeys = append(sortedResKeys, k)
	}
	sort.Strings(sortedResKeys)
	for _, addr := range sortedResKeys {
		fr, hasFrom := fromRes[addr]
		tr, hasTo := toRes[addr]
		switch {
		case !hasFrom && hasTo:
			diff.AddedCount++
			diff.ResourceChanges = append(diff.ResourceChanges, ResourceDiff{
				Address:   addr,
				Type:      tr.Type,
				Mode:      tr.Mode,
				Provider:  tr.Provider,
				Action:    "added",
				AfterJSON: tr.AttrJSON,
			})
		case hasFrom && !hasTo:
			diff.RemovedCount++
			diff.ResourceChanges = append(diff.ResourceChanges, ResourceDiff{
				Address:    addr,
				Type:       fr.Type,
				Mode:       fr.Mode,
				Provider:   fr.Provider,
				Action:     "removed",
				BeforeJSON: fr.AttrJSON,
			})
		case hasFrom && hasTo && fr.AttrJSON != tr.AttrJSON:
			diff.ChangedCount++
			diff.ResourceChanges = append(diff.ResourceChanges, ResourceDiff{
				Address:    addr,
				Type:       tr.Type,
				Mode:       tr.Mode,
				Provider:   tr.Provider,
				Action:     "changed",
				BeforeJSON: fr.AttrJSON,
				AfterJSON:  tr.AttrJSON,
			})
		}
	}

	diff.UnifiedDiff = buildStateUnifiedDiff(fromSerial, fromData, toSerial, toData)
	return diff
}

func sanitizeStateForDiff(data []byte) string {
	if len(bytes.TrimSpace(data)) == 0 {
		return "{}"
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return string(data)
	}
	if outs, ok := obj["outputs"].(map[string]any); ok {
		for _, rawOut := range outs {
			if om, ok := rawOut.(map[string]any); ok {
				if sens, _ := om["sensitive"].(bool); sens {
					om["value"] = "(sensitive)"
				}
			}
		}
	}
	b, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return string(data)
	}
	return string(b)
}

func buildStateUnifiedDiff(fromSerial int64, fromData []byte, toSerial int64, toData []byte) string {
	a := sanitizeStateForDiff(fromData)
	b := sanitizeStateForDiff(toData)
	if a == b {
		return ""
	}
	aLines := strings.Split(a, "\n")
	bLines := strings.Split(b, "\n")

	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("--- serial #%d\n+++ serial #%d\n", fromSerial, toSerial))

	// LCS-based line diff (bounded for performance on large states)
	if len(aLines)*len(bLines) <= 250000 {
		dp := make([][]int, len(aLines)+1)
		for i := range dp {
			dp[i] = make([]int, len(bLines)+1)
		}
		for i := len(aLines) - 1; i >= 0; i-- {
			for j := len(bLines) - 1; j >= 0; j-- {
				if aLines[i] == bLines[j] {
					dp[i][j] = 1 + dp[i+1][j+1]
				} else if dp[i+1][j] >= dp[i][j+1] {
					dp[i][j] = dp[i+1][j]
				} else {
					dp[i][j] = dp[i][j+1]
				}
			}
		}
		i, j := 0, 0
		for i < len(aLines) || j < len(bLines) {
			if i < len(aLines) && j < len(bLines) && aLines[i] == bLines[j] {
				buf.WriteString("  " + aLines[i] + "\n")
				i++
				j++
			} else if j < len(bLines) && (i == len(aLines) || dp[i][j+1] >= dp[i+1][j]) {
				buf.WriteString("+ " + bLines[j] + "\n")
				j++
			} else {
				buf.WriteString("- " + aLines[i] + "\n")
				i++
			}
		}
		return strings.TrimSuffix(buf.String(), "\n")
	}

	for _, l := range aLines {
		buf.WriteString("- " + l + "\n")
	}
	for _, l := range bLines {
		buf.WriteString("+ " + l + "\n")
	}
	return strings.TrimSuffix(buf.String(), "\n")
}
