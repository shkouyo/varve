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

package api

import (
	"net/http"
)

// signingKeyWire is the on-the-wire form of the signing key material
// (DESIGN §5.3: key_id / armored_private_key / passphrase). sign.KeyMaterial
// itself has no JSON tags; the API owns the snake_case contract so worker
// packages keep using the KeyMaterial alias (D3).
type signingKeyWire struct {
	KeyID             string `json:"key_id"`
	ArmoredPrivateKey string `json:"armored_private_key"`
	Passphrase        string `json:"passphrase"`
}

// resultResp is the POST /tasks/{id}/result success body (DESIGN §5.3).
type resultResp struct {
	Accepted bool `json:"accepted"`
}

// handleRegister implements POST /api/v1/register.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req RegisterReq
	if !decodeJSON(w, r, &req) {
		return
	}
	resp, err := s.orch.Register(r.Context(), req)
	if err != nil {
		s.writeOrchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleHeartbeat implements POST /api/v1/heartbeat; the response carries
// the cancellation signals (channel 1) and the server time.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req HeartbeatReq
	if !decodeJSON(w, r, &req) {
		return
	}
	resp, err := s.orch.Heartbeat(r.Context(), req)
	if err != nil {
		s.writeOrchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handlePoll implements POST /api/v1/poll (task claim for host/pool nodes).
func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	var req PollReq
	if !decodeJSON(w, r, &req) {
		return
	}
	resp, err := s.orch.Poll(r.Context(), req)
	if err != nil {
		s.writeOrchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGetTask implements GET /api/v1/tasks/{id} (one-shot claim).
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.orch.GetTask(r.Context(), r.PathValue("id"), claimToken(r))
	if err != nil {
		s.writeOrchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// handleAppendLog implements POST /api/v1/tasks/{id}/log.
func (s *Server) handleAppendLog(w http.ResponseWriter, r *http.Request) {
	var seg LogSegment
	if !decodeJSON(w, r, &seg) {
		return
	}
	ack, err := s.orch.AppendLog(r.Context(), r.PathValue("id"), claimToken(r), seg)
	if err != nil {
		s.writeOrchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ack)
}

// handleReportResult implements POST /api/v1/tasks/{id}/result.
func (s *Server) handleReportResult(w http.ResponseWriter, r *http.Request) {
	var res ResultReq
	if !decodeJSON(w, r, &res) {
		return
	}
	if err := s.orch.ReportResult(r.Context(), r.PathValue("id"), claimToken(r), res); err != nil {
		s.writeOrchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resultResp{Accepted: true})
}

// handleSigningKey implements POST /api/v1/tasks/{id}/signing-key; each task
// may claim it exactly once (repeat → 409, mapped from
// sign.ErrAlreadyExported).
func (s *Server) handleSigningKey(w http.ResponseWriter, r *http.Request) {
	km, err := s.orch.IssueSigningKey(r.Context(), r.PathValue("id"), claimToken(r))
	if err != nil {
		s.writeOrchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, signingKeyWire{
		KeyID:             km.KeyID,
		ArmoredPrivateKey: km.ArmoredPrivateKey,
		Passphrase:        km.Passphrase,
	})
}

// handleDeregister implements POST /api/v1/workers/{name}/deregister
// (normal shutdown, decision A18).
func (s *Server) handleDeregister(w http.ResponseWriter, r *http.Request) {
	if err := s.orch.Deregister(r.Context(), r.PathValue("name")); err != nil {
		s.writeOrchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}
