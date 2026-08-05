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
// no dependencies on other internal modules (DESIGN §2.8, DETAIL §8.1).
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
// failed build (DESIGN §7.9).
type Notifier interface {
	SendFailure(ctx context.Context, to []string, info FailureInfo) error
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

// Mailer sends failure notifications over SMTP. It holds no mutable state,
// so it is safe for concurrent use: every SendFailure opens its own
// connection and there is no connection pooling (DETAIL §8.6).
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
// calling, DETAIL §8.5). Each recipient is sent on its own SMTP connection,
// so a failure for one recipient does not affect the others; all failures
// are aggregated into the returned error. The caller only logs the error
// and never lets it affect task state.
func (m *Mailer) SendFailure(ctx context.Context, to []string, info FailureInfo) error {
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
		msg := m.buildMessage(info, []string{rcpt})
		if err := m.send(ctx, rcpt, msg); err != nil {
			errs = append(errs, fmt.Errorf("mail: send to %s: %w", rcpt, err))
		}
	}
	return errors.Join(errs...)
}
