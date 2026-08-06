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

package sign

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.0x0f.dev/varve/internal/config"
)

// requireGPG skips the test when the gpg binary is unavailable.
func requireGPG(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not available")
	}
}

// genTestKey creates a one-shot key in the given GNUPGHOME and returns its
// long key ID. With a non-empty passphrase the secret key is protected.
func genTestKey(t *testing.T, home, passphrase string) string {
	t.Helper()
	requireGPG(t)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("create GNUPGHOME: %v", err)
	}
	cmd := exec.Command("gpg", "--homedir", home, "--batch", "--pinentry-mode", "loopback",
		"--passphrase", passphrase, "--quick-gen-key",
		"Varve Test <varve@test.invalid>", "default", "default", "never")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("quick-gen-key: %v: %s", err, out)
	}
	out, err := exec.Command("gpg", "--homedir", home, "--batch",
		"--list-secret-keys", "--with-colons").CombinedOutput()
	if err != nil {
		t.Fatalf("list-secret-keys: %v: %s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "sec:") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) > 4 && fields[4] != "" {
			return fields[4]
		}
	}
	t.Fatal("generated key not listed as secret")
	return ""
}

// exportArmored dumps the secret key from home as armored text.
func exportArmored(t *testing.T, home, keyID, passphrase string) string {
	t.Helper()
	cmd := exec.Command("gpg", "--homedir", home, "--batch", "--pinentry-mode", "loopback",
		"--passphrase", passphrase, "--export-secret-keys", "--armor", keyID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("export-secret-keys: %v: %s", err, out)
	}
	return string(out)
}

// writeArmoredKeyFile stores armored key material in a fresh temp file and
// returns its path.
func writeArmoredKeyFile(t *testing.T, armored string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key.asc")
	if err := os.WriteFile(path, []byte(armored), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

// TestNewSignerImportKeyFile covers the key_file initialization path: the
// armored private key is imported into a fresh GNUPGHOME and the key ID is
// resolved from the keyring.
func TestNewSignerImportKeyFile(t *testing.T) {
	requireGPG(t)
	src := t.TempDir()
	keyID := genTestKey(t, src, "")
	keyFile := writeArmoredKeyFile(t, exportArmored(t, src, keyID, ""))

	home := t.TempDir()
	s, err := newSigner(&config.GPGConfig{KeyFile: keyFile}, home)
	if err != nil {
		t.Fatalf("newSigner(key_file): %v", err)
	}
	if s.keyID != keyID {
		t.Errorf("resolved keyID = %q, want %q", s.keyID, keyID)
	}
	if s.gnupgHome != home {
		t.Errorf("gnupgHome = %q, want %q", s.gnupgHome, home)
	}
}

// TestNewSignerKeyIDReference covers the key_id-only path: the key is
// referenced from the keyring already present in GNUPGHOME, with no import.
func TestNewSignerKeyIDReference(t *testing.T) {
	requireGPG(t)
	home := t.TempDir()
	keyID := genTestKey(t, home, "")

	s, err := newSigner(&config.GPGConfig{KeyID: keyID}, home)
	if err != nil {
		t.Fatalf("newSigner(key_id): %v", err)
	}
	if s.keyID != keyID {
		t.Errorf("keyID = %q, want %q", s.keyID, keyID)
	}
}

// TestNewSignerKeyFileWithKeyID asserts that an explicit key_id is
// verified after the file import.
func TestNewSignerKeyFileWithKeyID(t *testing.T) {
	requireGPG(t)
	src := t.TempDir()
	keyID := genTestKey(t, src, "")
	keyFile := writeArmoredKeyFile(t, exportArmored(t, src, keyID, ""))

	home := t.TempDir()
	s, err := newSigner(&config.GPGConfig{KeyFile: keyFile, KeyID: keyID}, home)
	if err != nil {
		t.Fatalf("newSigner(key_file+key_id): %v", err)
	}
	if s.keyID != keyID {
		t.Errorf("keyID = %q, want %q", s.keyID, keyID)
	}
}

// TestNewSignerMissingKey asserts that an unknown key ID is a startup
// error.
func TestNewSignerMissingKey(t *testing.T) {
	requireGPG(t)
	home := t.TempDir()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("create home: %v", err)
	}
	if _, err := newSigner(&config.GPGConfig{KeyID: "DEADBEEFDEADBEEF"}, home); err == nil {
		t.Fatal("newSigner with unknown key_id: want error, got nil")
	}
}

// TestNewSignerBadKeyFile asserts that an unimportable key file is a
// startup error.
func TestNewSignerBadKeyFile(t *testing.T) {
	requireGPG(t)
	bad := filepath.Join(t.TempDir(), "bad.asc")
	if err := os.WriteFile(bad, []byte("not a gpg key"), 0o600); err != nil {
		t.Fatalf("write bad key: %v", err)
	}
	if _, err := newSigner(&config.GPGConfig{KeyFile: bad}, t.TempDir()); err == nil {
		t.Fatal("newSigner with bad key file: want error, got nil")
	}
}

// TestNewSignerCreatesHome asserts that a missing GNUPGHOME directory is
// created with mode 0700.
func TestNewSignerCreatesHome(t *testing.T) {
	requireGPG(t)
	src := t.TempDir()
	keyID := genTestKey(t, src, "")
	keyFile := writeArmoredKeyFile(t, exportArmored(t, src, keyID, ""))

	home := filepath.Join(t.TempDir(), "gnupg")
	if _, err := newSigner(&config.GPGConfig{KeyFile: keyFile}, home); err != nil {
		t.Fatalf("newSigner: %v", err)
	}
	fi, err := os.Stat(home)
	if err != nil {
		t.Fatalf("stat home: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("home mode = %v, want 0700", fi.Mode().Perm())
	}
}

// TestNewSignerNilConfig guards the nil configuration precondition.
func TestNewSignerNilConfig(t *testing.T) {
	if _, err := newSigner(nil, t.TempDir()); err == nil {
		t.Fatal("newSigner(nil): want error, got nil")
	}
}
