package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/state"
	"infra-guy/internal/infrastructure/crypto"
)

var _ state.Repository = (*StateRepo)(nil)

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
	return tx.Commit(ctx)
}
