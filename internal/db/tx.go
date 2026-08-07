// SPDX-License-Identifier: AGPL-3.0-or-later

// Copyright (C) 2026 ShinKouyo <i@0x0f.dev>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Tx exposes the write-transaction methods. A *Tx is only obtainable
// through WithTx; keeping the underlying *sql.Tx private prevents Store
// methods from being called mid-transaction by mistake.
type Tx struct {
	tx *sql.Tx
}

// WithTx runs fn inside one write transaction (BEGIN IMMEDIATE) and
// commits it. Any error returned by fn rolls the transaction back. The
// transaction body may only use the *Tx methods, never the Store directly.
func (s *Store) WithTx(ctx context.Context, fn func(tx *Tx) error) error {
	if fn == nil {
		return errors.New("db: WithTx requires a non-nil function")
	}
	return s.withTx(ctx, func(t *sql.Tx) error {
		return fn(&Tx{tx: t})
	})
}

// FinalizeTask writes a terminal state to the task and mirrors it on the
// build row (status, finished_at, error, artifacts, resource_usage) in the
// same transaction. ErrConflict when the task already reached a terminal
// state, ErrNotFound when the task does not exist.
func (t *Tx) FinalizeTask(ctx context.Context, id, state, errMsg string, at time.Time, artifacts []Artifact, samples []Sample) error {
	res, err := t.tx.ExecContext(ctx, `UPDATE tasks SET state = ?
		WHERE id = ? AND state NOT IN ('succeeded', 'failed', 'cancelled')`, state, id)
	if err != nil {
		return fmt.Errorf("db: finalize task %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var cur string
		if err := t.tx.QueryRowContext(ctx,
			`SELECT state FROM tasks WHERE id = ?`, id).Scan(&cur); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: task %s", ErrNotFound, id)
			}
			return err
		}
		return ErrConflict
	}
	arts, err := encodeJSON(artifacts)
	if err != nil {
		return fmt.Errorf("db: encode artifacts for task %s: %w", id, err)
	}
	samps, err := encodeJSON(samples)
	if err != nil {
		return fmt.Errorf("db: encode resource samples for task %s: %w", id, err)
	}
	_, err = t.tx.ExecContext(ctx, `UPDATE builds SET
			status = ?, finished_at = ?, error = ?, artifacts = ?, resource_usage = ?
			WHERE id = (SELECT build_id FROM tasks WHERE id = ?)`,
		state, formatTime(at), errMsg, arts, samps, id)
	if err != nil {
		return fmt.Errorf("db: mirror build for task %s: %w", id, err)
	}
	return nil
}

// FinalizeFailed writes the failed terminal state (via FinalizeTask) and
// stamps the package's last_failed_at rebuild-cooldown marker in the same
// transaction. It is the terminal branch of every failure path; the
// marker is what makes detect hold the package's next re-enqueue.
func (t *Tx) FinalizeFailed(ctx context.Context, id, errMsg string, at time.Time, artifacts []Artifact, samples []Sample) error {
	if err := t.FinalizeTask(ctx, id, "failed", errMsg, at, artifacts, samples); err != nil {
		return err
	}
	_, err := t.tx.ExecContext(ctx,
		`UPDATE packages SET last_failed_at = ? WHERE id = (SELECT package_id FROM tasks WHERE id = ?)`,
		formatTime(at), id)
	if err != nil {
		return fmt.Errorf("db: stamp last_failed_at for task %s: %w", id, err)
	}
	return nil
}

// DeletePackageRows removes every row of a package in child-first order
// (tasks, then builds, then the package row) and returns the removed
// build ids so the caller can clean their on-disk logs. Missing rows are
// tolerated, which keeps the cascade idempotent across partial failures.
func (t *Tx) DeletePackageRows(ctx context.Context, packageID int64) ([]string, error) {
	rows, err := t.tx.QueryContext(ctx,
		`SELECT id FROM builds WHERE package_id = ?`, packageID)
	if err != nil {
		return nil, fmt.Errorf("db: list builds of package %d: %w", packageID, err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: scan build id of package %d: %w", packageID, err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list builds of package %d: %w", packageID, err)
	}
	for _, stmt := range []string{
		`DELETE FROM tasks WHERE package_id = ?`,
		`DELETE FROM builds WHERE package_id = ?`,
		`DELETE FROM packages WHERE id = ?`,
	} {
		if _, err := t.tx.ExecContext(ctx, stmt, packageID); err != nil {
			return nil, fmt.Errorf("db: delete package %d rows: %w", packageID, err)
		}
	}
	return ids, nil
}
