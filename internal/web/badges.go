// SPDX-License-Identifier: AGPL-3.0-or-later
//
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

import "html/template"

// funcs are the template helper functions registered on every template.
// Badge markup is generated here so the status colors stay consistent
// across pages; the markup is fully static apart from the status label.
var funcs = template.FuncMap{
	"buildBadge": buildBadge,
	"add":        func(a, b int) int { return a + b },
	"sub":        func(a, b int) int { return a - b },
}

// buildBadge renders the status pill for a build/task status. Color is
// paired with a status word so the badge never relies on color alone
// (WCAG 2.2 AA, no-JavaScript safe).
func buildBadge(status string) template.HTML {
	cls, icon := "bg-stone-200 text-stone-800", "M18 12H6"
	switch status {
	case "succeeded":
		cls, icon = "bg-green-100 text-green-900", "M8 12l2.5 2.5L16 9"
	case "failed":
		cls, icon = "bg-red-100 text-red-900", "M8 8l8 8M16 8l-8 8"
	case "cancelled":
		cls, icon = "bg-stone-200 text-stone-800", "M18 12H6"
	case "queued":
		cls, icon = "bg-amber-100 text-amber-900", "M12 6v6l4 2"
	case "assigned":
		cls, icon = "bg-indigo-100 text-indigo-900", "M12 6v6l4 2"
	case "running":
		cls, icon = "bg-blue-100 text-blue-900", "M12 6v6l4 2"
	}
	return template.HTML(`<span class="inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-semibold ` +
		cls + `"><svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="h-3.5 w-3.5" aria-hidden="true" focusable="false"><path d="` +
		icon + `"></path></svg>` + template.HTMLEscapeString(status) + `</span>`)
}
