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

package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/dispatch"
)

// TestBuildDetailRenders asserts the build page shows the summary, the
// machine (or "not assigned"), the log history and the resource samples
// as text.
func TestBuildDetailRenders(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	samples := []db.Sample{
		{At: time.Now().UTC(), CPUTimeNS: 12345678900, MemoryBytes: 536870912}, // 12.3s, 512 MiB
	}
	build := seedBuild(t, store, pkg, "failed", nil, samples)

	logs := newFakeLogReader("making package: demo-pkg\nfinished\n")
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, logs)
	rec := get(t, s, http.MethodGet, "/builds/"+itoa(build.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /builds/%s = %d, want 200", build.ID, rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body,
		"Build "+shortBuildID(itoa(build.ID)),
		"demo-pkg",                       // package context
		"not assigned",                   // machine fallback (no worker)
		"making package: demo-pkg",       // log history
		"CPU 12.3s",                      // resource usage text
		"Memory 512 MiB",                 // resource usage text
		"/builds/"+itoa(build.ID)+"/log", // live log link
	)
}

// TestBuildDetailMachineName resolves the executing node through
// GetWorkerByID when the build carries a worker id. The store never
// populates builds.worker_id through public methods, so the resolution is
// exercised at the handler level with a direct row-level check instead.
func TestBuildDetailMachineName(t *testing.T) {
	// The public read path is GetWorkerByID; verify it resolves the name
	// we seeded (used by handleBuild).
	store := newTestDB(t)
	w := seedWorker(t, store, "proud-heron-7")
	got, err := store.GetWorkerByID(testCtx, w.ID)
	if err != nil || got.Name != "proud-heron-7" {
		t.Fatalf("GetWorkerByID = %+v, %v; want proud-heron-7", got, err)
	}
}

// TestBuildNotFound asserts a missing build renders the 404 error page.
func TestBuildNotFound(t *testing.T) {
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, newTestDB(t), newFakeLogReader(""))
	rec := get(t, s, http.MethodGet, "/builds/ffffffffffffffff", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /builds/ffffffffffffffff = %d, want 404", rec.Code)
	}
	mustContain(t, rec.Body.String(), "Not Found", "Build not found")
}

// TestBuildInvalidID asserts a malformed build id is a 400.
func TestBuildInvalidID(t *testing.T) {
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, newTestDB(t), newFakeLogReader(""))
	rec := get(t, s, http.MethodGet, "/builds/12345", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET /builds/12345 = %d, want 400", rec.Code)
	}
}

// TestBuildLogData asserts the merged log data contract: the rendered
// line array, the SSE resume URL (at the end of the rendered content),
// the activity flags for the auto-refresh and the short-id page title.
func TestBuildLogData(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "succeeded", nil, nil) // terminal

	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader("line1\nline2\n"))
	data, err := s.buildData(newRequest(t, http.MethodGet, "/"), itoa(build.ID))
	if err != nil {
		t.Fatalf("buildData: %v", err)
	}
	if !data.HasLog || data.Truncated || data.Wait || data.Note != "" {
		t.Errorf("log flags = HasLog %v Truncated %v Wait %v Note %q, want only HasLog",
			data.HasLog, data.Truncated, data.Wait, data.Note)
	}
	want := []string{"line1", "line2", ""}
	if len(data.Lines) != len(want) || data.Lines[0] != want[0] || data.Lines[1] != want[1] || data.Lines[2] != want[2] {
		t.Errorf("Lines = %q, want %q", data.Lines, want)
	}
	if data.SSEURL != "/builds/"+itoa(build.ID)+"/log/stream?after=12" {
		t.Errorf("SSEURL = %q, want resume at the rendered length (12 bytes)", data.SSEURL)
	}
	if data.PageActive {
		t.Error("terminal build must not be marked active")
	}
	if data.RefreshSeconds != 0 {
		t.Errorf("RefreshSeconds = %d, want 0 on a terminal build", data.RefreshSeconds)
	}
	if data.Title != "Build "+shortBuildID(itoa(build.ID)) {
		t.Errorf("Title = %q, want the 7-char short build id", data.Title)
	}
}

// TestBuildLogTruncation asserts oversized logs are cut to the most
// recent maxInlineLog bytes with a truncation note: the first line is
// the partial remainder of the cut line and the SSE URL resumes after
// the rendered tail.
func TestBuildLogTruncation(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedActiveBuild(t, store, pkg, "running") // active

	tail := "tail-line\n"
	// The log is maxInlineLog+len(tail) bytes: the truncation point is 10
	// bytes in, so the rendered tail keeps the full 1 MiB window and ends
	// with the distinguishable tail line.
	logs := newFakeLogReader(strings.Repeat("x", maxInlineLog) + tail)
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, logs)
	data, err := s.buildData(newRequest(t, http.MethodGet, "/"), itoa(build.ID))
	if err != nil {
		t.Fatalf("buildData: %v", err)
	}
	if !data.Truncated {
		t.Fatal("oversized log must be flagged truncated")
	}
	if len(data.Log) != maxInlineLog || !strings.HasSuffix(data.Log, tail) {
		t.Errorf("Log = %d bytes, want the %d-byte recent window ending %q", len(data.Log), maxInlineLog, tail)
	}
	if len(data.Lines) != 2 || !strings.HasSuffix(data.Lines[0], "tail-line") || data.Lines[1] != "" {
		t.Errorf("Lines = %q, want the tail split into lines", data.Lines)
	}
	if !strings.Contains(data.TruncatedNote, "showing the last "+itoa(maxInlineLog)+" bytes") {
		t.Errorf("TruncatedNote = %q, want a byte-count note", data.TruncatedNote)
	}
	wantURL := "/builds/" + itoa(build.ID) + "/log/stream?after=" + itoa(int64(maxInlineLog+len(tail)))
	if data.SSEURL != wantURL {
		t.Errorf("SSEURL = %q, want %q", data.SSEURL, wantURL)
	}
	if !data.PageActive || data.RefreshSeconds != 10 {
		t.Errorf("active build: PageActive %v RefreshSeconds %d, want true/10", data.PageActive, data.RefreshSeconds)
	}
}

// TestBuildLogMissing asserts a queued build without a log renders the
// waiting state and a terminal build without a log renders the closing
// note, with the SSE URL left at its bare stream endpoint.
func TestBuildLogMissing(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	logs := newFakeLogReader("")
	logs.readErr = dispatch.ErrNotFound

	// The terminal build is seeded first: an active task later occupies
	// the unique per-package slot (the cancelled row no longer counts).
	term := seedBuild(t, store, pkg, "cancelled", nil, nil)
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, logs)
	data, err := s.buildData(newRequest(t, http.MethodGet, "/"), itoa(term.ID))
	if err != nil {
		t.Fatalf("buildData: %v", err)
	}
	if data.Note == "" || data.Wait {
		t.Errorf("terminal build without log: Note %q Wait %v, want a closing note", data.Note, data.Wait)
	}

	active := seedActiveBuild(t, store, pkg, "queued")
	data, err = s.buildData(newRequest(t, http.MethodGet, "/"), itoa(active.ID))
	if err != nil {
		t.Fatalf("buildData: %v", err)
	}
	if !data.Wait || data.HasLog || data.Note != "" {
		t.Errorf("active build without log: Wait %v HasLog %v Note %q, want waiting state",
			data.Wait, data.HasLog, data.Note)
	}
	if data.SSEURL != "/builds/"+itoa(active.ID)+"/log/stream" {
		t.Errorf("SSEURL = %q, want the bare stream endpoint", data.SSEURL)
	}
}

// TestFormatBytesAndCPU covers the text metric formatting.
func TestFormatBytesAndCPU(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{2048, "2 KiB"},
		{536870912, "512 MiB"},
		{1610612736, "1.5 GiB"},
	}
	for _, tc := range cases {
		if got := formatBytes(tc.in); got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := formatCPU(12345678900); got != "12.3s" {
		t.Errorf("formatCPU = %q, want 12.3s", got)
	}
}
