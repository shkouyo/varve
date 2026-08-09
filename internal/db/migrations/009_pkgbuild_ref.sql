-- Migration 009: external PKGBUILD source records.
--
-- A branch whose dotfile sets [pkgbuild_source] builds from an external
-- repository: the branch tree carries only the dotfile while the actual
-- PKGBUILD and .SRCINFO live in the external repo. Detection then triggers
-- on the branch commit OR the external repository head, and records both
-- after a successful build.
--
-- packages.pkgbuild_ref holds the external repository head of the last
-- successful build (the sibling of last_commit / last_upstream_ref; it only
-- advances on success, so a failed build keeps re-queuing). builds.pkgbuild_ref
-- carries the dispatched external head from enqueue time, mirroring how the
-- build row already snapshots commit / upstream_ref / srcinfo_hash.
--
-- The columns are additive with empty defaults (the empty value means "no
-- external source", matching last_upstream_ref), so a plain ALTER TABLE
-- migrates existing rows in place.
--
ALTER TABLE packages ADD COLUMN pkgbuild_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE builds ADD COLUMN pkgbuild_ref TEXT NOT NULL DEFAULT '';
