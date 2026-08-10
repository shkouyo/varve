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

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"git.0x0f.dev/varve/internal/api"
	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/detect"
	"git.0x0f.dev/varve/internal/dispatch"
	"git.0x0f.dev/varve/internal/mail"
	"git.0x0f.dev/varve/internal/repo"
	"git.0x0f.dev/varve/internal/sign"
	"git.0x0f.dev/varve/internal/storage"
	"git.0x0f.dev/varve/internal/web"
)

// orchestrator is the orchestration surface the serve path needs: the full
// Orchestrator contract consumed by the API server, the web server and the
// log reader, plus the detect.Sink methods (Submit, Remove) and the
// concrete Stop used for graceful shutdown. dispatch.OrchestratorImpl
// satisfies it.
type orchestrator interface {
	dispatch.Orchestrator
	Submit(ctx context.Context, c detect.Change) error
	Remove(ctx context.Context, pkgbase string) error
	Stop()
}

// detector is the detection surface serve starts and stops: run until the
// context is cancelled. detect.Detector satisfies it.
type detector interface {
	Run(ctx context.Context) error
}

// httpServer is a started HTTP server: Shutdown drains it within the
// given context, Close stops it immediately. The default is
// *http.Server; tests substitute a recorder.
type httpServer interface {
	Shutdown(ctx context.Context) error
	Close() error
}

// signerSurface is the signing surface the serve path threads into the
// repo updater (GnuPGEnv) and the orchestrator (the dispatch
// signVerifier methods); *sign.Signer satisfies it. The variable is
// declared with this interface type so a disabled signer
// (repo.sign="off") stays a true nil: a nil *sign.Signer stored inside
// an interface would be a non-nil interface wrapping a nil pointer, which
// defeats dispatch's nil checks and crashes task finalization.
type signerSurface interface {
	VerifyDetached(ctx context.Context, sigPath, pkgPath string) error
	ExportForTask(ctx context.Context, taskID string) (*sign.KeyMaterial, error)
	ClearTask(taskID string)
	GnuPGEnv() []string
}

// Injectables for the serve tests, following the pattern of
// cmd/varve-worker's runner constructors: runServe calls these package
// variables instead of the module constructors directly, so tests can
// replace them with recorders.
var (
	// newSigner prepares the GPG signer when cfg.Repo.Sign != "off".
	newSigner = sign.NewSigner

	// newOrchestrator builds the orchestration core (step 6); its
	// scheduler goroutine starts immediately and Stop halts it. The
	// signer travels as the signerSurface interface so a disabled
	// signer reaches dispatch as a true nil.
	newOrchestrator = func(cfg *config.ControllerConfig, store *db.Store, backend storage.Backend,
		signer signerSurface, updater repo.Updater, notifier mail.Notifier, logs *dispatch.Logs) orchestrator {
		return dispatch.NewOrchestrator(cfg, store, backend, signer, updater, notifier, logs)
	}

	// newDetector builds the source poller (step 8). Tests substitute a
	// recorder so no git mirror is touched.
	newDetector = func(cfg *config.SourceConfig, store *db.Store, sink detect.Sink, cooldown time.Duration) (detector, error) {
		return detect.NewDetector(cfg, store, sink, cooldown)
	}

	// startServer binds addr, serves h and reports fatal serve errors on
	// errCh (a graceful Close never sends). Tests replace it with a
	// recorder that observes start/close order without sockets.
	startServer = func(addr string, h http.Handler, errCh chan<- error) (httpServer, error) {
		srv := newHTTPServer(addr, h)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
		return srv, nil
	}

	// waitSignal blocks until the first SIGTERM/SIGINT arrives on sig.
	// Tests replace it to trigger the shutdown path deterministically;
	// the real implementation only reads the channel, because the
	// signal.Notify registration is owned by runServe.
	waitSignal = func(sig <-chan os.Signal) error {
		s := <-sig
		log.Printf("varve: received %s, shutting down", s)
		return nil
	}

	// forceExit replaces os.Exit so the second-signal force-exit path is
	// testable without killing the test binary.
	forceExit = os.Exit
)

// shutdownTimeout is the whole graceful-shutdown budget: each HTTP
// server drains within a context of this length (then falls back to
// Close) and the stop sequence forces an exit when the same budget
// elapses, so a stuck external call cannot hang the process forever.
// Tests may shorten it.
var shutdownTimeout = 30 * time.Second

// newHTTPServer builds an http.Server with the hardening settings shared
// by both ports: request headers must arrive within 5s (slowloris is cut
// off), header blocks over 1MiB are rejected and idle keep-alive
// connections are reaped after 60s. Write/ReadTimeout stay unset on
// purpose: the web port serves SSE streams and the API port accepts
// multi-gigabyte uploads, both of which need unbounded reads and writes.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB
	}
}

// runServe is the testable entry of the serve subcommand. args may carry
// the optional "--config <path>" pair (default /data/varve.toml). The ten
// startup steps run in order; any failure aborts startup. On
// SIGTERM/SIGINT the stack shuts down gracefully in the mandated order.
func runServe(args []string) error {
	// Signals are registered before any startup step: config loading, db
	// migration and gpg key handling can take seconds, and a SIGTERM
	// during them must reach the shutdown path instead of the default
	// hard kill. The registration stays active until runServe returns.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	path, err := configPath(args)
	if err != nil {
		return err
	}

	// 1. Configuration.
	cfg, err := config.LoadController(path)
	if err != nil {
		return err
	}

	// 1b. Process mutual exclusion with rebuild-index: the lock is held
	// for the whole lifetime of serve, so a rebuild against a running
	// controller is rejected up front instead of clearing its tasks.
	release, err := acquireLock(cfg.Database.Path + ".lock")
	if err != nil {
		return fmt.Errorf("varve: database is locked by another varve process (serve or rebuild-index); cannot start: %w", err)
	}
	defer release()

	// 2. Database with migrations (step 2).
	store, err := db.Open(cfg.Database.Path)
	if err != nil {
		return err
	}
	defer store.Close()

	// 3. Artifact storage backend (step 3).
	backend, err := openStorage(&cfg.Storage)
	if err != nil {
		return err
	}

	// 4. GPG signer: "off" leaves the signerSurface nil. Dispatch
	// and repo branch on cfg.Repo.Sign and never touch the signer
	// then. The interface type keeps the nil a true nil: a typed nil
	// *sign.Signer in the interface would defeat dispatch's checks and
	// crash task finalization.
	var signer signerSurface
	if cfg.Repo.Sign != "off" {
		signer, err = newSigner(&cfg.GPG)
		if err != nil {
			return err
		}
	}

	// 5. Modules without side effects at construction (step 5).
	updater := repo.NewUpdater(cfg, backend, signer, nil)
	notifier := mail.NewMailer(&cfg.Mail)
	logs := dispatch.NewLogs(cfg.Logs.Dir)

	// 6. Orchestration core (step 6); its scheduler starts immediately.
	orch := newOrchestrator(cfg, store, backend, signer, updater, notifier, logs)

	// 7. Startup validation: a conflicted repository refuses to serve.
	if err := orch.ValidateConflicts(context.Background()); err != nil {
		orch.Stop()
		return err
	}

	// 8. Detection poller (step 8).
	det, err := newDetector(&cfg.Source, store, orch, cfg.Worker.FailedRebuildCooldown)
	if err != nil {
		orch.Stop()
		return err
	}
	detCtx, detCancel := context.WithCancel(context.Background())
	detDone := make(chan struct{})
	go func() {
		defer close(detDone)
		_ = det.Run(detCtx)
	}()

	// 9. HTTP servers: the worker API on the API port, the web UI on
	// the web port. The web server receives the same orchestrator
	// instance as its LogReader.
	apiHandler := api.NewServer(cfg, orch).Handler()
	webHandler := web.New(cfg, orch, store, orch).Handler()

	serveErr := make(chan error, 2)
	apiSrv, err := startServer(cfg.Server.APIPort, apiHandler, serveErr)
	if err != nil {
		orch.Stop()
		detCancel()
		<-detDone
		return fmt.Errorf("varve: api server on %s: %w", cfg.Server.APIPort, err)
	}
	webSrv, err := startServer(cfg.Server.WebPort, webHandler, serveErr)
	if err != nil {
		orch.Stop()
		detCancel()
		<-detDone
		closeQuiet(apiSrv)
		return fmt.Errorf("varve: web server on %s: %w", cfg.Server.WebPort, err)
	}

	// 10. Wait for a signal or a fatal serve error (step 10).
	signalErr := make(chan error, 1)
	go func() { signalErr <- waitSignal(sigCh) }()
	select {
	case serr := <-serveErr:
		orch.Stop()
		detCancel()
		<-detDone
		closeQuiet(apiSrv)
		closeQuiet(webSrv)
		return fmt.Errorf("varve: http server: %w", serr)
	case err := <-signalErr:
		if err != nil {
			orch.Stop()
			detCancel()
			<-detDone
			closeQuiet(apiSrv)
			closeQuiet(webSrv)
			return err
		}
	}

	// A second signal during shutdown forces an immediate exit: the
	// operator's second SIGTERM/SIGINT must always work, even when a
	// shutdown step is stuck.
	go func() {
		if s, ok := <-sigCh; ok {
			log.Printf("varve: received second signal %v, forcing exit", s)
			forceExit(1)
		}
	}()

	// Graceful shutdown order: stop the orchestrator (halts the
	// scheduler, drains the ingest mutex), stop detection, then drain
	// both HTTP servers with Shutdown. The whole sequence is bounded by
	// shutdownTimeout; a stuck step forces an exit instead of hanging
	// the process forever.
	done := make(chan error, 1)
	go func() {
		orch.Stop()
		detCancel()
		<-detDone
		done <- errors.Join(shutdownServer(apiSrv), shutdownServer(webSrv))
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(shutdownTimeout):
		log.Printf("varve: graceful shutdown timed out after %s, forcing exit", shutdownTimeout)
		forceExit(1)
		return nil // unreachable: forceExit never returns
	}
}

// shutdownServer drains one HTTP server within the shutdown budget and
// falls back to Close when the drain cannot finish in time, so in-flight
// SSE streams and uploads get the budget to finish instead of being cut
// off by Close immediately.
func shutdownServer(s httpServer) error {
	if s == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		return errors.Join(err, s.Close())
	}
	return nil
}

// openStorage opens the configured artifact backend: the local
// filesystem or an S3-compatible object store.
func openStorage(cfg *config.StorageConfig) (storage.Backend, error) {
	if cfg.Backend == "s3" {
		return storage.OpenS3(cfg.S3)
	}
	return storage.OpenLocal(cfg.Local.Root, cfg.Local.StagingDir)
}

// closeQuiet closes a server during error-unwind paths, where the primary
// serve failure is the report. Close errors are best-effort there.
func closeQuiet(s httpServer) {
	if s == nil {
		return
	}
	_ = s.Close() // best-effort during error unwind
}
