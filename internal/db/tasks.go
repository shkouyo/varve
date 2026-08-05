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
	"reflect"
	"sort"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const taskColumns = `id, package_id, build_id, state, COALESCE(worker_id, 0), assigned_at, created_at, last_progress_at, attempts, claim_token, cancel_requested`

// CreateTask inserts a task and its mirrored build row in one transaction
// and fills b.ID plus b.LogPath ("logs/<id>.log"). The build's status
// mirrors the task state. ErrConflict when the package already has an
// active (queued/assigned/running) task or the task id is a duplicate.
func (s *Store) CreateTask(ctx context.Context, t *Task, b *Build) error {
	if t == nil || b == nil {
		return errors.New("db: CreateTask requires a task and a build")
	}
	if t.ID == "" {
		return errors.New("db: CreateTask requires a task id")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if t.LastProgressAt.IsZero() {
		t.LastProgressAt = t.CreatedAt
	}
	b.PackageID = t.PackageID
	b.Status = t.State // builds.status mirrors the task state (DESIGN 3.1)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO builds
			(package_id, branch, "commit", upstream_ref, srcinfo_hash, status, worker_id, log_path, started_at, finished_at, error, artifacts, resource_usage)
			VALUES (?, ?, ?, ?, ?, ?, NULL, '', NULL, NULL, '', '[]', '[]')`,
			b.PackageID, b.Branch, b.Commit, b.UpstreamRef, b.SrcinfoHash, b.Status)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		b.ID = id
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks
			(id, package_id, build_id, state, worker_id, assigned_at, created_at, last_progress_at, attempts, claim_token, cancel_requested)
			VALUES (?, ?, ?, ?, NULL, NULL, ?, ?, 0, '', 0)`,
			t.ID, t.PackageID, b.ID, t.State, formatTime(t.CreatedAt), formatTime(t.LastProgressAt)); err != nil {
			return err
		}
		logPath := fmt.Sprintf("logs/%d.log", b.ID)
		if _, err := tx.ExecContext(ctx,
			`UPDATE builds SET log_path = ? WHERE id = ?`, logPath, b.ID); err != nil {
			return err
		}
		b.LogPath = logPath
		return nil
	})
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("db: create task %s: %w", t.ID, err)
	}
	t.BuildID = b.ID
	return nil
}

// GetTask returns one task by id with all fields. ErrNotFound when the
// task does not exist.
func (s *Store) GetTask(ctx context.Context, id string) (*Task, error) {
	t, err := scanTask(s.read.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("db: get task %s: %w", id, err)
	}
	return t, nil
}

// MarkRunning transitions an assigned task to running and mirrors the
// build row (status + started_at). ErrConflict when the task is not in
// state assigned, ErrNotFound when it does not exist.
func (s *Store) MarkRunning(ctx context.Context, id string, at time.Time) error {
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks SET state = 'running' WHERE id = ? AND state = 'assigned'`, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			var state string
			if err := tx.QueryRowContext(ctx,
				`SELECT state FROM tasks WHERE id = ?`, id).Scan(&state); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrNotFound
				}
				return err
			}
			return ErrConflict
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE builds SET status = 'running', started_at = ? WHERE id = (SELECT build_id FROM tasks WHERE id = ?)`,
			formatTime(at), id)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
			return err
		}
		return fmt.Errorf("db: mark task %s running: %w", id, err)
	}
	return nil
}

// TouchTaskProgress refreshes a task's last_progress_at. ErrNotFound when
// the task does not exist.
func (s *Store) TouchTaskProgress(ctx context.Context, id string, at time.Time) error {
	res, err := s.write.ExecContext(ctx,
		`UPDATE tasks SET last_progress_at = ? WHERE id = ?`, formatTime(at), id)
	if err != nil {
		return fmt.Errorf("db: touch task %s: %w", id, err)
	}
	return requireAffected(res, fmt.Sprintf("touch task %s", id))
}

// AppendResourceSamples merges samples into the build's resource_usage
// JSON, keeping one sample per timestamp and sorting by time. ErrNotFound
// when the build does not exist.
func (s *Store) AppendResourceSamples(ctx context.Context, buildID int64, samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var raw string
		err := tx.QueryRowContext(ctx,
			`SELECT resource_usage FROM builds WHERE id = ?`, buildID).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		existing, err := decodeSamples(raw)
		if err != nil {
			return fmt.Errorf("db: decode resource_usage for build %d: %w", buildID, err)
		}
		merged := mergeSamples(existing, samples)
		if reflect.DeepEqual(existing, merged) {
			return nil
		}
		encoded, err := encodeJSON(merged)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE builds SET resource_usage = ? WHERE id = ?`, encoded, buildID)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return err
		}
		return fmt.Errorf("db: append resource samples to build %d: %w", buildID, err)
	}
	return nil
}

// mergeSamples deduplicates by timestamp (existing entries win) and sorts
// the merged list ascending by time.
func mergeSamples(existing, incoming []Sample) []Sample {
	seen := make(map[string]bool, len(existing)+len(incoming))
	merged := make([]Sample, 0, len(existing)+len(incoming))
	for _, s := range existing {
		seen[formatTime(s.At)] = true
		merged = append(merged, s)
	}
	for _, s := range incoming {
		key := formatTime(s.At)
		if seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, s)
	}
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].At.Before(merged[j].At)
	})
	return merged
}

// scanTask decodes one tasks row.
func scanTask(rs rowScanner) (*Task, error) {
	var t Task
	var assignedAt sql.NullString
	var cancelRequested int64
	var createdAt, lastProgressAt string
	if err := rs.Scan(&t.ID, &t.PackageID, &t.BuildID, &t.State, &t.WorkerID, &assignedAt,
		&createdAt, &lastProgressAt, &t.Attempts, &t.ClaimToken, &cancelRequested); err != nil {
		return nil, err
	}
	t.CancelRequested = cancelRequested != 0
	if assignedAt.Valid {
		at, err := parseTime(assignedAt.String)
		if err != nil {
			return nil, fmt.Errorf("db: decode assigned_at for task %s: %w", t.ID, err)
		}
		t.AssignedAt = &at
	}
	created, err := parseTime(createdAt)
	if err != nil {
		return nil, fmt.Errorf("db: decode created_at for task %s: %w", t.ID, err)
	}
	t.CreatedAt = created
	progress, err := parseTime(lastProgressAt)
	if err != nil {
		return nil, fmt.Errorf("db: decode last_progress_at for task %s: %w", t.ID, err)
	}
	t.LastProgressAt = progress
	return &t, nil
}

// isUniqueViolation reports whether err is a SQLite UNIQUE constraint
// violation (partial index or column constraint).
func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}
