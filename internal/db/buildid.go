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
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// buildIDLen is the fixed width of a build id: 16 lowercase hex
// characters (64 bits of randomness), the same shape the migration
// produces from legacy integer ids via printf('%016x', N).
const buildIDLen = 16

// newBuildID draws a fresh build id from crypto/rand and verifies inside
// the caller's transaction that it is unused, retrying on the
// astronomically unlikely collision (2^-64) instead of surfacing a
// duplicate-key conflict.
func newBuildID(ctx context.Context, tx *sql.Tx) (string, error) {
	for {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", fmt.Errorf("db: generate build id: %w", err)
		}
		id := hex.EncodeToString(raw[:])
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM builds WHERE id = ?`, id).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return id, nil
		}
		if err != nil {
			return "", fmt.Errorf("db: check build id %s: %w", id, err)
		}
	}
}
