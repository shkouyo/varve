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
	"fmt"
	"strings"
	"time"
)

// ListStalledTasks returns tasks whose state is one of states and whose
// last_progress_at is older than before, oldest first.
func (s *Store) ListStalledTasks(ctx context.Context, before time.Time, states ...string) ([]Task, error) {
	if len(states) == 0 {
		return []Task{}, nil
	}
	query := `SELECT ` + taskColumns + ` FROM tasks WHERE state IN (` +
		placeholders(len(states)) + `) AND last_progress_at < ? ORDER BY last_progress_at`
	args := make([]any, 0, len(states)+1)
	for _, st := range states {
		args = append(args, st)
	}
	args = append(args, formatTime(before))
	return queryTasks(ctx, s.read, query, args...)
}

// ListActiveTasks returns all queued, assigned and running tasks ordered
// by creation time.
func (s *Store) ListActiveTasks(ctx context.Context) ([]Task, error) {
	return queryTasks(ctx, s.read,
		`SELECT `+taskColumns+` FROM tasks WHERE state IN ('queued', 'assigned', 'running') ORDER BY created_at`)
}

// ListTasksByWorker returns all tasks assigned to a worker ordered by
// creation time.
func (s *Store) ListTasksByWorker(ctx context.Context, workerID int64) ([]Task, error) {
	return queryTasks(ctx, s.read,
		`SELECT `+taskColumns+` FROM tasks WHERE worker_id = ? ORDER BY created_at`, workerID)
}

// ListTimedOutTasks returns assigned/running tasks whose assigned_at is
// older than before (deadline = assigned_at + build_timeout, DETAIL §4.4),
// oldest first. Added by the M4 dispatch module: the timeout scan is a
// distinct predicate from last_progress_at (a task can be actively
// progressing and still past its deadline).
func (s *Store) ListTimedOutTasks(ctx context.Context, before time.Time) ([]Task, error) {
	return queryTasks(ctx, s.read,
		`SELECT `+taskColumns+` FROM tasks
		 WHERE state IN ('assigned', 'running') AND assigned_at IS NOT NULL AND assigned_at < ?
		 ORDER BY assigned_at`, formatTime(before))
}

// placeholders renders "?,?,?,..." for n arguments.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// queryTasks runs a read query and decodes all rows.
func queryTasks(ctx context.Context, db *sql.DB, query string, args ...any) ([]Task, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list tasks: %w", err)
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list tasks: %w", err)
		}
		out = append(out, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list tasks: %w", err)
	}
	return out, nil
}
