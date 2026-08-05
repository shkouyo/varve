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

// Package api implements the worker protocol (DESIGN §2.9, §5; DETAIL §9):
// the HTTP server (routing, Bearer authentication, JSON codec, error
// mapping) and the worker-side client, plus the shared wire types
// re-exported by alias (decision D3). The server depends only on the
// dispatch.Orchestrator interface and the sign.KeyMaterial type; it
// contains no business logic and no storage access.
package api

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/dispatch"
)

// taskTokenHeader is the claim-token header used by task-level endpoints
// (DESIGN §5.2, decision A26: the shared Bearer never enters build
// containers).
const taskTokenHeader = "X-Varve-Task-Token"

// Server serves the worker API (DESIGN §5.1). It is stateless between
// requests and safe for concurrent use; the orchestrator owns all state.
type Server struct {
	cfg  *config.ControllerConfig
	orch dispatch.Orchestrator
}

// NewServer builds the worker API server. cfg supplies the shared Bearer
// token (cfg.API.Token); orch is the orchestration backend. It is safe for
// concurrent use.
func NewServer(cfg *config.ControllerConfig, orch dispatch.Orchestrator) *Server {
	if cfg == nil {
		cfg = &config.ControllerConfig{}
	}
	return &Server{cfg: cfg, orch: orch}
}

// Handler returns the full worker API handler: the eleven DESIGN §5.1
// endpoints wrapped in their per-class authentication middlewares. Method
// mismatches on a registered path yield 405 (Go 1.22 ServeMux semantics).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Node-level endpoints: Authorization: Bearer <token> required.
	mux.HandleFunc("POST /api/v1/register", s.bearerAuth(s.handleRegister))
	mux.HandleFunc("POST /api/v1/heartbeat", s.bearerAuth(s.handleHeartbeat))
	mux.HandleFunc("POST /api/v1/poll", s.bearerAuth(s.handlePoll))
	mux.HandleFunc("POST /api/v1/workers/{name}/deregister", s.bearerAuth(s.handleDeregister))

	// Task-level endpoints: X-Varve-Task-Token required; its validity is
	// decided by the orchestrator (403 via dispatch.ErrForbidden).
	mux.HandleFunc("GET /api/v1/tasks/{id}", s.taskAuth(s.handleGetTask))
	mux.HandleFunc("POST /api/v1/tasks/{id}/log", s.taskAuth(s.handleAppendLog))
	mux.HandleFunc("POST /api/v1/tasks/{id}/result", s.taskAuth(s.handleReportResult))
	mux.HandleFunc("POST /api/v1/tasks/{id}/signing-key", s.taskAuth(s.handleSigningKey))
	mux.HandleFunc("PUT /api/v1/tasks/{id}/files/{name}", s.taskAuth(s.handleUploadFile))
	mux.HandleFunc("GET /api/v1/tasks/{id}/files/{name}", s.taskAuth(s.handleDownloadFile))

	return mux
}

// bearerAuth guards the node-level endpoints (DESIGN §5.7): the request
// must carry a valid shared Bearer token, compared in constant time.
func (s *Server) bearerAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, prefix) {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "missing bearer token")
			return
		}
		token := strings.TrimPrefix(header, prefix)
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.API.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "invalid bearer token")
			return
		}
		next(w, r)
	}
}

// taskAuth guards the task-level endpoints: the claim token header must be
// present (validity is decided by the orchestrator, DETAIL §9.5).
func (s *Server) taskAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(taskTokenHeader) == "" {
			writeError(w, http.StatusForbidden, codeForbidden, "missing task token")
			return
		}
		next(w, r)
	}
}

// claimToken reads the claim token of a task-level request.
func claimToken(r *http.Request) string {
	return r.Header.Get(taskTokenHeader)
}
