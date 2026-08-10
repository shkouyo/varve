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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.0x0f.dev/varve/internal/api"
)

// TestSignPackagesGpgArgs asserts the hardened gpg invocations: one
// stdin import into the temporary GNUPGHOME and one loopback-pinentry
// detach-sign per package. The passphrase travels on stdin (--passphrase-
// fd 0), never in argv, and the armored key is fed to the import through
// stdin instead of a private.asc file on disk.
func TestSignPackagesGpgArgs(t *testing.T) {
	f := &fakeClient{keyMaterial: &api.KeyMaterial{
		KeyID:             "DEADBEEF",
		ArmoredPrivateKey: "-----BEGIN PGP PRIVATE KEY BLOCK-----\nFAKE\n-----END PGP PRIVATE KEY BLOCK-----\n",
		Passphrase:        "secret",
	}}
	r := runOneShotRunner(t, f)

	importCapture := t.TempDir() + "/imported-key"
	passCapture := t.TempDir() + "/passphrase"
	exec := newFakeExec()
	exec.scripts["gpg --batch --import"] =
		writeScript(t, "cat > "+importCapture)
	exec.scripts["gpg --batch --pinentry-mode loopback --passphrase-fd 0 --detach-sign "+filepath.Join(r.workDir, "t-1", "a.pkg.tar.zst")] =
		writeScript(t, "read -r line && echo \"$line\" > "+passCapture)
	exec.scripts["gpg --batch --pinentry-mode loopback --passphrase-fd 0 --detach-sign "+filepath.Join(r.workDir, "t-1", "b.pkg.tar.zst")] =
		writeScript(t, "read -r line && echo \"$line\" > "+passCapture)
	r.execCommand = exec.command

	gnupgHome := filepath.Join(r.workDir, ".gnupg-t-1")
	sigs, err := r.signPackages(context.Background(), taskFor("t-1"), "tok", []string{
		filepath.Join(r.workDir, "t-1", "a.pkg.tar.zst"),
		filepath.Join(r.workDir, "t-1", "b.pkg.tar.zst"),
	}, gnupgHome, io.Discard)
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
	if len(importArgs) != 2 {
		t.Errorf("import args = %v, want exactly --batch --import (no key file)", importArgs)
	}
	for _, args := range gpgArgs[1:] {
		if !contains(args, "--pinentry-mode") || !contains(args, "loopback") ||
			!contains(args, "--passphrase-fd") || !contains(args, "0") ||
			!contains(args, "--detach-sign") {
			t.Errorf("detach-sign args = %v", args)
		}
		for _, a := range args {
			if a == "--passphrase" || a == "secret" {
				t.Errorf("detach-sign args %v leak the passphrase through argv", args)
			}
		}
	}

	// The armored key reached gpg through the import's stdin, and the
	// GNUPGHOME holds no private.asc file.
	keyData, err := os.ReadFile(importCapture)
	if err != nil {
		t.Fatalf("read imported key capture: %v", err)
	}
	if !strings.Contains(string(keyData), "BEGIN PGP PRIVATE KEY") {
		t.Errorf("import stdin does not carry the armored key: %q", keyData)
	}
	if _, err := os.Stat(filepath.Join(gnupgHome, "private.asc")); !os.IsNotExist(err) {
		t.Errorf("private.asc exists in the GNUPGHOME: %v", err)
	}
	passData, err := os.ReadFile(passCapture)
	if err != nil {
		t.Fatalf("read passphrase capture: %v", err)
	}
	if strings.TrimSpace(string(passData)) != "secret" {
		t.Errorf("detach-sign stdin = %q, want the passphrase", passData)
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
	if _, err := os.Stat(filepath.Join(r.workDir, ".gnupg-t-1")); !os.IsNotExist(err) {
		t.Errorf("temporary GNUPGHOME left behind: %v", err)
	}
}
