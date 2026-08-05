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
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/storage"
)

// Updater orchestrates the ingest of validated build artifacts into the
// pacman repository (DESIGN §2.6, §7.5-7.6; DETAIL §6): moving artifacts
// from staging into the flat repository root, pruning old versions, writing
// the authoritative side file and running repo-add / repo-remove.
//
// All methods are caller-serialized: the caller (dispatch) holds the ingest
// mutex, so no internal locking is performed (DETAIL §6.6).
type Updater interface {
	// Ingest ingests one validated build result. Caller contract:
	//
	//   - manifest entries have already been verified by the caller
	//     (existence, sha256 recomputation and, when signing is enabled,
	//     detached-signature verification, DESIGN §7.5 step 1);
	//   - build is the task's build record with Branch set, Commit resolved
	//     to the actually checked-out commit (result commit, falling back
	//     to the dispatched source commit, D1) and UpstreamRef set (D2);
	//   - workerName is the display name of the node that executed the
	//     build (host name for host mode, agent name for pool mode);
	//   - calls are serialized by the caller (DETAIL §6.6).
	//
	// Ingest is idempotent and safe to retry as a whole: every step is
	// re-runnable, and staging is only ever consumed, never created
	// (DESIGN §7.5 step 6 + 7, DETAIL §6.4 step 5).
	Ingest(ctx context.Context, task *db.Task, build *db.Build, workerName string, manifest []Artifact) error
}

// updater implements Updater. The signer dependency is narrowed to the
// minimum interface consumed here (DETAIL §0.3 rule 5).
type updater struct {
	cfg         *config.ControllerConfig
	backend     storage.Backend
	signer      interface{ GnuPGEnv() []string }
	now         func() time.Time
	execCommand func(ctx context.Context, name string, arg ...string) *exec.Cmd
}

// NewUpdater builds the repository updater. now may be nil (time.Now is
// used); tests inject a fixed clock for the side file timestamp.
func NewUpdater(cfg *config.ControllerConfig, backend storage.Backend, signer interface{ GnuPGEnv() []string }, now func() time.Time) *updater {
	if now == nil {
		now = time.Now
	}
	return &updater{
		cfg:         cfg,
		backend:     backend,
		signer:      signer,
		now:         now,
		execCommand: exec.CommandContext,
	}
}

// Ingest executes the ingest orchestration in the order mandated by D7
// (DETAIL §6.4): pkgbase resolution, artifact move, old-version cleanup,
// side file write and finally repo-add / repo-remove.
func (u *updater) Ingest(ctx context.Context, task *db.Task, build *db.Build, workerName string, manifest []Artifact) error {
	if task == nil {
		return errors.New("repo: ingest: nil task")
	}
	if build == nil {
		return errors.New("repo: ingest: nil build")
	}

	pkgs := packageEntries(manifest)
	if len(pkgs) == 0 {
		return errors.New("repo: ingest: manifest has no package artifacts (empty ingest is forbidden)")
	}

	// The side file is named after the pkgbase, which is not carried by
	// the manifest fields; the uploaded .SRCINFO snapshot is the only
	// authoritative source available without a db lookup. The agent always
	// uploads it (DESIGN §5.5, §12.4).
	src := srcinfoEntry(manifest)
	if src == nil {
		return errors.New("repo: ingest: manifest has no srcinfo artifact, cannot determine pkgbase")
	}
	if src.File == "" {
		return errors.New("repo: ingest: srcinfo artifact has an empty file name")
	}
	srcData, err := u.readStaged(ctx, task.ID, src.File)
	if err != nil {
		return err
	}
	pkgbase, err := extractPkgbase(srcData)
	if err != nil {
		return fmt.Errorf("repo: ingest: %w", err)
	}

	// 1. Move artifacts from the staging area into the flat repository
	// root (DETAIL §6.4 step 1). The .SRCINFO snapshot is excluded: it is
	// not persisted (decision A9, DESIGN §3.3) and only its hash is
	// recorded; the caller cleans it up together with the staging area.
	keep := manifestFileSet(manifest)
	for _, a := range manifest {
		if a.Kind == "srcinfo" {
			continue
		}
		if err := u.moveInto(ctx, storage.StagingPath(task.ID, a.File), a.File); err != nil {
			return fmt.Errorf("repo: ingest: move artifact %q: %w", a.File, err)
		}
	}

	// 2. Old-version cleanup (keep_versions=1, decision A19): files of the
	// previous build that are not part of the new manifest are removed,
	// together with their detached signatures. A missing or corrupt side
	// file is only a warning and is treated as "no previous version"
	// (DETAIL §6.5).
	sidecarName := pkgbase + ".meta.toml"
	old, hadOld := u.readOldSidecar(ctx, sidecarName)
	if hadOld {
		for _, oa := range old.Artifacts {
			if keep[oa.File] {
				continue
			}
			if err := u.backend.Delete(ctx, oa.File); err != nil {
				return fmt.Errorf("repo: ingest: delete old artifact %q: %w", oa.File, err)
			}
			if err := u.backend.Delete(ctx, oa.File+".sig"); err != nil {
				return fmt.Errorf("repo: ingest: delete old signature %q: %w", oa.File+".sig", err)
			}
		}
	}

	// 3. Atomically rewrite the side file (DESIGN §3.2). The backend's Put
	// is atomic on local storage (temp file + fsync + rename, DETAIL §5.4);
	// on s3 the ingest mutex provides the atomicity (DETAIL §6.4 step 3).
	vcs := ""
	if hadOld {
		vcs = old.VCS
	} else {
		vcs = inferVCS(pkgbase, pkgs)
	}
	sc := &Sidecar{
		Pkgbase:   pkgbase,
		Branch:    build.Branch,
		VCS:       vcs,
		Artifacts: manifest,
		Build: BuildInfo{
			Commit:      build.Commit,
			UpstreamRef: build.UpstreamRef,
			SrcinfoHash: src.SHA256,
			Time:        u.now().UTC(),
			Worker:      workerName,
		},
	}
	data, err := MarshalSidecar(sc)
	if err != nil {
		return err
	}
	if err := u.backend.Put(ctx, sidecarName, bytes.NewReader(data), int64(len(data))); err != nil {
		return fmt.Errorf("repo: ingest: write sidecar %q: %w", sidecarName, err)
	}

	// 4. Update the pacman database: every replaced pkgname (old side file
	// minus new manifest) is removed first, then all new packages are added
	// (remove-before-add, decision A19, DESIGN §7.6).
	removed := removedPkgnames(old, pkgs)
	if u.cfg.Storage.Backend == "s3" {
		if err := u.s3RepoUpdate(ctx, removed, pkgs); err != nil {
			return fmt.Errorf("repo: ingest: %w", err)
		}
	} else {
		if err := u.repoUpdateLocal(ctx, removed, pkgs); err != nil {
			return fmt.Errorf("repo: ingest: %w", err)
		}
	}
	return nil
}

// readStaged reads one artifact from the task staging area.
func (u *updater) readStaged(ctx context.Context, taskID, file string) ([]byte, error) {
	var buf bytes.Buffer
	name := storage.StagingPath(taskID, file)
	if err := u.backend.Get(ctx, name, &buf); err != nil {
		return nil, fmt.Errorf("repo: ingest: read staged %q: %w", file, err)
	}
	return buf.Bytes(), nil
}

// moveInto moves one object from the staging area into the repository root.
// The move is idempotent: if the destination already exists (a previous
// attempt of the same manifest), the entry is considered already ingested
// (DETAIL §6.4 step 5).
func (u *updater) moveInto(ctx context.Context, src, dst string) error {
	if m, ok := u.backend.(storage.Mover); ok {
		if err := m.Move(ctx, src, dst); err != nil {
			if _, serr := u.backend.Stat(ctx, dst); serr == nil {
				return nil
			}
			return fmt.Errorf("move %q -> %q: %w", src, dst, err)
		}
		return nil
	}
	// Degraded path without the Mover capability: Get + Put + Delete.
	var buf bytes.Buffer
	if err := u.backend.Get(ctx, src, &buf); err != nil {
		if _, serr := u.backend.Stat(ctx, dst); serr == nil {
			return nil
		}
		return fmt.Errorf("read staging %q: %w", src, err)
	}
	if err := u.backend.Put(ctx, dst, &buf, int64(buf.Len())); err != nil {
		return fmt.Errorf("put %q: %w", dst, err)
	}
	if err := u.backend.Delete(ctx, src); err != nil {
		return fmt.Errorf("delete staging %q: %w", src, err)
	}
	return nil
}

// readOldSidecar loads the previous side file of pkgbase. A missing file
// (ErrNotFound), a read failure or a parse failure is downgraded to a
// warning and reported as "no previous version" (DETAIL §6.5).
func (u *updater) readOldSidecar(ctx context.Context, name string) (*Sidecar, bool) {
	var buf bytes.Buffer
	if err := u.backend.Get(ctx, name, &buf); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, false
		}
		log.Printf("repo: warning: read old sidecar %q: %v", name, err)
		return nil, false
	}
	old, err := ParseSidecar(buf.Bytes())
	if err != nil {
		log.Printf("repo: warning: parse old sidecar %q: %v", name, err)
		return nil, false
	}
	return old, true
}

// removedPkgnames returns the pkgnames present in the old side file but
// absent from the new manifest package entries; each of them is removed
// from the repository database before the new packages are added (A19).
func removedPkgnames(old *Sidecar, pkgs []Artifact) []string {
	if old == nil {
		return nil
	}
	newSet := make(map[string]struct{}, len(pkgs))
	for _, p := range pkgs {
		newSet[p.Pkgname] = struct{}{}
	}
	var out []string
	for _, a := range old.Artifacts {
		if a.Kind != "package" {
			continue
		}
		if _, ok := newSet[a.Pkgname]; ok {
			continue
		}
		out = append(out, a.Pkgname)
	}
	return out
}

// packageEntries returns the manifest entries of kind "package".
func packageEntries(manifest []Artifact) []Artifact {
	var out []Artifact
	for _, a := range manifest {
		if a.Kind == "package" {
			out = append(out, a)
		}
	}
	return out
}

// srcinfoEntry returns the first manifest entry of kind "srcinfo".
func srcinfoEntry(manifest []Artifact) *Artifact {
	for i := range manifest {
		if manifest[i].Kind == "srcinfo" {
			return &manifest[i]
		}
	}
	return nil
}

// manifestFileSet returns the set of file names listed in the manifest.
func manifestFileSet(manifest []Artifact) map[string]bool {
	out := make(map[string]bool, len(manifest))
	for _, a := range manifest {
		out[a.File] = true
	}
	return out
}

// extractPkgbase reads the pkgbase key from an uploaded .SRCINFO snapshot
// (always emitted by makepkg --printsrcinfo as the first assignment).
func extractPkgbase(data []byte) (string, error) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "pkgbase" {
			continue
		}
		v := strings.Trim(strings.TrimSpace(val), `"`)
		if v == "" {
			return "", errors.New("ingest: .SRCINFO has an empty pkgbase")
		}
		return v, nil
	}
	return "", errors.New("ingest: .SRCINFO does not declare pkgbase")
}

// inferVCS derives the VCS kind of a package from its name suffix, mirroring
// the automatic branch of detect.DetectKind (DESIGN §2.3, proposal §7.3):
// "-git" / "-svn" suffixes mark VCS packages. It is only used on the first
// ingest; later ingests keep the value recorded in the previous side file.
func inferVCS(pkgbase string, pkgs []Artifact) string {
	for _, name := range []string{pkgbase} {
		if strings.HasSuffix(name, "-git") {
			return "git"
		}
		if strings.HasSuffix(name, "-svn") {
			return "svn"
		}
	}
	for _, p := range pkgs {
		if strings.HasSuffix(p.Pkgname, "-git") {
			return "git"
		}
		if strings.HasSuffix(p.Pkgname, "-svn") {
			return "svn"
		}
	}
	return ""
}
