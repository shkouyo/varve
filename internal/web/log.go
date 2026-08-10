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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/dispatch"
)

// maxInlineLog caps the log history served to a client at the most
// recent 1 MiB, both in the initial HTML render and in the SSE stream.
// A stream never starts before the truncation point, so a reconnect
// cannot pull unbounded history into the page.
const maxInlineLog = 1 << 20

// maxEventBytes caps one SSE log event at 64 KiB: a single read can
// never grow past two ping intervals of output, bounding the memory per
// event and giving every event a resumable byte offset.
const maxEventBytes = 64 << 10

// sseChunkBackoff is the pause after a full 64 KiB event. A high-output
// build otherwise turns the stream into a tight read-write-flush loop;
// 10 ms caps the per-connection write rate at ~6.4 MiB/s.
var sseChunkBackoff = 10 * time.Millisecond

// logEvent is the JSON payload of an SSE "log" event (JSON so multi-line
// log chunks survive). Offset is the byte offset the chunk ends at: the
// position the next ?after=/Last-Event-ID resume starts from. Data is
// raw log text that may split lines across events; the client appends
// it verbatim.
type logEvent struct {
	Offset int64  `json:"offset"`
	Data   string `json:"data"`
}

// handleLog redirects legacy log URLs to the merged build page anchor
// (302, so old links and bookmarks land on the live log section). A
// malformed build id is a 400; a well-formed but unknown one a 404.
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		s.renderError(w, r, http.StatusBadRequest, "Invalid build id.")
		return
	}
	if _, err := s.store.GetBuild(ctx, id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "Build not found: "+id)
			return
		}
		s.renderError(w, r, http.StatusInternalServerError, "Failed to load the build.")
		return
	}
	http.Redirect(w, r, "/builds/"+id+"#log", http.StatusFound)
}

// handleLogStream serves GET /builds/{id}/log/stream, the resumable SSE
// log stream consumed by the EventSource on the build page. A malformed
// build id is a 400; a well-formed but unknown one a 404.
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		s.renderError(w, r, http.StatusBadRequest, "Invalid build id.")
		return
	}
	build, err := s.store.GetBuild(ctx, id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "Build not found: "+id)
			return
		}
		s.renderError(w, r, http.StatusInternalServerError, "Failed to load the build.")
		return
	}
	s.serveSSE(w, r, build.ID)
}

// handleLogDownload serves GET /builds/{id}/log/download: the full
// build log as an attachment, streamed byte-for-byte from the log
// store (no truncation, unlike the inline page view). The response is
// text/plain with an attachment filename so the browser saves the
// original file. A malformed build id is a 400; a well-formed but
// unknown one or a build without a log is a 404.
func (s *Server) handleLogDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		s.renderError(w, r, http.StatusBadRequest, "Invalid build id.")
		return
	}
	if _, err := s.store.GetBuild(ctx, id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "Build not found: "+id)
			return
		}
		s.renderError(w, r, http.StatusInternalServerError, "Failed to load the build.")
		return
	}
	if _, err := s.logs.Size(ctx, id); err != nil {
		if errors.Is(err, dispatch.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "No log was recorded for this build.")
			return
		}
		s.renderError(w, r, http.StatusInternalServerError, "Failed to load the build log.")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="build-%s.log"`, id))
	w.WriteHeader(http.StatusOK)
	// The log is streamed straight to the client from offset 0. A read
	// error after the headers cannot change the status anymore; the
	// partial body stands as is.
	if _, err := s.logs.TailLog(ctx, id, 0, w, 0); err != nil {
		// Headers are already sent, so nothing more can be done.
	}
}

// serveSSE streams the build log to the client: each incremental read
// becomes an event: log with a JSON payload and the byte offset as the
// event id, an empty read is followed by a terminal-state check (event:
// done) or a keep-alive comment ping, and the goroutine exits when the
// client disconnects. The stream resumes from the Last-Event-ID header
// (browsers send it automatically on reconnection) or the ?after= query
// value, and never serves history older than the most recent
// maxInlineLog bytes.
func (s *Server) serveSSE(w http.ResponseWriter, r *http.Request, buildID string) {
	// Bound the number of live streams: each one polls the build row
	// every few seconds, so an unbounded count amplifies database load.
	select {
	case s.sseSem <- struct{}{}:
	default:
		s.renderError(w, r, http.StatusServiceUnavailable, "Too many live log streams.")
		return
	}
	defer func() { <-s.sseSem }()

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.renderError(w, r, http.StatusInternalServerError, "Streaming is not supported.")
		return
	}
	offset, ok := sseResume(r)
	if !ok {
		s.renderError(w, r, http.StatusBadRequest, "Invalid log offset.")
		return
	}

	// Clamp the resume point to the truncation cap: a client asking for
	// history older than the most recent maxInlineLog bytes is moved to
	// the truncation point. Only the byte size is consulted, so the
	// clamp costs one stat instead of a full log read.
	if size, err := s.logs.Size(r.Context(), buildID); err == nil {
		if size > maxInlineLog {
			if capStart := size - maxInlineLog; offset < capStart {
				offset = capStart
			}
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Vary", "Accept")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	for {
		var chunk bytes.Buffer
		newOffset, err := s.logs.TailLog(ctx, buildID, offset, &chunk, maxEventBytes)
		if err != nil {
			if errors.Is(err, dispatch.ErrNotFound) {
				// No log file yet: keep the stream open while the build is
				// still active (the first segment may arrive), and close
				// with a notice once the build reaches a terminal state
				// without ever writing a log.
				if terminal, ok := s.isTerminalBuild(ctx, buildID); ok {
					if terminal {
						writeSSELog(w, offset, "No log was recorded for this build.\n")
						writeSSEDone(w)
						return
					}
				} else {
					// The build row cannot be loaded: no further
					// increments will arrive. Close the stream; the
					// EventSource reconnects with Last-Event-ID and the
					// state check recovers if the row reappears.
					writeSSEDone(w)
					return
				}
			} else {
				writeSSELog(w, offset, "Failed to read the build log: "+err.Error()+"\n")
				writeSSEDone(w)
				return
			}
		}
		if chunk.Len() > 0 {
			offset = newOffset
			writeSSELog(w, offset, chunk.String())
			flusher.Flush()
			if chunk.Len() == maxEventBytes {
				// A full chunk: the log outgrew one event. Pause briefly
				// so a high-output build cannot spin the read loop.
				select {
				case <-time.After(sseChunkBackoff):
				case <-ctx.Done():
					return // client disconnected
				}
			}
			continue
		}

		// Empty increment: check whether the build reached a terminal
		// state (no more increments coming).
		if terminal, _ := s.isTerminalBuild(ctx, buildID); terminal {
			writeSSEDone(w)
			flusher.Flush()
			return
		}

		// Keep-alive comment ping (2s).
		_, _ = fmt.Fprint(w, ": ping\n\n")
		flusher.Flush()

		select {
		case <-time.After(s.pingInterval):
		case <-ctx.Done():
			return // client disconnected
		}
	}
}

// sseResume resolves the byte offset a client resumes from: the
// Last-Event-ID header (the protocol reconnection mechanism) wins over
// the ?after= query value. A missing value yields 0, a negative value
// clamps to 0, and a malformed value is rejected (ok=false).
func sseResume(r *http.Request) (int64, bool) {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("after")
	}
	if raw == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	if n < 0 {
		return 0, true
	}
	return n, true
}

// isTerminalBuild reports whether the build row reached a terminal
// status. ok is false when the row cannot be loaded; a missing or
// unreadable build produces no further increments, so callers treat it
// as terminal and close the stream (the client's EventSource reconnects
// with Last-Event-ID and the state check recovers if the row reappears).
func (s *Server) isTerminalBuild(ctx context.Context, buildID string) (terminal, ok bool) {
	b, err := s.store.GetBuild(ctx, buildID)
	if err != nil {
		log.Printf("web: check terminal state of build %s: %v", buildID, err)
		return true, false
	}
	return isTerminalStatus(b.Status), true
}

// writeSSELog emits one event: log with the JSON payload. The event id
// carries the byte offset the next resume starts from, so Last-Event-ID
// reconnection continues exactly where the client left off. The id line
// comes after the data line: a half-received frame then only shifts the
// reconnection window by the id line instead of the whole payload.
func writeSSELog(w http.ResponseWriter, offset int64, data string) {
	payload, _ := json.Marshal(logEvent{Offset: offset, Data: data})
	_, _ = fmt.Fprintf(w, "event: log\ndata: %s\nid: %d\n\n", payload, offset)
}

// writeSSEDone emits the terminal event: done.
func writeSSEDone(w http.ResponseWriter) {
	_, _ = fmt.Fprint(w, "event: done\ndata: {}\n\n")
}
