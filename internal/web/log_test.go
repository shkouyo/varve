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
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/dispatch"
)

// TestLogRedirect asserts the legacy log URL redirects to the merged
// build page anchor, keeping 404/400 semantics for unknown or malformed
// build ids.
func TestLogRedirect(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "succeeded", nil, nil)
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader(""))

	rec := get(t, s, http.MethodGet, "/builds/"+itoa(build.ID)+"/log", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /builds/%s/log = %d, want 302", build.ID, rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/builds/"+itoa(build.ID)+"#log" {
		t.Errorf("Location = %q, want /builds/%s#log", loc, build.ID)
	}

	rec = get(t, s, http.MethodGet, "/builds/ffffffffffffffff/log", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown build log = %d, want 404", rec.Code)
	}
	rec = get(t, s, http.MethodGet, "/builds/99999/log", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed build log = %d, want 400", rec.Code)
	}
}

// TestLogDownload asserts GET /builds/{id}/log/download streams the
// full log byte-for-byte as an attachment: the body equals the log
// file, the content type is plain text and the disposition names a
// build-scoped filename. A build without a log or an unknown build is
// a 404 and a malformed id a 400.
func TestLogDownload(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "succeeded", nil, nil)
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader("line1\nline2\n"))
	rec := get(t, s, http.MethodGet, "/builds/"+itoa(build.ID)+"/log/download", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /log/download = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "line1\nline2\n" {
		t.Errorf("download body = %q, want the full log byte-for-byte", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", ct)
	}
	want := fmt.Sprintf(`attachment; filename="build-%s.log"`, build.ID)
	if cd := rec.Header().Get("Content-Disposition"); cd != want {
		t.Errorf("Content-Disposition = %q, want %q", cd, want)
	}

	// No log file: 404.
	missing := seedBuild(t, store, pkg, "cancelled", nil, nil)
	logs := newFakeLogReader("")
	logs.readErr = dispatch.ErrNotFound
	s = newTestServer(t, testConfig(), &fakeOrchestrator{}, store, logs)
	rec = get(t, s, http.MethodGet, "/builds/"+itoa(missing.ID)+"/log/download", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing log download = %d, want 404", rec.Code)
	}

	// Unknown build and malformed id.
	rec = get(t, s, http.MethodGet, "/builds/ffffffffffffffff/log/download", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown build download = %d, want 404", rec.Code)
	}
	rec = get(t, s, http.MethodGet, "/builds/99999/log/download", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed build download = %d, want 400", rec.Code)
	}
}

// TestSSEStream asserts the stream endpoint emits the log as event: log
// with a JSON {"offset","data"} payload, an id: line carrying the byte
// offset the next resume starts from, and closes a terminal build with
// event: done.
func TestSSEStream(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "succeeded", nil, nil) // terminal

	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader("line1\nline2\n"))
	rec := get(t, s, http.MethodGet, "/builds/"+itoa(build.ID)+"/log/stream", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /log/stream = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	mustContain(t, rec.Body.String(),
		"id: 12",                  // resume offset
		"event: log",              // "line1\nline2\n" is 12 bytes
		`"offset":12`,             //
		`"data":"line1\nline2\n"`, // JSON-encoded payload
		"event: done",
	)
}

// TestSSEResumeMatrix drives the ?after= and Last-Event-ID resume
// semantics: the stream starts at the requested offset, Last-Event-ID
// wins over the query value, negative values clamp to 0 and malformed
// values are a 400.
func TestSSEResumeMatrix(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "succeeded", nil, nil) // terminal
	id := itoa(build.ID)
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader("line1\nline2\n"))

	// after=6: the stream resumes mid-log and emits the remainder
	// ("line1\n" is 6 bytes, so offset 6 starts at "line2\n").
	rec := get(t, s, http.MethodGet, "/builds/"+id+"/log/stream?after=6", nil)
	mustContain(t, rec.Body.String(), `"data":"line2\n"`, "id: 12")

	// after=12: nothing left, the terminal build closes immediately.
	rec = get(t, s, http.MethodGet, "/builds/"+id+"/log/stream?after=12", nil)
	body := rec.Body.String()
	mustContain(t, body, "event: done")
	if strings.Contains(body, "event: log") {
		t.Error("no data event expected past the end of the log")
	}

	// Negative offsets clamp to 0: the full log is delivered.
	rec = get(t, s, http.MethodGet, "/builds/"+id+"/log/stream?after=-1", nil)
	mustContain(t, rec.Body.String(), `"data":"line1\nline2\n"`)

	// Last-Event-ID wins over ?after=.
	rec = get(t, s, http.MethodGet, "/builds/"+id+"/log/stream?after=0",
		map[string]string{"Last-Event-ID": "6"})
	mustContain(t, rec.Body.String(), `"data":"line2\n"`)

	// Malformed offsets are a 400.
	for _, q := range []string{"after=abc", "after=1.5"} {
		rec = get(t, s, http.MethodGet, "/builds/"+id+"/log/stream?"+q, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("?%s = %d, want 400", q, rec.Code)
		}
	}

	// Vary: Accept announces the negotiation on the stream response.
	rec = get(t, s, http.MethodGet, "/builds/"+id+"/log/stream", nil)
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept") {
		t.Errorf("Vary = %q, want Accept", got)
	}
}

// TestSSETruncationCap asserts the stream never serves history older
// than the most recent maxInlineLog bytes: a client resuming from 0 on
// an oversized log is clamped to the truncation point.
func TestSSETruncationCap(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "succeeded", nil, nil) // terminal

	head := strings.Repeat("A", 64)           // older than the cap: never served
	tail := strings.Repeat("B", maxInlineLog) // the most recent 1 MiB
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader(head+tail))

	rec := get(t, s, http.MethodGet, "/builds/"+itoa(build.ID)+"/log/stream?after=0", nil)
	body := rec.Body.String()
	mustContain(t, body, `"data":"`+tail+`"`, "id: "+itoa(int64(len(head)+len(tail))))
	if strings.Contains(body, "A") {
		t.Error("stream must not serve history older than the truncation point")
	}
}

// TestSSEDoneOnTerminal asserts an empty log on a terminal build closes
// immediately with event: done and no ping.
func TestSSEDoneOnTerminal(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "cancelled", nil, nil) // terminal

	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader(""))
	rec := get(t, s, http.MethodGet, "/builds/"+itoa(build.ID)+"/log/stream", nil)
	body := rec.Body.String()
	mustContain(t, body, "event: done")
	if strings.Contains(body, ": ping") {
		t.Error("terminal build must not emit keep-alive pings")
	}
}

// TestSSEPingAndDisconnect asserts the keep-alive comment ping appears on
// a non-terminal build and the handler exits when the client disconnects.
func TestSSEPingAndDisconnect(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedActiveBuild(t, store, pkg, "running") // non-terminal

	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader(""))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := newRequest(t, http.MethodGet, "/builds/"+itoa(build.ID)+"/log/stream")
	req = req.WithContext(ctx)

	// Cancel the client after a few ping intervals; the handler must
	// observe ctx.Done and return (the test completing proves it).
	time.AfterFunc(120*time.Millisecond, cancel)

	rec := serve(t, s, req)
	body := rec.Body.String()
	mustContain(t, body, ": ping")
	if strings.Contains(body, "event: done") {
		t.Error("disconnect must not emit event: done")
	}
}

// TestSSELogMissing asserts a missing log file on a terminal build yields
// an immediate event: log with a notice plus event: done.
func TestSSELogMissing(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "cancelled", nil, nil) // terminal

	logs := newFakeLogReader("")
	logs.tailErr = dispatch.ErrNotFound
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, logs)
	rec := get(t, s, http.MethodGet, "/builds/"+itoa(build.ID)+"/log/stream", nil)
	mustContain(t, rec.Body.String(), "No log was recorded", "event: done")
}

// TestSSELogMissingWaits asserts a missing log file on a still-active
// build keeps the stream open with keep-alive pings instead of closing:
// the first log segment may arrive at any moment.
func TestSSELogMissingWaits(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedActiveBuild(t, store, pkg, "queued") // non-terminal

	logs := newFakeLogReader("")
	logs.tailErr = dispatch.ErrNotFound
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, logs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := newRequest(t, http.MethodGet, "/builds/"+itoa(build.ID)+"/log/stream")
	req = req.WithContext(ctx)
	time.AfterFunc(120*time.Millisecond, cancel)
	rec := serve(t, s, req)
	body := rec.Body.String()
	mustContain(t, body, ": ping")
	if strings.Contains(body, "event: done") {
		t.Error("active build with missing log must not close the stream")
	}
}

// TestLogStreamBuildMissing asserts a missing build is a 404 and a
// malformed build id a 400 on the stream endpoint.
func TestLogStreamBuildMissing(t *testing.T) {
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, newTestDB(t), newFakeLogReader(""))
	rec := get(t, s, http.MethodGet, "/builds/ffffffffffffffff/log/stream", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown build stream = %d, want 404", rec.Code)
	}
	rec = get(t, s, http.MethodGet, "/builds/99999/log/stream", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed build stream = %d, want 400", rec.Code)
	}
}
