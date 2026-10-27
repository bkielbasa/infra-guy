package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/state"
	"infra-guy/internal/infrastructure/crypto"
)

var (
	_ state.Repository        = (*StateRepo)(nil)
	_ state.VersionRepository = (*StateRepo)(nil)
)

type StateRepo struct {
	pool   *pgxpool.Pool
	sealer *crypto.Sealer
}

func NewStateRepo(pool *pgxpool.Pool) *StateRepo { return &StateRepo{pool: pool} }

func (r *StateRepo) WithSealer(sealer *crypto.Sealer) *StateRepo {
	r.sealer = sealer
	return r
}

func StateDataAAD(projectID string) []byte {
	return []byte("state:" + projectID + ":data")
}

func StateVersionDataAAD(projectID string, serial int64) []byte {
	return []byte("state_version:" + projectID + ":" + strconv.FormatInt(serial, 10) + ":data")
}

func (r *StateRepo) Load(ctx context.Context, projectID string) (*state.State, error) {
	var s state.Snapshot
	var updated *time.Time
	var lockID *string
	var rawChanged []byte
	err := r.pool.QueryRow(ctx, `
		SELECT data, lock_id, lock_info, serial, updated_at, changed_outputs
		FROM states WHERE project_id = $1`, projectID,
	).Scan(&s.Data, &lockID, &s.LockInfo, &s.Serial, &updated, &rawChanged)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.sealer != nil && crypto.IsEnvelopeCiphertext(s.Data) {
		pt, err := r.sealer.OpenWithAAD(s.Data, StateDataAAD(projectID))
		if err != nil {
			return nil, fmt.Errorf("decrypt state for project %s: %w", projectID, err)
		}
		s.Data = pt
	}
	s.ProjectID = projectID
	if lockID != nil {
		s.LockID = *lockID
	}
	if updated != nil {
		s.UpdatedAt = utcTime(*updated)
	}
	if len(rawChanged) > 0 {
		var changed map[string]bool
		if err := json.Unmarshal(rawChanged, &changed); err == nil {
			s.ChangedOutputs = changed
		}
	}
	return state.Restore(s), nil
}

func (r *StateRepo) Save(ctx context.Context, st *state.State) error {
	s := st.Snapshot()
	changedJSON := []byte("{}")
	if len(s.ChangedOutputs) > 0 {
		b, err := json.Marshal(s.ChangedOutputs)
		if err != nil {
			return err
		}
		changedJSON = b
	}
	dataToStore := s.Data
	if r.sealer != nil && len(dataToStore) > 0 && !crypto.IsEnvelopeCiphertext(dataToStore) {
		ct, err := r.sealer.SealWithAAD(dataToStore, StateDataAAD(s.ProjectID))
		if err != nil {
			return fmt.Errorf("seal state for project %s: %w", s.ProjectID, err)
		}
		dataToStore = ct
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		INSERT INTO states (project_id) VALUES ($1) ON CONFLICT (project_id) DO NOTHING`, s.ProjectID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM states WHERE project_id = $1 FOR UPDATE`, s.ProjectID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE states SET data = $2, lock_id = $3, lock_info = $4, serial = $5, updated_at = $6, changed_outputs = $7
		WHERE project_id = $1`,
		s.ProjectID, dataToStore, s.LockID, s.LockInfo, s.Serial, s.UpdatedAt, changedJSON,
	); err != nil {
		return err
	}

	// Versions is populated only by Put, Import, and Rollback. Lock and unlock
	// saves leave it empty so they do not rewrite history timestamps.
	for _, v := range s.Versions {
		if len(v.Data) == 0 {
			continue
		}
		versionData := v.Data
		if r.sealer != nil && !crypto.IsEnvelopeCiphertext(versionData) {
			ct, err := r.sealer.SealWithAAD(versionData, StateVersionDataAAD(s.ProjectID, v.Serial))
			if err != nil {
				return fmt.Errorf("seal state version %d for project %s: %w", v.Serial, s.ProjectID, err)
			}
			versionData = ct
		}
		changedListJSON := []byte("[]")
		if len(v.ChangedOutputs) > 0 {
			if b, err := json.Marshal(v.ChangedOutputs); err == nil {
				changedListJSON = b
			}
		}
		createdAt := v.CreatedAt
		if createdAt.IsZero() {
			createdAt = s.UpdatedAt
		}
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO state_versions (
				project_id, serial, data, size_bytes, run_id, actor, resource_count, changed_outputs, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (project_id, serial) DO UPDATE SET
				data = EXCLUDED.data,
				size_bytes = EXCLUDED.size_bytes,
				run_id = CASE WHEN EXCLUDED.run_id <> '' THEN EXCLUDED.run_id ELSE state_versions.run_id END,
				actor = CASE WHEN EXCLUDED.actor <> '' THEN EXCLUDED.actor ELSE state_versions.actor END,
				resource_count = EXCLUDED.resource_count,
				changed_outputs = EXCLUDED.changed_outputs,
				created_at = EXCLUDED.created_at`,
			s.ProjectID, v.Serial, versionData, len(v.Data), v.RunID, v.Actor, v.ResourceCount, changedListJSON, createdAt,
		); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func parseChangedOutputsColumn(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		sort.Strings(list)
		return list
	}
	var obj map[string]bool
	if err := json.Unmarshal(raw, &obj); err == nil {
		for k, v := range obj {
			if v {
				list = append(list, k)
			}
		}
		sort.Strings(list)
		return list
	}
	return nil
}

func (r *StateRepo) ListVersions(ctx context.Context, projectID string) ([]state.VersionSummary, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT serial, size_bytes, run_id, actor, resource_count, changed_outputs, created_at
		FROM state_versions
		WHERE project_id = $1
		ORDER BY serial DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []state.VersionSummary
	for rows.Next() {
		var v state.VersionSummary
		var rawChanged []byte
		var createdAt time.Time
		if err := rows.Scan(&v.Serial, &v.Size, &v.RunID, &v.Actor, &v.ResourceCount, &rawChanged, &createdAt); err != nil {
			return nil, err
		}
		v.ChangedOutputs = parseChangedOutputsColumn(rawChanged)
		v.CreatedAt = utcTime(createdAt)
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *StateRepo) GetVersion(ctx context.Context, projectID string, serial int64) (*state.Version, error) {
	var v state.Version
	var rawChanged []byte
	var createdAt time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT serial, data, size_bytes, run_id, actor, resource_count, changed_outputs, created_at
		FROM state_versions
		WHERE project_id = $1 AND serial = $2`, projectID, serial,
	).Scan(&v.Serial, &v.Data, &v.Size, &v.RunID, &v.Actor, &v.ResourceCount, &rawChanged, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.sealer != nil && crypto.IsEnvelopeCiphertext(v.Data) {
		pt, err := r.sealer.OpenWithAAD(v.Data, StateVersionDataAAD(projectID, serial))
		if err != nil {
			// Fallback to StateDataAAD for state_versions backfilled from states by SQL migration 016.
			ptFallback, errFallback := r.sealer.OpenWithAAD(v.Data, StateDataAAD(projectID))
			if errFallback != nil {
				return nil, fmt.Errorf("decrypt state version %d for project %s: %w", serial, projectID, err)
			}
			pt = ptFallback
		}
		v.Data = pt
		v.Size = len(pt)
		if v.ResourceCount == 0 {
			v.ResourceCount = state.CountResources(pt)
		}
	}
	v.ChangedOutputs = parseChangedOutputsColumn(rawChanged)
	v.CreatedAt = utcTime(createdAt)
	return &v, nil
}
