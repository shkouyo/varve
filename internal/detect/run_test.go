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

package detect

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// runScript builds the shared fake script for the Run tests: it records
// every invocation, fails the very first clone (to exercise round-level
// error handling) and serves a single package branch afterwards.
func runScript(record, counter string) string {
	return fmt.Sprintf(`n=0
if [ -f '%s' ]; then n=$(cat '%s'); fi
n=$((n+1))
echo "$n" > '%s'
echo "$*" >> '%s'
if [ "$2" = "clone" ] && [ "$n" -eq 1 ]; then
    echo "simulated clone failure" >&2
    exit 1
fi
case "$*" in
  *for-each-ref*) printf 'pkg\nmain\n' ;;
  *:SRCINFO) printf 'pkgbase = pkg\n\tpkgver = 1.0\n\tpkgrel = 1\n\tarch = x86_64\npkgname = pkg\n' ;;
esac
`, counter, counter, counter, record)
}

// TestRunPollsAndStops asserts Run keeps polling on a short interval,
// survives a failing round and exits cleanly when the context is
// cancelled (DETAIL §3.7 #10 / T3.7).
func TestRunPollsAndStops(t *testing.T) {
	record := filepath.Join(t.TempDir(), "calls")
	counter := filepath.Join(t.TempDir(), "count")
	store, _ := openStore(t)
	d := newTestDetector(t, "git@git.example.org:pkgbuilds.git", store, &fakeSink{})
	d.cfg.PollInterval = 10 * time.Millisecond
	d.execCommand = fakeExecScript(t, runScript(record, counter))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("Run returned too early (%v)", elapsed)
	}

	clones := countLines(t, record, "clone")
	if clones < 3 {
		t.Errorf("expected several poll rounds, saw %d clone calls", clones)
	}
}

// TestRunContinuesAfterRoundFailure asserts a failing round does not stop
// the loop: the first clone fails, later rounds succeed and the package
// is eventually submitted (DETAIL §3.4 #5).
func TestRunContinuesAfterRoundFailure(t *testing.T) {
	record := filepath.Join(t.TempDir(), "calls")
	counter := filepath.Join(t.TempDir(), "count")
	store, _ := openStore(t)
	sink := &fakeSink{}
	d := newTestDetector(t, "git@git.example.org:pkgbuilds.git", store, sink)
	d.cfg.PollInterval = 10 * time.Millisecond
	d.execCommand = fakeExecScript(t, runScript(record, counter))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	clones := countLines(t, record, "clone")
	if clones < 2 {
		t.Errorf("expected the loop to keep polling after the failed round, saw %d clones", clones)
	}
	// Each round re-enqueues the unrecorded package (decision A16), so
	// the eventual submission count proves later rounds completed.
	if got := sink.snapshot(); len(got) < 1 || got[0].Package.Pkgbase != "pkg" {
		t.Errorf("submissions = %+v, want at least one change for pkg", got)
	}
}
