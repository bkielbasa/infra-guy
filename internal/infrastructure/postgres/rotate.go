package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"infra-guy/internal/domain/cloudintegration"
	"infra-guy/internal/domain/project"
	"infra-guy/internal/domain/run"
	"infra-guy/internal/domain/tfcontext"
	"infra-guy/internal/infrastructure/crypto"
)

// RotateSummary reports the number of records re-encrypted or sealed during RotateAndSealAll.
type RotateSummary struct {
	ContextEntries    int
	CloudIntegrations int
	ProjectTokens     int
	States            int
	RunPlanArtifacts  int
}

func (s RotateSummary) Total() int {
	return s.ContextEntries + s.CloudIntegrations + s.ProjectTokens + s.States + s.RunPlanArtifacts
}

// RotateAndSealAll re-encrypts any legacy ciphertexts or unsealed sensitive blobs
// in PostgreSQL using the active key of sealer. It is idempotent and safe to run
// online across multiple replicas.
func RotateAndSealAll(ctx context.Context, pool *pgxpool.Pool, sealer *crypto.Sealer) (RotateSummary, error) {
	var sum RotateSummary
	if pool == nil || sealer == nil {
		return sum, nil
	}

	// 1. Context entries (secret_env, secret_file)
	if n, err := rotateContextEntries(ctx, pool, sealer); err != nil {
		return sum, err
	} else {
		sum.ContextEntries = n
	}

	// 2. Cloud integrations (secret_access_key_ciphertext)
	if n, err := rotateCloudIntegrations(ctx, pool, sealer); err != nil {
		return sum, err
	} else {
		sum.CloudIntegrations = n
	}

	// 3. Project state tokens (state_token -> state_token_hash + state_token_ciphertext, clear state_token)
	if n, err := rotateProjectTokens(ctx, pool, sealer); err != nil {
		return sum, err
	} else {
		sum.ProjectTokens = n
	}

	// 4. Terraform states (states.data)
	if n, err := rotateStates(ctx, pool, sealer); err != nil {
		return sum, err
	} else {
		sum.States = n
	}

	// 5. Terraform run plan artifacts (runs.plan_file, runs.plan_json)
	if n, err := rotateRunArtifacts(ctx, pool, sealer); err != nil {
		return sum, err
	} else {
		sum.RunPlanArtifacts = n
	}

	return sum, nil
}

func rotateContextEntries(ctx context.Context, pool *pgxpool.Pool, sealer *crypto.Sealer) (int, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, context_id, kind, key, value_ciphertext
		FROM context_entries
		WHERE kind IN ('secret_env', 'secret_file') AND length(value_ciphertext) > 0`)
	if err != nil {
		return 0, err
	}
	type item struct {
		id, contextID, kind, key string
		ct                       []byte
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.contextID, &it.kind, &it.key, &it.ct); err != nil {
			rows.Close()
			return 0, err
		}
		if sealer.NeedsRotation(it.ct) {
			items = append(items, it)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	rotated := 0
	for _, it := range items {
		aad := []byte("context_entry:" + it.contextID + ":" + it.kind + ":" + it.key)
		pt, err := sealer.OpenWithAAD(it.ct, aad)
		if err != nil {
			pt, err = sealer.Open(it.ct)
		}
		if err != nil {
			return rotated, fmt.Errorf("open context_entry %s: %w", it.id, err)
		}
		newCT, err := sealer.SealWithAAD(pt, aad)
		if err != nil {
			return rotated, fmt.Errorf("seal context_entry %s: %w", it.id, err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE context_entries
			SET value_ciphertext = $2, value_text = ''
			WHERE id = $1`, it.id, newCT); err != nil {
			return rotated, err
		}
		rotated++
	}
	_ = tfcontext.KindSecretEnv
	return rotated, nil
}

func rotateCloudIntegrations(ctx context.Context, pool *pgxpool.Pool, sealer *crypto.Sealer) (int, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, secret_access_key_ciphertext
		FROM cloud_integrations
		WHERE length(secret_access_key_ciphertext) > 0`)
	if err != nil {
		return 0, err
	}
	type item struct {
		id string
		ct []byte
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.ct); err != nil {
			rows.Close()
			return 0, err
		}
		if sealer.NeedsRotation(it.ct) {
			items = append(items, it)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	rotated := 0
	for _, it := range items {
		aad := []byte("cloud_integration:" + string(cloudintegration.ID(it.id)) + ":secret_access_key")
		pt, err := sealer.OpenWithAAD(it.ct, aad)
		if err != nil {
			pt, err = sealer.Open(it.ct)
		}
		if err != nil {
			return rotated, fmt.Errorf("open cloud_integration %s: %w", it.id, err)
		}
		newCT, err := sealer.SealWithAAD(pt, aad)
		if err != nil {
			return rotated, fmt.Errorf("seal cloud_integration %s: %w", it.id, err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE cloud_integrations SET secret_access_key_ciphertext = $2 WHERE id = $1`, it.id, newCT); err != nil {
			return rotated, err
		}
		rotated++
	}
	return rotated, nil
}

func rotateProjectTokens(ctx context.Context, pool *pgxpool.Pool, sealer *crypto.Sealer) (int, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, state_token, state_token_hash, state_token_ciphertext
		FROM projects
		WHERE state_token <> '' OR length(state_token_ciphertext) > 0`)
	if err != nil {
		return 0, err
	}
	type item struct {
		id, plainToken, tokenHash string
		ct                        []byte
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.plainToken, &it.tokenHash, &it.ct); err != nil {
			rows.Close()
			return 0, err
		}
		if it.plainToken != "" || sealer.NeedsRotation(it.ct) {
			items = append(items, it)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	rotated := 0
	for _, it := range items {
		aad := ProjectStateTokenAAD(project.ID(it.id))
		rawToken := it.plainToken
		if rawToken == "" && len(it.ct) > 0 {
			pt, err := sealer.OpenWithAAD(it.ct, aad)
			if err != nil {
				return rotated, fmt.Errorf("open project %s state_token: %w", it.id, err)
			}
			rawToken = string(pt)
		}
		if rawToken == "" {
			continue
		}
		hash := crypto.HashToken(rawToken)
		newCT, err := sealer.SealWithAAD([]byte(rawToken), aad)
		if err != nil {
			return rotated, fmt.Errorf("seal project %s state_token: %w", it.id, err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE projects
			SET state_token = '', state_token_hash = $2, state_token_ciphertext = $3
			WHERE id = $1`, it.id, hash, newCT); err != nil {
			return rotated, err
		}
		rotated++
	}
	return rotated, nil
}

func rotateStates(ctx context.Context, pool *pgxpool.Pool, sealer *crypto.Sealer) (int, error) {
	rows, err := pool.Query(ctx, `
		SELECT project_id, data
		FROM states
		WHERE length(data) > 0`)
	if err != nil {
		return 0, err
	}
	type item struct {
		projectID string
		data      []byte
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.projectID, &it.data); err != nil {
			rows.Close()
			return 0, err
		}
		if sealer.NeedsRotation(it.data) {
			items = append(items, it)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	rotated := 0
	for _, it := range items {
		aad := StateDataAAD(it.projectID)
		pt := it.data
		if crypto.IsEnvelopeCiphertext(it.data) {
			opened, err := sealer.OpenWithAAD(it.data, aad)
			if err != nil {
				return rotated, fmt.Errorf("open state for project %s: %w", it.projectID, err)
			}
			pt = opened
		}
		newCT, err := sealer.SealWithAAD(pt, aad)
		if err != nil {
			return rotated, fmt.Errorf("seal state for project %s: %w", it.projectID, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE states SET data = $2 WHERE project_id = $1`, it.projectID, newCT); err != nil {
			return rotated, err
		}
		rotated++
	}
	return rotated, nil
}

func rotateRunArtifacts(ctx context.Context, pool *pgxpool.Pool, sealer *crypto.Sealer) (int, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, plan_file, plan_json
		FROM runs
		WHERE length(plan_file) > 0 OR length(plan_json) > 0`)
	if err != nil {
		return 0, err
	}
	type item struct {
		id                 string
		planFile, planJSON []byte
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.planFile, &it.planJSON); err != nil {
			rows.Close()
			return 0, err
		}
		if (len(it.planFile) > 0 && sealer.NeedsRotation(it.planFile)) ||
			(len(it.planJSON) > 0 && sealer.NeedsRotation(it.planJSON)) {
			items = append(items, it)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	rotated := 0
	for _, it := range items {
		runID := run.ID(it.id)
		pf := it.planFile
		if len(pf) > 0 && sealer.NeedsRotation(pf) {
			aad := RunPlanFileAAD(runID)
			if crypto.IsEnvelopeCiphertext(pf) {
				opened, err := sealer.OpenWithAAD(pf, aad)
				if err != nil {
					return rotated, fmt.Errorf("open run %s plan_file: %w", it.id, err)
				}
				pf = opened
			}
			newPF, err := sealer.SealWithAAD(pf, aad)
			if err != nil {
				return rotated, fmt.Errorf("seal run %s plan_file: %w", it.id, err)
			}
			pf = newPF
		}
		pj := it.planJSON
		if len(pj) > 0 && sealer.NeedsRotation(pj) {
			aad := RunPlanJSONAAD(runID)
			if crypto.IsEnvelopeCiphertext(pj) {
				opened, err := sealer.OpenWithAAD(pj, aad)
				if err != nil {
					return rotated, fmt.Errorf("open run %s plan_json: %w", it.id, err)
				}
				pj = opened
			}
			newPJ, err := sealer.SealWithAAD(pj, aad)
			if err != nil {
				return rotated, fmt.Errorf("seal run %s plan_json: %w", it.id, err)
			}
			pj = newPJ
		}
		if _, err := pool.Exec(ctx, `
			UPDATE runs SET plan_file = $2, plan_json = $3 WHERE id = $1`, it.id, pf, pj); err != nil {
			return rotated, err
		}
		rotated++
	}
	return rotated, nil
}
