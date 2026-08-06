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
	"testing"

	"git.0x0f.dev/varve/internal/config"
)

// signDetached creates a detached signature of file into sigPath using the
// keyring at home — the worker-side flow, gpg --detach-sign.
func signDetached(t *testing.T, home, keyID, passphrase, file, sigPath string) {
	t.Helper()
	cmd := exec.Command("gpg", "--homedir", home, "--batch", "--pinentry-mode", "loopback",
		"--passphrase", passphrase, "--detach-sign", "--output", sigPath, file)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("detach-sign: %v: %s", err, out)
	}
}

// newSignedSigner builds a signer over an imported protected key and signs
// the given package with the same keyring, mimicking the worker flow that
// produced the .sig to verify.
func newSignedSigner(t *testing.T) (*Signer, string, string) {
	t.Helper()
	const pass = "test-pass-123"
	src := t.TempDir()
	keyID := genTestKey(t, src, pass)
	keyFile := writeArmoredKeyFile(t, exportArmored(t, src, keyID, pass))

	home := t.TempDir()
	s, err := newSigner(&config.GPGConfig{KeyFile: keyFile, Passphrase: pass}, home)
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}
	pkg := filepath.Join(t.TempDir(), "pkg-1.0-1-x86_64.pkg.tar.zst")
	if err := os.WriteFile(pkg, []byte("package contents"), 0o600); err != nil {
		t.Fatalf("write package: %v", err)
	}
	sig := pkg + ".sig"
	signDetached(t, home, keyID, pass, pkg, sig)
	return s, sig, pkg
}

// TestVerifyDetachedValid asserts that a matching detached signature
// verifies against the managed keyring.
func TestVerifyDetachedValid(t *testing.T) {
	requireGPG(t)
	s, sig, pkg := newSignedSigner(t)
	if err := s.VerifyDetached(sig, pkg); err != nil {
		t.Fatalf("VerifyDetached(valid): %v", err)
	}
}

// TestVerifyDetachedTampered asserts that a modified package fails
// verification.
func TestVerifyDetachedTampered(t *testing.T) {
	requireGPG(t)
	s, sig, pkg := newSignedSigner(t)
	f, err := os.OpenFile(pkg, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open package: %v", err)
	}
	if _, err := f.WriteString("tampered"); err != nil {
		t.Fatalf("tamper package: %v", err)
	}
	f.Close()

	if err := s.VerifyDetached(sig, pkg); err == nil {
		t.Fatal("VerifyDetached(tampered): want error, got nil")
	}
}

// TestVerifyDetachedWrongKey asserts that a signature made by a foreign key
// fails verification against the managed keyring.
func TestVerifyDetachedWrongKey(t *testing.T) {
	requireGPG(t)
	const pass = "test-pass-123"
	src := t.TempDir()
	keyID := genTestKey(t, src, pass)
	keyFile := writeArmoredKeyFile(t, exportArmored(t, src, keyID, pass))

	s, err := newSigner(&config.GPGConfig{KeyFile: keyFile, Passphrase: pass}, t.TempDir())
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}
	pkg := filepath.Join(t.TempDir(), "pkg-2.0-1-x86_64.pkg.tar.zst")
	if err := os.WriteFile(pkg, []byte("package contents"), 0o600); err != nil {
		t.Fatalf("write package: %v", err)
	}
	// A foreign keyring signs the package.
	foreign := t.TempDir()
	foreignID := genTestKey(t, foreign, "")
	sig := pkg + ".sig"
	signDetached(t, foreign, foreignID, "", pkg, sig)

	if err := s.VerifyDetached(sig, pkg); err == nil {
		t.Fatal("VerifyDetached(wrong key): want error, got nil")
	}
}

// TestVerifyDetachedMissingFiles asserts that missing signature or package
// files fail verification.
func TestVerifyDetachedMissingFiles(t *testing.T) {
	requireGPG(t)
	src := t.TempDir()
	keyID := genTestKey(t, src, "")
	keyFile := writeArmoredKeyFile(t, exportArmored(t, src, keyID, ""))

	s, err := newSigner(&config.GPGConfig{KeyFile: keyFile}, t.TempDir())
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}
	if err := s.VerifyDetached("/nonexistent.sig", "/nonexistent.pkg"); err == nil {
		t.Fatal("VerifyDetached(missing files): want error, got nil")
	}
}

// TestGnuPGEnv asserts the environment fragment points at the managed home.
func TestGnuPGEnv(t *testing.T) {
	requireGPG(t)
	src := t.TempDir()
	keyID := genTestKey(t, src, "")
	keyFile := writeArmoredKeyFile(t, exportArmored(t, src, keyID, ""))

	home := t.TempDir()
	s, err := newSigner(&config.GPGConfig{KeyFile: keyFile}, home)
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}
	env := s.GnuPGEnv()
	if len(env) != 1 {
		t.Fatalf("GnuPGEnv() = %v, want a single GNUPGHOME entry", env)
	}
	if want := "GNUPGHOME=" + home; env[0] != want {
		t.Errorf("GnuPGEnv()[0] = %q, want %q", env[0], want)
	}
}
