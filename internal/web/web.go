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
// the auto-attached Basic credentials), and a resumable SSE log stream
// on GET /builds/{id}/log/stream. The build log is merged into the
// build detail page; the legacy /builds/{id}/log URL redirects to its
// anchor. The UI is fully usable without JavaScript: every admin action
// is a plain form POST and every page renders semantic, keyboard
// navigable markup (WCAG 2.2 AA).
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

//go:embed COPYING.txt
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
// credentials. Two middlewares wrap the table: URL normalization (301
// to the canonical slashless path) and the minimal security header set
// on every response.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /builds", s.handleBuilds)
	mux.HandleFunc("GET /builds/{id}", s.handleBuild)
	mux.HandleFunc("GET /builds/{id}/log", s.handleLog)
	mux.HandleFunc("GET /builds/{id}/log/stream", s.handleLogStream)
	mux.HandleFunc("GET /packages", s.handlePackages)
	mux.HandleFunc("GET /packages/{pkgbase}", s.handlePackage)
	mux.HandleFunc("GET /COPYING.txt", s.handleCopying)
	mux.HandleFunc("GET /copying.txt", s.handleCopyingLegacy)
	mux.HandleFunc("GET /favicon.svg", s.handleFavicon)
	mux.HandleFunc("GET /favicon.ico", s.handleFaviconLegacy)

	mux.HandleFunc("GET /admin", s.requireAuth(s.handleAdmin))
	mux.HandleFunc("GET /admin/logout", s.handleLogout)
	mux.HandleFunc("POST /admin/packages/{pkgbase}/rebuild", s.requireAuth(s.requireSameOrigin(s.handleAdminRebuild)))
	mux.HandleFunc("POST /admin/tasks/{id}/cancel", s.requireAuth(s.requireSameOrigin(s.handleAdminCancel)))
	mux.HandleFunc("POST /admin/workers/{name}/disable", s.requireAuth(s.requireSameOrigin(s.handleAdminDisable)))
	mux.HandleFunc("POST /admin/workers/{name}/enable", s.requireAuth(s.requireSameOrigin(s.handleAdminEnable)))
	mux.HandleFunc("POST /admin/workers/{name}/remove", s.requireAuth(s.requireSameOrigin(s.handleAdminRemove)))
	mux.HandleFunc("GET /admin/builds", s.requireAuth(s.handleAdminBuilds))
	return securityHeaders(stripTrailingSlash(mux))
}

// base carries the fields every page template renders: the page title,
// the inlined compiled stylesheet, an optional redirect-back flash
// message (admin actions), the signed-in admin (empty when anonymous)
// and the auto-refresh cadence. RefreshSeconds is 10 on every page
// except the merged log section of a terminal build, which must not
// refresh anymore; PageActive marks a build page whose build is still
// in progress. The template renders the meta refresh inside <noscript>
// so JavaScript users are never reloaded (the build page streams live
// log increments over SSE instead).
type base struct {
	Title string
	CSS   template.CSS
	Flash *flash
	// Nav names the active section for the header navigation
	// (aria-current), or "" when none applies.
	Nav            string
	User           string // authenticated admin username, "" when anonymous
	LoggedIn       bool
	PageActive     bool
	RefreshSeconds int
}

// flash is a one-shot message carried through the redirect query string;
// there are no cookies on the web UI.
type flash struct {
	Kind    string // "error" or "ok"
	Message string
}

// page builds the base fields shared by every page, resolving the
// signed-in admin for the header chrome (username plus logout link).
func (s *Server) page(r *http.Request, title string, f *flash) base {
	b := base{Title: title, CSS: template.CSS(s.css), Flash: f, RefreshSeconds: 10}
	if user, _, ok := r.BasicAuth(); ok && s.authorized(r) {
		b.User = user
		b.LoggedIn = true
	}
	return b
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
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, "error.html", errorData{
		base:    s.page(r, http.StatusText(status), nil),
		Status:  status,
		Message: message,
	}); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// buildIDLen is the fixed width of a build id (16 lowercase hex
// characters), mirroring the store's id generator.
const buildIDLen = 16

// faviconSVG is the embedded site icon: three sediment strata on a slate
// square, a nod to the varve naming (annual lake sediment layers).
const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" rx="12" fill="#334155"/><rect x="12" y="16" width="40" height="8" rx="2" fill="#64748b"/><rect x="12" y="28" width="40" height="8" rx="2" fill="#94a3b8"/><rect x="12" y="40" width="40" height="8" rx="2" fill="#cbd5e1"/></svg>`

// handleCopying serves the embedded license text. The asset is a verbatim
// mirror of the repository-root COPYING file; keep both in sync when the
// license text changes.
func (s *Server) handleCopying(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(copyingText)
}

// handleCopyingLegacy redirects the old lowercase license path to its
// canonical spelling, permanently, so existing links keep working.
func (s *Server) handleCopyingLegacy(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/COPYING.txt", http.StatusMovedPermanently)
}

// handleFavicon serves the embedded SVG icon.
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Write([]byte(faviconSVG))
}

// handleFaviconLegacy redirects the traditional .ico path to the SVG
// icon so browsers that request it by convention land on the same asset.
func (s *Server) handleFaviconLegacy(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/favicon.svg", http.StatusFound)
}

// securityHeaders hardens every response with the minimal header set:
// frame embedding is denied outright and content-type sniffing is
// disabled, so mixed content cannot be reinterpreted.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// stripTrailingSlash normalizes URLs: every non-root path ending in "/"
// redirects 301 to the canonical slashless form, preserving the query
// string. Only GET/HEAD requests redirect; a form POST to a trailing-
// slash URL must keep its method instead of being silently converted to
// GET by the client.
func stripTrailingSlash(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Path; len(p) > 1 && p[len(p)-1] == '/' {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				u := *r.URL
				u.Path = p[:len(p)-1]
				http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
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

// parsePage parses a ?page= value, clamping malformed and negative
// values to page 1 (invalid input never 400s; oversized values clamp to
// the last page once the total is known).
func parsePage(raw string) int {
	if raw == "" {
		return 1
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 1
	}
	return n
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
