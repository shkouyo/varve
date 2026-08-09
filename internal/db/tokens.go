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

// DispatchBinding is the persisted record of a queued task handed to a
// one-shot runner (worker.actions): the pre-issued claim token and the
// dispatch time. A restarted controller rebuilds its in-memory dispatch
// map from these rows, so a run that is still within its claim window is
// never double-dispatched and keeps its original token.
type DispatchBinding struct {
	TaskID       string
	Token        string
	DispatchedAt time.Time
}

// SetDispatchBinding persists a fresh one-shot dispatch binding for a
// queued task (claim_token and dispatched_at together). The state guard
// makes the claim race atomic: a task concurrently claimed by a poll
// (whose token was written inside the claim transaction) is never
// clobbered. ErrConflict when the task is no longer queued, ErrNotFound
// when it does not exist.
func (s *Store) SetDispatchBinding(ctx context.Context, id, token string, at time.Time) error {
	res, err := s.write.ExecContext(ctx, `UPDATE tasks SET
		claim_token = ?, dispatched_at = ?
		WHERE id = ? AND state = 'queued'`,
		token, formatTime(at), id)
	if err != nil {
		return fmt.Errorf("db: set dispatch binding for task %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var one int
		if err := s.read.QueryRowContext(ctx,
			`SELECT 1 FROM tasks WHERE id = ?`, id).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		return ErrConflict
	}
	return nil
}

// ClearDispatchBinding drops the one-shot dispatch binding of a task
// (token and dispatch time). It is idempotent: a task without a binding
// is a no-op. The requeue paths and the claim-timeout expiry use it so a
// re-dispatched task always carries a fresh token.
func (s *Store) ClearDispatchBinding(ctx context.Context, id string) error {
	if _, err := s.write.ExecContext(ctx, `UPDATE tasks SET
		claim_token = '', dispatched_at = NULL WHERE id = ?`, id); err != nil {
		return fmt.Errorf("db: clear dispatch binding for task %s: %w", id, err)
	}
	return nil
}

// TaskClaimToken returns the persisted claim token of a task. It is the
// restart-recoverable source of truth behind the orchestrator's
// in-memory token cache: a controller restart clears the cache but the
// claimed and dispatched tokens survive in this column. ErrNotFound when
// the task does not exist.
func (s *Store) TaskClaimToken(ctx context.Context, id string) (string, error) {
	var token string
	err := s.read.QueryRowContext(ctx,
		`SELECT claim_token FROM tasks WHERE id = ?`, id).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("db: read claim token of task %s: %w", id, err)
	}
	return token, nil
}

// ListDispatchBindings returns every queued task with a persisted
// one-shot dispatch binding, so a restarted controller can rebuild its
// in-memory dispatch map before the claim-timeout scan runs.
func (s *Store) ListDispatchBindings(ctx context.Context) ([]DispatchBinding, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, claim_token, dispatched_at FROM tasks
		 WHERE state = 'queued' AND dispatched_at IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("db: list dispatch bindings: %w", err)
	}
	defer rows.Close()
	out := []DispatchBinding{}
	for rows.Next() {
		var b DispatchBinding
		var dispatchedAt string
		if err := rows.Scan(&b.TaskID, &b.Token, &dispatchedAt); err != nil {
			return nil, fmt.Errorf("db: list dispatch bindings: %w", err)
		}
		at, err := parseTime(dispatchedAt)
		if err != nil {
			return nil, fmt.Errorf("db: decode dispatched_at of task %s: %w", b.TaskID, err)
		}
		b.DispatchedAt = at
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list dispatch bindings: %w", err)
	}
	return out, nil
}
