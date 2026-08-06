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

package agent

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/api"
)

func TestNewRunnerMode(t *testing.T) {
	one := NewRunner(configForTest(t, true), &fakeClient{})
	if one.mode != modeOneShot {
		t.Errorf("OneShot=true mode = %v, want one-shot", one.mode)
	}
	pool := NewRunner(configForTest(t, false), &fakeClient{})
	if pool.mode != modePool {
		t.Errorf("OneShot=false mode = %v, want pool", pool.mode)
	}
	if one.workDir != one.cfg.DataDir+"/work" {
		t.Errorf("workDir = %q, want <data>/work", one.workDir)
	}
}

// TestOneShotDoesNotRegister runs the full one-shot flow and asserts the
// agent never calls Register.
func TestOneShotDoesNotRegister(t *testing.T) {
	f := &fakeClient{taskDetail: taskFor("t-1")}
	r := runOneShotRunner(t, f)
	exec := flowExec(t, r.workDir, "t-1", `pkgbase = foo
pkgver = 1.0
pkgrel = 1
arch = x86_64
pkgname = foo
`, []string{"foo-1.0-1-x86_64.pkg.tar.zst"}, nil)
	r.execCommand = exec.command
	r.logInterval = 20 * time.Millisecond
	r.logThreshold = 1 << 20

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.callCount("Register") != 0 {
		t.Errorf("one-shot agent registered a node (calls=%d)", f.callCount("Register"))
	}
	if f.callCount("GetTask") != 1 {
		t.Errorf("GetTask calls = %d, want 1", f.callCount("GetTask"))
	}
	res := f.lastResult()
	if res == nil || res.Status != statusSucceeded {
		t.Fatalf("result = %+v, want succeeded", res)
	}
	// The upload sequence must cover the package and the .SRCINFO snapshot.
	uploads := f.uploads
	if len(uploads) != 2 {
		t.Fatalf("uploads = %d, want 2 (package + .SRCINFO)", len(uploads))
	}
	if uploads[0].name != "foo-1.0-1-x86_64.pkg.tar.zst" || uploads[1].name != ".SRCINFO" {
		t.Errorf("upload order = %v", []string{uploads[0].name, uploads[1].name})
	}
}

func TestOneShotGetTaskForbiddenExitsNonZero(t *testing.T) {
	f := &fakeClient{getTaskErr: &api.APIError{Status: http.StatusForbidden, Code: "forbidden"}}
	r := runOneShotRunner(t, f)
	r.execCommand = newFakeExec().command

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("Run with a 403 GetTask should return an error (non-zero container exit)")
	}
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
		t.Errorf("error = %v, want the wrapped 403", err)
	}
	if f.callCount("ReportResult") != 0 {
		t.Errorf("one-shot agent reported after a failed GetTask")
	}
}

func TestOneShotGetTaskNotFoundExitsNonZero(t *testing.T) {
	f := &fakeClient{getTaskErr: &api.APIError{Status: http.StatusNotFound, Code: "not_found"}}
	r := runOneShotRunner(t, f)
	r.execCommand = newFakeExec().command
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("Run with a 404 GetTask should return an error (D4③)")
	}
}

func TestOneShotMissingTaskIDIsFatal(t *testing.T) {
	f := &fakeClient{taskDetail: taskFor("t-1")}
	cfg := configForTest(t, true)
	cfg.TaskID, cfg.TaskToken = "", ""
	r := NewRunner(cfg, f)
	r.execCommand = newFakeExec().command
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("Run without task id/token should fail fast")
	}
}
