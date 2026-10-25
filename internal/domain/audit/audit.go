package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

type ID string

type Snapshot struct {
	ID           ID             `json:"id"`
	TenantID     string         `json:"tenant_id"`
	ActorID      string         `json:"actor_id"`
	ActorLogin   string         `json:"actor_login"`
	ActorRole    string         `json:"actor_role"`
	Action       string         `json:"action"`
	ResourceType string         `json:"resource_type"`
	ResourceID   string         `json:"resource_id"`
	ResourceName string         `json:"resource_name"`
	Summary      string         `json:"summary"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	IPAddress    string         `json:"ip_address,omitempty"`
	PrevHash     string         `json:"prev_hash"`
	EntryHash    string         `json:"entry_hash"`
	CreatedAt    time.Time      `json:"created_at"`
}

type Filter struct {
	ResourceType string
	ActorLogin   string
	Action       string
	Query        string
	Since        time.Time
	Limit        int
}

type Repository interface {
	Append(ctx context.Context, entry Snapshot) (Snapshot, error)
	ListByTenant(ctx context.Context, tenantID string, filter Filter) ([]Snapshot, error)
}

// ComputeHash computes a deterministic SHA-256 digest linking the entry to the previous entry hash in the tenant's chain.
func ComputeHash(prevHash, tenantID string, createdAt time.Time, actorLogin, action, resourceType, resourceID, summary string) string {
	payload := fmt.Sprintf(
		"%s|%s|%d|%s|%s|%s|%s|%s",
		prevHash,
		tenantID,
		createdAt.UTC().UnixNano(),
		actorLogin,
		action,
		resourceType,
		resourceID,
		summary,
	)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// VerifyEntry checks whether a single audit entry's EntryHash matches its canonical fields and PrevHash.
func VerifyEntry(e Snapshot) bool {
	if e.EntryHash == "" {
		return false
	}
	expected := ComputeHash(e.PrevHash, e.TenantID, e.CreatedAt, e.ActorLogin, e.Action, e.ResourceType, e.ResourceID, e.Summary)
	return e.EntryHash == expected
}

// VerifyChain checks that every entry in a reverse-chronological (newest-first) list has a valid SHA-256 hash,
// and if the slice is an unfiltered contiguous tail/chain, that each entry's PrevHash matches the older entry's EntryHash.
func VerifyChain(entries []Snapshot, contiguous bool) bool {
	for i, e := range entries {
		if !VerifyEntry(e) {
			return false
		}
		if contiguous && i+1 < len(entries) {
			older := entries[i+1]
			if e.PrevHash != older.EntryHash {
				return false
			}
		}
	}
	return true
}

func MatchesFilter(e Snapshot, f Filter) bool {
	if f.ResourceType != "" && !strings.EqualFold(e.ResourceType, f.ResourceType) {
		return false
	}
	if f.ActorLogin != "" && !strings.EqualFold(e.ActorLogin, f.ActorLogin) {
		return false
	}
	if f.Action != "" && !strings.EqualFold(e.Action, f.Action) {
		return false
	}
	if !f.Since.IsZero() && e.CreatedAt.Before(f.Since) {
		return false
	}
	if q := strings.ToLower(strings.TrimSpace(f.Query)); q != "" {
		haystack := strings.ToLower(strings.Join([]string{
			e.ActorLogin,
			e.Action,
			e.ResourceType,
			e.ResourceID,
			e.ResourceName,
			e.Summary,
		}, " "))
		if !strings.Contains(haystack, q) {
			return false
		}
	}
	return true
}

type MemoryRepository struct {
	mu      sync.Mutex
	entries []Snapshot
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{}
}

func (r *MemoryRepository) Append(_ context.Context, entry Snapshot) (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var prevHash string
	for i := len(r.entries) - 1; i >= 0; i-- {
		if r.entries[i].TenantID == entry.TenantID {
			prevHash = r.entries[i].EntryHash
			break
		}
	}
	entry.CreatedAt = entry.CreatedAt.UTC().Truncate(time.Microsecond)
	entry.PrevHash = prevHash
	entry.EntryHash = ComputeHash(
		entry.PrevHash,
		entry.TenantID,
		entry.CreatedAt,
		entry.ActorLogin,
		entry.Action,
		entry.ResourceType,
		entry.ResourceID,
		entry.Summary,
	)
	if entry.Metadata != nil {
		cp := make(map[string]any, len(entry.Metadata))
		for k, v := range entry.Metadata {
			cp[k] = v
		}
		entry.Metadata = cp
	}
	r.entries = append(r.entries, entry)
	return entry, nil
}

func (r *MemoryRepository) ListByTenant(_ context.Context, tenantID string, filter Filter) ([]Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []Snapshot
	for i := len(r.entries) - 1; i >= 0; i-- {
		e := r.entries[i]
		if e.TenantID != tenantID {
			continue
		}
		if !MatchesFilter(e, filter) {
			continue
		}
		out = append(out, e)
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}
