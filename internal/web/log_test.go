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
	"net/http"
	"strings"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/dispatch"
)

// TestLogContentNegotiation drives the Accept matrix:
// text/event-stream selects SSE, everything else gets the HTML page.
func TestLogContentNegotiation(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	// A terminal build lets every SSE case finish (log increment → done).
	build := seedBuild(t, store, pkg, "succeeded", nil, nil)

	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store,
		newFakeLogReader("==> building\n"))

	cases := []struct {
		name   string
		accept string
		sse    bool
	}{
		{"exact sse", "text/event-stream", true},
		{"sse in list", "text/event-stream, text/html", true},
		{"sse with param", "text/event-stream; charset=utf-8", true},
		{"html", "text/html", false},
		{"html list", "text/html, application/xhtml+xml", false},
		{"none", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(t, s, http.MethodGet, "/builds/"+itoa(build.ID)+"/log",
				map[string]string{"Accept": tc.accept})
			if rec.Code != http.StatusOK {
				t.Fatalf("log page = %d, want 200", rec.Code)
			}
			if tc.sse {
				if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
					t.Errorf("Content-Type = %q, want text/event-stream", got)
				}
				mustContain(t, rec.Body.String(), "event: log", "event: done")
			} else {
				if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
					t.Errorf("Content-Type = %q, want text/html", got)
				}
				mustContain(t, rec.Body.String(), "<noscript>", `http-equiv="refresh"`, "EventSource")
			}
		})
	}
}

// TestSSEEventSequence asserts the log increment becomes event: log with
// the JSON {"offset","data"} payload and the terminal build closes the
// stream with event: done.
func TestSSEEventSequence(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "succeeded", nil, nil) // terminal

	logs := newFakeLogReader("line1\nline2\n")
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, logs)
	rec := get(t, s, http.MethodGet, "/builds/"+itoa(build.ID)+"/log",
		map[string]string{"Accept": "text/event-stream"})
	body := rec.Body.String()

	mustContain(t, body,
		"event: log",
		`"offset":12`,             // "line1\nline2\n" is 12 bytes
		`"data":"line1\nline2\n"`, // JSON-encoded payload
		"event: done",
	)
}

// TestSSEDoneOnTerminal asserts an empty log on a terminal build closes
// immediately with event: done and no ping.
func TestSSEDoneOnTerminal(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "cancelled", nil, nil) // terminal

	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, newFakeLogReader(""))
	rec := get(t, s, http.MethodGet, "/builds/"+itoa(build.ID)+"/log",
		map[string]string{"Accept": "text/event-stream"})
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
	req := newRequest(t, http.MethodGet, "/builds/"+itoa(build.ID)+"/log")
	req.Header.Set("Accept", "text/event-stream")
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
	rec := get(t, s, http.MethodGet, "/builds/"+itoa(build.ID)+"/log",
		map[string]string{"Accept": "text/event-stream"})
	body := rec.Body.String()
	mustContain(t, body, "No log was recorded", "event: done")
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
	req := newRequest(t, http.MethodGet, "/builds/"+itoa(build.ID)+"/log")
	req.Header.Set("Accept", "text/event-stream")
	req = req.WithContext(ctx)
	time.AfterFunc(120*time.Millisecond, cancel)
	rec := serve(t, s, req)
	body := rec.Body.String()
	mustContain(t, body, ": ping")
	if strings.Contains(body, "event: done") {
		t.Error("active build with missing log must not close the stream")
	}
}

// TestLogHTMLMissing asserts the HTML log page maps a missing log file to
// a 200 with a waiting state (queued/not-started), and a terminal build
// without a log renders a closing note.
func TestLogHTMLMissing(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")

	// Active build without a log: 200 + waiting.
	build := seedActiveBuild(t, store, pkg, "queued")
	logs := newFakeLogReader("")
	logs.readErr = dispatch.ErrNotFound
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store, logs)
	rec := get(t, s, http.MethodGet, "/builds/"+itoa(build.ID)+"/log", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("log HTML (active) = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Waiting for log output") {
		t.Error("active build log page must render the waiting state")
	}

	// Terminal build without a log: 200 + note.
	other := seedPackage(t, store, "other-pkg", "A second package")
	term := seedBuild(t, store, other, "cancelled", nil, nil)
	rec = get(t, s, http.MethodGet, "/builds/"+itoa(term.ID)+"/log", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("log HTML (terminal) = %d, want 200", rec.Code)
	}
}

// TestLogBuildMissing asserts a missing build is a 404 and a malformed
// build id a 400 for both the HTML page and the SSE stream.
func TestLogBuildMissing(t *testing.T) {
	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, newTestDB(t), newFakeLogReader(""))
	for _, accept := range []string{"", "text/event-stream"} {
		rec := get(t, s, http.MethodGet, "/builds/ffffffffffffffff/log", map[string]string{"Accept": accept})
		if rec.Code != http.StatusNotFound {
			t.Errorf("accept=%q: log page = %d, want 404", accept, rec.Code)
		}
		rec = get(t, s, http.MethodGet, "/builds/99999/log", map[string]string{"Accept": accept})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("accept=%q: malformed log id = %d, want 400", accept, rec.Code)
		}
	}
}

// TestLogHTMLNoScript asserts the no-JavaScript fallback meta refresh is
// present in the HTML log page.
func TestLogHTMLNoScript(t *testing.T) {
	store := newTestDB(t)
	pkg := seedPackage(t, store, "demo-pkg", "A demo package")
	build := seedBuild(t, store, pkg, "succeeded", nil, nil)

	s := newTestServer(t, testConfig(), &fakeOrchestrator{}, store,
		newFakeLogReader("tail of log"))
	rec := get(t, s, http.MethodGet, "/builds/"+itoa(build.ID)+"/log", nil)
	body := rec.Body.String()
	mustContain(t, body, "<noscript>", `http-equiv="refresh"`, "tail of log", "EventSource")
}
