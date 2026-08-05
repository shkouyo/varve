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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/dispatch"
	"git.0x0f.dev/varve/internal/sign"
)

// Error codes carried by the wire error object (DESIGN §5.6).
const (
	codeInvalidRequest = "invalid_request"
	codeUnauthorized   = "unauthorized"
	codeForbidden      = "forbidden"
	codeNotFound       = "not_found"
	codeConflict       = "conflict"
	codeInternal       = "internal"
)

// errorDetail is the single error object of the wire error body.
type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// errorResponse is the standard error body (DESIGN §5.6):
// {"error":{"code":...,"message":...}}.
type errorResponse struct {
	Error errorDetail `json:"error"`
}

// conflictResponse extends errorResponse with the current server-side offset
// so resumable log/file uploads can be continued (DESIGN §5.3: "附当前
// offset"). The offset is only rendered when non-nil.
type conflictResponse struct {
	Error  errorDetail `json:"error"`
	Offset *int64      `json:"offset,omitempty"`
}

// writeError writes a standard error body with the given status.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: errorDetail{Code: code, Message: message}})
}

// writeConflict writes a 409 carrying the current server-side offset for
// resumable log segments and file uploads.
func writeConflict(w http.ResponseWriter, message string, offset int64) {
	writeJSON(w, http.StatusConflict, conflictResponse{
		Error:  errorDetail{Code: codeConflict, Message: message},
		Offset: &offset,
	})
}

// writeJSON encodes v as the response body. Marshal errors are logged and
// dropped: headers were already committed, so the client sees a truncated
// body rather than a clean failure. json.Marshal is used (not the encoder)
// so the body is byte-exact without a trailing newline.
func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("api: encode response: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		log.Printf("api: write response: %v", err)
	}
}

// decodeJSON strictly decodes one JSON value from the request body into dst.
// Unknown fields, type mismatches, syntax errors and trailing data all map
// to 400 invalid_request; the message names the offending field when the
// decoder provides one. It reports false after writing the error response.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeDecodeError(w, err)
		return false
	}
	// Reject trailing content after the first JSON value.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			writeError(w, http.StatusBadRequest, codeInvalidRequest,
				"invalid request: multiple JSON values in body")
		} else {
			writeDecodeError(w, err)
		}
		return false
	}
	return true
}

// writeDecodeError maps a json decode failure to 400 invalid_request,
// including the field name when available (DETAIL §9.5).
func writeDecodeError(w http.ResponseWriter, err error) {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		field := typeErr.Field
		if field == "" {
			field = "body"
		}
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			fmt.Sprintf("invalid request: field %q: %s", field, typeErr.Error()))
		return
	}
	writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid request: "+err.Error())
}

// writeOrchError maps an orchestrator (or sign) error to the wire contract
// (DETAIL §0.3 rule 1, DESIGN §5.6): 404/403/409 for the sentinels, 500 for
// anything else. Unknown errors are logged server-side without leaking
// internals to the client.
func (s *Server) writeOrchError(w http.ResponseWriter, err error) {
	var offErr *dispatch.OffsetError
	if errors.As(err, &offErr) {
		// Resumable log/file conflict: carry the current server offset.
		writeConflict(w, err.Error(), offErr.Current)
		return
	}
	switch {
	case errors.Is(err, dispatch.ErrForbidden):
		writeError(w, http.StatusForbidden, codeForbidden, err.Error())
	case errors.Is(err, dispatch.ErrConflict), errors.Is(err, sign.ErrAlreadyExported):
		writeError(w, http.StatusConflict, codeConflict, err.Error())
	case errors.Is(err, dispatch.ErrNotFound), errors.Is(err, db.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, err.Error())
	default:
		log.Printf("api: internal error: %v", err)
		writeError(w, http.StatusInternalServerError, codeInternal, "internal server error")
	}
}
