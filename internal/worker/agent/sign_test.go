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

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.0x0f.dev/varve/internal/api"
)

// TestSignPackagesGpgArgs asserts the gpg invocations: one import into the
// temporary GNUPGHOME and one loopback-pinentry detach-sign per package.
func TestSignPackagesGpgArgs(t *testing.T) {
	f := &fakeClient{keyMaterial: &api.KeyMaterial{
		KeyID:             "DEADBEEF",
		ArmoredPrivateKey: "-----BEGIN PGP PRIVATE KEY BLOCK-----\nFAKE\n-----END PGP PRIVATE KEY BLOCK-----\n",
		Passphrase:        "secret",
	}}
	r := runOneShotRunner(t, f)

	exec := newFakeExec()
	exec.scripts["gpg --batch --import "+filepath.Join(r.workDir, "t-1", ".gnupg", "private.asc")] =
		writeScript(t, "true")
	exec.scripts["gpg --batch --pinentry-mode loopback --passphrase secret --detach-sign "+filepath.Join(r.workDir, "t-1", "a.pkg.tar.zst")] =
		writeScript(t, "true")
	exec.scripts["gpg --batch --pinentry-mode loopback --passphrase secret --detach-sign "+filepath.Join(r.workDir, "t-1", "b.pkg.tar.zst")] =
		writeScript(t, "true")
	r.execCommand = exec.command

	gnupgHome := filepath.Join(r.workDir, "t-1", ".gnupg")
	sigs, err := r.signPackages(context.Background(), taskFor("t-1"), "tok", []string{
		filepath.Join(r.workDir, "t-1", "a.pkg.tar.zst"),
		filepath.Join(r.workDir, "t-1", "b.pkg.tar.zst"),
	}, gnupgHome, discardWriter{})
	if err != nil {
		t.Fatalf("signPackages: %v", err)
	}
	if len(sigs) != 2 || sigs[0] != filepath.Join(r.workDir, "t-1", "a.pkg.tar.zst.sig") {
		t.Errorf("sigs = %v", sigs)
	}

	gpgArgs := exec.callArgs("gpg")
	if len(gpgArgs) != 3 {
		t.Fatalf("gpg calls = %d, want import + 2 detach-signs", len(gpgArgs))
	}
	importArgs := gpgArgs[0]
	if !contains(importArgs, "--batch") || !contains(importArgs, "--import") {
		t.Errorf("import args = %v", importArgs)
	}
	for _, args := range gpgArgs[1:] {
		if !contains(args, "--pinentry-mode") || !contains(args, "loopback") ||
			!contains(args, "--passphrase") || !contains(args, "secret") ||
			!contains(args, "--detach-sign") {
			t.Errorf("detach-sign args = %v", args)
		}
	}

	// The armored key material lands in the temporary GNUPGHOME.
	keyData, err := os.ReadFile(filepath.Join(gnupgHome, "private.asc"))
	if err != nil {
		t.Fatalf("read private.asc: %v", err)
	}
	if !strings.Contains(string(keyData), "BEGIN PGP PRIVATE KEY") {
		t.Errorf("private.asc does not carry the armored key")
	}
}

// TestSignKeyClaimFailureFailsTask asserts a GetSigningKey failure reports
// failed(sign).
func TestSignKeyClaimFailureFailsTask(t *testing.T) {
	f := &fakeClient{taskDetail: taskFor("t-1"), keyErr: errors.New("claim denied")}
	r := runOneShotRunner(t, f)
	exec := flowExec(t, r.workDir, "t-1", testSrcinfo, []string{"foo-1.0-1-x86_64.pkg.tar.zst"}, nil)
	r.execCommand = exec.command

	task := taskFor("t-1")
	task.Signing.Required = true
	r.executeTask(context.Background(), task, "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusFailed {
		t.Fatalf("result = %+v, want failed", res)
	}
	if res.Error == nil || res.Error.Stage != "sign" {
		t.Errorf("error = %+v, want stage sign", res.Error)
	}
	if !strings.Contains(res.Error.Summary, "claim denied") {
		t.Errorf("summary = %q, want the claim error", res.Error.Summary)
	}
	if f.callCount("UploadFile") != 0 {
		t.Errorf("upload ran after a signing failure")
	}
	// The temporary GNUPGHOME is cleaned up on the failure path.
	if _, err := os.Stat(filepath.Join(r.workDir, "t-1", ".gnupg")); !os.IsNotExist(err) {
		t.Errorf("temporary GNUPGHOME left behind: %v", err)
	}
}

// discardWriter discards build output in unit tests.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
