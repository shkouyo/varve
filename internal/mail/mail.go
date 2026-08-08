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

// Package mail sends plain-text build failure notifications over SMTP
// (RFC 5322). It is an optional component: when mail is disabled the
// notifier is a no-op. The package depends only on internal/config and has
// no dependencies on other internal modules.
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"

	"git.0x0f.dev/varve/internal/config"
)

// Notifier delivers build failure notifications. Dispatch snapshots the
// dotfile maintainers at enqueue time and calls SendFailure for every
// failed build; AUR push failures use SendAURFailure with the same
// recipients.
type Notifier interface {
	SendFailure(ctx context.Context, to []string, info FailureInfo) error
	SendAURFailure(ctx context.Context, to []string, info AURPushInfo) error
}

// FailureInfo carries the fields rendered into a failure notification.
type FailureInfo struct {
	Pkgbase string // package base name whose build failed
	Branch  string // source branch under test
	Commit  string // commit (or revision) being built
	Stage   string // build stage at which the failure occurred
	Summary string // short human-readable error summary
	LogURL  string // public Web log link (cfg.Server.WebURL + /builds/{id})
}

// AURPushInfo carries the fields rendered into an AUR push failure
// notification.
type AURPushInfo struct {
	Pkgbase string // package base name whose AUR push failed
	Branch  string // source branch pushed
	AURName string // AUR package name
	Commit  string // commit the push attempted
	Error   string // short human-readable push error
}

// Mailer sends failure notifications over SMTP. It holds no mutable state,
// so it is safe for concurrent use: every SendFailure opens its own
// connection and there is no connection pooling.
type Mailer struct {
	cfg       *config.MailConfig
	tlsConfig *tls.Config // test hook; nil in production builds the default
}

// NewMailer returns a Mailer configured by cfg.
func NewMailer(cfg *config.MailConfig) *Mailer {
	return &Mailer{cfg: cfg}
}

// SendFailure sends a plain-text failure notification to each recipient.
//
// It is a no-op returning nil when mail is disabled or when no recipients
// are given (a belt-and-braces guard; dispatch already checks before
// calling). Each recipient is sent on its own SMTP connection, so a
// failure for one recipient does not affect the others; all failures are
// aggregated into the returned error. The caller only logs the error and
// never lets it affect task state.
func (m *Mailer) SendFailure(ctx context.Context, to []string, info FailureInfo) error {
	return m.sendEach(ctx, to, func(rcpt string) []byte {
		return m.buildMessage(info, []string{rcpt})
	})
}

// SendAURFailure sends a plain-text AUR push failure notification to each
// recipient, following the same connection and error semantics as
// SendFailure.
func (m *Mailer) SendAURFailure(ctx context.Context, to []string, info AURPushInfo) error {
	return m.sendEach(ctx, to, func(rcpt string) []byte {
		return m.buildAURMessage(info, []string{rcpt})
	})
}

// sendEach delivers one message per recipient over its own SMTP
// connection. It is a no-op when mail is disabled or no recipients are
// given; send failures are aggregated into the returned error.
func (m *Mailer) sendEach(ctx context.Context, to []string, build func(rcpt string) []byte) error {
	if m == nil || m.cfg == nil || !m.cfg.Enabled || len(to) == 0 {
		return nil
	}
	var errs []error
	for _, rcpt := range to {
		if rcpt == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("mail: send to %s: %w", rcpt, err))
			break
		}
		if err := m.send(ctx, rcpt, build(rcpt)); err != nil {
			errs = append(errs, fmt.Errorf("mail: send to %s: %w", rcpt, err))
		}
	}
	return errors.Join(errs...)
}
