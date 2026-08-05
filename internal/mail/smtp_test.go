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
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/config"
)

// testMsg is one fully received mail transaction recorded by testSMTP.
type testMsg struct {
	from     string
	rcpts    []string
	data     string
	tlsMode  string // "none" | "starttls" | "implicit"
	authUser string
	authPass string
}

// testSMTP is a minimal in-memory SMTP server (DETAIL §8.7): it answers the
// classic 220/250/334/235 sequence, records every envelope and DATA payload,
// and can be configured to reject authentication or individual recipients.
type testSMTP struct {
	t        *testing.T
	ln       net.Listener
	tlsCfg   *tls.Config // server TLS config; nil disables TLS features
	implicit bool        // wrap accepted connections in TLS immediately
	authFail bool        // reject AUTH with 535
	reject   map[string]bool

	mu   sync.Mutex
	msgs []testMsg
}

func newTestSMTP(t *testing.T, implicit bool, tlsCfg *tls.Config) *testSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &testSMTP{t: t, ln: ln, tlsCfg: tlsCfg, implicit: implicit, reject: map[string]bool{}}
	go s.acceptLoop()
	t.Cleanup(func() { ln.Close() })
	return s
}

// host returns the host part of the listen address.
func (s *testSMTP) host() string {
	host, _, _ := net.SplitHostPort(s.ln.Addr().String())
	return host
}

// port returns the port part of the listen address.
func (s *testSMTP) port() int {
	_, port, _ := net.SplitHostPort(s.ln.Addr().String())
	var p int
	fmt.Sscanf(port, "%d", &p)
	return p
}

// messages returns a snapshot of all received transactions.
func (s *testSMTP) messages() []testMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]testMsg(nil), s.msgs...)
}

// waitMessages blocks until at least n transactions are recorded.
func (s *testSMTP) waitMessages(t *testing.T, n int) []testMsg {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if msgs := s.messages(); len(msgs) >= n {
			return msgs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d messages; got %d", n, len(s.messages()))
	return nil
}

func (s *testSMTP) record(m testMsg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, m)
}

func (s *testSMTP) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.serve(conn)
	}
}

// serve runs one SMTP session, tolerating both single- and multi-line
// replies and the client's AUTH abort ("*").
func (s *testSMTP) serve(conn net.Conn) {
	defer conn.Close()
	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	replyf := func(format string, args ...any) bool {
		if _, err := fmt.Fprintf(rw, format, args...); err != nil {
			return false
		}
		return rw.Flush() == nil
	}

	cur := testMsg{tlsMode: "none"}
	if s.implicit {
		tc := tls.Server(conn, s.tlsCfg)
		if err := tc.Handshake(); err != nil {
			return
		}
		conn = tc
		rw = bufio.NewReadWriter(bufio.NewReader(tc), bufio.NewWriter(tc))
		cur.tlsMode = "implicit"
	}
	if !replyf("220 test smtp ready\r\n") {
		return
	}

	authWait := 0 // 0 = idle, 1 = username expected, 2 = password expected
	inData := false
	for {
		line, err := rw.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")

		if inData {
			if line == "." {
				inData = false
				if !replyf("250 2.0.0 queued\r\n") {
					return
				}
				s.record(cur)
				cur = testMsg{}
			} else {
				cur.data += line + "\r\n"
			}
			continue
		}

		if authWait > 0 {
			dec, _ := base64.StdEncoding.DecodeString(line)
			if authWait == 1 {
				cur.authUser = string(dec)
				authWait = 2
				if !replyf("334 %s\r\n", base64.StdEncoding.EncodeToString([]byte("Password:"))) {
					return
				}
			} else {
				cur.authPass = string(dec)
				authWait = 0
				if !replyf("235 2.7.0 authentication successful\r\n") {
					return
				}
			}
			continue
		}

		cmd, arg := parseSMTPCmd(line)
		switch cmd {
		case "EHLO", "HELO":
			if s.tlsCfg != nil {
				if !replyf("250-localhost\r\n250-AUTH LOGIN\r\n250 STARTTLS\r\n") {
					return
				}
			} else if !replyf("250-localhost\r\n250 AUTH LOGIN\r\n") {
				return
			}
		case "STARTTLS":
			if s.tlsCfg == nil {
				if !replyf("454 TLS not available\r\n") {
					return
				}
				continue
			}
			if !replyf("220 2.0.0 ready to start tls\r\n") {
				return
			}
			tc := tls.Server(conn, s.tlsCfg)
			if err := tc.Handshake(); err != nil {
				return
			}
			conn = tc
			rw = bufio.NewReadWriter(bufio.NewReader(tc), bufio.NewWriter(tc))
			cur.tlsMode = "starttls"
		case "AUTH":
			if s.authFail {
				if !replyf("535 5.7.8 authentication failed\r\n") {
					return
				}
				continue
			}
			authWait = 1
			if !replyf("334 %s\r\n", base64.StdEncoding.EncodeToString([]byte("Username:"))) {
				return
			}
		case "MAIL":
			cur.from = addrArg(arg, "FROM:")
			if !replyf("250 2.1.0 ok\r\n") {
				return
			}
		case "RCPT":
			rcpt := addrArg(arg, "TO:")
			if s.reject[rcpt] {
				if !replyf("550 5.1.1 recipient rejected\r\n") {
					return
				}
				continue
			}
			cur.rcpts = append(cur.rcpts, rcpt)
			if !replyf("250 2.1.5 ok\r\n") {
				return
			}
		case "DATA":
			inData = true
			if !replyf("354 end of data\r\n") {
				return
			}
		case "QUIT":
			replyf("221 2.0.0 bye\r\n")
			return
		case "RSET":
			cur = testMsg{}
			if !replyf("250 2.0.0 ok\r\n") {
				return
			}
		case "*":
			if !replyf("501 5.5.2 auth aborted\r\n") {
				return
			}
		default:
			if !replyf("500 5.5.2 command unrecognized\r\n") {
				return
			}
		}
	}
}

// parseSMTPCmd splits a command line into verb and argument.
func parseSMTPCmd(line string) (cmd, arg string) {
	if i := strings.IndexByte(line, ' '); i >= 0 {
		return line[:i], strings.TrimSpace(line[i+1:])
	}
	return line, ""
}

// addrArg extracts the address from a "<addr>" argument such as
// "FROM:<a@b>" or "TO:<a@b>".
func addrArg(arg, prefix string) string {
	arg = strings.TrimSpace(strings.TrimPrefix(arg, prefix))
	return strings.Trim(arg, "<>")
}

// testTLSPair returns a server and a client TLS configuration for a
// self-signed certificate valid for "localhost" and 127.0.0.1.
func testTLSPair(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	server = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	pool := x509.NewCertPool()
	pool.AddCert(&x509.Certificate{Raw: der})
	client = &tls.Config{RootCAs: pool, ServerName: "localhost"}
	return server, client
}

// testMailer builds a Mailer with a given config and optional client TLS
// override, matching how production code uses NewMailer.
func testMailer(cfg config.MailConfig, clientTLS *tls.Config) *Mailer {
	return &Mailer{cfg: &cfg, tlsConfig: clientTLS}
}

const testMessage = "From: varve@example.org\r\nTo: who@example.org\r\nSubject: test\r\n\r\nplain body\r\n"

// TestSendPlain asserts the plaintext transport drives the exact SMTP
// envelope (MAIL FROM / RCPT TO) and delivers the DATA payload verbatim.
func TestSendPlain(t *testing.T) {
	srv := newTestSMTP(t, false, nil)
	m := testMailer(config.MailConfig{
		Enabled: true, Host: srv.host(), Port: srv.port(),
		From: "varve@example.org", TLS: "none",
	}, nil)

	if err := m.send(context.Background(), "who@example.org", []byte(testMessage)); err != nil {
		t.Fatalf("send: %v", err)
	}
	msgs := srv.waitMessages(t, 1)
	got := msgs[0]
	if got.from != "varve@example.org" {
		t.Errorf("MAIL FROM = %q, want %q", got.from, "varve@example.org")
	}
	if len(got.rcpts) != 1 || got.rcpts[0] != "who@example.org" {
		t.Errorf("RCPT TO = %v, want [who@example.org]", got.rcpts)
	}
	if got.data != testMessage {
		t.Errorf("DATA mismatch:\n got %q\nwant %q", got.data, testMessage)
	}
	if got.tlsMode != "none" {
		t.Errorf("tlsMode = %q, want %q", got.tlsMode, "none")
	}
}

// TestSendStartTLS asserts the plaintext-then-STARTTLS upgrade path.
func TestSendStartTLS(t *testing.T) {
	serverTLS, clientTLS := testTLSPair(t)
	srv := newTestSMTP(t, false, serverTLS)
	m := testMailer(config.MailConfig{
		Enabled: true, Host: srv.host(), Port: srv.port(),
		From: "varve@example.org", TLS: "starttls",
	}, clientTLS)

	if err := m.send(context.Background(), "who@example.org", []byte(testMessage)); err != nil {
		t.Fatalf("send: %v", err)
	}
	msgs := srv.waitMessages(t, 1)
	got := msgs[0]
	if got.tlsMode != "starttls" {
		t.Errorf("tlsMode = %q, want %q", got.tlsMode, "starttls")
	}
	if got.from != "varve@example.org" || len(got.rcpts) != 1 {
		t.Errorf("envelope = %q -> %v, want varve@example.org -> [who@example.org]", got.from, got.rcpts)
	}
}

// TestSendImplicit asserts the direct-TLS (465 semantics) path.
func TestSendImplicit(t *testing.T) {
	serverTLS, clientTLS := testTLSPair(t)
	srv := newTestSMTP(t, true, serverTLS)
	m := testMailer(config.MailConfig{
		Enabled: true, Host: srv.host(), Port: srv.port(),
		From: "varve@example.org", TLS: "implicit",
	}, clientTLS)

	if err := m.send(context.Background(), "who@example.org", []byte(testMessage)); err != nil {
		t.Fatalf("send: %v", err)
	}
	msgs := srv.waitMessages(t, 1)
	got := msgs[0]
	if got.tlsMode != "implicit" {
		t.Errorf("tlsMode = %q, want %q", got.tlsMode, "implicit")
	}
	if got.from != "varve@example.org" || len(got.rcpts) != 1 {
		t.Errorf("envelope = %q -> %v, want varve@example.org -> [who@example.org]", got.from, got.rcpts)
	}
}

// TestSendTLSModes is the branch-selection table for cfg.TLS.
func TestSendTLSModes(t *testing.T) {
	serverTLS, clientTLS := testTLSPair(t)
	cases := []struct {
		name     string
		mode     string
		implicit bool
		want     string
	}{
		{"none", "none", false, "none"},
		{"starttls", "starttls", false, "starttls"},
		{"implicit", "implicit", true, "implicit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestSMTP(t, tc.implicit, serverTLS)
			m := testMailer(config.MailConfig{
				Enabled: true, Host: srv.host(), Port: srv.port(),
				From: "varve@example.org", TLS: tc.mode,
			}, clientTLS)
			if err := m.send(context.Background(), "who@example.org", []byte(testMessage)); err != nil {
				t.Fatalf("send: %v", err)
			}
			msgs := srv.waitMessages(t, 1)
			if got := msgs[0].tlsMode; got != tc.want {
				t.Errorf("tlsMode = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSendAuth asserts AUTH LOGIN credentials flow through the
// 334/334/235 challenge sequence and the message is still delivered.
func TestSendAuth(t *testing.T) {
	srv := newTestSMTP(t, false, nil)
	m := testMailer(config.MailConfig{
		Enabled: true, Host: srv.host(), Port: srv.port(),
		From: "varve@example.org", Username: "alice", Password: "s3cret", TLS: "none",
	}, nil)

	if err := m.send(context.Background(), "who@example.org", []byte(testMessage)); err != nil {
		t.Fatalf("send: %v", err)
	}
	msgs := srv.waitMessages(t, 1)
	got := msgs[0]
	if got.authUser != "alice" || got.authPass != "s3cret" {
		t.Errorf("auth = %q/%q, want alice/s3cret", got.authUser, got.authPass)
	}
	if got.data != testMessage {
		t.Errorf("DATA mismatch after auth")
	}
}

// TestSendAuthFailure asserts a rejected AUTH surfaces an error naming the
// auth stage and no message is queued.
func TestSendAuthFailure(t *testing.T) {
	srv := newTestSMTP(t, false, nil)
	srv.authFail = true
	m := testMailer(config.MailConfig{
		Enabled: true, Host: srv.host(), Port: srv.port(),
		From: "varve@example.org", Username: "alice", Password: "wrong", TLS: "none",
	}, nil)

	err := m.send(context.Background(), "who@example.org", []byte(testMessage))
	if err == nil {
		t.Fatal("want error for rejected authentication")
	}
	if !strings.Contains(err.Error(), "auth") {
		t.Errorf("error should carry auth stage context, got: %v", err)
	}
	if got := len(srv.messages()); got != 0 {
		t.Errorf("queued %d messages despite auth failure", got)
	}
}

// TestSendRcptRejected asserts a server-side recipient refusal returns an
// error naming the recipient stage.
func TestSendRcptRejected(t *testing.T) {
	srv := newTestSMTP(t, false, nil)
	srv.reject["bad@example.org"] = true
	m := testMailer(config.MailConfig{
		Enabled: true, Host: srv.host(), Port: srv.port(),
		From: "varve@example.org", TLS: "none",
	}, nil)

	err := m.send(context.Background(), "bad@example.org", []byte(testMessage))
	if err == nil {
		t.Fatal("want error for rejected recipient")
	}
	if !strings.Contains(err.Error(), "RCPT") {
		t.Errorf("error should mention RCPT stage, got: %v", err)
	}
	if got := len(srv.messages()); got != 0 {
		t.Errorf("queued %d messages despite recipient rejection", got)
	}
}

// TestSendDialFailure asserts a refused connection surfaces a connect-stage
// error.
func TestSendDialFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close() // nothing is listening anymore

	var p int
	fmt.Sscanf(port, "%d", &p)
	m := testMailer(config.MailConfig{
		Enabled: true, Host: host, Port: p, From: "varve@example.org", TLS: "none",
	}, nil)

	err = m.send(context.Background(), "who@example.org", []byte(testMessage))
	if err == nil {
		t.Fatal("want error for refused connection")
	}
	if !strings.Contains(err.Error(), "connect") {
		t.Errorf("error should carry connect stage context, got: %v", err)
	}
}

// TestSendInvalidTLSMode asserts an unknown TLS mode is rejected before any
// network traffic.
func TestSendInvalidTLSMode(t *testing.T) {
	m := testMailer(config.MailConfig{
		Enabled: true, Host: "127.0.0.1", Port: 1, From: "varve@example.org", TLS: "bogus",
	}, nil)
	err := m.send(context.Background(), "who@example.org", []byte(testMessage))
	if err == nil || !strings.Contains(err.Error(), `"bogus"`) {
		t.Errorf("want unsupported-mode error, got: %v", err)
	}
}

// TestAddrArg sanity-checks the helper used by the test server to parse
// MAIL FROM / RCPT TO arguments.
func TestAddrArg(t *testing.T) {
	for _, tc := range []struct {
		arg, prefix, want string
	}{
		{"FROM:<a@b>", "FROM:", "a@b"},
		{"TO: <c@d>", "TO:", "c@d"},
		{"FROM:a@b", "FROM:", "a@b"},
	} {
		if got := addrArg(tc.arg, tc.prefix); got != tc.want {
			t.Errorf("addrArg(%q, %q) = %q, want %q", tc.arg, tc.prefix, got, tc.want)
		}
	}
}
