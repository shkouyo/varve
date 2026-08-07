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
	"database/sql"
	"testing"
	"time"
)

// seedFailedPackage inserts a packages row whose last build failed: the
// last-success records stay behind (so the current source still "differs")
// and last_failed_at marks the failure. A builds row with failedHash
// mirrors the failed build's own snapshot, which is what the cooldown
// compares against to separate a stale difference from a fresh change.
func seedFailedPackage(t *testing.T, dbPath, pkgbase, lastSuccessHash, failedHash string, failedAt time.Time) {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`INSERT INTO packages
		(pkgbase, branch, vcs_kind, arch, current_version, pkgdesc,
		 last_srcinfo_hash, last_upstream_ref, last_failed_at, maintainers)
		VALUES (?, '', '', 'x86_64', '', '', ?, '', ?, '[]')`,
		pkgbase, lastSuccessHash, failedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed package %s: %v", pkgbase, err)
	}
	if _, err := raw.Exec(`INSERT INTO builds
		(id, seq, package_id, branch, "commit", upstream_ref, srcinfo_hash, status,
		 worker_id, worker_name, log_path, started_at, finished_at, error, artifacts, resource_usage)
		VALUES ('failed-1', 1, 1, '', 'c', '', ?, 'failed',
		        NULL, '', '', NULL, NULL, '', '[]', '[]')`, failedHash); err != nil {
		t.Fatalf("seed failed build for %s: %v", pkgbase, err)
	}
}

// TestPollOnceRebuildCooldownHolds covers the cooldown gate: inside the
// window the failed build's residue is not re-submitted, and after the
// window elapses the same change is submitted again.
func TestPollOnceRebuildCooldownHolds(t *testing.T) {
	body := srcinfoBody("cool", "1.0", "1")
	src := newSourceRepo(t, "cool", map[string]string{".SRCINFO": body})
	store, dbPath := openStore(t)
	sink := &fakeSink{}
	seedFailedPackage(t, dbPath, "cool", "old-success-hash", hashOf(body),
		time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC))

	d := newTestDetector(t, "file://"+src, store, sink)
	d.failedRebuildCooldown = time.Hour
	inside := time.Date(2026, 8, 5, 0, 30, 0, 0, time.UTC)
	d.now = func() time.Time { return inside }

	// Inside the cooldown: the srcinfo difference is the failed build's
	// own, so the change is held.
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	assertChangeCount(t, sink, 0)

	// Past the cooldown: the stale difference is submitted again.
	d.now = func() time.Time { return inside.Add(2 * time.Hour) }
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	assertChangeCount(t, sink, 1)
}

// TestPollOnceRebuildCooldownBypass covers the bypass: a fresh source
// change inside the cooldown is submitted immediately, even though the
// last build failed.
func TestPollOnceRebuildCooldownBypass(t *testing.T) {
	src := newSourceRepo(t, "cool", map[string]string{".SRCINFO": srcinfoBody("cool", "1.0", "1")})
	store, dbPath := openStore(t)
	sink := &fakeSink{}
	seedFailedPackage(t, dbPath, "cool", "old-success-hash", hashOf(srcinfoBody("cool", "1.0", "1")),
		time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC))

	d := newTestDetector(t, "file://"+src, store, sink)
	d.failedRebuildCooldown = time.Hour
	d.now = func() time.Time { return time.Date(2026, 8, 5, 0, 30, 0, 0, time.UTC) }

	// The source bumps to 2.0 while the failed 1.0 build is still inside
	// the cooldown: the new hash differs from the failed build's snapshot,
	// so the change bypasses the gate.
	commitFiles(t, src, map[string]string{".SRCINFO": srcinfoBody("cool", "2.0", "1")}, "bump")

	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	changes := assertChangeCount(t, sink, 1)
	if changes[0].Reason != ReasonSrcinfo {
		t.Errorf("change = %+v, want a srcinfo change bypassing the cooldown", changes[0])
	}
}

// TestPollOnceRebuildCooldownClearedBySuccess covers the success side: a
// package with no last_failed_at marker (a success cleared it) is
// submitted normally even when the cooldown would otherwise be active.
func TestPollOnceRebuildCooldownClearedBySuccess(t *testing.T) {
	body := srcinfoBody("ok", "1.0", "1")
	src := newSourceRepo(t, "ok", map[string]string{".SRCINFO": body})
	store, dbPath := openStore(t)
	sink := &fakeSink{}
	// No last_failed_at: the package's last build succeeded.
	seedPackageRow(t, dbPath, "ok", "old-success-hash", "")

	d := newTestDetector(t, "file://"+src, store, sink)
	d.failedRebuildCooldown = time.Hour
	d.now = func() time.Time { return time.Date(2026, 8, 5, 0, 30, 0, 0, time.UTC) }

	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	assertChangeCount(t, sink, 1)
}

// TestPollOnceRebuildCooldownBoundary pins the expiry semantics of the
// cooldown gate: the window is half-open, [last_failed_at, last_failed_at
// + cooldown). The failed build's residue is held on every round inside
// the window, and the first round at or past the expiry submits again.
// This locks in the behavior seen live: a round five seconds past the
// expiry re-enqueued the package, which is correct expiry behavior rather
// than a missed hold.
func TestPollOnceRebuildCooldownBoundary(t *testing.T) {
	body := srcinfoBody("cool", "1.0", "1")
	src := newSourceRepo(t, "cool", map[string]string{".SRCINFO": body})
	const cooldown = time.Hour
	failedAt := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		now  time.Time
		want int // submitted changes
	}{
		{"deep inside window", failedAt.Add(cooldown / 2), 0},
		{"just before expiry", failedAt.Add(cooldown - time.Second), 0},
		{"exactly at expiry", failedAt.Add(cooldown), 1},
		{"just past expiry", failedAt.Add(cooldown + time.Second), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, dbPath := openStore(t)
			sink := &fakeSink{}
			seedFailedPackage(t, dbPath, "cool", "old-success-hash", hashOf(body), failedAt)
			d := newTestDetector(t, "file://"+src, store, sink)
			d.failedRebuildCooldown = cooldown
			d.now = func() time.Time { return tc.now }
			if err := d.PollOnce(context.Background()); err != nil {
				t.Fatalf("PollOnce: %v", err)
			}
			assertChangeCount(t, sink, tc.want)
		})
	}
}

// stampLastFailedAt rewrites the package's last_failed_at marker directly,
// simulating a re-enqueued build that failed again (the failure path
// stamps the marker in the same transaction as the build's finished_at).
func stampLastFailedAt(t *testing.T, dbPath string, at time.Time) {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE packages SET last_failed_at = ?`,
		at.UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("stamp last_failed_at: %v", err)
	}
}

// TestPollOnceRebuildCooldownRestartsOnNewFailure covers the restart
// semantics: when a re-enqueued build fails again, the failure path moves
// last_failed_at to the new failure and a fresh window starts from there.
// Without the move the residue would be re-submitted right after the old
// window, duplicating the same failed build; with it the gate keeps
// holding until the newer window elapses.
func TestPollOnceRebuildCooldownRestartsOnNewFailure(t *testing.T) {
	body := srcinfoBody("cool", "1.0", "1")
	src := newSourceRepo(t, "cool", map[string]string{".SRCINFO": body})
	store, dbPath := openStore(t)
	sink := &fakeSink{}
	failedAt := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	seedFailedPackage(t, dbPath, "cool", "old-success-hash", hashOf(body), failedAt)

	d := newTestDetector(t, "file://"+src, store, sink)
	d.failedRebuildCooldown = time.Hour
	d.now = func() time.Time { return failedAt.Add(30 * time.Minute) }
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	assertChangeCount(t, sink, 0)

	// The re-enqueued build fails 40 minutes in: the marker moves and the
	// round 30 minutes later (past the old expiry, inside the new window)
	// still holds.
	newer := failedAt.Add(40 * time.Minute)
	stampLastFailedAt(t, dbPath, newer)
	d.now = func() time.Time { return newer.Add(30 * time.Minute) }
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	assertChangeCount(t, sink, 0)

	// Past the restarted window the stale residue is finally submitted.
	d.now = func() time.Time { return newer.Add(2 * time.Hour) }
	if err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	assertChangeCount(t, sink, 1)
}
