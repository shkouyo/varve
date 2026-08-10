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
// branch commit and VCS upstream references per enabled branch and
// enqueues changes through the Sink interface. It also owns dotfile
// parsing and merging. The database is read-only here: the
// packages.last_commit / last_upstream_ref records are only updated
// after a successful build, which is what makes failed builds naturally
// re-queue on the next round (throttled by the failed-rebuild cooldown).
//
// Tests in this package swap package-level stubs (mirrorTimeout) and must
// not run t.Parallel.
package detect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"sort"
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

// upstreamQueryTimeout bounds one upstream VCS query (git ls-remote /
// svn info) so a hung remote can never hold a query slot forever; with
// all slots stuck, the whole poll round would stop. It is a variable so
// tests can shorten it, mirroring mirrorTimeout.
var upstreamQueryTimeout = 60 * time.Second

// Change reasons. ReasonManual is used by the admin rebuild
// path in dispatch, not by detect itself. ReasonSrcinfo is retained
// for callers that classify legacy .SRCINFO-driven changes; detect now
// triggers on branch commits and, for pkgbuild_source branches, on the
// external repository head (ReasonPkgbuild).
const (
	ReasonCommit   = "commit"
	ReasonSrcinfo  = "srcinfo"
	ReasonUpstream = "upstream"
	ReasonPkgbuild = "pkgbuild"
	ReasonBoth     = "commit+upstream"
	ReasonManual   = "manual"
)

// Package identifies one detected package within a source branch.
// VCSKind is "" for plain packages, "git" or "svn" otherwise. Arch is the
// canonical architecture set: "any" for architecture-independent
// packages, otherwise every .SRCINFO arch joined with "|" (claim matching
// treats any element as a match).
type Package struct {
	Pkgbase string
	Branch  string
	VCSKind string
	Arch    string
}

// Change is one detected package update handed to the Sink. UpstreamRef
// carries the upstream reference queried at detection time for VCS
// packages and is empty for plain packages; URL/Licenses/Conflicts/
// Provides/Pkgname/Source/Pkgver/Epoch/Pkgrel carry the .SRCINFO
// metadata recorded on the package row. For a pkgbuild_source branch
// PkgbuildSource points at the external repository and PkgbuildRef
// carries its head at detection time.
type Change struct {
	Package        Package
	Maintainers    []db.Maintainer
	URL            string
	Licenses       []string
	Conflicts      []string
	Provides       []string
	Pkgname        []string
	Source         []string
	Pkgver         string
	Pkgrel         string
	Epoch          int
	Hooks          Hooks
	Collect        Collect
	AUR            AURConfig
	UpstreamRef    string
	PkgbuildSource *PkgbuildSource // external PKGBUILD repo of a pkgbuild_source branch
	PkgbuildRef    string          // external repo head at detection time
	Reason         string
}

// Sink consumes detected changes; the dispatch module implements it.
// Implementations must be safe for concurrent Submit calls.
type Sink interface {
	// Submit enqueues one detected change for building.
	Submit(ctx context.Context, c Change) error
	// Remove cascades the removal of a package whose source branch
	// vanished from the mirror (rows, repository files and database
	// entries). A missing package is a no-op.
	Remove(ctx context.Context, pkgbase string) error
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

	// failedRebuildCooldown is the minimum wait before a package whose
	// last build failed is re-submitted without a source change (the
	// rebuild cooldown gate). Zero disables the gate.
	failedRebuildCooldown time.Duration
}

// execCommand is the command constructor used for every external git call;
// same-package tests may replace it with a recorder.
var execCommand = exec.CommandContext

// NewDetector builds a Detector for cfg and derives its mirror directory
// from the source URL. It performs no network operation.
// failedRebuildCooldown comes from the controller's worker settings.
func NewDetector(cfg *config.SourceConfig, store *db.Store, sink Sink, failedRebuildCooldown time.Duration) (*Detector, error) {
	return newDetector(cfg, store, sink, sourceRoot, failedRebuildCooldown)
}

// newDetector is NewDetector with an injectable mirror root (tests use a
// temp directory instead of /data/source).
func newDetector(cfg *config.SourceConfig, store *db.Store, sink Sink, root string, failedRebuildCooldown time.Duration) (*Detector, error) {
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
		cfg:                   cfg,
		store:                 store,
		sink:                  sink,
		mirrorDir:             filepath.Join(root, name+".git"),
		execCommand:           execCommand,
		now:                   time.Now,
		logger:                slog.Default(),
		failedRebuildCooldown: failedRebuildCooldown,
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
// enumeration, vanished-branch cascade removal and the per-branch
// pipeline. A mirror fetch failure aborts the round and is reported;
// per-branch problems only produce warnings so one bad branch never
// blocks the others.
func (d *Detector) PollOnce(ctx context.Context) error {
	if err := d.ensureMirror(ctx); err != nil {
		return err
	}
	branches, err := d.listBranches(ctx)
	if err != nil {
		return err
	}
	d.removeVanishedBranches(ctx, branches)

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

// removeVanishedBranches cascades the removal of every tracked package
// whose branch is no longer enumerated by the mirror (a branch deleted
// upstream disappears after the pruned fetch). Excluded branches are not
// enumerated either, so a package that moved into an exclude pattern is
// removed too, since exclusion means "stop serving this branch". Removal
// failures are warnings: the package row stays and the next round retries
// the idempotent cascade.
func (d *Detector) removeVanishedBranches(ctx context.Context, branches []string) {
	known := make(map[string]bool, len(branches))
	for _, b := range branches {
		known[b] = true
	}
	const pageSize = 1000
	for page := 1; ; page++ {
		pkgs, total, err := d.store.ListPackages(ctx, "", page, pageSize)
		if err != nil {
			d.logger.Warn("detect: cannot list packages for branch cleanup", "error", err)
			return
		}
		for _, p := range pkgs {
			if known[p.Branch] {
				continue
			}
			d.logger.Warn("detect: branch vanished, cascading removal", "branch", p.Branch, "pkgbase", p.Pkgbase)
			if err := d.sink.Remove(ctx, p.Pkgbase); err != nil {
				d.logger.Warn("detect: cascade removal failed", "branch", p.Branch, "pkgbase", p.Pkgbase, "error", err)
			}
		}
		if len(pkgs) == 0 || page*pageSize >= total {
			return
		}
	}
}

// branchPlan carries everything the pipeline learned about one branch so
// the upstream queries can overlap before the comparisons and submits
// happen in branch order.
type branchPlan struct {
	branch      string
	info        *srcinfo.Info
	commit      string
	dotfile     *Dotfile
	kind        vcs.Kind
	upstreamURL string
	upstreamRef string
	upstreamErr error
	pkgbuildRef string
}

// planBranch runs steps 1-3 of the per-branch pipeline: parse the dotfile
// (with extras) first, then read .SRCINFO either from the branch tree or,
// for a pkgbuild_source branch, from the external repository, snapshot the
// branch commit, resolve the external head and detect the VCS kind. It
// returns nil when the branch must be skipped with a warning.
func (d *Detector) planBranch(ctx context.Context, branch string) *branchPlan {
	// The dotfile comes first: a pkgbuild_source branch replaces the
	// branch tree as the PKGBUILD source, so its .SRCINFO is read from
	// the external repository instead.
	dotfile, err := d.parseDotfile(ctx, branch)
	if err != nil {
		d.logger.Warn("detect: invalid dotfile, skipping", "branch", branch, "error", err)
		return nil
	}
	var info *srcinfo.Info
	pkgbuildRef := ""
	if dotfile.PkgbuildSource != nil {
		data, err := d.pkgbuildFile(ctx, dotfile.PkgbuildSource, ".SRCINFO")
		if err != nil {
			d.logger.Warn("detect: cannot read external .SRCINFO, skipping", "branch", branch, "error", err)
			return nil
		}
		info, err = srcinfo.Parse(data)
		if err != nil {
			d.logger.Warn("detect: invalid external .SRCINFO, skipping", "branch", branch, "error", err)
			return nil
		}
		ref, err := d.pkgbuildHead(ctx, dotfile.PkgbuildSource)
		if err != nil {
			d.logger.Warn("detect: cannot resolve external head, skipping", "branch", branch, "error", err)
			return nil
		}
		pkgbuildRef = ref
	} else {
		data, err := d.showFile(ctx, branch, ".SRCINFO")
		if err != nil {
			d.logger.Warn("detect: branch has no .SRCINFO, skipping", "branch", branch, "error", err)
			return nil
		}
		info, err = srcinfo.Parse(data)
		if err != nil {
			d.logger.Warn("detect: invalid .SRCINFO, skipping", "branch", branch, "error", err)
			return nil
		}
	}
	commit, err := d.BranchSnapshot(ctx, branch)
	if err != nil {
		d.logger.Warn("detect: cannot snapshot branch commit, skipping", "branch", branch, "error", err)
		return nil
	}
	kind := vcs.DetectKind(info.Pkgbase, info.Pkgname, dotfile.VCS)
	p := &branchPlan{
		branch:      branch,
		info:        info,
		commit:      commit,
		dotfile:     dotfile,
		kind:        kind,
		pkgbuildRef: pkgbuildRef,
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
	// The fetch_key identity is shared by every upstream query; it is
	// resolved once per round (fetchKeyEnv stats the key file).
	env := fetchKeyEnv(d.cfg.FetchKey)
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
			qctx, cancel := context.WithTimeout(ctx, upstreamQueryTimeout)
			defer cancel()
			switch p.kind {
			case vcs.Git:
				p.upstreamRef, p.upstreamErr = vcs.GitHead(qctx, p.upstreamURL, env)
			case vcs.SVN:
				p.upstreamRef, p.upstreamErr = vcs.SVNRevision(qctx, p.upstreamURL, env)
			}
		}(p)
	}
	wg.Wait()
}

// submitChange runs steps 5-6 of the pipeline: compare the current branch
// commit, the upstream ref and (for pkgbuild_source branches) the external
// repository head against the last successful-build records and submit a
// Change when any of them differs. A package whose last build failed is
// additionally gated by the rebuild cooldown (see withinCooldown).
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
	if p.commit != prev.LastCommit {
		reason = ReasonCommit
	}
	if p.upstreamRef != prev.LastUpstreamRef {
		reason = mergeReason(reason, ReasonUpstream)
	}
	if p.pkgbuildRef != prev.PkgbuildRef {
		reason = mergeReason(reason, ReasonPkgbuild)
	}
	if reason == "" {
		return
	}
	if d.withinCooldown(ctx, prev, p) {
		d.logger.Info("detect: failed package inside rebuild cooldown, holding", "branch", p.branch,
			"pkgbase", p.info.Pkgbase)
		return
	}

	c := Change{
		Package: Package{
			Pkgbase: p.info.Pkgbase,
			Branch:  p.branch,
			VCSKind: vcsKindName(p.kind),
			Arch:    archSet(p.info.Arch),
		},
		Maintainers:    p.dotfile.Maintainers,
		URL:            p.info.URL,
		Licenses:       p.info.Licenses,
		Conflicts:      p.info.Conflicts,
		Provides:       p.info.Provides,
		Pkgname:        p.info.Pkgname,
		Source:         p.info.Source,
		Pkgver:         p.info.Pkgver,
		Pkgrel:         p.info.Pkgrel,
		Epoch:          p.info.Epoch,
		Hooks:          p.dotfile.Hooks,
		Collect:        p.dotfile.Collect,
		AUR:            p.dotfile.AUR,
		UpstreamRef:    p.upstreamRef,
		PkgbuildSource: p.dotfile.PkgbuildSource,
		PkgbuildRef:    p.pkgbuildRef,
		Reason:         reason,
	}
	if err := d.sink.Submit(ctx, c); err != nil {
		d.logger.Warn("detect: sink rejected change, skipping", "branch", p.branch,
			"pkgbase", p.info.Pkgbase, "error", err)
	}
}

// withinCooldown reports whether the change is the residue of a failed
// build still inside the rebuild cooldown: the package's last build
// failed (last_failed_at set) and the current snapshot still matches that
// failed build's records. A source change since the failure (a branch
// commit or upstream ref differing from the failed build's snapshot)
// bypasses the gate and is submitted immediately.
//
// A failed build never advances the package's last_commit /
// last_upstream_ref records (they only move on success), so the plain
// comparison above stays "changed" on every round; comparing against the
// failed build row itself separates that stale difference from a fresh
// source change. The latest build row is the failed one whenever
// last_failed_at is set: any success clears the marker.
func (d *Detector) withinCooldown(ctx context.Context, prev *db.Package, p *branchPlan) bool {
	if d.failedRebuildCooldown <= 0 || prev.LastFailedAt == nil {
		return false
	}
	if !d.now().Before(prev.LastFailedAt.Add(d.failedRebuildCooldown)) {
		return false // cooldown elapsed: submit again
	}
	latest, err := d.store.LatestBuildForPackage(ctx, prev.ID)
	if err != nil {
		d.logger.Warn("detect: cannot read latest build for cooldown check, holding", "branch", p.branch,
			"pkgbase", prev.Pkgbase, "error", err)
		return true // be conservative: hold until the cooldown ends
	}
	return p.commit == latest.Commit && p.upstreamRef == latest.UpstreamRef && p.pkgbuildRef == latest.PkgbuildRef
}

// mergeReason combines two change reasons into the canonical "+"-joined
// form ("commit+upstream", "commit+pkgbuild"), keeping the empty string
// when neither fired.
func mergeReason(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "+" + b
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

// archSet canonically renders the declared .SRCINFO architecture list
// into the packages.arch storage format: "any" (architecture-independent)
// dominates the set, otherwise the deduplicated elements are sorted and
// joined with "|" so every declared architecture is stored and matched at
// claim time, never just the first element. An empty declaration keeps
// the x86_64 deployment baseline.
func archSet(arch []string) string {
	seen := make(map[string]bool, len(arch))
	elems := make([]string, 0, len(arch))
	for _, a := range arch {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		if a == "any" {
			return "any"
		}
		elems = append(elems, a)
	}
	if len(elems) == 0 {
		return "x86_64"
	}
	sort.Strings(elems)
	return strings.Join(elems, "|")
}
