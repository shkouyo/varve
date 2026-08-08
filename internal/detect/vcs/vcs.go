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

// Package vcs identifies VCS packages by their -git/-svn naming convention
// (following yay) and queries the upstream reference of a VCS source: the
// HEAD commit for git and the last-changed revision for svn. Every
// external command goes through the package variable execCommand so
// same-package tests can substitute a recorder. Tests in this package
// replace execCommand and must not run t.Parallel.
package vcs

import (
	"context"
	"encoding/xml"
	"fmt"
	"os/exec"
	"strings"
)

// Kind enumerates the VCS nature of a package.
type Kind int

const (
	// None marks a plain (non-VCS) package.
	None Kind = iota
	// Git marks a package built from a git upstream.
	Git
	// SVN marks a package built from an svn upstream.
	SVN
)

// execCommand is the command constructor used for every external git/svn
// call; same-package tests may replace it with a recorder.
var execCommand = exec.CommandContext

// DetectKind decides whether a package is a VCS package. A non-"auto"
// dotfileVCS value (git/svn/none) wins directly; "auto" (or an empty
// value) falls back to the -git/-svn suffix of pkgbase, then of each
// pkgname in order.
func DetectKind(pkgbase string, pkgnames []string, dotfileVCS string) Kind {
	switch dotfileVCS {
	case "git":
		return Git
	case "svn":
		return SVN
	case "none":
		return None
	}
	// "auto", "" or any unrecognized value: suffix detection.
	if k := kindBySuffix(pkgbase); k != None {
		return k
	}
	for _, name := range pkgnames {
		if k := kindBySuffix(name); k != None {
			return k
		}
	}
	return None
}

// kindBySuffix maps the yay-style -git/-svn naming suffix to a Kind.
func kindBySuffix(name string) Kind {
	switch {
	case strings.HasSuffix(name, "-git"):
		return Git
	case strings.HasSuffix(name, "-svn"):
		return SVN
	default:
		return None
	}
}

// UpstreamURLs extracts the VCS upstream addresses from source= entries:
// values carrying a git+/svn+ prefix (prefix stripped) or a .git suffix
// are returned, in order, skipping plain tarball sources.
func UpstreamURLs(source []string) []string {
	var out []string
	for _, s := range source {
		if u, ok := upstreamURL(s); ok {
			out = append(out, u)
		}
	}
	return out
}

// upstreamURL reports whether s is a VCS source and returns the usable
// URL (with any git+/svn+ scheme prefix removed).
func upstreamURL(s string) (string, bool) {
	switch {
	case strings.HasPrefix(s, "git+"):
		return strings.TrimPrefix(s, "git+"), true
	case strings.HasPrefix(s, "svn+"):
		return strings.TrimPrefix(s, "svn+"), true
	case strings.HasSuffix(s, ".git"):
		return s, true
	default:
		return "", false
	}
}

// GitHead returns the full HEAD hash of a git repository as reported by
// "git ls-remote <url> HEAD". An empty repository (no HEAD) yields an
// empty string, not an error.
func GitHead(ctx context.Context, url string) (string, error) {
	cmd := execCommand(ctx, "git", "ls-remote", url, "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("vcs: git ls-remote %s: %w: %s", url, err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 {
			return fields[0], nil
		}
	}
	return "", nil
}

// svnInfo is the subset of the "svn info --xml" document the package
// needs: the last-changed-revision lives on the <commit> element of the
// first <entry> as its revision attribute.
type svnInfo struct {
	Entry struct {
		Commit struct {
			Revision string `xml:"revision,attr"`
		} `xml:"commit"`
	} `xml:"entry"`
}

// SVNRevision returns the last-changed revision of an svn repository as
// reported by "svn info --xml <url>".
func SVNRevision(ctx context.Context, url string) (string, error) {
	cmd := execCommand(ctx, "svn", "info", "--xml", url)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("vcs: svn info %s: %w: %s", url, err, strings.TrimSpace(string(out)))
	}
	var doc svnInfo
	if err := xml.Unmarshal(out, &doc); err != nil {
		return "", fmt.Errorf("vcs: parse svn info for %s: %w", url, err)
	}
	if doc.Entry.Commit.Revision == "" {
		return "", fmt.Errorf("vcs: svn info for %s: no last-changed-revision", url)
	}
	return doc.Entry.Commit.Revision, nil
}
