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

package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"git.0x0f.dev/varve/internal/api"
	"git.0x0f.dev/varve/internal/config"
)

// fakeClient implements the agent's client interface, recording every call
// and offering programmable responses. All methods are safe for concurrent
// use.
type fakeClient struct {
	mu    sync.Mutex
	calls []string

	taskDetail *api.TaskDetail
	getTaskErr error

	logAck      api.LogAck
	logAckErr   error
	cancelAfter int // remaining append calls before a Cancelled ack
	logOffset   int64
	segments    []api.LogSegment
	// logMaxSegment mirrors the controller's per-segment cap (0 = no
	// cap); logAppendDelay simulates a real append round trip.
	logMaxSegment  int
	logAppendDelay time.Duration

	results     []api.ResultReq
	reportErr   error
	reportErrTo int // report attempts before reportErr

	hbCancelIDs []string
	hbErr       error
	heartbeats  []api.HeartbeatReq

	pollResp  api.PollResp
	pollResps []api.PollResp // consumed in order, last one repeated
	pollErr   error
	polls     []api.PollReq

	regErr       error
	regFailTimes int
	regCalls     int
	regNames     []string
	regReqs      []api.RegisterReq

	keyMaterial *api.KeyMaterial
	keyErr      error

	uploadConflict int   // remaining 409 responses for the next file
	uploadOffset   int64 // offset carried by those 409 responses
	uploads        []uploadCall

	downloadBody io.ReadCloser
	downloadErr  error
	downloadName string

	deregNames []string
	deregErr   error
}

type uploadCall struct {
	name   string
	size   int64
	offset int64
}

func (f *fakeClient) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeClient) Register(ctx context.Context, req api.RegisterReq) (*api.RegisterResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Register")
	f.regCalls++
	f.regNames = append(f.regNames, req.Name)
	f.regReqs = append(f.regReqs, req)
	if f.regFailTimes > 0 {
		f.regFailTimes--
		return nil, f.regErr
	}
	return &api.RegisterResp{ID: 1, Name: req.Name}, nil
}

func (f *fakeClient) Heartbeat(ctx context.Context, req api.HeartbeatReq) (*api.HeartbeatResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Heartbeat")
	f.heartbeats = append(f.heartbeats, req)
	if f.hbErr != nil {
		return nil, f.hbErr
	}
	return &api.HeartbeatResp{CancelledTaskIDs: f.hbCancelIDs}, nil
}

func (f *fakeClient) Poll(ctx context.Context, req api.PollReq) (*api.PollResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Poll")
	f.polls = append(f.polls, req)
	if f.pollErr != nil {
		return nil, f.pollErr
	}
	if len(f.pollResps) > 0 {
		resp := f.pollResps[0]
		f.pollResps = f.pollResps[1:]
		return &resp, nil
	}
	return &f.pollResp, nil
}

func (f *fakeClient) GetTask(ctx context.Context, id, token string) (*api.TaskDetail, error) {
	f.record("GetTask")
	if f.getTaskErr != nil {
		return nil, f.getTaskErr
	}
	return f.taskDetail, nil
}

func (f *fakeClient) AppendLog(ctx context.Context, id, token string, seg api.LogSegment) (*api.LogAck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "AppendLog")
	if f.logAckErr != nil {
		return nil, f.logAckErr
	}
	if f.logMaxSegment > 0 && len(seg.Data) > f.logMaxSegment {
		return nil, &api.APIError{Status: http.StatusBadRequest, Code: "invalid_request",
			Message: "log segment must not exceed " + strconv.Itoa(f.logMaxSegment) + " bytes"}
	}
	if f.logAppendDelay > 0 {
		time.Sleep(f.logAppendDelay)
	}
	f.segments = append(f.segments, seg)
	ack := f.logAck
	if ack.Offset == 0 {
		ack.Offset = f.logOffset + int64(len(seg.Data))
	}
	f.logOffset = ack.Offset
	if f.cancelAfter > 0 {
		f.cancelAfter--
		if f.cancelAfter == 0 {
			ack.Cancelled = true
		}
	}
	return &ack, nil
}

func (f *fakeClient) ReportResult(ctx context.Context, id, token string, res api.ResultReq) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "ReportResult")
	f.results = append(f.results, res)
	if f.reportErrTo > 0 {
		f.reportErrTo--
		return f.reportErr
	}
	return nil
}

func (f *fakeClient) GetSigningKey(ctx context.Context, id, token string) (*api.KeyMaterial, error) {
	f.record("GetSigningKey")
	if f.keyErr != nil {
		return nil, f.keyErr
	}
	return f.keyMaterial, nil
}

func (f *fakeClient) UploadFile(ctx context.Context, id, token, name string, r io.Reader, size, offset int64) (*api.FileMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "UploadFile")
	f.uploads = append(f.uploads, uploadCall{name: name, size: size, offset: offset})
	if f.uploadConflict > 0 {
		f.uploadConflict--
		return nil, &api.APIError{Status: http.StatusConflict, Code: "conflict", Offset: f.uploadOffset}
	}
	return &api.FileMeta{Name: name, Offset: size}, nil
}

func (f *fakeClient) DownloadFile(ctx context.Context, id, token, name string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "DownloadFile")
	f.downloadName = name
	if f.downloadErr != nil {
		return nil, f.downloadErr
	}
	return f.downloadBody, nil
}

func (f *fakeClient) Deregister(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Deregister")
	f.deregNames = append(f.deregNames, name)
	return f.deregErr
}

// lastResult returns the most recent result report, or nil.
func (f *fakeClient) lastResult() *api.ResultReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.results) == 0 {
		return nil
	}
	r := f.results[len(f.results)-1]
	return &r
}

// callCount returns how many times the named call was recorded.
func (f *fakeClient) callCount(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

// callIndex returns the index of the first call with the given name.
func (f *fakeClient) callIndex(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.calls {
		if c == call {
			return i
		}
	}
	return -1
}

// regCount returns how many Register calls were recorded.
func (f *fakeClient) regCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.regCalls
}

// execCall records one external command invocation. cmd keeps the
// built *exec.Cmd so tests can assert the environment the command
// actually ran with (the agent injects it after the recorder returns).
type execCall struct {
	name string
	args []string
	cmd  *exec.Cmd
}

// fakeExec builds commands for the agent, recording every invocation and
// dispatching programmable scripts (keyed by "name arg..."). Unknown
// commands fall back to /bin/true (exit 0). With realGit set, git runs
// for real (local file:// repos only).
type fakeExec struct {
	mu      sync.Mutex
	calls   []execCall
	scripts map[string]string
	realGit bool
}

func newFakeExec() *fakeExec {
	return &fakeExec{scripts: map[string]string{}}
}

func (f *fakeExec) command(ctx context.Context, name string, arg ...string) *exec.Cmd {
	var cmd *exec.Cmd
	if f.realGit && name == "git" {
		cmd = exec.CommandContext(ctx, name, arg...)
	} else {
		key := strings.Join(append([]string{name}, arg...), " ")
		if script, ok := f.scripts[key]; ok {
			cmd = exec.Command(script)
			// Pass the recorded args through so scripts can act on them
			// (e.g. the gpg signer touches "$last.sig").
			cmd.Args = append([]string{script}, arg...)
		} else {
			cmd = exec.Command("/bin/true")
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, execCall{name: name, args: append([]string(nil), arg...), cmd: cmd})
	f.mu.Unlock()
	return cmd
}

// callEnv returns the environment of the last recorded invocation of
// name (nil when the command inherits the process environment). The
// recorder keeps the *exec.Cmd pointer, so the value reflects the
// environment the command ran with, including the agent's injection.
func (f *fakeExec) callEnv(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].name == name && f.calls[i].cmd != nil {
			return f.calls[i].cmd.Env
		}
	}
	return nil
}

// callArgs returns the recorded arguments of the n-th invocation of name.
func (f *fakeExec) callArgs(name string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if c.name == name {
			out = append(out, c.args)
		}
	}
	return out
}

// writeScript writes an executable shell script and returns its path.
func writeScript(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/script.sh"
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+content+"\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

// taskFor builds the minimal TaskDetail used by the flow tests. It is
// decoded from the JSON wire form so tests never name the nested dispatch
// types (worker packages depend only on api). Tests mutate the fields they
// need afterwards.
func taskFor(id string) *api.TaskDetail {
	var task api.TaskDetail
	if err := json.Unmarshal([]byte(`{
		"id": "`+id+`",
		"package": {"pkgbase": "foo", "branch": "main", "arch": "x86_64"},
		"source": {"mode": "clone", "url": "https://example.invalid/repo.git", "branch": "main"},
		"hooks": {},
		"collect": {},
		"signing": {"required": false},
		"build": {}
	}`), &task); err != nil {
		panic(err)
	}
	return &task
}

// runOneShotRunner wires a one-shot runner with a temp data dir and the
// given fake client; the caller overrides execCommand and tunables.
func runOneShotRunner(t *testing.T, f *fakeClient) *Runner {
	t.Helper()
	cfg := configForTest(t, true)
	r := NewRunner(cfg, f)
	return r
}

// configForTest builds a minimal WorkerConfig rooted in a temp dir.
func configForTest(t *testing.T, oneShot bool) *config.WorkerConfig {
	t.Helper()
	cfg := &config.WorkerConfig{
		ControllerURL:   "http://controller.invalid",
		Role:            "agent",
		WorkerArch:      "x86_64",
		OneShot:         oneShot,
		TaskID:          "t-1",
		TaskToken:       "tok",
		DataDir:         t.TempDir(),
		PoolIdleTimeout: 10 * time.Minute,
	}
	return cfg
}

// waitFor polls cond until it holds or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// makeRepo creates a local git repository (file:// protocol only)
// containing .SRCINFO and returns its URL and the HEAD commit.
func makeRepo(t *testing.T, srcinfo string) (url, commit string) {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.invalid")
	runGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(dir+"/.SRCINFO", []byte(srcinfo), 0o644); err != nil {
		t.Fatalf("write .SRCINFO: %v", err)
	}
	runGit(t, dir, "add", ".SRCINFO")
	runGit(t, dir, "commit", "-m", "initial")
	commit = strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	return "file://" + dir, commit
}

// flowExec wires a fakeExec for a full successful task flow: a fake clone
// that creates the checkout with the given .SRCINFO text and a fake
// makepkg that creates the given package files. extra scripts (keyed by
// "name arg...") are merged in for the hooks/signing variants.
func flowExec(t *testing.T, workDir, taskID, srcinfo string, pkgs []string, extra map[string]string) *fakeExec {
	t.Helper()
	taskDir := workDir + "/" + taskID
	fe := newFakeExec()
	clone := writeScript(t, "cat > .SRCINFO <<'EOF'\n"+srcinfo+"EOF")
	var mkpkgBody strings.Builder
	mkpkgBody.WriteString("echo 'building'\n")
	for _, p := range pkgs {
		mkpkgBody.WriteString("touch " + p + "\n")
	}
	mkpkg := writeScript(t, mkpkgBody.String())
	fe.scripts["git clone --depth 1 --branch main https://example.invalid/repo.git "+taskDir] = clone
	fe.scripts["makepkg -s --noconfirm"] = mkpkg
	for k, v := range extra {
		fe.scripts[k] = v
	}
	return fe
}

// runGit runs git in dir and returns its combined output (fatal on error).
func runGit(t *testing.T, dir string, arg ...string) string {
	t.Helper()
	cmd := exec.Command("git", arg...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arg, err, out)
	}
	return string(out)
}

// fakeClock is an injectable time source for the idle timeout tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t0 time.Time) *fakeClock { return &fakeClock{t: t0} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
