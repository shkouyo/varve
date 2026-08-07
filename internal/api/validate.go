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
	"errors"
	"fmt"
)

// Wire input limits: plain anti-injection and length bounds for every
// untrusted string the worker protocol accepts. They deliberately do not
// enumerate the wire enums (role/mode/stage/status stay whatever the
// controller validates), only what the HTTP boundary must reject.
const (
	maxTaskIDLen     = 64   // task id path parameters (UUIDs and similar URL-safe ids)
	maxWorkerNameLen = 255  // worker/agent names and the deregister path value
	maxLabelLen      = 64   // role / mode / arch / version / commit / error stage
	maxStatusLen     = 16   // result status ("succeeded" / "failed" / "cancelled")
	maxSummaryLen    = 4096 // result error summary
	maxFileNameLen   = 255  // staged artifact file names

	// maxLogSegmentLen caps one buffered log batch. Clients flush at 64
	// KiB, so 1 MiB leaves ample headroom while bounding memory per POST.
	maxLogSegmentLen = 1 << 20

	// maxJSONBodyLen caps any JSON request body; it is far above the
	// largest legitimate payload (a 1 MiB log segment plus overhead) and
	// stops oversized bodies from being buffered during decode.
	maxJSONBodyLen = 16 << 20

	// maxUploadSegment caps one upload request; maxUploadTotal caps the
	// final staged artifact size (offset + segment). A built .pkg.tar.zst
	// rarely exceeds a few hundred MiB, so 1 GiB per request and 8 GiB
	// per artifact bound a buggy or malicious worker without affecting
	// real builds.
	maxUploadSegment = 1 << 30
	maxUploadTotal   = 8 << 30
)

// validToken reports whether s is a printable ASCII string (no control
// characters, no whitespace) of at most maxLen bytes. It guards short
// identifier-ish fields such as worker names, roles and architectures.
func validToken(s string, maxLen int) bool {
	if len(s) == 0 || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

// validTaskID reports whether s is a plausible task id: 1 to 64 URL-safe
// characters (letters, digits, '-' and '_'). Task ids are generated as
// UUIDs but kept free-form so future formats stay valid.
func validTaskID(s string) bool {
	if len(s) == 0 || len(s) > maxTaskIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'z') &&
			!(c >= 'A' && c <= 'Z') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// bounded rejects a field longer than maxLen bytes.
func bounded(field, v string, maxLen int) error {
	if len(v) > maxLen {
		return fmt.Errorf("%s: must not exceed %d bytes", field, maxLen)
	}
	return nil
}

// validateRegisterReq checks the POST /register payload.
func validateRegisterReq(r *RegisterReq) error {
	if !validToken(r.Name, maxWorkerNameLen) {
		return errors.New("name: must be 1-255 printable ASCII characters")
	}
	for _, f := range []struct{ name, v string }{
		{"role", r.Role},
		{"mode", r.Mode},
		{"arch", r.Arch},
		{"version", r.Version},
	} {
		if err := bounded(f.name, f.v, maxLabelLen); err != nil {
			return err
		}
	}
	return nil
}

// validatePollReq checks the POST /poll payload.
func validatePollReq(r *PollReq) error {
	if !validToken(r.Name, maxWorkerNameLen) {
		return errors.New("name: must be 1-255 printable ASCII characters")
	}
	return bounded("arch", r.Arch, maxLabelLen)
}

// validateHeartbeatReq checks the POST /heartbeat payload.
func validateHeartbeatReq(r *HeartbeatReq) error {
	if !validToken(r.Name, maxWorkerNameLen) {
		return errors.New("name: must be 1-255 printable ASCII characters")
	}
	return nil
}

// validateLogSegment checks one POST /tasks/{id}/log batch.
func validateLogSegment(s *LogSegment) error {
	if len(s.Data) > maxLogSegmentLen {
		return fmt.Errorf("data: log segment must not exceed %d bytes", maxLogSegmentLen)
	}
	return nil
}

// validateResultReq checks the POST /tasks/{id}/result payload.
func validateResultReq(r *ResultReq) error {
	if err := bounded("status", r.Status, maxStatusLen); err != nil {
		return err
	}
	if err := bounded("commit", r.Commit, maxLabelLen); err != nil {
		return err
	}
	if r.Error != nil {
		if err := bounded("error.stage", r.Error.Stage, maxLabelLen); err != nil {
			return err
		}
		if err := bounded("error.summary", r.Error.Summary, maxSummaryLen); err != nil {
			return err
		}
	}
	return nil
}
