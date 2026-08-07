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

// Package dispatch implements the controller's orchestration core: the
// task queue and scheduler, the task state machine, stall recovery,
// cancellation, worker management, artifact verification and ingest
// orchestration, failure notifications and build log storage. It is the
// single implementation of detect.Sink and exposes the Orchestrator
// interface consumed by the API server and the web UI.
package dispatch

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"git.0x0f.dev/varve/internal/config"
	"git.0x0f.dev/varve/internal/db"
	"git.0x0f.dev/varve/internal/detect"
	"git.0x0f.dev/varve/internal/mail"
	"git.0x0f.dev/varve/internal/repo"
	"git.0x0f.dev/varve/internal/sign"
	"git.0x0f.dev/varve/internal/storage"
)

// Sentinel errors returned by the orchestrator; the API layer maps them
// to HTTP 403/404/409.
var (
	// ErrNotFound reports a missing task, worker, package or log.
	ErrNotFound = errors.New("dispatch: not found")
	// ErrForbidden reports a claim-token mismatch or an unknown task token.
	ErrForbidden = errors.New("dispatch: forbidden")
	// ErrConflict reports a state or name conflict (duplicate result,
	// offset mismatch, cancellation late report, worker with active tasks).
	ErrConflict = errors.New("dispatch: conflict")
	// ErrArchUnsupported reports a change whose declared architectures
	// have no intersection with the architectures the deployment can
	// build; the package is skipped instead of queuing forever.
	ErrArchUnsupported = errors.New("dispatch: unsupported architecture")
)

// OffsetError wraps ErrConflict with the current server-side offset so
// the API layer can include it in the 409 body of resumable log and file
// uploads.
type OffsetError struct {
	Current int64
}

// Error implements error.
func (e *OffsetError) Error() string {
	return fmt.Sprintf("dispatch: offset mismatch: server offset is %d", e.Current)
}

// Unwrap exposes the underlying sentinel for errors.Is.
func (e *OffsetError) Unwrap() error { return ErrConflict }

// sourceArchiveName is the staged source snapshot produced by Enqueue in
// archive mode ("staging/<task-id>/source.tar.zst").
const sourceArchiveName = "source.tar.zst"

// sourceMirrorRoot is the controller-side mirror directory; it must
// match detect's own layout (detect sourceRoot).
const sourceMirrorRoot = "/data/source"

// signVerifier is the minimum signing surface consumed by dispatch
// (interfaces are defined by the consumer). The *sign.Signer satisfies
// it.
type signVerifier interface {
	VerifyDetached(sigPath, pkgPath string) error
	ExportForTask(taskID string) (*sign.KeyMaterial, error)
	ClearTask(taskID string)
}

// Compile-time assertions: the orchestrator exposes the Orchestrator
// contract to the API/web modules and implements detect.Sink for the
// detection pipeline.
var (
	_ Orchestrator = (*OrchestratorImpl)(nil)
	_ detect.Sink  = (*OrchestratorImpl)(nil)
)

// Orchestrator is the controller's orchestration surface consumed by the
// API server and the web UI. The detect module consumes a narrower slice
// of it through detect.Sink (Submit). All methods are safe for concurrent
// use: writes serialize on the store's single write connection and the
// shared in-memory state (token cache, ingest mutex, per-build log locks)
// is synchronized internally.
type Orchestrator interface {
	// detect → dispatch (implements detect.Sink).
	Enqueue(ctx context.Context, c detect.Change, force bool) error // force=true skips the name-conflict comparison (admin rebuild)
	// worker protocol (api server calls).
	Register(ctx context.Context, reg RegisterReq) (*RegisterResp, error)
	Heartbeat(ctx context.Context, hb HeartbeatReq) (*HeartbeatResp, error) // response carries cancellation signals
	Poll(ctx context.Context, poll PollReq) (*PollResp, error)              // FIFO claim; doubles as a heartbeat
	GetTask(ctx context.Context, taskID, token string) (*TaskDetail, error)
	AppendLog(ctx context.Context, taskID, token string, seg LogSegment) (*LogAck, error)
	ReportResult(ctx context.Context, taskID, token string, res ResultReq) error
	IssueSigningKey(ctx context.Context, taskID, token string) (*sign.KeyMaterial, error)
	UploadFile(ctx context.Context, taskID, token, name string, r io.Reader, size, offset int64) (*FileMeta, error)
	DownloadFile(ctx context.Context, taskID, token, name string) (io.ReadCloser, error)
	Deregister(ctx context.Context, name string) error
	// admin (web calls).
	CancelTask(ctx context.Context, taskID string) error
	RebuildPackage(ctx context.Context, pkgbase string) error
	DisableWorker(ctx context.Context, name string) error
	EnableWorker(ctx context.Context, name string) error
	RemoveWorker(ctx context.Context, name string) error
	// dashboard and logs (web consumes; the log reader interface is
	// defined by web and implemented here).
	Stats(ctx context.Context) (*Stats, error)
	ValidateConflicts(ctx context.Context) error
	ReadLog(ctx context.Context, buildID string) ([]byte, error)
	TailLog(ctx context.Context, buildID string, offset int64, w io.Writer) (int64, error)
}

// OrchestratorImpl implements Orchestrator. Field notes:
//
//   - now is the injectable clock (default time.Now) behind every timestamp
//     decision (stall/timeout scans, requeue, finalization);
//   - execCommand is the injectable command constructor (default
//     exec.CommandContext) used for git mirror reads and source archives;
//   - mirrorDir is the source mirror directory, derived from cfg.Source.URL
//     exactly like detect derives its own;
//   - ingestMu serializes the whole ingest orchestration (single-repo
//     mutex);
//   - tokenCache holds taskID → claim token in memory; a controller restart
//     clears it, orphaning running agents until stall recovery re-claims;
//   - roundSet tracks the pkgbases enqueued in the current detection round
//     for the name-conflict check, pruned after cfg.Source.PollInterval.
//
// NewOrchestrator starts the periodic scheduler goroutine; Stop halts it
// and drains the ingest mutex.
type OrchestratorImpl struct {
	cfg         *config.ControllerConfig
	store       *db.Store
	storage     storage.Backend
	signer      signVerifier
	updater     repo.Updater
	notifier    mail.Notifier
	logs        *Logs
	mirrorDir   string
	now         func() time.Time
	execCommand func(ctx context.Context, name string, arg ...string) *exec.Cmd

	ingestMu   sync.Mutex
	tokenMu    sync.Mutex
	tokenCache map[string]string
	roundMu    sync.Mutex
	roundSet   map[string]time.Time

	// actions autoscaling (worker.actions): the dispatcher is built at
	// construction and the last dispatch attempt is tracked under a lock
	// (the scheduler reads and writes it).
	actions        workflowDispatcher
	actionsMu      sync.Mutex
	lastDispatchAt time.Time

	// scheduler lifecycle (started by NewOrchestrator, stopped by Stop).
	stopOnce       sync.Once
	schedCancel    context.CancelFunc
	schedDone      chan struct{}
	stallInterval  time.Duration // default 30s; injectable for tests
	hourlyInterval time.Duration // default 1h; injectable for tests
}

// signerUsable reports whether a signer dependency is present and
// dereferenceable. A plain interface == nil comparison is defeated by a
// typed nil stored inside the interface — e.g. a nil *sign.Signer passed
// by a caller with repo.sign="off": the interface itself is non-nil and
// the first method call would panic with a nil pointer dereference. Every
// nilable kind is handled so any future concrete implementation type
// stays safe.
func signerUsable(s signVerifier) bool {
	if s == nil {
		return false
	}
	v := reflect.ValueOf(s)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return !v.IsNil()
	}
	return true
}

// NewOrchestrator builds the orchestration core. The scheduler goroutine
// starts immediately; call Stop for a graceful shutdown. All dependencies
// except the config may be nil in tests that do not exercise them. The
// signer is normalized: an interface wrapping a typed nil pointer (the
// shape a caller produces with repo.sign="off") is stored as a true nil
// so every later nil check is sound.
func NewOrchestrator(cfg *config.ControllerConfig, store *db.Store, backend storage.Backend,
	signer signVerifier, updater repo.Updater, notifier mail.Notifier, logs *Logs) *OrchestratorImpl {
	if cfg == nil {
		cfg = &config.ControllerConfig{}
	}
	if !signerUsable(signer) {
		signer = nil // typed nil pointer in the interface == no signer
	}
	o := &OrchestratorImpl{
		cfg:            cfg,
		store:          store,
		storage:        backend,
		signer:         signer,
		updater:        updater,
		notifier:       notifier,
		logs:           logs,
		mirrorDir:      sourceMirrorDir(cfg.Source.URL),
		now:            time.Now,
		execCommand:    exec.CommandContext,
		tokenCache:     make(map[string]string),
		roundSet:       make(map[string]time.Time),
		stallInterval:  30 * time.Second,
		hourlyInterval: time.Hour,
		actions:        newActionsDispatcher(&cfg.Worker.Actions),
	}
	ctx, cancel := context.WithCancel(context.Background())
	o.schedCancel = cancel
	o.schedDone = make(chan struct{})
	go o.runScheduler(ctx)
	return o
}

// sourceMirrorDir derives the mirror directory from the source URL the
// same way detect does: /data/source/<MirrorDir>.git.
func sourceMirrorDir(sourceURL string) string {
	if sourceURL == "" {
		return ""
	}
	return filepath.Join(sourceMirrorRoot, detect.MirrorDir(sourceURL)+".git")
}

// Submit implements detect.Sink: every detected change is enqueued for
// building. Concurrently safe.
func (o *OrchestratorImpl) Submit(ctx context.Context, c detect.Change) error {
	return o.Enqueue(ctx, c, false)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// uuidV4 generates a random RFC 4122 version-4 UUID without external
// dependencies (go.mod is frozen; google/uuid remains an indirect dep).
func uuidV4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("dispatch: crypto/rand failed: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// randomToken returns a 32-byte random value hex-encoded (64 chars): the
// per-task claim token.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("dispatch: generate claim token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// isTerminal reports whether a task state is final.
func isTerminal(state string) bool {
	switch state {
	case "succeeded", "failed", "cancelled":
		return true
	}
	return false
}

// checkToken validates a claim token against the in-memory cache with a
// constant-time comparison. Unknown tasks and tasks claimed before a
// controller restart have no cached token and are forbidden.
func (o *OrchestratorImpl) checkToken(taskID, token string) error {
	o.tokenMu.Lock()
	got := o.tokenCache[taskID]
	o.tokenMu.Unlock()
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
		return ErrForbidden
	}
	return nil
}

// setToken caches a freshly issued claim token.
func (o *OrchestratorImpl) setToken(taskID, token string) {
	o.tokenMu.Lock()
	o.tokenCache[taskID] = token
	o.tokenMu.Unlock()
}

// clearToken drops a claim token. Tokens of terminal tasks are
// deliberately kept so a late report is classified as a state conflict
// (409) instead of a forbidden token (403); the token is dropped on
// requeue so a re-claimed task can never be driven by the stale
// container's token, and stale entries are replaced by the next claim.
func (o *OrchestratorImpl) clearToken(taskID string) {
	o.tokenMu.Lock()
	delete(o.tokenCache, taskID)
	o.tokenMu.Unlock()
}

// finalizeTask writes a terminal state through the store transaction
// helper. ErrConflict (already terminal — a concurrent agent report, cancel
// or scheduler scan won the race) and ErrNotFound propagate unwrapped so
// callers can classify them.
func (o *OrchestratorImpl) finalizeTask(ctx context.Context, taskID, state, errMsg string, artifacts []db.Artifact, samples []db.Sample) error {
	return o.store.WithTx(ctx, func(tx *db.Tx) error {
		return tx.FinalizeTask(ctx, taskID, state, errMsg, o.now().UTC(), artifacts, samples)
	})
}

// notifyFailure sends a failure notification to the package maintainers
// snapshot (packages.maintainers, refreshed at enqueue). Send failures
// are only logged: they never affect task state.
func (o *OrchestratorImpl) notifyFailure(ctx context.Context, task *db.Task, build *db.Build, stage, summary string) {
	if o.notifier == nil {
		return
	}
	pkg, err := o.store.GetPackageByID(ctx, task.PackageID)
	if err != nil {
		log.Printf("dispatch: notify failure for %s: read package: %v", task.ID, err)
		return
	}
	if len(pkg.Maintainers) == 0 {
		return
	}
	url := strings.TrimRight(o.cfg.Server.WebURL, "/") + "/builds/" + build.ID
	info := mail.FailureInfo{
		Pkgbase: pkg.Pkgbase,
		Branch:  build.Branch,
		Commit:  build.Commit,
		Stage:   stage,
		Summary: summary,
		LogURL:  url,
	}
	if err := o.notifier.SendFailure(ctx, pkg.Maintainers, info); err != nil {
		log.Printf("dispatch: notify failure for %s: %v", task.ID, err)
	}
}

// cleanupStaging removes the staged files of a task plus its directory on
// the local backend. Deletion is best-effort: leftovers are swept by the
// hourly stale-staging pass.
func (o *OrchestratorImpl) cleanupStaging(ctx context.Context, taskID string, files []string) {
	for _, f := range files {
		if err := o.storage.Delete(ctx, storage.StagingPath(taskID, f)); err != nil {
			log.Printf("dispatch: cleanup staging %s/%s: %v", taskID, f, err)
		}
	}
	if o.cfg.Storage.Backend == "local" {
		dir := filepath.Join(o.cfg.Storage.Local.Root, "staging", taskID)
		if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("dispatch: cleanup staging dir %s: %v", taskID, err)
		}
	}
}

// stagedFiles returns the files expected in a task's staging area: every
// manifest entry plus the source archive when archive mode is active.
func (o *OrchestratorImpl) stagedFiles(manifest []repo.Artifact) []string {
	files := make([]string, 0, len(manifest)+1)
	for _, a := range manifest {
		files = append(files, a.File)
	}
	if o.archiveMode() {
		files = append(files, sourceArchiveName)
	}
	return files
}

// archiveMode reports whether source delivery uses git-archive snapshots
// (cfg.Source.FetchKey non-empty) instead of cloning.
func (o *OrchestratorImpl) archiveMode() bool {
	return o.cfg.Source.FetchKey != ""
}

// workerName resolves the display name of the node that executed a task
// (host name for host mode, agent name for pool mode). It feeds the side
// file [build].worker through repo.Updater.Ingest via the workerName
// parameter.
func (o *OrchestratorImpl) workerName(ctx context.Context, workerID int64) string {
	if workerID == 0 {
		return ""
	}
	w, err := o.store.GetWorkerByID(ctx, workerID)
	if err != nil {
		log.Printf("dispatch: resolve worker %d name: %v", workerID, err)
		return ""
	}
	return w.Name
}

// cancelledTaskIDs returns the ids of this worker's tasks that carry a
// durable cancel request and are still active. Both signals are always
// read from the database: there is no in-memory cancel state.
func (o *OrchestratorImpl) cancelledTaskIDs(ctx context.Context, workerID int64) ([]string, error) {
	tasks, err := o.store.ListTasksByWorker(ctx, workerID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range tasks {
		if t.CancelRequested && (t.State == "assigned" || t.State == "running") {
			out = append(out, t.ID)
		}
	}
	return out, nil
}

// mergeSamples merges the samples accumulated in the DB (via heartbeats and
// log progress) with the result report's sample list, deduplicating by
// timestamp (existing entries win) and sorting ascending. db's own merge is
// unexported; this mirrors its semantics so FinalizeTask never clobbers
// samples that arrived through the streaming channels.
func mergeSamples(existing, incoming []db.Sample) []db.Sample {
	seen := make(map[string]bool, len(existing)+len(incoming))
	out := make([]db.Sample, 0, len(existing)+len(incoming))
	for _, s := range existing {
		key := s.At.UTC().Format(time.RFC3339Nano)
		seen[key] = true
		out = append(out, s)
	}
	for _, s := range incoming {
		key := s.At.UTC().Format(time.RFC3339Nano)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// toDBArtifacts converts the protocol manifest (repo.Artifact) into the
// db.Artifact representation stored on the builds row.
func toDBArtifacts(manifest []repo.Artifact) []db.Artifact {
	out := make([]db.Artifact, 0, len(manifest))
	for _, a := range manifest {
		out = append(out, db.Artifact{
			File:    a.File,
			Kind:    a.Kind,
			Pkgname: a.Pkgname,
			Version: a.Version,
			Arch:    a.Arch,
			Size:    a.Size,
			SHA256:  a.SHA256,
		})
	}
	return out
}
