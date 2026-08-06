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

package repo

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"git.0x0f.dev/varve/internal/storage"
)

// s3RepoUpdate runs repo-add / repo-remove on the controller host for an
// s3-backed repository: the current database (+ signature) and the new
// package files are downloaded into the local work dir (cfg.Repo.WorkDir),
// the commands execute there, and the regenerated database files and
// packages are uploaded back. The work dir is cleared afterwards; a failed
// cleanup only warns and is overwritten by the next ingest.
func (u *updater) s3RepoUpdate(ctx context.Context, removed []string, pkgs []Artifact) error {
	dir := u.cfg.Repo.WorkDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("s3 repo update: create work dir %q: %w", dir, err)
	}

	// 1. Download the database (+ signature) and the new packages.
	db := u.repoDBName()
	if err := u.downloadToWork(ctx, db, filepath.Join(dir, db)); err != nil {
		return err
	}
	if err := u.downloadToWork(ctx, db+".sig", filepath.Join(dir, db+".sig")); err != nil {
		return err
	}
	for _, p := range pkgs {
		if err := u.downloadToWork(ctx, p.File, filepath.Join(dir, p.File)); err != nil {
			return err
		}
	}

	// 2. Run the database commands locally (remove before add).
	for _, name := range removed {
		if err := u.runRepoRemove(ctx, dir, name); err != nil {
			return err
		}
	}
	if err := u.runRepoAdd(ctx, dir, packageFiles(pkgs)); err != nil {
		return err
	}

	// 3. Upload the regenerated files back: the package database, its file
	// database sibling (both signed when --sign was used) and the packages.
	uploads := []string{db, db + ".sig", u.repoFilesDBName(), u.repoFilesDBName() + ".sig"}
	for _, name := range uploads {
		local := filepath.Join(dir, name)
		if _, err := os.Stat(local); err != nil {
			continue // not produced by repo-add (e.g. no --sign)
		}
		if err := u.uploadFromWork(ctx, local, name); err != nil {
			return err
		}
	}
	for _, p := range pkgs {
		if err := u.uploadFromWork(ctx, filepath.Join(dir, p.File), p.File); err != nil {
			return err
		}
	}

	// 4. Clear the work dir. Failure only warns: stale files are
	// overwritten by the next ingest and never block it.
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("repo: warning: clear work dir %q: %v", dir, err)
	}
	return nil
}

// downloadToWork streams one object into the local work dir. A missing
// object is accepted for optional files (the database does not exist yet
// on the first ingest and is created by repo-add).
func (u *updater) downloadToWork(ctx context.Context, name, local string) error {
	f, err := os.Create(local)
	if err != nil {
		return fmt.Errorf("s3 repo update: create work file %q: %w", local, err)
	}
	err = u.backend.Get(ctx, name, f)
	cerr := f.Close()
	if err != nil {
		os.Remove(local)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("s3 repo update: download %q: %w", name, err)
	}
	if cerr != nil {
		return fmt.Errorf("s3 repo update: close work file %q: %w", local, cerr)
	}
	return nil
}

// uploadFromWork streams one work dir file back into the backend.
func (u *updater) uploadFromWork(ctx context.Context, local, name string) error {
	f, err := os.Open(local)
	if err != nil {
		return fmt.Errorf("s3 repo update: open work file %q: %w", local, err)
	}
	defer f.Close()
	if err := u.backend.Put(ctx, name, f, -1); err != nil {
		return fmt.Errorf("s3 repo update: upload %q: %w", name, err)
	}
	return nil
}
