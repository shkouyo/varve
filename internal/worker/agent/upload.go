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
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"git.0x0f.dev/varve/internal/api"
	"git.0x0f.dev/varve/internal/repo"
)

// uploadFiles streams every collected file (packages, signatures, the
// .SRCINFO snapshot) into the task staging area and builds the manifest
// entries: name, kind, package identity from the .SRCINFO, size and a
// streaming sha256. Upload order is preserved. Any upload failure after
// retry exhaustion fails the task (stage=upload).
func (r *Runner) uploadFiles(ctx context.Context, task *api.TaskDetail, token string,
	files []string, src *srcInfo) ([]repo.Artifact, error) {
	manifest := make([]repo.Artifact, 0, len(files))
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", f, err)
		}
		h := sha256.New()
		if err := hashStream(f, h); err != nil {
			return nil, fmt.Errorf("hash %s: %w", f, err)
		}
		name := filepath.Base(f)
		kind, pkgname, ver, arch := classifyArtifact(name, src, task.Package.Arch)
		if err := r.uploadWithResume(ctx, task.ID, token, name, f, st.Size()); err != nil {
			return nil, err
		}
		manifest = append(manifest, repo.Artifact{
			File:    name,
			Kind:    kind,
			Pkgname: pkgname,
			Version: ver,
			Arch:    arch,
			Size:    st.Size(),
			SHA256:  hex.EncodeToString(h.Sum(nil)),
		})
	}
	return manifest, nil
}

// uploadWithResume streams one file with resumable offsets: a 409 conflict
// carrying the server-side offset resumes from there, up to 3 attempts.
func (r *Runner) uploadWithResume(ctx context.Context, taskID, token, name, path string, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer f.Close()

	offset := int64(0)
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return fmt.Errorf("seek %s: %w", name, err)
		}
		_, err := r.client.UploadFile(ctx, taskID, token, name, f, size, offset)
		if err == nil {
			return nil
		}
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
			offset = apiErr.Offset
			continue
		}
		return fmt.Errorf("upload %s: %w", name, err)
	}
	return fmt.Errorf("upload %s: retries exhausted", name)
}

// classifyArtifact maps an uploaded file name to its manifest entry:
// kind "package" with the identity parsed from .SRCINFO, "signature" for
// .sig files and "srcinfo" for the snapshot.
func classifyArtifact(name string, src *srcInfo, defaultArch string) (kind, pkgname, version, arch string) {
	switch {
	case strings.HasSuffix(name, ".pkg.tar.zst"):
		kind = "package"
		stem := strings.TrimSuffix(name, ".pkg.tar.zst")
		if src != nil {
			if pn, ver, a, ok := matchSrcinfo(stem, src); ok {
				return "package", pn, ver, a
			}
		}
		if arch == "" {
			arch = defaultArch
		}
		return "package", "", "", arch
	case strings.HasSuffix(name, ".sig"):
		return "signature", "", "", ""
	case name == ".SRCINFO":
		return "srcinfo", "", "", ""
	}
	return "", "", "", ""
}

// matchSrcinfo resolves a package file stem (name without .pkg.tar.zst)
// against the parsed .SRCINFO: exact "<pkgname>-<version>-<arch>
// construction, falling back to the longest pkgname prefix.
func matchSrcinfo(stem string, src *srcInfo) (pkgname, version, arch string, ok bool) {
	version = src.Pkgver + "-" + src.Pkgrel
	want := make(map[string]bool, len(src.Pkgname)*len(src.Arch))
	for _, pn := range src.Pkgname {
		for _, a := range src.Arch {
			want[pn+"-"+version+"-"+a] = true
		}
	}
	if want[stem] {
		for _, pn := range src.Pkgname {
			for _, a := range src.Arch {
				if pn+"-"+version+"-"+a == stem {
					return pn, version, a, true
				}
			}
		}
	}
	// Longest-prefix fallback: "<pkgname>-<version>-<arch>" with version
	// and arch both allowed to contain hyphens.
	candidates := append([]string(nil), src.Pkgname...)
	sort.Slice(candidates, func(i, j int) bool { return len(candidates[i]) > len(candidates[j]) })
	for _, pn := range candidates {
		rest, found := strings.CutPrefix(stem, pn+"-")
		if !found {
			continue
		}
		if i := strings.LastIndex(rest, "-"); i > 0 {
			return pn, rest[:i], rest[i+1:], true
		}
	}
	return "", "", "", false
}

// hashStream hashes a file's content by streaming.
func hashStream(path string, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}
