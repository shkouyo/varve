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
		(pkgbase, branch, vcs_kind, arch, enabled, current_version, pkgdesc,
		 last_srcinfo_hash, last_upstream_ref, last_failed_at, maintainers)
		VALUES (?, '', '', 'x86_64', 1, '', '', ?, '', ?, '[]')`,
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
