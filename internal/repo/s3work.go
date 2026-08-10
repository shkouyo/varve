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
	"io"
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

	// 1. Download the database (+ signature) and the new packages. The
	// canonical object name is <name>.db; buckets that only hold the
	// <name>.db.tar.gz archive from older installs are accepted as a
	// fallback. The work dir file always keeps the archive name because
	// repo-add derives <name>.db / <name>.files from it.
	archive := u.repoDBArchiveName()
	if err := u.downloadToWork(ctx, u.repoDBName(), archive, filepath.Join(dir, archive)); err != nil {
		return err
	}
	if err := u.downloadToWork(ctx, u.repoDBName()+".sig", archive+".sig", filepath.Join(dir, archive+".sig")); err != nil {
		return err
	}
	for _, p := range pkgs {
		if err := u.downloadToWork(ctx, p.File, "", filepath.Join(dir, p.File)); err != nil {
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

	// 3. Upload the regenerated files back: the package database and its
	// file database sibling in both forms (the gzip archive under the
	// .tar.gz name and the identical bytes under the pacman-facing
	// .db / .files name), plus the packages.
	if err := u.uploadDualDB(ctx, dir, u.repoDBArchiveName(), u.repoDBName()); err != nil {
		return err
	}
	if err := u.uploadDualDB(ctx, dir, u.repoFilesDBArchiveName(), u.repoFilesDBName()); err != nil {
		return err
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

// downloadToWork streams one object into the local work dir. name is the
// canonical object name; when it is missing, fallback is tried (used for
// the database, where older installs only uploaded the .tar.gz archive; an
// empty fallback disables the retry). Every attempt that ends in
// ErrNotFound is accepted: the database does not exist yet on the first
// ingest, and optional files (signatures) may be absent.
func (u *updater) downloadToWork(ctx context.Context, name, fallback, local string) error {
	f, err := os.Create(local)
	if err != nil {
		return fmt.Errorf("s3 repo update: create work file %q: %w", local, err)
	}
	for i, n := range []string{name, fallback} {
		if n == "" {
			continue
		}
		if i > 0 {
			// Rewind between attempts: a failed Get must not leave
			// partial bytes in front of the fallback's content.
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				f.Close()
				os.Remove(local)
				return fmt.Errorf("s3 repo update: rewind work file %q: %w", local, err)
			}
			if err := f.Truncate(0); err != nil {
				f.Close()
				os.Remove(local)
				return fmt.Errorf("s3 repo update: truncate work file %q: %w", local, err)
			}
		}
		err = u.backend.Get(ctx, n, f)
		if err == nil {
			break
		}
		if !errors.Is(err, storage.ErrNotFound) {
			f.Close()
			os.Remove(local)
			return fmt.Errorf("s3 repo update: download %q: %w", n, err)
		}
	}
	cerr := f.Close()
	if cerr != nil {
		os.Remove(local)
		return fmt.Errorf("s3 repo update: close work file %q: %w", local, cerr)
	}
	return nil
}

// uploadDualDB uploads one regenerated database (the package database or
// the file database) back to the backend in both forms: the gzip archive
// under its .tar.gz name and the identical bytes under the pacman-facing
// name (<name>.db / <name>.files). Detached signatures, when present,
// follow the same dual naming. Files repo-add did not produce (e.g.
// without --sign) are skipped.
func (u *updater) uploadDualDB(ctx context.Context, dir, archive, name string) error {
	local := filepath.Join(dir, archive)
	if _, err := os.Stat(local); err != nil {
		return nil // not produced by repo-add
	}
	if err := u.uploadFromWork(ctx, local, archive); err != nil {
		return err
	}
	// The pacman-facing name carries the archive bytes: the content is
	// read from the archive file, never from a symlink target string.
	if err := u.uploadFromWork(ctx, local, name); err != nil {
		return err
	}
	if _, err := os.Stat(local + ".sig"); err != nil {
		return nil
	}
	if err := u.uploadFromWork(ctx, local+".sig", archive+".sig"); err != nil {
		return err
	}
	return u.uploadFromWork(ctx, local+".sig", name+".sig")
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
