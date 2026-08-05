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

package sign

import (
	"strings"
	"sync"
	"testing"

	"git.0x0f.dev/varve/internal/config"
)

// TestExportForTaskMaterial asserts the returned key material carries the
// armored private key (with the PGP armor markers), the key ID and the
// configured passphrase (DETAIL §7.7 case 2).
func TestExportForTaskMaterial(t *testing.T) {
	requireGPG(t)
	const pass = "test-pass-123"
	src := t.TempDir()
	keyID := genTestKey(t, src, pass)
	keyFile := writeArmoredKeyFile(t, exportArmored(t, src, keyID, pass))

	s, err := newSigner(&config.GPGConfig{KeyFile: keyFile, Passphrase: pass}, t.TempDir())
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}
	km, err := s.ExportForTask("task-material")
	if err != nil {
		t.Fatalf("ExportForTask: %v", err)
	}
	if km.KeyID != keyID {
		t.Errorf("KeyID = %q, want %q", km.KeyID, keyID)
	}
	if !strings.HasPrefix(km.ArmoredPrivateKey, "-----BEGIN PGP PRIVATE KEY BLOCK-----") {
		t.Error("ArmoredPrivateKey missing BEGIN marker")
	}
	if !strings.HasSuffix(strings.TrimSpace(km.ArmoredPrivateKey), "-----END PGP PRIVATE KEY BLOCK-----") {
		t.Error("ArmoredPrivateKey missing END marker")
	}
	if km.Passphrase != pass {
		t.Errorf("Passphrase = %q, want %q", km.Passphrase, pass)
	}
}

// TestExportForTaskOnceOnly asserts the one-shot semantics: the second
// export of the same task is refused with ErrAlreadyExported (DETAIL §7.7
// case 2, DETAIL §7.5).
func TestExportForTaskOnceOnly(t *testing.T) {
	requireGPG(t)
	src := t.TempDir()
	keyID := genTestKey(t, src, "")
	keyFile := writeArmoredKeyFile(t, exportArmored(t, src, keyID, ""))

	s, err := newSigner(&config.GPGConfig{KeyFile: keyFile}, t.TempDir())
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}
	if _, err := s.ExportForTask("task-once"); err != nil {
		t.Fatalf("first export: %v", err)
	}
	if _, err := s.ExportForTask("task-once"); err != ErrAlreadyExported {
		t.Fatalf("second export error = %v, want ErrAlreadyExported", err)
	}
}

// TestExportForTaskIndependentTasks asserts that distinct tasks do not
// interfere: clearing one task leaves another claimable and re-claimable
// per its own one-shot semantics.
func TestExportForTaskIndependentTasks(t *testing.T) {
	requireGPG(t)
	src := t.TempDir()
	keyID := genTestKey(t, src, "")
	keyFile := writeArmoredKeyFile(t, exportArmored(t, src, keyID, ""))

	s, err := newSigner(&config.GPGConfig{KeyFile: keyFile}, t.TempDir())
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}
	if _, err := s.ExportForTask("task-a"); err != nil {
		t.Fatalf("export task-a: %v", err)
	}
	s.ClearTask("task-a")
	// task-b was never exported: still claimable after clearing task-a.
	if _, err := s.ExportForTask("task-b"); err != nil {
		t.Fatalf("export task-b: %v", err)
	}
	if _, err := s.ExportForTask("task-b"); err != ErrAlreadyExported {
		t.Fatalf("re-export task-b: got %v, want ErrAlreadyExported", err)
	}
}

// TestClearTaskReexport asserts that after ClearTask the same task can be
// exported again (documented semantics, DETAIL §7.7 case 3, §7.5).
func TestClearTaskReexport(t *testing.T) {
	requireGPG(t)
	src := t.TempDir()
	keyID := genTestKey(t, src, "")
	keyFile := writeArmoredKeyFile(t, exportArmored(t, src, keyID, ""))

	s, err := newSigner(&config.GPGConfig{KeyFile: keyFile}, t.TempDir())
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}
	if _, err := s.ExportForTask("task-clear"); err != nil {
		t.Fatalf("first export: %v", err)
	}
	s.ClearTask("task-clear")
	if _, err := s.ExportForTask("task-clear"); err != nil {
		t.Fatalf("export after ClearTask: %v", err)
	}
}

// TestClearTaskIdempotent asserts that clearing an unknown task succeeds.
func TestClearTaskIdempotent(t *testing.T) {
	requireGPG(t)
	src := t.TempDir()
	keyID := genTestKey(t, src, "")
	keyFile := writeArmoredKeyFile(t, exportArmored(t, src, keyID, ""))

	s, err := newSigner(&config.GPGConfig{KeyFile: keyFile}, t.TempDir())
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}
	s.ClearTask("never-exported") // must neither panic nor error
	s.ClearTask("never-exported")
}

// TestExportForTaskConcurrent guards the mutex (DETAIL §7.6): concurrent
// claims of the same task yield exactly one success and
// ErrAlreadyExported for the rest.
func TestExportForTaskConcurrent(t *testing.T) {
	requireGPG(t)
	src := t.TempDir()
	keyID := genTestKey(t, src, "")
	keyFile := writeArmoredKeyFile(t, exportArmored(t, src, keyID, ""))

	s, err := newSigner(&config.GPGConfig{KeyFile: keyFile}, t.TempDir())
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}
	const n = 8
	results := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ExportForTask("task-concurrent")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	ok, dup := 0, 0
	for err := range results {
		switch err {
		case nil:
			ok++
		case ErrAlreadyExported:
			dup++
		default:
			t.Errorf("unexpected export error: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("successful exports = %d, want 1", ok)
	}
	if dup != n-1 {
		t.Errorf("ErrAlreadyExported count = %d, want %d", dup, n-1)
	}
}
