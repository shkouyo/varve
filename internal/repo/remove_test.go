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
	"io"
	"strings"
	"testing"
)

// TestRemoveLocal covers the local cascade: the side file, the artifacts
// it lists (with detached signatures) and the pacman database entries are
// removed, and a second run is a no-op (idempotent).
func TestRemoveLocal(t *testing.T) {
	e := newIngestEnv(t, "local", execCfg{})
	manifest := testManifest()
	sidecarName := testPkgbase + ".meta.toml"
	sidecarData, err := MarshalSidecar(&Sidecar{
		Pkgbase:   testPkgbase,
		Branch:    "foo",
		Artifacts: manifest,
		Build:     BuildInfo{Worker: "machine"},
	})
	if err != nil {
		t.Fatalf("MarshalSidecar: %v", err)
	}
	if err := e.fs.Put(context.Background(), sidecarName, strings.NewReader(string(sidecarData)), int64(len(sidecarData))); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}
	for _, a := range manifest {
		if err := e.fs.Put(context.Background(), a.File, strings.NewReader("x"), 1); err != nil {
			t.Fatalf("seed artifact %s: %v", a.File, err)
		}
	}

	if err := e.upd.Remove(context.Background(), testPkgbase); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// The side file and every listed artifact (+ .sig) are gone.
	if err := e.fs.Get(context.Background(), sidecarName, io.Discard); err == nil {
		t.Error("side file still present after Remove")
	}
	for _, a := range manifest {
		if a.Kind == "srcinfo" {
			continue // .SRCINFO is never persisted in the repository
		}
		if err := e.fs.Get(context.Background(), a.File, io.Discard); err == nil {
			t.Errorf("artifact %s still present after Remove", a.File)
		}
	}
	// The pacman database entry is repo-removed.
	execs := e.log.read()
	if !strings.Contains(strings.Join(execs, "\n"), "exec repo-remove "+e.root+" "+testDBArchive+" "+testPkgbase) {
		t.Errorf("no repo-remove recorded: %v", execs)
	}

	// Idempotent: a second run on the now-empty repository is a no-op.
	if err := e.upd.Remove(context.Background(), testPkgbase); err != nil {
		t.Fatalf("Remove again: %v", err)
	}
	if err := e.fs.Get(context.Background(), sidecarName, io.Discard); err == nil {
		t.Error("side file resurrected on the second Remove")
	}
}

// TestRemoveUnknown tolerates a package with no side file (already removed
// or never ingested): nothing to delete, no error.
func TestRemoveUnknown(t *testing.T) {
	e := newIngestEnv(t, "local", execCfg{})
	if err := e.upd.Remove(context.Background(), "ghost"); err != nil {
		t.Fatalf("Remove(unknown): %v", err)
	}
}
