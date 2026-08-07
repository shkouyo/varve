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

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log"

	"git.0x0f.dev/varve/internal/db"
)

// Remove implements detect.Sink: a branch that vanished from the source
// mirror cascades the removal of its package. The repository is cleaned
// first (artifact files, detached signatures, the side file and the
// pacman database entries via the updater), then the package's rows
// (tasks, builds, package) and the on-disk build logs. A repository
// failure aborts before the rows are dropped, so the next detection round
// retries the cascade (the repo side is idempotent). A package that is
// already gone is a no-op. The ingest mutex serializes the repository
// writes against concurrent ingests.
func (o *OrchestratorImpl) Remove(ctx context.Context, pkgbase string) error {
	pkg, err := o.store.GetPackageByBase(ctx, pkgbase)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil // already removed: idempotent
		}
		return err
	}
	log.Printf("dispatch: cascading removal of package %q (branch %q vanished from the source)", pkgbase, pkg.Branch)

	o.ingestMu.Lock()
	defer o.ingestMu.Unlock()

	if err := o.updater.Remove(ctx, pkgbase); err != nil {
		return fmt.Errorf("dispatch: remove package %q from repository: %w", pkgbase, err)
	}

	var buildIDs []string
	if err := o.store.WithTx(ctx, func(tx *db.Tx) error {
		ids, err := tx.DeletePackageRows(ctx, pkg.ID)
		if err != nil {
			return err
		}
		buildIDs = ids
		return nil
	}); err != nil {
		return fmt.Errorf("dispatch: remove package %q rows: %w", pkgbase, err)
	}

	for _, id := range buildIDs {
		if err := o.logs.Delete(id); err != nil && !errors.Is(err, ErrNotFound) {
			log.Printf("dispatch: delete log of removed build %s: %v", id, err)
		}
	}
	return nil
}
