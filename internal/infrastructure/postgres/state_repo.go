package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/state"
)

var _ state.Repository = (*StateRepo)(nil)

type StateRepo struct{ pool *pgxpool.Pool }

func NewStateRepo(pool *pgxpool.Pool) *StateRepo { return &StateRepo{pool: pool} }

func (r *StateRepo) Load(ctx context.Context, projectID string) (*state.State, error) {
	var s state.Snapshot
	var updated *time.Time
	var lockID *string
	err := r.pool.QueryRow(ctx, `
		SELECT data, lock_id, lock_info, serial, updated_at
		FROM states WHERE project_id = $1`, projectID,
	).Scan(&s.Data, &lockID, &s.LockInfo, &s.Serial, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return state.New(projectID), nil
	}
	if err != nil {
		return nil, err
	}
	s.ProjectID = projectID
	if lockID != nil {
		s.LockID = *lockID
	}
	if updated != nil {
		s.UpdatedAt = utcTime(*updated)
	}
	history, err := r.loadHistory(ctx, projectID)
	if err != nil {
		return nil, err
	}
	s.History = history
	return state.Restore(s), nil
}

func (r *StateRepo) Save(ctx context.Context, st *state.State) error {
	s := st.Snapshot()
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
		UPDATE states SET data = $2, lock_id = $3, lock_info = $4, serial = $5, updated_at = $6
		WHERE project_id = $1`,
		s.ProjectID, s.Data, s.LockID, s.LockInfo, s.Serial, s.UpdatedAt,
	); err != nil {
		return err
	}
	for _, h := range st.NewHistory() {
		if _, err := tx.Exec(ctx, `
			INSERT INTO state_history (project_id, run_id, serial, data, created_at)
			VALUES ($1,$2,$3,$4,$5)`,
			s.ProjectID, h.RunID, h.Serial, h.Data, h.CreatedAt,
		); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *StateRepo) loadHistory(ctx context.Context, projectID string) ([]state.HistoryEntry, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT run_id, serial, data, created_at
		FROM state_history WHERE project_id = $1 ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var history []state.HistoryEntry
	for rows.Next() {
		var h state.HistoryEntry
		var created time.Time
		var runID *string
		if err := rows.Scan(&runID, &h.Serial, &h.Data, &created); err != nil {
			return nil, err
		}
		if runID != nil {
			h.RunID = *runID
		}
		h.CreatedAt = utcTime(created)
		history = append(history, h)
	}
	return history, rows.Err()
}
