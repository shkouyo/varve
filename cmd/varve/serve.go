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
// log reader, plus Submit (detect.Sink) and the concrete Stop used for
// graceful shutdown. dispatch.OrchestratorImpl satisfies it.
type orchestrator interface {
	dispatch.Orchestrator
	Submit(ctx context.Context, c detect.Change) error
	Stop()
}

// detector is the detection surface serve starts and stops: run until the
// context is cancelled. detect.Detector satisfies it.
type detector interface {
	Run(ctx context.Context) error
}

// httpServer is a started HTTP server: Close stops it. The default is
// *http.Server; tests substitute a recorder.
type httpServer interface {
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
	VerifyDetached(sigPath, pkgPath string) error
	ExportForTask(taskID string) (*sign.KeyMaterial, error)
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
	newDetector = func(cfg *config.SourceConfig, store *db.Store, sink detect.Sink) (detector, error) {
		return detect.NewDetector(cfg, store, sink)
	}

	// startServer binds addr, serves h and reports fatal serve errors on
	// errCh (a graceful Close never sends). Tests replace it with a
	// recorder that observes start/close order without sockets.
	startServer = func(addr string, h http.Handler, errCh chan<- error) (httpServer, error) {
		srv := &http.Server{Addr: addr, Handler: h}
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

	// waitSignal blocks until SIGTERM/SIGINT. Tests replace it to
	// trigger the shutdown path deterministically.
	waitSignal = func() error {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sig)
		s := <-sig
		log.Printf("varve: received %s, shutting down", s)
		return nil
	}
)

// runServe is the testable entry of the serve subcommand. args may carry
// the optional "--config <path>" pair (default /data/varve.toml). The ten
// startup steps run in order; any failure aborts startup. On
// SIGTERM/SIGINT the stack shuts down gracefully in the mandated order.
func runServe(args []string) error {
	path, err := configPath(args)
	if err != nil {
		return err
	}

	// 1. Configuration.
	cfg, err := config.LoadController(path)
	if err != nil {
		return err
	}

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

	// 4. GPG signer: "off" leaves the signerSurface nil — dispatch
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
	det, err := newDetector(&cfg.Source, store, orch)
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
	go func() { signalErr <- waitSignal() }()
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

	// Graceful shutdown order: stop the orchestrator (halts the
	// scheduler, drains the ingest mutex), stop detection, then close
	// both HTTP servers.
	orch.Stop()
	detCancel()
	<-detDone
	apiErr := apiSrv.Close()
	webErr := webSrv.Close()
	return errors.Join(apiErr, webErr)
}

// openStorage opens the configured artifact backend: the local
// filesystem or an S3-compatible object store.
func openStorage(cfg *config.StorageConfig) (storage.Backend, error) {
	if cfg.Backend == "s3" {
		return storage.OpenS3(cfg.S3)
	}
	return storage.OpenLocal(cfg.Local.Root)
}

// closeQuiet closes a server during error-unwind paths, where the primary
// serve failure is the report. Close errors are best-effort there.
func closeQuiet(s httpServer) {
	if s == nil {
		return
	}
	_ = s.Close() // best-effort during error unwind
}
