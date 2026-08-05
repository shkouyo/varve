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

const workerColumns = `id, name, role, mode, arch, capacity, status, last_heartbeat, version`

// RegisterWorker upserts a worker keyed by name and fills w.ID. A second
// registration with the same name keeps the original id and updates the
// other fields.
func (s *Store) RegisterWorker(ctx context.Context, w *Worker) error {
	if w == nil || w.Name == "" {
		return errors.New("db: RegisterWorker requires a non-nil worker with a name")
	}
	var id int64
	err := s.write.QueryRowContext(ctx, `INSERT INTO workers
		(name, role, mode, arch, capacity, status, last_heartbeat, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			role = excluded.role,
			mode = excluded.mode,
			arch = excluded.arch,
			capacity = excluded.capacity,
			status = excluded.status,
			last_heartbeat = excluded.last_heartbeat,
			version = excluded.version
		RETURNING id`,
		w.Name, w.Role, w.Mode, w.Arch, w.Capacity, w.Status,
		formatNullableTime(w.LastHeartbeat), w.Version).Scan(&id)
	if err != nil {
		return fmt.Errorf("db: register worker %q: %w", w.Name, err)
	}
	w.ID = id
	return nil
}

// Heartbeat refreshes the worker's heartbeat and marks it online.
// ErrNotFound when the worker is not registered.
func (s *Store) Heartbeat(ctx context.Context, name string, at time.Time) error {
	res, err := s.write.ExecContext(ctx,
		`UPDATE workers SET status = 'online', last_heartbeat = ? WHERE name = ?`,
		formatTime(at), name)
	if err != nil {
		return fmt.Errorf("db: heartbeat worker %q: %w", name, err)
	}
	return requireAffected(res, fmt.Sprintf("heartbeat worker %q", name))
}

// SetWorkerStatus updates a worker's status field. ErrNotFound when the
// worker is not registered.
func (s *Store) SetWorkerStatus(ctx context.Context, name, status string) error {
	res, err := s.write.ExecContext(ctx,
		`UPDATE workers SET status = ? WHERE name = ?`, status, name)
	if err != nil {
		return fmt.Errorf("db: set worker %q status: %w", name, err)
	}
	return requireAffected(res, fmt.Sprintf("set worker %q status", name))
}

// GetWorkerByName returns one worker by its stable name (decision A21).
// ErrNotFound when the worker is not registered. Added by the M4 dispatch
// module: register/poll/heartbeat/deregister paths resolve the row by name.
func (s *Store) GetWorkerByName(ctx context.Context, name string) (*Worker, error) {
	w, err := scanWorker(s.read.QueryRowContext(ctx,
		`SELECT `+workerColumns+` FROM workers WHERE name = ?`, name))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("db: get worker %q: %w", name, err)
	}
	return w, nil
}

// GetWorkerByID returns one worker by its primary key. ErrNotFound when
// the worker does not exist. Added by the M4 dispatch module: the ingest
// path resolves the executing node's display name from task.worker_id.
func (s *Store) GetWorkerByID(ctx context.Context, id int64) (*Worker, error) {
	w, err := scanWorker(s.read.QueryRowContext(ctx,
		`SELECT `+workerColumns+` FROM workers WHERE id = ?`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("db: get worker %d: %w", id, err)
	}
	return w, nil
}

// ListWorkers returns all workers ordered by name.
func (s *Store) ListWorkers(ctx context.Context) ([]Worker, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+workerColumns+` FROM workers ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("db: list workers: %w", err)
	}
	defer rows.Close()
	out := []Worker{}
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list workers: %w", err)
		}
		out = append(out, *w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list workers: %w", err)
	}
	return out, nil
}

// DeleteWorker removes a worker by name without cascading. ErrNotFound
// when the worker is not registered. A worker referenced by builds or
// tasks rows fails with a foreign-key error (dispatch pre-checks active
// tasks and tolerates history references during the offline sweep).
func (s *Store) DeleteWorker(ctx context.Context, name string) error {
	res, err := s.write.ExecContext(ctx,
		`DELETE FROM workers WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("db: delete worker %q: %w", name, err)
	}
	return requireAffected(res, fmt.Sprintf("delete worker %q", name))
}

// scanWorker decodes one workers row.
func scanWorker(rs rowScanner) (*Worker, error) {
	var w Worker
	var hb sql.NullString
	if err := rs.Scan(&w.ID, &w.Name, &w.Role, &w.Mode, &w.Arch, &w.Capacity,
		&w.Status, &hb, &w.Version); err != nil {
		return nil, err
	}
	if hb.Valid {
		t, err := parseTime(hb.String)
		if err != nil {
			return nil, fmt.Errorf("db: decode last_heartbeat for worker %q: %w", w.Name, err)
		}
		w.LastHeartbeat = &t
	}
	return &w, nil
}
