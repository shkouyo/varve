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
	"net/http"
	"strconv"
	"strings"
	"time"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/dispatch"
)

// logData feeds log.html (DESIGN §6.3): the full log history rendered
// server-side plus an EventSource that appends live increments. Without
// JavaScript a <meta http-equiv="refresh"> fallback keeps the page fresh.
type logData struct {
	base
	BuildID string
	SSEURL  string
	Log     string
	HasLog  bool
}

// logEvent is the JSON payload of an SSE "log" event (DETAIL §10.4 point
// 2, derived: the payload is JSON so multi-line log chunks survive).
type logEvent struct {
	Offset int64  `json:"offset"`
	Data   string `json:"data"`
}

// handleLog renders GET /builds/{id}/log. Content negotiation (decision
// A5): requests with Accept: text/event-stream get the SSE stream;
// everything else gets the HTML page.
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		s.renderError(w, http.StatusNotFound, "Invalid build id.")
		return
	}
	build, err := s.store.GetBuild(ctx, id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			s.renderError(w, http.StatusNotFound, "Build not found: "+strconv.FormatInt(id, 10))
			return
		}
		s.renderError(w, http.StatusInternalServerError, "Failed to load the build.")
		return
	}

	buildID := strconv.FormatInt(build.ID, 10)
	if wantsSSE(r) {
		s.serveSSE(w, r, buildID)
		return
	}
	s.renderLogPage(w, r, buildID)
}

// wantsSSE reports whether the request negotiates an event stream.
func wantsSSE(r *http.Request) bool {
	for _, part := range r.Header.Values("Accept") {
		for _, accept := range splitAccept(part) {
			if accept == "text/event-stream" {
				return true
			}
		}
	}
	return false
}

// splitAccept splits one Accept header value on commas and trims
// parameters (q=, charset=...).
func splitAccept(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		media := item
		if i := strings.IndexByte(media, ';'); i >= 0 {
			media = media[:i]
		}
		media = strings.TrimSpace(media)
		if media != "" {
			out = append(out, media)
		}
	}
	return out
}

// renderLogPage renders the HTML log page: the full history plus an
// EventSource for live increments and a no-JavaScript refresh fallback.
func (s *Server) renderLogPage(w http.ResponseWriter, r *http.Request, buildID string) {
	data := logData{
		base:    s.page("Log #"+buildID, nil),
		BuildID: buildID,
		SSEURL:  r.URL.Path,
	}
	content, err := s.logs.ReadLog(r.Context(), buildID)
	if err != nil {
		if errors.Is(err, dispatch.ErrNotFound) {
			s.renderError(w, http.StatusNotFound, "Build log not found: "+buildID)
			return
		}
		s.renderError(w, http.StatusInternalServerError, "Failed to read the build log.")
		return
	}
	data.Log = string(content)
	data.HasLog = len(content) > 0
	s.render(w, "log.html", data)
}

// serveSSE streams the build log to the client (DETAIL §10.4 point 2):
// each incremental read becomes an event: log with a JSON payload, an
// empty read is followed by a terminal-state check (event: done) or a 2s
// comment ping, and the goroutine exits when the client disconnects.
func (s *Server) serveSSE(w http.ResponseWriter, r *http.Request, buildID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.renderError(w, http.StatusInternalServerError, "Streaming is not supported.")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	offset := int64(0)
	for {
		var chunk bytes.Buffer
		newOffset, err := s.logs.TailLog(ctx, buildID, offset, &chunk)
		if err != nil {
			if errors.Is(err, dispatch.ErrNotFound) {
				// Log missing: done immediately with a message
				// (DETAIL §10.5).
				writeSSELog(w, offset, "Build log not found.\n")
				writeSSEDone(w)
				return
			}
			writeSSELog(w, offset, "Failed to read the build log: "+err.Error()+"\n")
			writeSSEDone(w)
			return
		}
		if chunk.Len() > 0 {
			offset = newOffset
			writeSSELog(w, offset, chunk.String())
			flusher.Flush()
			continue
		}

		// Empty increment: check whether the build reached a terminal
		// state (no more increments coming).
		if s.isTerminalBuild(ctx, r) {
			writeSSEDone(w)
			flusher.Flush()
			return
		}

		// Keep-alive comment ping (2s, DETAIL §10.4).
		_, _ = fmt.Fprint(w, ": ping\n\n")
		flusher.Flush()

		select {
		case <-time.After(s.pingInterval):
		case <-ctx.Done():
			return // client disconnected
		}
	}
}

// isTerminalBuild reports whether the build row reached a terminal status.
func (s *Server) isTerminalBuild(ctx context.Context, r *http.Request) bool {
	id, _ := parseID(r.PathValue("id"))
	b, err := s.store.GetBuild(ctx, id)
	return err == nil && isTerminalStatus(b.Status)
}

// writeSSELog emits one event: log with the JSON payload.
func writeSSELog(w http.ResponseWriter, offset int64, data string) {
	payload, _ := json.Marshal(logEvent{Offset: offset, Data: data})
	_, _ = fmt.Fprintf(w, "event: log\ndata: %s\n\n", payload)
}

// writeSSEDone emits the terminal event: done.
func writeSSEDone(w http.ResponseWriter) {
	_, _ = fmt.Fprint(w, "event: done\ndata: {}\n\n")
}
