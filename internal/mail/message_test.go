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
	"mime"
	"strings"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/config"
)

// splitMessage splits a raw RFC 5322 message into its header block and body.
func splitMessage(msg string) (headers, body string) {
	parts := strings.SplitN(msg, crlf+crlf, 2)
	if len(parts) != 2 {
		return msg, ""
	}
	return parts[0], parts[1]
}

// headerValue returns the single-line value of the named header.
func headerValue(headers, name string) string {
	for _, line := range strings.Split(headers, crlf) {
		if v, ok := strings.CutPrefix(line, name+": "); ok {
			return v
		}
	}
	return ""
}

// headerBlock returns the named header including folded continuation lines.
func headerBlock(headers, name string) string {
	lines := strings.Split(headers, crlf)
	for i := 0; i < len(lines); i++ {
		if v, ok := strings.CutPrefix(lines[i], name+": "); ok {
			block := v
			for i+1 < len(lines) && (strings.HasPrefix(lines[i+1], " ") || strings.HasPrefix(lines[i+1], "\t")) {
				i++
				block += crlf + lines[i]
			}
			return block
		}
	}
	return ""
}

// TestBuildMessageHeaders asserts every required RFC 5322 header is present
// and well-formed.
func TestBuildMessageHeaders(t *testing.T) {
	m := testMailer(config.MailConfig{From: "varve@example.org"}, nil)
	info := testInfo()
	to := []string{"a@example.org", "b@example.org"}

	msg := string(m.buildMessage(info, to))
	if !strings.HasSuffix(msg, crlf) {
		t.Error("message must end with CRLF")
	}
	headers, body := splitMessage(msg)
	if body == "" {
		t.Fatal("message has no body separator")
	}

	if got := headerValue(headers, "From"); got != "varve@example.org" {
		t.Errorf("From = %q, want varve@example.org", got)
	}
	if got := headerValue(headers, "To"); got != "a@example.org, b@example.org" {
		t.Errorf("To = %q, want a@example.org, b@example.org", got)
	}
	if got := headerValue(headers, "Date"); got == "" {
		t.Error("Date header missing")
	} else if _, err := time.Parse(time.RFC1123Z, got); err != nil {
		t.Errorf("Date %q does not parse as RFC 5322: %v", got, err)
	}
	mid := headerValue(headers, "Message-ID")
	if mid == "" {
		t.Error("Message-ID header missing")
	} else if !strings.HasPrefix(mid, "<") || !strings.HasSuffix(mid, ">") ||
		!strings.Contains(mid, "@example.org") {
		t.Errorf("Message-ID %q must look like <id@example.org>", mid)
	}
	if got := headerValue(headers, "Subject"); got != "Build failed: foo (main)" {
		t.Errorf("Subject = %q, want %q", got, "Build failed: foo (main)")
	}
}

// TestBuildMessageBody asserts the plain-text body carries every field the
// notification must contain, with no HTML markup.
func TestBuildMessageBody(t *testing.T) {
	m := testMailer(config.MailConfig{From: "varve@example.org"}, nil)
	info := testInfo()

	msg := string(m.buildMessage(info, []string{"a@example.org"}))
	_, body := splitMessage(msg)
	for _, want := range []string{
		"Package: foo",
		"Branch: main",
		"Commit: 0123abc",
		"Stage: build",
		"Summary: error: failed to compile foo.c",
		"Log: https://varve.example.org/builds/7",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if strings.Contains(msg, "<html") || strings.Contains(msg, "<body") {
		t.Error("message must not contain HTML markup")
	}
	// Line endings must be CRLF throughout (RFC 5322, section 2.1).
	for i := 0; i < len(msg); i++ {
		if msg[i] == '\n' && (i == 0 || msg[i-1] != '\r') {
			t.Fatalf("bare LF at byte %d", i)
		}
	}
}

// TestBuildMessageRFC2047 asserts a non-ASCII subject (e.g. a Chinese
// package name) is RFC 2047 encoded and decodes back to the original.
func TestBuildMessageRFC2047(t *testing.T) {
	m := testMailer(config.MailConfig{From: "varve@example.org"}, nil)
	info := testInfo()
	info.Pkgbase = "变分构建包"

	headers, _ := splitMessage(string(m.buildMessage(info, []string{"a@example.org"})))
	subject := headerBlock(headers, "Subject")
	if !strings.Contains(subject, "=?utf-8?q?") {
		t.Fatalf("subject not RFC 2047 encoded: %q", subject)
	}
	got, err := (&mime.WordDecoder{}).DecodeHeader(subject)
	if err != nil {
		t.Fatalf("decode subject: %v", err)
	}
	if want := "Build failed: 变分构建包 (main)"; got != want {
		t.Errorf("decoded subject = %q, want %q", got, want)
	}
}

// TestBuildMessageRFC2047Fold asserts long encoded subjects are folded onto
// continuation lines so no physical header line overflows 78 octets.
func TestBuildMessageRFC2047Fold(t *testing.T) {
	m := testMailer(config.MailConfig{From: "varve@example.org"}, nil)
	info := testInfo()
	info.Pkgbase = strings.Repeat("长", 40) // 120 UTF-8 bytes -> several words

	headers, _ := splitMessage(string(m.buildMessage(info, []string{"a@example.org"})))
	subject := headerBlock(headers, "Subject")
	if !strings.Contains(subject, crlf+" ") {
		t.Fatalf("long subject not folded: %q", subject)
	}
	for _, line := range strings.Split(subject, crlf) {
		if len(line) > 78 {
			t.Errorf("physical header line exceeds 78 octets (%d): %q", len(line), line)
		}
	}
	got, err := (&mime.WordDecoder{}).DecodeHeader(subject)
	if err != nil {
		t.Fatalf("decode folded subject: %v", err)
	}
	if want := "Build failed: " + strings.Repeat("长", 40) + " (main)"; got != want {
		t.Errorf("decoded folded subject = %q, want %q", got, want)
	}
}

// TestBuildMessageSanitize asserts CR/LF in metadata cannot inject headers
// into the message.
func TestBuildMessageSanitize(t *testing.T) {
	m := testMailer(config.MailConfig{From: "varve@example.org"}, nil)
	info := testInfo()
	info.Pkgbase = "evil\r\nBcc: victim@example.org"
	info.Summary = "line1\rline2\nline3"

	msg := string(m.buildMessage(info, []string{"a@example.org"}))
	for _, line := range strings.Split(msg, crlf) {
		if strings.HasPrefix(line, "Bcc:") {
			t.Error("header injection via metadata succeeded")
		}
	}
	if strings.Contains(msg, "line1\rline2") {
		t.Error("body field contained raw line break")
	}
	headers, _ := splitMessage(msg)
	if got := headerValue(headers, "Subject"); !strings.Contains(got, "evil") {
		t.Errorf("sanitized subject missing package name: %q", got)
	}
}

// TestBuildAURMessageBody asserts the AUR push failure message carries
// every field: package, branch, AUR package name, attempted commit and
// the push error, with no HTML markup and CRLF line endings.
func TestBuildAURMessageBody(t *testing.T) {
	m := testMailer(config.MailConfig{From: "varve@example.org"}, nil)
	info := AURPushInfo{
		Pkgbase: "foo",
		Branch:  "main",
		AURName: "foo-aur",
		Commit:  "0123abc",
		Error:   "git push rejected: non-fast-forward",
	}

	msg := string(m.buildAURMessage(info, []string{"a@example.org"}))
	_, body := splitMessage(msg)
	for _, want := range []string{
		"Package: foo",
		"Branch: main",
		"AUR package: foo-aur",
		"Commit: 0123abc",
		"Error: git push rejected: non-fast-forward",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if strings.Contains(msg, "<html") || strings.Contains(msg, "<body") {
		t.Error("message must not contain HTML markup")
	}
	for i := 0; i < len(msg); i++ {
		if msg[i] == '\n' && (i == 0 || msg[i-1] != '\r') {
			t.Fatalf("bare LF at byte %d", i)
		}
	}
}

// TestBuildAURMessageSubject asserts the AUR failure subject uses the
// "AUR push failed" prefix and encodes non-ASCII package names.
func TestBuildAURMessageSubject(t *testing.T) {
	m := testMailer(config.MailConfig{From: "varve@example.org"}, nil)
	plain := AURPushInfo{Pkgbase: "foo", Branch: "main"}
	headers, _ := splitMessage(string(m.buildAURMessage(plain, []string{"a@example.org"})))
	if subject := headerValue(headers, "Subject"); subject != "AUR push failed: foo (main)" {
		t.Errorf("Subject = %q", subject)
	}

	nonASCII := AURPushInfo{Pkgbase: "构建包", Branch: "main"}
	headers, _ = splitMessage(string(m.buildAURMessage(nonASCII, []string{"a@example.org"})))
	subject := headerBlock(headers, "Subject")
	if !strings.Contains(subject, "=?utf-8?q?") {
		t.Fatalf("subject not RFC 2047 encoded: %q", subject)
	}
	got, err := (&mime.WordDecoder{}).DecodeHeader(subject)
	if err != nil {
		t.Fatalf("decode subject: %v", err)
	}
	if want := "AUR push failed: 构建包 (main)"; got != want {
		t.Errorf("decoded subject = %q, want %q", got, want)
	}
}

// TestBuildAURMessageSanitize asserts untrusted fields cannot inject
// header lines or break the plain-text layout.
func TestBuildAURMessageSanitize(t *testing.T) {
	m := testMailer(config.MailConfig{From: "varve@example.org"}, nil)
	info := AURPushInfo{
		Pkgbase: "foo\r\nBcc: attacker@example.org",
		Branch:  "main",
		AURName: "pkg\r\nX-Evil: 1",
		Commit:  "c",
		Error:   "boom\nignored",
	}
	msg := string(m.buildAURMessage(info, []string{"a@example.org"}))
	headers, _ := splitMessage(msg)
	if v := headerValue(headers, "Bcc"); v != "" {
		t.Errorf("Bcc header injected: %q", v)
	}
	if v := headerValue(headers, "X-Evil"); v != "" {
		t.Errorf("X-Evil header injected: %q", v)
	}
	// The offending text survives only inside the body, with the line
	// breaks replaced by spaces.
	if !strings.Contains(msg, "Bcc: attacker@example.org") || !strings.Contains(msg, "X-Evil: 1") {
		t.Error("sanitized content missing from the body")
	}
}

// TestValidRecipient covers the bare addr-spec rule: plain and plus-
// tagged addresses pass; whitespace, control characters, display-name
// forms and unparseable strings are rejected.
func TestValidRecipient(t *testing.T) {
	valid := []string{
		"a@b.c",
		"a+b@example.org",
		"user@sub.example.org",
	}
	for _, s := range valid {
		if !validRecipient(s) {
			t.Errorf("validRecipient(%q) = false, want true", s)
		}
	}
	invalid := []string{
		"",
		"a@b c",
		"a\r\nb@c",
		"a\nb@c",
		"<script>@x",
		"Name <a@b>",
		" a@b.c",
		"a@b.c ",
		"a\x00b@c",
	}
	for _, s := range invalid {
		if validRecipient(s) {
			t.Errorf("validRecipient(%q) = true, want false", s)
		}
	}
}

// TestToHeaderInjection asserts a recipient carrying CR/LF cannot inject
// header lines into the To header of either message shape.
func TestToHeaderInjection(t *testing.T) {
	m := testMailer(config.MailConfig{From: "varve@example.org"}, nil)
	payloads := []string{
		"good@example.org\r\nBcc: evil@example.org",
		"good@example.org\nCc: evil@example.org",
		"good@example.org\x00X-Evil: 1",
	}
	for _, payload := range payloads {
		msg := string(m.buildMessage(testInfo(), []string{payload, "real@example.org"}))
		headers, _ := splitMessage(msg)
		for _, name := range []string{"Bcc", "Cc", "X-Evil"} {
			if v := headerValue(headers, name); v != "" {
				t.Errorf("payload %q injected header %s: %q", payload, name, v)
			}
		}
		if strings.ContainsAny(headers, "\r\n") && strings.Contains(headerBlock(headers, "To"), "\r\n") {
			// A folded To block is legitimate; raw line breaks inside the
			// value are not. headerValue strips nothing, so check the To
			// value contains no bare CR/LF byte.
			to := headerValue(headers, "To")
			if strings.Contains(to, "\r") || strings.Contains(to, "\n") {
				t.Errorf("payload %q left a raw line break in the To value: %q", payload, to)
			}
		}
	}

	aur := string(m.buildAURMessage(AURPushInfo{Pkgbase: "foo", Branch: "main"}, []string{"good@example.org\r\nBcc: evil@example.org"}))
	headers, _ := splitMessage(aur)
	if v := headerValue(headers, "Bcc"); v != "" {
		t.Errorf("AUR message: Bcc header injected: %q", v)
	}
}

// TestSanitizeC0 asserts the extended control-character handling: CR/LF
// become spaces, other C0 controls are dropped.
func TestSanitizeC0(t *testing.T) {
	if got := sanitize("a\rb\nc"); got != "a b c" {
		t.Errorf("sanitize CR/LF = %q, want %q", got, "a b c")
	}
	if got := sanitize("a\x00b\x1bc"); got != "abc" {
		t.Errorf("sanitize C0 = %q, want %q", got, "abc")
	}
	if got := sanitize("\x07\x1b\x00"); got != "" {
		t.Errorf("sanitize all-C0 = %q, want empty", got)
	}
}
