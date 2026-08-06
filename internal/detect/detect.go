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

// Package detect polls the configured source repository mirror, computes
// .SRCINFO hashes and VCS upstream references per enabled branch and
// enqueues changes through the Sink interface. It also owns dotfile
// parsing and merging. The database is read-only here: the
// packages.last_srcinfo_hash / last_upstream_ref records are only updated
// after a successful build, which is what makes failed builds naturally
// re-queue on the next round.
package detect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/detect/srcinfo"
	"git.0x0f.dev/varve/internal/detect/vcs"
)

// dotfileName is the fixed per-branch dotfile name.
const dotfileName = ".varve.toml"

// sourceRoot is the controller-side mirror directory.
const sourceRoot = "/data/source"

// vcsQueryConcurrency bounds the number of concurrent upstream queries
// within one poll round.
const vcsQueryConcurrency = 4

// Change reasons. ReasonManual is used by the admin rebuild
// path in dispatch, not by detect itself.
const (
	ReasonSrcinfo  = "srcinfo"
	ReasonUpstream = "upstream"
	ReasonBoth     = "srcinfo+upstream"
	ReasonManual   = "manual"
)

// Package identifies one detected package within a source branch.
// VCSKind is "" for plain packages, "git" or "svn" otherwise.
type Package struct {
	Pkgbase string
	Branch  string
	VCSKind string
	Arch    string
}

// Change is one detected package update handed to the Sink. UpstreamRef
// carries the upstream reference queried at detection time for VCS
// packages and is empty for plain packages.
type Change struct {
	Package     Package
	Maintainers []string
	Hooks       Hooks
	Collect     Collect
	UpstreamRef string
	Reason      string
}

// Sink consumes detected changes; the dispatch module implements it.
// Implementations must be safe for concurrent Submit calls.
type Sink interface {
	Submit(ctx context.Context, c Change) error
}

// Detector polls the source mirror and submits changes. Public methods
// (Run, PollOnce, BranchSnapshot) are mutually exclusive: the caller must
// not invoke them concurrently.
type Detector struct {
	cfg         *config.SourceConfig
	store       *db.Store
	sink        Sink
	mirrorDir   string
	execCommand func(ctx context.Context, name string, arg ...string) *exec.Cmd
	now         func() time.Time
	logger      *slog.Logger
}

// execCommand is the command constructor used for every external git call;
// same-package tests may replace it with a recorder.
var execCommand = exec.CommandContext

// NewDetector builds a Detector for cfg and derives its mirror directory
// from the source URL. It performs no network operation.
func NewDetector(cfg *config.SourceConfig, store *db.Store, sink Sink) (*Detector, error) {
	return newDetector(cfg, store, sink, sourceRoot)
}

// newDetector is NewDetector with an injectable mirror root (tests use a
// temp directory instead of /data/source).
func newDetector(cfg *config.SourceConfig, store *db.Store, sink Sink, root string) (*Detector, error) {
	if cfg == nil {
		return nil, errors.New("detect: nil SourceConfig")
	}
	if cfg.URL == "" {
		return nil, errors.New("detect: source URL is required")
	}
	name := MirrorDir(cfg.URL)
	if name == "" {
		return nil, errors.New("detect: cannot derive mirror directory name from source URL")
	}
	return &Detector{
		cfg:         cfg,
		store:       store,
		sink:        sink,
		mirrorDir:   filepath.Join(root, name+".git"),
		execCommand: execCommand,
		now:         time.Now,
		logger:      slog.Default(),
	}, nil
}

// MirrorDir derives the mirror directory name from a source repository
// URL: the URL path part without the ".git" suffix, with "/" replaced by
// "_". For example "git@git.example.org:pkgbuilds.git" yields
// "pkgbuilds" and "https://git.example.org/pkgs/foo.git" yields
// "pkgs_foo". The dispatch module reuses this helper when packaging source
// archives.
func MirrorDir(url string) string {
	path := url
	if i := strings.Index(url, "://"); i >= 0 {
		path = url[i+len("://"):]
		if j := strings.IndexByte(path, '/'); j >= 0 {
			path = path[j+1:]
		}
	} else if i := strings.IndexByte(url, ':'); i >= 0 {
		// scp-like "user@host:path" syntax.
		path = url[i+1:]
	}
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.TrimSuffix(path, "/")
	return strings.ReplaceAll(path, "/", "_")
}

// PollOnce runs one full detection round: mirror maintenance, branch
// enumeration and the per-branch pipeline. A mirror fetch failure aborts
// the round and is reported; per-branch problems only produce warnings so
// one bad branch never blocks the others.
func (d *Detector) PollOnce(ctx context.Context) error {
	if err := d.ensureMirror(ctx); err != nil {
		return err
	}
	branches, err := d.listBranches(ctx)
	if err != nil {
		return err
	}

	plans := make([]*branchPlan, 0, len(branches))
	for _, branch := range branches {
		if p := d.planBranch(ctx, branch); p != nil {
			plans = append(plans, p)
		}
	}
	d.queryUpstream(ctx, plans)
	for _, p := range plans {
		d.submitChange(ctx, p)
	}
	return nil
}

// branchPlan carries everything the pipeline learned about one branch so
// the upstream queries can overlap before the comparisons and submits
// happen in branch order.
type branchPlan struct {
	branch      string
	info        *srcinfo.Info
	hash        string
	dotfile     *Dotfile
	kind        vcs.Kind
	upstreamURL string
	upstreamRef string
	upstreamErr error
}

// planBranch runs steps 1-3 of the per-branch pipeline: read SRCINFO, hash
// it, parse the dotfile (with extras) and detect the VCS kind. It returns
// nil when the branch must be skipped with a warning.
func (d *Detector) planBranch(ctx context.Context, branch string) *branchPlan {
	data, err := d.showFile(ctx, branch, "SRCINFO")
	if err != nil {
		d.logger.Warn("detect: branch has no SRCINFO, skipping", "branch", branch, "error", err)
		return nil
	}
	info, err := srcinfo.Parse(data)
	if err != nil {
		d.logger.Warn("detect: invalid SRCINFO, skipping", "branch", branch, "error", err)
		return nil
	}
	dotfile, err := d.parseDotfile(ctx, branch)
	if err != nil {
		d.logger.Warn("detect: invalid dotfile, skipping", "branch", branch, "error", err)
		return nil
	}
	kind := vcs.DetectKind(info.Pkgbase, info.Pkgname, dotfile.VCS)
	p := &branchPlan{
		branch:  branch,
		info:    info,
		hash:    srcinfo.Hash(data),
		dotfile: dotfile,
		kind:    kind,
	}
	if kind != vcs.None {
		urls := vcs.UpstreamURLs(info.Source)
		if len(urls) > 0 {
			p.upstreamURL = urls[0]
		}
	}
	return p
}

// parseDotfile reads .varve.toml from the branch and merges its extras
// through a git-show-backed get callback. A missing dotfile is not an
// error: the branch is treated as a plain PKGBUILD branch.
func (d *Detector) parseDotfile(ctx context.Context, branch string) (*Dotfile, error) {
	data, err := d.showFile(ctx, branch, dotfileName)
	if err != nil {
		return &Dotfile{}, nil
	}
	return ParseDotfileWithExtras(func(path string) ([]byte, error) {
		return d.showFile(ctx, branch, path)
	}, data)
}

// queryUpstream runs step 4 of the pipeline: VCS packages resolve their
// upstream reference, bounded to vcsQueryConcurrency concurrent queries.
// Query errors are kept per plan so submitChange can skip the branch
// without a false positive.
func (d *Detector) queryUpstream(ctx context.Context, plans []*branchPlan) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, vcsQueryConcurrency)
	for _, p := range plans {
		if p.kind == vcs.None || p.upstreamURL == "" {
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(p *branchPlan) {
			defer wg.Done()
			defer func() { <-sem }()
			switch p.kind {
			case vcs.Git:
				p.upstreamRef, p.upstreamErr = vcs.GitHead(ctx, p.upstreamURL)
			case vcs.SVN:
				p.upstreamRef, p.upstreamErr = vcs.SVNRevision(ctx, p.upstreamURL)
			}
		}(p)
	}
	wg.Wait()
}

// submitChange runs steps 5-6 of the pipeline: compare the current hash
// and the upstream ref against the last successful-build records and
// submit a Change when either differs.
func (d *Detector) submitChange(ctx context.Context, p *branchPlan) {
	if p.upstreamErr != nil {
		d.logger.Warn("detect: upstream query failed, skipping", "branch", p.branch,
			"pkgbase", p.info.Pkgbase, "error", p.upstreamErr)
		return
	}

	var prev *db.Package
	got, err := d.store.GetPackageByBase(ctx, p.info.Pkgbase)
	switch {
	case err == nil:
		prev = got
	case errors.Is(err, db.ErrNotFound):
		prev = &db.Package{} // first build: everything differs
	default:
		d.logger.Warn("detect: cannot read package record, skipping", "branch", p.branch,
			"pkgbase", p.info.Pkgbase, "error", err)
		return
	}

	reason := ""
	if p.hash != prev.LastSrcinfoHash {
		reason = ReasonSrcinfo
	}
	if p.upstreamRef != prev.LastUpstreamRef {
		switch reason {
		case ReasonSrcinfo:
			reason = ReasonBoth
		default:
			reason = ReasonUpstream
		}
	}
	if reason == "" {
		return
	}

	c := Change{
		Package: Package{
			Pkgbase: p.info.Pkgbase,
			Branch:  p.branch,
			VCSKind: vcsKindName(p.kind),
			Arch:    archOf(p.info.Arch),
		},
		Maintainers: p.dotfile.Maintainers,
		Hooks:       p.dotfile.Hooks,
		Collect:     p.dotfile.Collect,
		UpstreamRef: p.upstreamRef,
		Reason:      reason,
	}
	if err := d.sink.Submit(ctx, c); err != nil {
		d.logger.Warn("detect: sink rejected change, skipping", "branch", p.branch,
			"pkgbase", p.info.Pkgbase, "error", err)
	}
}

// showFile reads one file from the branch tree via "git show".
func (d *Detector) showFile(ctx context.Context, branch, path string) ([]byte, error) {
	cmd := d.execCommand(ctx, "git", "-C", d.mirrorDir, "show", branch+":"+path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("detect: git show %s:%s: %w: %s", branch, path, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// vcsKindName renders a vcs.Kind as the packages.vcs_kind value.
func vcsKindName(k vcs.Kind) string {
	switch k {
	case vcs.Git:
		return "git"
	case vcs.SVN:
		return "svn"
	default:
		return ""
	}
}

// archOf picks the first architecture the package supports, defaulting to
// the only implemented architecture.
func archOf(arch []string) string {
	for _, a := range arch {
		if a != "" {
			return a
		}
	}
	return "x86_64"
}
