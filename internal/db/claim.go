// SPDX-License-Identifier: AGPL-3.0-or-later
//
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

// ClaimTask atomically claims the FIFO head task for a worker: the task
// must be queued, match the worker's architecture, have no other active
// task for the same package, and the worker must have spare capacity. The
// task is moved to assigned and the mirrored build row to assigned in the
// same BEGIN IMMEDIATE transaction, which serializes concurrent polls.
// ErrNoTask when nothing is claimable, ErrNotFound when the worker does
// not exist.
func (s *Store) ClaimTask(ctx context.Context, workerID int64, capacity int, token string) (*Task, error) {
	if token == "" {
		return nil, errors.New("db: ClaimTask requires a claim token")
	}
	var claimed *Task
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var arch string
		if err := tx.QueryRowContext(ctx,
			`SELECT arch FROM workers WHERE id = ?`, workerID).Scan(&arch); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: worker %d", ErrNotFound, workerID)
			}
			return err
		}
		var active int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tasks WHERE worker_id = ? AND state IN ('assigned', 'running')`,
			workerID).Scan(&active); err != nil {
			return err
		}
		if active >= capacity {
			return ErrNoTask
		}
		var id string
		err := tx.QueryRowContext(ctx, `SELECT t.id
			FROM tasks t
			JOIN packages p ON p.id = t.package_id
			WHERE t.state = 'queued'
			  AND p.arch = ?
			  AND NOT EXISTS (
				SELECT 1 FROM tasks a
				WHERE a.package_id = t.package_id
				  AND a.id != t.id
				  AND a.state IN ('queued', 'assigned', 'running')
			  )
			ORDER BY t.created_at, t.id
			LIMIT 1`, arch).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoTask
		}
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET
				state = 'assigned', worker_id = ?, assigned_at = ?, claim_token = ?, last_progress_at = ?
				WHERE id = ?`,
			workerID, formatTime(now), token, formatTime(now), id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE builds SET status = 'assigned' WHERE id = (SELECT build_id FROM tasks WHERE id = ?)`,
			id); err != nil {
			return err
		}
		task, err := scanTask(tx.QueryRowContext(ctx,
			`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id))
		if err != nil {
			return err
		}
		claimed = task
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNoTask) || errors.Is(err, ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("db: claim task for worker %d: %w", workerID, err)
	}
	return claimed, nil
}
