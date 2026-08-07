-- Migration 007: packages gains the .SRCINFO epoch prefix. pacman
-- versions are [epoch:]pkgver-pkgrel; the epoch is a non-negative
-- integer that sorts before every pkgver and is displayed with the
-- "epoch:" prefix. The column is additive with a default, so a plain
-- ALTER TABLE migrates existing rows in place; no table rebuild is
-- required.

ALTER TABLE packages ADD COLUMN epoch INTEGER NOT NULL DEFAULT 0;
