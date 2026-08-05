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

package mail

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
)

// send delivers msg to a single recipient over one SMTP connection. The
// transport is selected by cfg.TLS: "none" is plaintext, "starttls" starts
// plaintext and upgrades with STARTTLS, and "implicit" speaks TLS from the
// first byte (port 465 semantics). When cfg.Username is non-empty the
// client authenticates with AUTH LOGIN (DETAIL §8.4).
func (m *Mailer) send(ctx context.Context, rcpt string, msg []byte) error {
	client, err := m.dial(ctx)
	if err != nil {
		return fmt.Errorf("mail: connect: %w", err)
	}
	defer client.Close()

	if m.cfg.Username != "" {
		if err := client.Auth(loginAuth{user: m.cfg.Username, pass: m.cfg.Password}); err != nil {
			return fmt.Errorf("mail: auth: %w", err)
		}
	}
	if err := client.Mail(m.cfg.From); err != nil {
		return fmt.Errorf("mail: MAIL FROM: %w", err)
	}
	if err := client.Rcpt(rcpt); err != nil {
		return fmt.Errorf("mail: RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("mail: DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		w.Close()
		return fmt.Errorf("mail: write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: finish message: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("mail: quit: %w", err)
	}
	return nil
}

// dial opens an SMTP session to the configured host and port, applying the
// TLS mode and reading the 220 greeting.
func (m *Mailer) dial(ctx context.Context) (*smtp.Client, error) {
	switch m.cfg.TLS {
	case "none", "starttls", "implicit", "":
	default:
		return nil, fmt.Errorf("unsupported mail.tls mode %q", m.cfg.TLS)
	}
	addr := net.JoinHostPort(m.cfg.Host, strconv.Itoa(m.cfg.Port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	switch m.cfg.TLS {
	case "implicit":
		tc := tls.Client(conn, m.tlsConfigFor())
		if err := tc.HandshakeContext(ctx); err != nil {
			tc.Close()
			return nil, err
		}
		conn = tc
	case "none", "starttls", "":
		// Plaintext for now; starttls upgrades after the greeting.
	}

	client, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if m.cfg.TLS == "starttls" {
		if err := client.StartTLS(m.tlsConfigFor()); err != nil {
			client.Close()
			return nil, err
		}
	}
	return client, nil
}

// tlsConfigFor returns the TLS client configuration, pinning the server
// name to the configured host unless the caller already set one (the test
// hook trusts a self-signed certificate instead of the system roots).
func (m *Mailer) tlsConfigFor() *tls.Config {
	if m.tlsConfig != nil {
		cfg := m.tlsConfig.Clone()
		if cfg.ServerName == "" {
			cfg.ServerName = m.cfg.Host
		}
		return cfg
	}
	return &tls.Config{ServerName: m.cfg.Host}
}

// loginAuth implements AUTH LOGIN (RFC 4954). The net/smtp client performs
// the base64 framing: it decodes each 334 challenge before calling Next and
// encodes our response before sending it.
type loginAuth struct {
	user, pass string
}

func (a loginAuth) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", nil, nil
}

func (a loginAuth) Next(challenge []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	line := strings.ToLower(strings.TrimSpace(string(challenge)))
	switch {
	case strings.HasPrefix(line, "user"):
		return []byte(a.user), nil
	case strings.HasPrefix(line, "pass"):
		return []byte(a.pass), nil
	default:
		return nil, fmt.Errorf("mail: unexpected AUTH LOGIN challenge %q", challenge)
	}
}
