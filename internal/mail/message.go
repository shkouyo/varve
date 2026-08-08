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

package mail

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime"
	"strings"
	"time"
)

const crlf = "\r\n"

// buildMessage assembles a plain-text RFC 5322 failure notification. It is
// a pure function of its inputs (no network access and no side effects),
// apart from the wall-clock Date and the random Message-ID, which keeps it
// trivially testable without a server.
func (m *Mailer) buildMessage(info FailureInfo, to []string) []byte {
	var b strings.Builder
	b.Grow(512)
	fmt.Fprintf(&b, "From: %s%s", m.cfg.From, crlf)
	fmt.Fprintf(&b, "To: %s%s", strings.Join(to, ", "), crlf)
	fmt.Fprintf(&b, "Date: %s%s", time.Now().Format(time.RFC1123Z), crlf)
	fmt.Fprintf(&b, "Message-ID: %s%s", newMessageID(m.cfg.From), crlf)
	fmt.Fprintf(&b, "Subject: %s%s", encodeSubject(info), crlf)
	b.WriteString(crlf)
	b.WriteString(buildBody(info))
	return []byte(b.String())
}

// buildBody renders the plain-text body (no HTML) with every field the
// notification must carry: package, branch, commit, failing stage, error
// summary and the Web log link.
func buildBody(info FailureInfo) string {
	var b strings.Builder
	b.WriteString("Build failed" + crlf)
	b.WriteString(crlf)
	fmt.Fprintf(&b, "Package: %s%s", sanitize(info.Pkgbase), crlf)
	fmt.Fprintf(&b, "Branch: %s%s", sanitize(info.Branch), crlf)
	fmt.Fprintf(&b, "Commit: %s%s", sanitize(info.Commit), crlf)
	fmt.Fprintf(&b, "Stage: %s%s", sanitize(info.Stage), crlf)
	fmt.Fprintf(&b, "Summary: %s%s", sanitize(info.Summary), crlf)
	fmt.Fprintf(&b, "Log: %s%s", sanitize(info.LogURL), crlf)
	return b.String()
}

// buildAURMessage assembles the plain-text RFC 5322 AUR push failure
// notification; it shares the header layout of buildMessage.
func (m *Mailer) buildAURMessage(info AURPushInfo, to []string) []byte {
	var b strings.Builder
	b.Grow(512)
	fmt.Fprintf(&b, "From: %s%s", m.cfg.From, crlf)
	fmt.Fprintf(&b, "To: %s%s", strings.Join(to, ", "), crlf)
	fmt.Fprintf(&b, "Date: %s%s", time.Now().Format(time.RFC1123Z), crlf)
	fmt.Fprintf(&b, "Message-ID: %s%s", newMessageID(m.cfg.From), crlf)
	fmt.Fprintf(&b, "Subject: %s%s", encodeAURSubject(info), crlf)
	b.WriteString(crlf)
	b.WriteString(buildAURBody(info))
	return []byte(b.String())
}

// buildAURBody renders the plain-text AUR push failure body: package,
// branch, AUR package name, attempted commit and the push error.
func buildAURBody(info AURPushInfo) string {
	var b strings.Builder
	b.WriteString("AUR push failed" + crlf)
	b.WriteString(crlf)
	fmt.Fprintf(&b, "Package: %s%s", sanitize(info.Pkgbase), crlf)
	fmt.Fprintf(&b, "Branch: %s%s", sanitize(info.Branch), crlf)
	fmt.Fprintf(&b, "AUR package: %s%s", sanitize(info.AURName), crlf)
	fmt.Fprintf(&b, "Commit: %s%s", sanitize(info.Commit), crlf)
	fmt.Fprintf(&b, "Error: %s%s", sanitize(info.Error), crlf)
	return b.String()
}

// encodeAURSubject builds the RFC 2047-encoded AUR push failure subject.
func encodeAURSubject(info AURPushInfo) string {
	subject := fmt.Sprintf("AUR push failed: %s (%s)",
		sanitize(info.Pkgbase), sanitize(info.Branch))
	enc := mime.QEncoding.Encode("utf-8", subject)
	if enc == subject {
		return subject
	}
	return strings.ReplaceAll(enc, "?= =?", "?="+crlf+" =?")
}

// encodeSubject encodes the Subject header per RFC 2047 when it contains
// non-ASCII characters (e.g. a Chinese package name) and folds long encoded
// words onto continuation lines so that no physical header line exceeds 78
// octets (RFC 5322, section 2.2.3).
func encodeSubject(info FailureInfo) string {
	subject := fmt.Sprintf("Build failed: %s (%s)",
		sanitize(info.Pkgbase), sanitize(info.Branch))
	enc := mime.QEncoding.Encode("utf-8", subject)
	if enc == subject {
		return subject
	}
	// The encoder splits long words into several adjacent encoded-words
	// joined by a space; fold them onto continuation lines so no physical
	// line overflows 78 octets (RFC 5322, section 2.2.3). The "?= =?" boundary
	// cannot occur inside Q-encoded content.
	return strings.ReplaceAll(enc, "?= =?", "?="+crlf+" =?")
}

// newMessageID builds an RFC 5322 Message-ID of the form
// <id@domain> with the domain taken from the From address.
func newMessageID(from string) string {
	domain := "localhost"
	if i := strings.LastIndex(from, "@"); i >= 0 && i+1 < len(from) {
		domain = from[i+1:]
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Randomness unavailable: fall back to a timestamp-based id.
		return fmt.Sprintf("<%d.%d@%s>", time.Now().UnixNano(), time.Now().Unix(), domain)
	}
	return fmt.Sprintf("<%s.%d@%s>", hex.EncodeToString(b[:]), time.Now().UnixNano(), domain)
}

// sanitize strips line breaks from header/body fields so that untrusted
// metadata cannot inject headers or corrupt the plain-text layout.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, s)
}
