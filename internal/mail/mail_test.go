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
	"context"
	"strings"
	"testing"

	"git.0x0f.dev/varve/internal/config"
)

func testInfo() FailureInfo {
	return FailureInfo{
		Pkgbase: "foo",
		Branch:  "main",
		Commit:  "0123abc",
		Stage:   "build",
		Summary: "error: failed to compile foo.c",
		LogURL:  "https://varve.example.org/builds/7",
	}
}

// TestSendFailureNoop asserts the documented no-op semantics (DETAIL §8.5):
// mail disabled, empty recipient list, and a nil Mailer all return nil
// without touching the network.
func TestSendFailureNoop(t *testing.T) {
	srv := newTestSMTP(t, false, nil)
	cfg := config.MailConfig{
		Enabled: true, Host: srv.host(), Port: srv.port(),
		From: "varve@example.org", TLS: "none",
	}
	info := testInfo()
	ctx := context.Background()

	m0 := &Mailer{cfg: &cfg}
	if err := m0.SendFailure(ctx, nil, info); err != nil {
		t.Fatalf("empty recipients: %v", err)
	}
	disabled := cfg
	disabled.Enabled = false
	m1 := &Mailer{cfg: &disabled}
	if err := m1.SendFailure(ctx, []string{"a@example.org"}, info); err != nil {
		t.Fatalf("disabled: %v", err)
	}
	var nilMailer *Mailer
	if err := nilMailer.SendFailure(ctx, []string{"a@example.org"}, info); err != nil {
		t.Fatalf("nil mailer: %v", err)
	}

	if got := len(srv.messages()); got != 0 {
		t.Errorf("no-op paths queued %d messages", got)
	}
}

// TestSendFailureSuccess asserts a full SendFailure round trip: one SMTP
// transaction per recipient with the correct envelope, headers and body.
func TestSendFailureSuccess(t *testing.T) {
	srv := newTestSMTP(t, false, nil)
	m := testMailer(config.MailConfig{
		Enabled: true, Host: srv.host(), Port: srv.port(),
		From: "varve@example.org", TLS: "none",
	}, nil)

	to := []string{"a@example.org", "b@example.org"}
	info := testInfo()
	if err := m.SendFailure(context.Background(), to, info); err != nil {
		t.Fatalf("SendFailure: %v", err)
	}

	msgs := srv.waitMessages(t, 2)
	for i, got := range msgs {
		if got.from != "varve@example.org" {
			t.Errorf("msg %d: MAIL FROM = %q, want varve@example.org", i, got.from)
		}
		if len(got.rcpts) != 1 || got.rcpts[0] != to[i] {
			t.Errorf("msg %d: RCPT TO = %v, want [%s]", i, got.rcpts, to[i])
		}
		for _, want := range []string{
			"From: varve@example.org",
			"To: " + to[i],
			"Date: ",
			"Message-ID: ",
			"Subject: Build failed: foo (main)",
			"Package: foo",
			"Branch: main",
			"Commit: 0123abc",
			"Stage: build",
			"Summary: error: failed to compile foo.c",
			"Log: https://varve.example.org/builds/7",
		} {
			if !strings.Contains(got.data, want) {
				t.Errorf("msg %d: DATA missing %q", i, want)
			}
		}
	}
}

// TestSendFailurePartialFailure asserts one bad recipient does not block the
// others: the good recipient is still delivered and the error aggregates
// every failure with per-recipient context.
func TestSendFailurePartialFailure(t *testing.T) {
	srv := newTestSMTP(t, false, nil)
	srv.reject["bad1@example.org"] = true
	srv.reject["bad2@example.org"] = true
	m := testMailer(config.MailConfig{
		Enabled: true, Host: srv.host(), Port: srv.port(),
		From: "varve@example.org", TLS: "none",
	}, nil)

	to := []string{"good@example.org", "bad1@example.org", "bad2@example.org"}
	err := m.SendFailure(context.Background(), to, testInfo())
	if err == nil {
		t.Fatal("want aggregated error for rejected recipients")
	}
	es := err.Error()
	for _, bad := range []string{"bad1@example.org", "bad2@example.org"} {
		if !strings.Contains(es, bad) {
			t.Errorf("aggregated error missing %q: %v", bad, err)
		}
	}
	if got := strings.Count(es, "mail: send to"); got != 2 {
		t.Errorf("aggregated error should wrap 2 failures, got %d: %v", got, err)
	}

	msgs := srv.waitMessages(t, 1)
	if got := msgs[0].rcpts; len(got) != 1 || got[0] != "good@example.org" {
		t.Errorf("delivered rcpts = %v, want [good@example.org]", got)
	}
}

// TestSendFailureContextCanceled asserts a canceled context aborts the
// remaining sends and reports the cancellation.
func TestSendFailureContextCanceled(t *testing.T) {
	srv := newTestSMTP(t, false, nil)
	m := testMailer(config.MailConfig{
		Enabled: true, Host: srv.host(), Port: srv.port(),
		From: "varve@example.org", TLS: "none",
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before any send
	err := m.SendFailure(ctx, []string{"a@example.org", "b@example.org"}, testInfo())
	if err == nil {
		t.Fatal("want error for canceled context")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("error should mention cancellation, got: %v", err)
	}
	if got := len(srv.messages()); got != 0 {
		t.Errorf("canceled path queued %d messages", got)
	}
}
