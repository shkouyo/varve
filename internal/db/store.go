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
	"strings"
)

// Store is the SQLite persistence entry point: one serialized write
// connection (SetMaxOpenConns(1)) plus a pooled read connection set. All
// methods are safe for concurrent use; writes queue on the single write
// connection, reads run on the read pool.
type Store struct {
	write *sql.DB
	read  *sql.DB
}

// Open opens the database at path (":memory:" is allowed for tests),
// applies the required PRAGMAs and migrates the schema to the latest
// version.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("db: empty database path")
	}
	write, err := sql.Open("sqlite", writeDSN(path))
	if err != nil {
		return nil, fmt.Errorf("db: open write connection: %w", err)
	}
	write.SetMaxOpenConns(1)
	if err := configureConn(write); err != nil {
		write.Close()
		return nil, fmt.Errorf("db: configure write connection: %w", err)
	}
	s := &Store{write: write}
	if path == ":memory:" {
		// Every *sql.DB pool opened on ":memory:" would see its own
		// private database, so reads share the single write connection.
		s.read = write
	} else {
		read, err := sql.Open("sqlite", readDSN(path))
		if err != nil {
			write.Close()
			return nil, fmt.Errorf("db: open read connection: %w", err)
		}
		if err := configureConn(read); err != nil {
			write.Close()
			read.Close()
			return nil, fmt.Errorf("db: configure read connection: %w", err)
		}
		s.read = read
	}
	if err := s.migrate(context.Background()); err != nil {
		s.Close()
		return nil, fmt.Errorf("db: migrate %s: %w", path, err)
	}
	return s, nil
}

// Close releases both database handles.
func (s *Store) Close() error {
	var errs []error
	if s.read != s.write {
		if err := s.read.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.write.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// writeDSN enables BEGIN IMMEDIATE for every transaction on the write
// connection (_txlock) and applies the connection PRAGMAs at open time so
// they hold for every pooled connection.
func writeDSN(path string) string {
	if strings.Contains(path, "?") {
		return path
	}
	return path + "?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
}

// readDSN applies the same connection PRAGMAs to every read-pool
// connection. journal_mode=WAL is a persistent database property and only
// needs to be set once.
func readDSN(path string) string {
	if strings.Contains(path, "?") {
		return path
	}
	return path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
}

// configureConn applies the per-connection PRAGMAs required by the design
// (DETAIL 2.3).
func configureConn(db *sql.DB) error {
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return fmt.Errorf("PRAGMA journal_mode=WAL: %w", err)
	}
	for _, stmt := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return nil
}

// withTx runs fn inside one BEGIN IMMEDIATE transaction and commits it;
// any error returned by fn rolls the transaction back. Transactions
// serialize on the single write connection.
func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("%w (db: rollback: %v)", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: commit transaction: %w", err)
	}
	return nil
}

// requireAffected turns an UPDATE/DELETE that matched no row into
// ErrNotFound with context.
func requireAffected(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return nil
}
