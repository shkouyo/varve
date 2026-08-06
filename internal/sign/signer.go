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

// Package sign implements controller-side GPG key custody
// (GNUPGHOME=/data/gnupg), per-task key material export, artifact signature
// verification and the signing environment consumed by repo-add --sign.
// Secret keys are never baked into images and never persist on workers.
package sign

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"git.0x0f.dev/varve/internal/config"
)

// defaultGNUPGHome is the controller-side key custody directory.
const defaultGNUPGHome = "/data/gnupg"

// execCommand is the command constructor used for every external gpg call;
// same-package tests may replace it with a recorder.
var execCommand = exec.CommandContext

// KeyMaterial is the one-shot signing material handed to a task over HTTPS
// (defined in this package; the API depends on this package only for the
// type). It carries the armored private key plus the passphrase needed to
// use it inside the worker build container.
type KeyMaterial struct {
	KeyID             string
	ArmoredPrivateKey string
	Passphrase        string
}

// Signer manages the controller GNUPGHOME and hands out per-task key
// material. All public methods are safe for concurrent use.
type Signer struct {
	cfg         *config.GPGConfig
	gnupgHome   string
	keyID       string // signing key ID (cfg.KeyID, or derived from the keyring)
	cache       map[string]*KeyMaterial
	mu          sync.Mutex
	execCommand func(ctx context.Context, name string, arg ...string) *exec.Cmd
}

// NewSigner prepares the managed GNUPGHOME and the signing key. It creates
// /data/gnupg with mode 0700, imports the armored private key from
// cfg.KeyFile when set, otherwise references the key identified by
// cfg.KeyID already present in the keyring, and verifies that a usable
// secret key exists afterwards. A missing gpg binary, a failed import or
// an unknown key ID are all startup errors. Not safe for concurrent use:
// call once at startup.
func NewSigner(cfg *config.GPGConfig) (*Signer, error) {
	return newSigner(cfg, defaultGNUPGHome)
}

// newSigner builds a Signer rooted at home. NewSigner uses the default
// /data/gnupg; tests use a t.TempDir() home.
func newSigner(cfg *config.GPGConfig, home string) (*Signer, error) {
	if cfg == nil {
		return nil, errors.New("sign: nil GPGConfig")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("sign: create GNUPGHOME %s: %w", home, err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		return nil, fmt.Errorf("sign: chmod GNUPGHOME %s: %w", home, err)
	}
	s := &Signer{
		cfg:         cfg,
		gnupgHome:   home,
		cache:       make(map[string]*KeyMaterial),
		execCommand: execCommand,
	}
	if cfg.KeyFile != "" {
		if err := s.importKeyFile(cfg.KeyFile); err != nil {
			return nil, err
		}
	}
	keyID := cfg.KeyID
	if keyID == "" {
		// Only a key file was configured: reference the imported key.
		id, err := s.listSecretKeyID()
		if err != nil {
			return nil, err
		}
		keyID = id
	}
	if err := s.verifySecretKey(keyID); err != nil {
		return nil, err
	}
	s.keyID = keyID
	return s, nil
}

// importKeyFile imports the armored private key file into the managed
// keyring (gpg --batch --import).
func (s *Signer) importKeyFile(path string) error {
	cmd := s.execCommand(context.Background(), "gpg", "--homedir", s.gnupgHome,
		"--batch", "--import", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sign: import key file %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// listSecretKeyID returns the primary secret key ID found in the managed
// keyring, used when only a key file was configured and no explicit
// key_id was given.
func (s *Signer) listSecretKeyID() (string, error) {
	cmd := s.execCommand(context.Background(), "gpg", "--homedir", s.gnupgHome,
		"--batch", "--list-secret-keys", "--with-colons")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("sign: list secret keys: %w: %s", err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "sec:") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) > 4 && fields[4] != "" {
			return fields[4], nil
		}
	}
	return "", errors.New("sign: no secret key found in keyring")
}

// verifySecretKey asserts that a secret key with the given ID exists in
// the managed keyring; startup fails otherwise.
func (s *Signer) verifySecretKey(keyID string) error {
	cmd := s.execCommand(context.Background(), "gpg", "--homedir", s.gnupgHome,
		"--batch", "--list-secret-keys", keyID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sign: secret key %s not found: %w: %s", keyID, err, strings.TrimSpace(string(out)))
	}
	return nil
}
