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

// Package web serves the varve web UI: server-side rendered templates
// over html/template, a build-time Tailwind stylesheet embedded into the
// binary, a Basic Auth protected /admin area (no cookies), an
// Origin/Referer same-origin gate on every admin POST (CSRF defense for
// the auto-attached Basic credentials), and an SSE log stream on
// GET /builds/{id}/log with a no-JavaScript meta-refresh fallback. The
// UI is fully usable without JavaScript: every admin action is a plain
// form POST and every page renders semantic, keyboard navigable markup
// (WCAG 2.2 AA).
package web

import (
	"bytes"
	"context"
	"embed"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"time"

	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/dispatch"
)

// LogReader is the build-log interface consumed by the web UI and
// implemented by dispatch.OrchestratorImpl.
type LogReader interface {
	// ReadLog returns the full log of a build. ErrNotFound when the log
	// does not exist.
	ReadLog(ctx context.Context, buildID string) ([]byte, error)
	// TailLog streams the log bytes from offset onwards into w and
	// returns the new offset. ErrNotFound when the log does not exist.
	TailLog(ctx context.Context, buildID string, offset int64, w io.Writer) (int64, error)
}

// The stylesheet is compiled from static/input.css at generate time
// (tailwindcss 4.3.3 on this machine). app.css is a build artifact and
// is gitignored; it must exist before the package compiles, which `go
// generate ./...` guarantees in CI.
//go:generate tailwindcss -i static/input.css -o static/app.css --minify

//go:embed templates
var templatesFS embed.FS

//go:embed static/app.css
var appCSS []byte

//go:embed copying.txt
var copyingText []byte

// sanitizeCSS drops the tailwind version banner comment from the embedded
// stylesheet. The banner is harmless but references a website; keeping it
// out keeps every page free of external URL mentions.
func sanitizeCSS(css []byte) []byte {
	if len(css) >= 3 && string(css[:3]) == "/*!" {
		if i := bytes.Index(css, []byte("*/")); i >= 0 {
			css = css[i+2:]
		}
	}
	return css
}

// Server hosts the whole web UI. Handlers are stateless and safe for
// concurrent use; the template set and the compiled stylesheet are
// read-only after New.
type Server struct {
	cfg   *config.ControllerConfig
	orch  dispatch.Orchestrator
	store *db.Store
	logs  LogReader

	tmpl *template.Template
	css  []byte

	// pingInterval is the SSE keep-alive comment interval (2s). Tests
	// shorten it to keep SSE runs fast.
	pingInterval time.Duration
}

// New builds a web server over the controller configuration, the
// orchestrator, the read-only store and the log reader.
func New(cfg *config.ControllerConfig, orch dispatch.Orchestrator, store *db.Store, logs LogReader) *Server {
	return &Server{
		cfg:          cfg,
		orch:         orch,
		store:        store,
		logs:         logs,
		tmpl:         template.Must(template.New("").Funcs(funcs).ParseFS(templatesFS, "templates/*.html")),
		css:          sanitizeCSS(appCSS),
		pingInterval: 2 * time.Second,
	}
}

// Handler returns the full web route table. The /admin subtree is gated
// by Basic Auth and every admin POST additionally by the same-origin
// check; every admin action is a plain form POST so the UI works without
// JavaScript. GET /admin redirects to the merged dashboard page (admin
// content renders there for authenticated requests) and
// GET /admin/logout forces a 401 so the browser drops its saved
// credentials.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /builds", s.handleBuilds)
	mux.HandleFunc("GET /builds/{id}", s.handleBuild)
	mux.HandleFunc("GET /builds/{id}/log", s.handleLog)
	mux.HandleFunc("GET /packages", s.handlePackages)
	mux.HandleFunc("GET /packages/{pkgbase}", s.handlePackage)
	mux.HandleFunc("GET /copying.txt", s.handleCopying)

	mux.HandleFunc("GET /admin", s.requireAuth(s.handleAdmin))
	mux.HandleFunc("GET /admin/logout", s.handleLogout)
	mux.HandleFunc("POST /admin/packages/{pkgbase}/rebuild", s.requireAuth(s.requireSameOrigin(s.handleAdminRebuild)))
	mux.HandleFunc("POST /admin/tasks/{id}/cancel", s.requireAuth(s.requireSameOrigin(s.handleAdminCancel)))
	mux.HandleFunc("POST /admin/workers/{name}/disable", s.requireAuth(s.requireSameOrigin(s.handleAdminDisable)))
	mux.HandleFunc("POST /admin/workers/{name}/enable", s.requireAuth(s.requireSameOrigin(s.handleAdminEnable)))
	mux.HandleFunc("POST /admin/workers/{name}/remove", s.requireAuth(s.requireSameOrigin(s.handleAdminRemove)))
	mux.HandleFunc("GET /admin/builds", s.requireAuth(s.handleAdminBuilds))
	return mux
}

// base carries the fields every page template renders: the page title,
// the inlined compiled stylesheet and an optional redirect-back flash
// message (admin actions).
type base struct {
	Title string
	CSS   template.CSS
	Flash *flash
	// Nav names the active section for the header navigation
	// (aria-current), or "" when none applies.
	Nav string
}

// flash is a one-shot message carried through the redirect query string;
// there are no cookies on the web UI.
type flash struct {
	Kind    string // "error" or "ok"
	Message string
}

// page builds the base fields shared by every page.
func (s *Server) page(title string, f *flash) base {
	return base{Title: title, CSS: template.CSS(s.css), Flash: f}
}

// render executes a named template with the page data. The stylesheet is
// inlined via the base struct, so the page issues no external requests
// and the fixed route table needs no /static route.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		// The body is already partially written; emit a plain fallback.
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// errorData feeds error.html (404/401/500 pages).
type errorData struct {
	base
	Status  int
	Message string
}

// renderError writes a full error page with the given status. The
// template itself depends on no JavaScript.
func (s *Server) renderError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, "error.html", errorData{
		base:    s.page(http.StatusText(status), nil),
		Status:  status,
		Message: message,
	}); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// buildIDLen is the fixed width of a build id (16 lowercase hex
// characters), mirroring the store's id generator.
const buildIDLen = 16

// handleCopying serves the embedded license text. The asset is a mirror
// of the repository-root COPYING file; keep both in sync when the license
// text changes.
func (s *Server) handleCopying(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(copyingText)
}

// parseID validates a route path value as a build id: exactly 16
// lowercase hex characters, the fixed shape the store generates.
func parseID(raw string) (string, bool) {
	if len(raw) != buildIDLen {
		return "", false
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return "", false
		}
	}
	return raw, true
}

// parsePage parses a ?page= query value. An absent value yields page 1;
// a present value must be a positive integer (ok=false otherwise, mapped
// to a 400 by the callers).
func parsePage(raw string) (int, bool) {
	if raw == "" {
		return 1, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// maxQueryLen caps the length of user-supplied search terms and package
// names so a hostile query cannot push unbounded data into the store.
const maxQueryLen = 200

// validPkgbase reports whether raw is a well-formed package base name:
// letters, digits and the AUR separator set (@ . _ + -), up to
// maxQueryLen bytes.
func validPkgbase(raw string) bool {
	if raw == "" || len(raw) > maxQueryLen {
		return false
	}
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '@' || c == '.' || c == '_' || c == '+' || c == '-':
		default:
			return false
		}
	}
	return true
}

// isTerminalStatus reports whether a build status is final. Terminal
// builds have no more log increments coming.
func isTerminalStatus(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled":
		return true
	}
	return false
}
