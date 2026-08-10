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

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Request policy: ordinary requests time out after 30s;
// uploads and downloads carry no per-request timeout and stream for as long
// as the caller's context allows. Retries apply to idempotent requests only
// (heartbeat / log append / upload / result report): up to 3 retries with
// a fixed 1s interval, on network errors and 5xx responses. A timed-out
// request is never retried (the server may have applied it), except for
// the result report: a lost report is unrecoverable and duplicate reports
// are safe (409 when already terminal), so the report is also retried on
// timeouts to ride out a slow or hung upstream.
const (
	requestTimeout = 30 * time.Second
	maxRetries     = 3

	// maxResponseBytes caps any success response body the client buffers
	// (64 MiB far exceeds the largest legitimate payload) and
	// maxErrorBytes caps non-2xx response bodies, so a hostile or broken
	// controller cannot push unbounded data into the worker.
	maxResponseBytes = 64 << 20
	maxErrorBytes    = 1 << 20
)

// retryInterval is the fixed backoff between retry attempts. It is a
// variable so same-package tests can shorten it.
var retryInterval = time.Second

// Client is the worker-side network entry point. It is safe for
// concurrent use. baseURL is the controller origin without a trailing
// slash; token is the shared Bearer token used by node-level endpoints
// only (task-level endpoints carry the per-task claim token instead).
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient builds a worker client. A baseURL of "https://controller:31759"
// style (scheme and authority) is expected; an empty token is valid for
// one-shot agents, which only call task-level endpoints.
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{},
	}
}

// APIError is returned for every non-2xx response. Code and Message mirror
// the wire error object; Offset is set when a 409 conflict carries the
// current server-side offset (resumable log/file uploads).
type APIError struct {
	Status  int
	Code    string
	Message string
	Offset  int64
}

// Error implements error.
func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("api: %s (http %d): %s", e.Code, e.Status, e.Message)
	}
	return fmt.Sprintf("api: http %d: %s", e.Status, e.Message)
}

// Register registers a node (POST /api/v1/register).
func (c *Client) Register(ctx context.Context, req RegisterReq) (*RegisterResp, error) {
	var resp RegisterResp
	if err := c.nodeRequest(ctx, http.MethodPost, "/api/v1/register", req, &resp, requestTimeout, false, false); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Heartbeat sends a heartbeat with system metrics and running-task progress
// (POST /api/v1/heartbeat); the response carries cancellation signals.
// Idempotent: retried on transient failures.
func (c *Client) Heartbeat(ctx context.Context, req HeartbeatReq) (*HeartbeatResp, error) {
	var resp HeartbeatResp
	if err := c.nodeRequest(ctx, http.MethodPost, "/api/v1/heartbeat", req, &resp, requestTimeout, true, false); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Poll claims one task for a host/pool node (POST /api/v1/poll). The
// response carries the task detail plus the claim token to hand to the
// one-shot agent container.
func (c *Client) Poll(ctx context.Context, req PollReq) (*PollResp, error) {
	var resp PollResp
	if err := c.nodeRequest(ctx, http.MethodPost, "/api/v1/poll", req, &resp, requestTimeout, false, false); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetTask fetches the full task detail for one-shot mode
// (GET /api/v1/tasks/{id}).
func (c *Client) GetTask(ctx context.Context, taskID, token string) (*TaskDetail, error) {
	var task TaskDetail
	if err := c.taskRequest(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID),
		token, nil, &task, requestTimeout, false, false); err != nil {
		return nil, err
	}
	return &task, nil
}

// AppendLog appends one buffered log segment (POST /api/v1/tasks/{id}/log).
// Idempotent at a given offset: retried on transient failures.
func (c *Client) AppendLog(ctx context.Context, taskID, token string, seg LogSegment) (*LogAck, error) {
	var ack LogAck
	if err := c.taskRequest(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(taskID)+"/log",
		token, seg, &ack, requestTimeout, true, false); err != nil {
		return nil, err
	}
	return &ack, nil
}

// ReportResult reports the final build outcome (POST
// /api/v1/tasks/{id}/result). The report is retried on transient failures
// (network errors and 5xx) because a lost report is unrecoverable: the
// controller never learns the outcome, the task stays "running" and
// cancellation (which needs the worker's acknowledgement) is ineffective.
// Duplicate reports are safe: the controller returns 409 once the task is
// terminal, and concurrent ingests serialize on the ingest mutex.
func (c *Client) ReportResult(ctx context.Context, taskID, token string, res ResultReq) error {
	return c.taskRequest(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(taskID)+"/result",
		token, res, nil, requestTimeout, true, true)
}

// GetSigningKey claims the one-shot signing key material of a task (POST
// /api/v1/tasks/{id}/signing-key); each task may claim it exactly once.
func (c *Client) GetSigningKey(ctx context.Context, taskID, token string) (*KeyMaterial, error) {
	var wire signingKeyWire
	if err := c.taskRequest(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(taskID)+"/signing-key",
		token, nil, &wire, requestTimeout, false, false); err != nil {
		return nil, err
	}
	return &KeyMaterial{
		KeyID:             wire.KeyID,
		ArmoredPrivateKey: wire.ArmoredPrivateKey,
		Passphrase:        wire.Passphrase,
	}, nil
}

// UploadFile streams one artifact segment into the task staging area (PUT
// /api/v1/tasks/{id}/files/{name}?offset=N). size is the byte length of
// the body being sent (the whole file on the first attempt, the remainder
// on a resumed one) and becomes the request's declared Content-Length. No
// per-request timeout: the whole file streams under the caller's context.
// Idempotent at a given offset, so transient failures are retried, but a
// retry must re-send the same bytes, which requires a re-readable source:
// when r implements io.Seeker (a file or byte buffer) it is rewound for
// every attempt, otherwise the request runs once and the first error is
// returned.
func (c *Client) UploadFile(ctx context.Context, taskID, token, name string, r io.Reader, size, offset int64) (*FileMeta, error) {
	path := "/api/v1/tasks/" + url.PathEscape(taskID) + "/files/" + url.PathEscape(name)
	if offset != 0 {
		path += "?offset=" + fmt.Sprintf("%d", offset)
	}
	seeker, rewindable := r.(io.Seeker)
	var meta FileMeta
	err := c.exec(ctx, http.MethodPut, path, token, false,
		func() (io.ReadCloser, error) {
			if rewindable {
				if _, err := seeker.Seek(0, io.SeekStart); err != nil {
					return nil, fmt.Errorf("api: rewind upload source: %w", err)
				}
			}
			return io.NopCloser(r), nil
		}, size, &meta, 0, rewindable, false)
	if err != nil {
		return nil, err
	}
	return &meta, nil
}

// DownloadFile streams a staged file back (GET
// /api/v1/tasks/{id}/files/{name}); the caller reads and closes the
// returned body. No per-request timeout: it streams for as long as the
// caller's context allows.
func (c *Client) DownloadFile(ctx context.Context, taskID, token, name string) (io.ReadCloser, error) {
	path := "/api/v1/tasks/" + url.PathEscape(taskID) + "/files/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("api: build download request: %w", err)
	}
	if token != "" {
		req.Header.Set(taskTokenHeader, token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("api: download %s: %w", name, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, decodeAPIError(resp)
	}
	return resp.Body, nil
}

// Deregister marks the node offline on normal shutdown (POST
// /api/v1/workers/{name}/deregister).
func (c *Client) Deregister(ctx context.Context, name string) error {
	return c.nodeRequest(ctx, http.MethodPost, "/api/v1/workers/"+url.PathEscape(name)+"/deregister",
		nil, nil, requestTimeout, false, false)
}

// nodeRequest issues a node-level request authenticated with the shared
// Bearer token.
func (c *Client) nodeRequest(ctx context.Context, method, path string, reqBody, respBody any,
	timeout time.Duration, retryable, retryOnTimeout bool) error {
	return c.jsonRequest(ctx, method, path, reqBody, respBody, timeout, retryable, retryOnTimeout, "", true, nil)
}

// taskRequest issues a task-level request authenticated with the per-task
// claim token (no shared Bearer on task endpoints).
func (c *Client) taskRequest(ctx context.Context, method, path string, taskToken string,
	reqBody, respBody any, timeout time.Duration, retryable, retryOnTimeout bool) error {
	return c.jsonRequest(ctx, method, path, reqBody, respBody, timeout, retryable, retryOnTimeout, taskToken, false, nil)
}

// jsonRequest runs a request whose body is a JSON payload marshaled once
// and re-marshaled per retry attempt; newBody overrides the default body
// factory and is used by the streaming upload.
func (c *Client) jsonRequest(ctx context.Context, method, path string, reqBody, respBody any,
	timeout time.Duration, retryable, retryOnTimeout bool, taskToken string, useBearer bool,
	newBody func() (io.ReadCloser, error)) error {
	payloadLen := int64(0)
	if reqBody != nil && newBody == nil {
		payload, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("api: marshal request: %w", err)
		}
		payloadLen = int64(len(payload))
		newBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(payload)), nil
		}
	}
	return c.exec(ctx, method, path, taskToken, useBearer, newBody, payloadLen, respBody, timeout, retryable, retryOnTimeout)
}

// exec runs the request, retrying transient failures for idempotent calls.
// contentLength is the declared body size in bytes (0 when unknown, which
// keeps the request chunked): every varve request body has a known size,
// so the server-side ContentLength checks and limits apply to real
// traffic instead of being bypassed by chunked encoding.
// exec runs the request, retrying transient failures for idempotent calls.
// contentLength is the declared body size in bytes (0 when unknown, which
// keeps the request chunked): every varve request body has a known size,
// so the server-side ContentLength checks and limits apply to real
// traffic instead of being bypassed by chunked encoding. retryOnTimeout
// additionally retries per-request deadline failures, an escape hatch for
// the result report only.
func (c *Client) exec(ctx context.Context, method, path, taskToken string, useBearer bool,
	newBody func() (io.ReadCloser, error), contentLength int64, respBody any, timeout time.Duration, retryable, retryOnTimeout bool) error {
	var lastErr error
	for attempt := 0; ; attempt++ {
		err := c.do(ctx, method, path, taskToken, useBearer, newBody, contentLength, respBody, timeout)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable || attempt >= maxRetries || !isRetryable(ctx, err) ||
			(!retryOnTimeout && errors.Is(err, context.DeadlineExceeded)) {
			return lastErr
		}
		if err := sleepContext(ctx, retryInterval); err != nil {
			return err
		}
	}
}

// readAllLimited reads r up to limit bytes and rejects anything larger:
// the caller gets an explicit error instead of a silently truncated body.
func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("api: response exceeds %d bytes", limit)
	}
	return data, nil
}

// do performs a single request attempt and decodes the response. A
// positive contentLength is declared on the request so the body travels
// with a known size instead of chunked encoding.
func (c *Client) do(ctx context.Context, method, path, taskToken string, useBearer bool,
	newBody func() (io.ReadCloser, error), contentLength int64, respBody any, timeout time.Duration) error {
	reqCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var body io.ReadCloser
	if newBody != nil {
		var err error
		body, err = newBody()
		if err != nil {
			return err
		}
		defer body.Close()
	}
	req, err := http.NewRequestWithContext(reqCtx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("api: build request %s %s: %w", method, path, err)
	}
	if contentLength > 0 {
		req.ContentLength = contentLength
	}
	if useBearer && c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if taskToken != "" {
		req.Header.Set(taskTokenHeader, taskToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("api: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAPIError(resp)
	}
	if respBody != nil {
		data, err := readAllLimited(resp.Body, maxResponseBytes)
		if err != nil {
			return fmt.Errorf("api: read response: %w", err)
		}
		if err := json.Unmarshal(data, respBody); err != nil {
			return fmt.Errorf("api: decode response: %w", err)
		}
	}
	return nil
}

// decodeAPIError parses a non-2xx response into an APIError, extracting
// the wire error object and the optional resume offset.
func decodeAPIError(resp *http.Response) error {
	data, err := readAllLimited(resp.Body, maxErrorBytes)
	if err != nil {
		return fmt.Errorf("api: read error response: %w", err)
	}
	var body struct {
		Error  *errorDetail `json:"error"`
		Offset *int64       `json:"offset"`
	}
	apiErr := &APIError{Status: resp.StatusCode}
	if err := json.Unmarshal(data, &body); err == nil {
		if body.Error != nil {
			apiErr.Code = body.Error.Code
			apiErr.Message = body.Error.Message
		}
		if body.Offset != nil {
			apiErr.Offset = *body.Offset
		}
	}
	if apiErr.Message == "" {
		apiErr.Message = strings.TrimSpace(string(data))
		if apiErr.Message == "" {
			apiErr.Message = resp.Status
		}
	}
	return apiErr
}

// isRetryable reports whether a failed attempt may be retried: network
// errors and 5xx responses only (4xx responses are never retried).
func isRetryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status >= 500
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

// sleepContext sleeps for d or until ctx is done, whichever comes first.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
