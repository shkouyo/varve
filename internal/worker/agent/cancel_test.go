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

package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/api"
)

// trapScript builds a makepkg stand-in that records SIGTERM and keeps
// running until SIGKILL — making the SIGTERM→SIGKILL escalation observable
// (DETAIL §12.7 #5).
func trapScript(t *testing.T, record string) string {
	t.Helper()
	return writeScript(t, fmt.Sprintf("trap 'echo TERM >> %s' TERM\necho started\nwhile :; do sleep 0.5; echo tick; done", record))
}

// TestCancelViaLogAck asserts channel 2: a Cancelled=true log ack stops
// makepkg with SIGTERM escalated to SIGKILL and reports cancelled
// (DETAIL §12.4 #3, §12.7 #5).
func TestCancelViaLogAck(t *testing.T) {
	record := t.TempDir() + "/signals"
	f := &fakeClient{taskDetail: taskFor("t-1")}
	f.cancelAfter = 1 // first AppendLog ack carries Cancelled=true
	r := runOneShotRunner(t, f)
	exec := flowExec(t, r.workDir, "t-1", testSrcinfo, nil, map[string]string{
		"makepkg -s --noconfirm": trapScript(t, record),
	})
	r.execCommand = exec.command
	r.logThreshold = 8 // "started\n" reaches the threshold immediately
	r.logInterval = time.Hour
	r.killGrace = 200 * time.Millisecond

	start := time.Now()
	r.executeTask(context.Background(), taskFor("t-1"), "tok")
	elapsed := time.Since(start)

	res := f.lastResult()
	if res == nil || res.Status != statusCancelled {
		t.Fatalf("result = %+v, want cancelled", res)
	}
	// SIGTERM was delivered and recorded by the trap.
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read signal record: %v", err)
	}
	if !strings.Contains(string(data), "TERM") {
		t.Errorf("makepkg never received SIGTERM (record=%q)", data)
	}
	// The process ignored SIGTERM, so the kill had to escalate to SIGKILL
	// after killGrace — the run cannot have finished faster.
	if elapsed < r.killGrace*8/10 {
		t.Errorf("cancel finished after %v, want the SIGTERM→SIGKILL grace to elapse", elapsed)
	}
}

// TestCancelViaPoolHeartbeat asserts channel 1: heartbeat cancelled_task_ids
// stops the running pool task and reports cancelled (DETAIL §12.7 #5).
func TestCancelViaPoolHeartbeat(t *testing.T) {
	record := t.TempDir() + "/signals"
	f := &fakeClient{}
	f.hbCancelIDs = []string{"t-1"}
	task := taskFor("t-1")
	f.pollResps = []api.PollResp{{Task: task, ClaimToken: "tok"}}
	f.taskDetail = task

	cfg := configForTest(t, false)
	cfg.TaskID, cfg.TaskToken = "", "" // pool mode: no one-shot ids
	r := NewRunner(cfg, f)
	exec := flowExec(t, r.workDir, "t-1", testSrcinfo, nil, map[string]string{
		"makepkg -s --noconfirm": trapScript(t, record),
	})
	r.execCommand = exec.command
	r.pollInterval = 10 * time.Millisecond
	r.heartbeatInterval = 10 * time.Millisecond
	r.killGrace = 150 * time.Millisecond
	r.logThreshold = 8
	r.procDir = t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Wait for the cancelled report, then shut the pool down.
	if !waitFor(t, 5*time.Second, func() bool {
		res := f.lastResult()
		return res != nil && res.Status == statusCancelled
	}) {
		t.Fatalf("no cancelled result; results=%+v", f.results)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read signal record: %v", err)
	}
	if !strings.Contains(string(data), "TERM") {
		t.Errorf("makepkg never received SIGTERM (record=%q)", data)
	}
	if len(f.deregNames) != 1 {
		t.Errorf("deregister calls = %v, want 1 on shutdown", f.deregNames)
	}
}

// TestLateReportConflictIgnoredAfterCancel asserts a 409 on the cancelled
// report is tolerated (DETAIL §12.5: late reports are ignored).
func TestLateReportConflictIgnoredAfterCancel(t *testing.T) {
	record := t.TempDir() + "/signals"
	f := &fakeClient{taskDetail: taskFor("t-1")}
	f.cancelAfter = 1
	f.reportErr = &api.APIError{Status: 409, Code: "conflict"}
	f.reportErrTo = 1
	r := runOneShotRunner(t, f)
	exec := flowExec(t, r.workDir, "t-1", testSrcinfo, nil, map[string]string{
		"makepkg -s --noconfirm": trapScript(t, record),
	})
	r.execCommand = exec.command
	r.logThreshold = 8
	r.logInterval = time.Hour
	r.killGrace = 100 * time.Millisecond

	r.executeTask(context.Background(), taskFor("t-1"), "tok")
	if res := f.lastResult(); res == nil || res.Status != statusCancelled {
		t.Fatalf("result = %+v, want cancelled (conflict ignored)", res)
	}
}
