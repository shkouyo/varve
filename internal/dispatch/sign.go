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

package dispatch

import (
	"context"
	"errors"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/sign"
)

// IssueSigningKey hands out the one-shot signing key material of a task
// (DESIGN §7.7). Each task may claim it exactly once; a second claim
// returns sign.ErrAlreadyExported, which the API maps to 409. Claim-token
// protected. Concurrently safe.
func (o *OrchestratorImpl) IssueSigningKey(ctx context.Context, taskID, token string) (*sign.KeyMaterial, error) {
	if err := o.checkToken(taskID, token); err != nil {
		return nil, err
	}
	if _, err := o.store.GetTask(ctx, taskID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if o.signer == nil {
		return nil, ErrConflict // signing is disabled (cfg.Repo.Sign == "off")
	}
	return o.signer.ExportForTask(taskID)
}
