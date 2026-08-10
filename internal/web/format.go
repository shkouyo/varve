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

package web

import (
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// displayArch renders the stored architecture set ("x86_64",
// "aarch64|x86_64", "any") for display: "any" dominates, then x86_64,
// matching the farm's single display vocabulary. Exotic-only sets (no
// any, no x86_64) render as-is rather than lying about the build target.
func displayArch(set string) string {
	for _, elem := range strings.Split(set, "|") {
		if elem == "any" {
			return "any"
		}
	}
	for _, elem := range strings.Split(set, "|") {
		if elem == "x86_64" {
			return "x86_64"
		}
	}
	return set
}

// shortID truncates a long identifier (task uuid) to its first eight
// characters for table cells; build ids (16 hex) stay full.
func shortID(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8] + "…"
}

// humanSize renders a byte count in the binary unit set with a
// one-decimal mantissa ("512.0 B", "1.2 KiB"), for artifact and disk
// sizes.
func humanSize(n int64) string {
	const (
		kb = 1 << 10
		mb = 1 << 20
		gb = 1 << 30
	)
	switch {
	case n >= gb:
		return strconv.FormatFloat(float64(n)/gb, 'f', 1, 64) + " GiB"
	case n >= mb:
		return strconv.FormatFloat(float64(n)/mb, 'f', 1, 64) + " MiB"
	case n >= kb:
		return strconv.FormatFloat(float64(n)/kb, 'f', 1, 64) + " KiB"
	default:
		return strconv.FormatFloat(float64(n), 'f', 1, 64) + " B"
	}
}

// absTime renders a timestamp as local wall-clock time with second
// precision. Templates call it only after a {{if .StartedAt}}-style nil
// guard and render a "·" placeholder otherwise (aria-hidden, screen
// reader friendly), so the nil case is unreachable from templates.
func absTime(t *time.Time) string {
	return t.Local().Format("2006-01-02 15:04:05")
}

// formatRenderTime scales a render duration to its display unit: whole
// milliseconds for durations of 1ms or more ("12ms"), whole microseconds
// below that ("250µs"). A sub-microsecond duration truncates to zero
// microseconds, so it is reported as 1µs instead: time.Now has
// nanosecond resolution on every supported platform, and a bare "0µs"
// would read as a failed timer rather than a fast render.
func formatRenderTime(d time.Duration) string {
	if d >= time.Millisecond {
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	}
	us := d.Microseconds()
	if us < 1 {
		us = 1
	}
	return strconv.FormatInt(us, 10) + "µs"
}

// maxAURSummaryLen caps the rendered AUR push error so a wall of remote
// git/ssh output cannot overflow the metadata row.
const maxAURSummaryLen = 200

// aurErrorSummary condenses a recorded AUR push error into a safe,
// single-line display string for the package page. Push errors carry
// arbitrary remote output, so every control character is dropped, runs
// of whitespace collapse into one space and the result is capped (by
// runes) at maxAURSummaryLen with an ellipsis. html/template escapes the
// text for the page; the collapse keeps it a tidy one-liner whatever the
// remote side printed. The empty input renders empty, so the template
// gate stays the source of truth for whether the failure line appears.
func aurErrorSummary(err string) string {
	if err == "" {
		return ""
	}
	fields := strings.FieldsFunc(err, func(r rune) bool {
		return r < 0x20 || r == 0x7f || unicode.IsSpace(r)
	})
	s := strings.Join(fields, " ")
	if r := []rune(s); len(r) > maxAURSummaryLen {
		return string(r[:maxAURSummaryLen]) + "…"
	}
	return s
}

// pkgEpoch renders the package version with its epoch prefix
// ("e:epoch:pkgver-rel") when the backend row carries an Epoch field,
// else "". The field is landing in a parallel wave and may be a string
// or an int (0 meaning none), so reflection keeps the template valid
// whichever lands.
func pkgEpoch(pkg any) string {
	v := reflect.ValueOf(pkg)
	for v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return ""
	}
	f := v.FieldByName("Epoch")
	if !f.IsValid() {
		return ""
	}
	var epoch string
	switch f.Kind() {
	case reflect.String:
		epoch = f.String()
	case reflect.Int, reflect.Int32, reflect.Int64:
		if n := f.Int(); n > 0 {
			epoch = strconv.FormatInt(n, 10)
		}
	default:
		return ""
	}
	if epoch == "" {
		return ""
	}
	pkgver := v.FieldByName("Pkgver").String()
	pkgrel := v.FieldByName("Pkgrel").String()
	if pkgver == "" {
		return ""
	}
	if pkgrel != "" {
		return "e:" + epoch + ":" + pkgver + "-" + pkgrel
	}
	return "e:" + epoch + ":" + pkgver
}
