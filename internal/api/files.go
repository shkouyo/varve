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

package api

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
)

// validUploadName enforces the upload whitelist [A-Za-z0-9._+-]: every
// byte must be a whitelisted character, which excludes path separators
// and shell metacharacters and therefore prevents staging escapes. The
// bare "." and ".." are rejected as well: they would resolve outside the
// task directory despite matching the character class.
func validUploadName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '+', c == '-':
		default:
			return false
		}
	}
	return true
}

// parseOffset reads the ?offset=N query parameter; an absent parameter
// means offset 0 (whole-file upload).
func parseOffset(r *http.Request) (int64, error) {
	raw := r.URL.Query().Get("offset")
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid offset %q", raw)
	}
	return n, nil
}

// handleUploadFile streams a raw artifact segment into the task staging
// area (PUT /api/v1/tasks/{id}/files/{name}?offset=N): the request body
// is forwarded to the orchestrator as an io.Reader without loading it
// into memory. An offset mismatch surfaces as 409 carrying the current
// server-side offset so the worker can resume.
func (s *Server) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validUploadName(name) {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			fmt.Sprintf("invalid file name %q", name))
		return
	}
	offset, err := parseOffset(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid request: "+err.Error())
		return
	}
	meta, err := s.orch.UploadFile(r.Context(), r.PathValue("id"), claimToken(r), name,
		r.Body, r.ContentLength, offset)
	if err != nil {
		s.writeOrchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

// handleDownloadFile streams a staged file back to the worker
// (GET /api/v1/tasks/{id}/files/{name}). The existence check happens inside
// the orchestrator so a missing file maps to 404 before any bytes flow.
func (s *Server) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validUploadName(name) {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			fmt.Sprintf("invalid file name %q", name))
		return
	}
	rc, err := s.orch.DownloadFile(r.Context(), r.PathValue("id"), claimToken(r), name)
	if err != nil {
		s.writeOrchError(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		// Headers are already committed; the truncated body is the only
		// signal the client gets. Log for diagnosis.
		log.Printf("api: download %s: %v", name, err)
	}
}
