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
	"errors"
	"testing"
	"time"
)

// TestWithTxCommit asserts that a successful transaction persists.
func TestWithTxCommit(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "tx-ok")
	createTask(t, s, "tx-ok-1", "assigned", pkg, at(0))

	err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.FinalizeTask(testCtx, "tx-ok-1", "succeeded", "", at(time.Second), nil, nil)
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	task, err := s.GetTask(testCtx, "tx-ok-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "succeeded" {
		t.Errorf("state = %q, want succeeded", task.State)
	}
}

// TestWithTxRollback asserts that an error inside fn rolls back every
// write: no partial state survives.
func TestWithTxRollback(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "tx-roll")
	_, b := createTask(t, s, "tx-roll-1", "assigned", pkg, at(0))

	wantErr := errors.New("boom")
	err := s.WithTx(testCtx, func(tx *Tx) error {
		// First write succeeds inside the transaction...
		if err := tx.FinalizeTask(testCtx, "tx-roll-1", "succeeded", "", at(time.Second), nil, nil); err != nil {
			return err
		}
		// ...then the fn fails, so everything must roll back.
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("WithTx error = %v, want %v", err, wantErr)
	}

	task, err := s.GetTask(testCtx, "tx-roll-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != "assigned" {
		t.Errorf("state after rollback = %q, want assigned (no partial write)", task.State)
	}
	build, err := s.GetBuild(testCtx, b.ID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if build.Status != "assigned" || build.FinishedAt != nil || build.Error != "" {
		t.Errorf("build after rollback = %+v, want untouched", build)
	}

	// The rollback freed the partial-index slot: the task can still be
	// finalized.
	err = s.WithTx(testCtx, func(tx *Tx) error {
		return tx.FinalizeTask(testCtx, "tx-roll-1", "failed", "rolled back", at(2*time.Second), nil, nil)
	})
	if err != nil {
		t.Fatalf("finalize after rollback: %v", err)
	}
}

// TestFinalizeTaskConflict asserts that finalizing an already-terminal
// task fails with ErrConflict and never rewrites history.
func TestFinalizeTaskConflict(t *testing.T) {
	s := newTestStore(t)
	pkg := mustSeedPackage(t, s, "fx")
	_, b := createTask(t, s, "fx-1", "assigned", pkg, at(0))

	artifacts := []Artifact{{File: "a.pkg.tar.zst", Kind: "package"}}
	if err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.FinalizeTask(testCtx, "fx-1", "succeeded", "", at(time.Second), artifacts, nil)
	}); err != nil {
		t.Fatalf("first finalize: %v", err)
	}

	err := s.WithTx(testCtx, func(tx *Tx) error {
		return tx.FinalizeTask(testCtx, "fx-1", "failed", "late result", at(2*time.Second), nil, nil)
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second finalize = %v, want ErrConflict", err)
	}

	// History unchanged.
	build, err := s.GetBuild(testCtx, b.ID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if build.Status != "succeeded" || build.Error != "" || len(build.Artifacts) != 1 {
		t.Errorf("build rewritten after conflict: %+v", build)
	}

	// Finalizing an unknown task -> ErrNotFound.
	err = s.WithTx(testCtx, func(tx *Tx) error {
		return tx.FinalizeTask(testCtx, "ghost", "failed", "", at(3*time.Second), nil, nil)
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("finalize ghost = %v, want ErrNotFound", err)
	}
}

// TestFinalizeTaskMirror asserts tasks and builds stay in sync for every
// terminal state.
func TestFinalizeTaskMirror(t *testing.T) {
	cases := []struct {
		state string
	}{
		{"succeeded"},
		{"failed"},
		{"cancelled"},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			s := newTestStore(t)
			pkg := mustSeedPackage(t, s, "mirror-"+tc.state)
			_, b := createTask(t, s, "m-"+tc.state, "assigned", pkg, at(0))

			finished := at(5 * time.Second)
			errMsg := ""
			if tc.state == "failed" {
				errMsg = "makepkg: error"
			}
			if err := s.WithTx(testCtx, func(tx *Tx) error {
				return tx.FinalizeTask(testCtx, "m-"+tc.state, tc.state, errMsg, finished, nil, nil)
			}); err != nil {
				t.Fatalf("finalize: %v", err)
			}
			task, err := s.GetTask(testCtx, "m-"+tc.state)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if task.State != tc.state {
				t.Errorf("task state = %q, want %q", task.State, tc.state)
			}
			build, err := s.GetBuild(testCtx, b.ID)
			if err != nil {
				t.Fatalf("GetBuild: %v", err)
			}
			if build.Status != tc.state {
				t.Errorf("build status = %q, want %q (mirror)", build.Status, tc.state)
			}
			if build.FinishedAt == nil || !build.FinishedAt.Equal(finished) {
				t.Errorf("build.FinishedAt = %v, want %v", build.FinishedAt, finished)
			}
			if build.Error != errMsg {
				t.Errorf("build.Error = %q, want %q", build.Error, errMsg)
			}
		})
	}
}

// TestWithTxNilFn guards the WithTx precondition.
func TestWithTxNilFn(t *testing.T) {
	s := newTestStore(t)
	if err := s.WithTx(testCtx, nil); err == nil {
		t.Fatal("WithTx(nil) = nil error, want error")
	}
}
