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
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestUploadWithResume409 asserts a 409 with an offset resumes the upload
// from that offset and eventually succeeds (DETAIL §12.7 #6).
func TestUploadWithResume409(t *testing.T) {
	f := &fakeClient{uploadConflict: 1, uploadOffset: 123}
	r := runOneShotRunner(t, f)
	r.execCommand = newFakeExec().command

	path := filepath.Join(t.TempDir(), "a.pkg.tar.zst")
	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := r.uploadWithResume(context.Background(), "t-1", "tok", "a.pkg.tar.zst", path, 10); err != nil {
		t.Fatalf("uploadWithResume: %v", err)
	}
	if len(f.uploads) != 2 {
		t.Fatalf("upload attempts = %d, want 2 (409 then resume)", len(f.uploads))
	}
	if f.uploads[0].offset != 0 {
		t.Errorf("first attempt offset = %d, want 0", f.uploads[0].offset)
	}
	if f.uploads[1].offset != 123 {
		t.Errorf("resumed offset = %d, want 123", f.uploads[1].offset)
	}
}

// TestUploadRetriesExhausted asserts a persistent 409 fails the upload
// after 3 attempts and reports failed(upload) (DETAIL §12.5).
func TestUploadRetriesExhausted(t *testing.T) {
	f := &fakeClient{taskDetail: taskFor("t-1"), uploadConflict: 99, uploadOffset: 123}
	r := runOneShotRunner(t, f)
	exec := flowExec(t, r.workDir, "t-1", testSrcinfo, []string{"foo-1.0-1-x86_64.pkg.tar.zst"}, nil)
	r.execCommand = exec.command

	r.executeTask(context.Background(), taskFor("t-1"), "tok")

	res := f.lastResult()
	if res == nil || res.Status != statusFailed {
		t.Fatalf("result = %+v, want failed", res)
	}
	if res.Error == nil || res.Error.Stage != "upload" {
		t.Errorf("error = %+v, want stage upload", res.Error)
	}
	// 3 attempts for the package, then the upload stops.
	if got := f.callCount("UploadFile"); got != 3 {
		t.Errorf("upload attempts = %d, want 3", got)
	}
}

// TestUploadManifest asserts the manifest entries carry name, kind,
// package identity, size and the streaming sha256 (DETAIL §12.7 #6).
func TestUploadManifest(t *testing.T) {
	f := &fakeClient{}
	r := runOneShotRunner(t, f)
	r.execCommand = newFakeExec().command

	dir := t.TempDir()
	content := []byte("package payload")
	if err := os.WriteFile(filepath.Join(dir, "foo-1.2.3-1-x86_64.pkg.tar.zst"), content, 0o644); err != nil {
		t.Fatalf("write package: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "foo-1.2.3-1-x86_64.pkg.tar.zst.sig"), []byte("sig"), 0o644); err != nil {
		t.Fatalf("write sig: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".SRCINFO"), []byte("pkgbase = foo\n"), 0o644); err != nil {
		t.Fatalf("write srcinfo: %v", err)
	}
	src := &srcInfo{Pkgver: "1.2.3", Pkgrel: "1", Pkgname: []string{"foo"}, Arch: []string{"x86_64"}}

	manifest, err := r.uploadFiles(context.Background(), taskFor("t-1"), "tok", []string{
		filepath.Join(dir, "foo-1.2.3-1-x86_64.pkg.tar.zst"),
		filepath.Join(dir, "foo-1.2.3-1-x86_64.pkg.tar.zst.sig"),
		filepath.Join(dir, ".SRCINFO"),
	}, src)
	if err != nil {
		t.Fatalf("uploadFiles: %v", err)
	}
	if len(manifest) != 3 {
		t.Fatalf("manifest entries = %d, want 3", len(manifest))
	}
	wantHash := sha256.Sum256(content)
	pkg := manifest[0]
	if pkg.File != "foo-1.2.3-1-x86_64.pkg.tar.zst" || pkg.Kind != "package" ||
		pkg.Pkgname != "foo" || pkg.Version != "1.2.3-1" || pkg.Arch != "x86_64" ||
		pkg.Size != int64(len(content)) || pkg.SHA256 != hex.EncodeToString(wantHash[:]) {
		t.Errorf("package entry = %+v", pkg)
	}
	if manifest[1].Kind != "signature" || manifest[1].File != "foo-1.2.3-1-x86_64.pkg.tar.zst.sig" {
		t.Errorf("signature entry = %+v", manifest[1])
	}
	if manifest[2].Kind != "srcinfo" || manifest[2].File != ".SRCINFO" {
		t.Errorf("srcinfo entry = %+v", manifest[2])
	}
}

// TestClassifyArtifact covers the manifest classification fallbacks.
func TestClassifyArtifact(t *testing.T) {
	src := &srcInfo{Pkgver: "2.0", Pkgrel: "1", Pkgname: []string{"gcc", "gcc-libs"}, Arch: []string{"x86_64"}}

	// Exact construction wins.
	kind, pn, ver, arch := classifyArtifact("gcc-libs-2.0-1-x86_64.pkg.tar.zst", src, "x86_64")
	if kind != "package" || pn != "gcc-libs" || ver != "2.0-1" || arch != "x86_64" {
		t.Errorf("exact match = %q %q %q %q", kind, pn, ver, arch)
	}
	// Longest-prefix fallback (gcc must not steal gcc-libs).
	kind, pn, ver, arch = classifyArtifact("gcc-2.0-1-x86_64.pkg.tar.zst", src, "x86_64")
	if pn != "gcc" || ver != "2.0-1" || arch != "x86_64" {
		t.Errorf("fallback match = %q %q %q %q", kind, pn, ver, arch)
	}
	// Unknown package: arch from the task, identity empty.
	kind, pn, ver, arch = classifyArtifact("mystery-1-1-any.pkg.tar.zst", src, "x86_64")
	if kind != "package" || pn != "" || arch != "x86_64" {
		t.Errorf("unknown match = %q %q %q", kind, pn, arch)
	}
	if kind, _, _, _ := classifyArtifact("x.pkg.tar.zst.sig", src, "x86_64"); kind != "signature" {
		t.Errorf("sig kind = %q", kind)
	}
	if kind, _, _, _ := classifyArtifact(".SRCINFO", src, "x86_64"); kind != "srcinfo" {
		t.Errorf("srcinfo kind = %q", kind)
	}
}

// TestMatchSrcinfoLongestPrefix guards the gcc/gcc-libs ambiguity.
func TestMatchSrcinfoLongestPrefix(t *testing.T) {
	src := &srcInfo{Pkgver: "2.0", Pkgrel: "1", Pkgname: []string{"gcc", "gcc-libs"}, Arch: []string{"x86_64"}}
	pn, ver, arch, ok := matchSrcinfo("gcc-libs-2.0-1-x86_64", src)
	if !ok || pn != "gcc-libs" || ver != "2.0-1" || arch != "x86_64" {
		t.Errorf("matchSrcinfo = %q %q %q %v", pn, ver, arch, ok)
	}
}
