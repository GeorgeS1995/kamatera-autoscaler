// Package kamatera is a Kamatera Cloud REST client tailored to the autoscaler.
// It speaks two of Kamatera's APIs in tandem:
//
//   - cloudcli (https://cloudcli.cloudwm.com), header-pair auth, used for
//     POST /service/server (create, with the script-file field needed to
//     inject cloud-init) and POST /service/server/terminate (idempotent).
//
//   - console (https://console.kamatera.com/service), Bearer-token auth,
//     used for GET /queue (batched recent-commands list) and GET /servers
//     (server list). The cloudcli analogues here either always return empty
//     (/service/queue) or intermittently 500 (/service/server/info).
//
// Retry policy is asymmetric: idempotent reads (queue, server list,
// terminate) retry on 5xx / transport. POST /service/server does NOT retry —
// a lost response could leave a VM provisioned at real cost. Instead, on
// transport failure CreateServer enters a recovery loop polling
// FindServerByName; if the server materialised, it returns
// *ErrCreateRecovered.
package kamatera

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GeorgeS1995/kamatera-autoscaler/internal/config"
)

// DefaultBaseURL is the production Kamatera API endpoint.
const DefaultBaseURL = "https://cloudcli.cloudwm.com"

// Client is the public interface used by the controller.
type Client interface {
	// CreateServer issues POST /service/server. On a clean success the
	// returned commandID can be passed to WaitProvision to await VM readiness.
	// On a transport-level failure that the recovery loop resolves by finding
	// the server already provisioned, CreateServer returns an
	// *ErrCreateRecovered whose embedded Server should be treated as the
	// result — DO NOT retry the call in that case.
	CreateServer(ctx context.Context, req CreateServerRequest) (commandID string, err error)

	// WaitProvision blocks until commandID reaches a terminal state and
	// returns the fully populated Server (with id, IPs) for the named server.
	WaitProvision(ctx context.Context, commandID, name string, timeout time.Duration) (Server, error)

	// WaitTerminate blocks until commandID reaches a terminal state. It is
	// the terminate-side counterpart of WaitProvision; no Server enrichment
	// is performed (the server is gone).
	WaitTerminate(ctx context.Context, commandID string, timeout time.Duration) error

	FindServerByName(ctx context.Context, name string) (Server, error)
	TerminateServer(ctx context.Context, serverID string) (commandID string, err error)
}

// Option mutates an httpClient at construction time.
type Option func(*httpClient)

// WithBaseURL overrides the cloudcli (create / terminate / info) base URL.
// Useful for tests pointing to httptest.NewServer.
func WithBaseURL(u string) Option {
	return func(c *httpClient) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithConsoleBaseURL overrides the console.kamatera.com (auth + queue) base
// URL. Useful for tests that want to mock both APIs against one httptest
// server.
func WithConsoleBaseURL(u string) Option {
	return func(c *httpClient) { c.consoleBaseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(h *http.Client) Option { return func(c *httpClient) { c.http = h } }

// WithMaxRetries sets the number of *retries* (additional attempts) for
// idempotent requests on 5xx / transport errors. The first attempt is always
// made and is NOT counted as a retry: total attempts = n + 1. n=0 means "one
// attempt, no retries"; n=3 (default) means "up to four attempts total".
func WithMaxRetries(n int) Option { return func(c *httpClient) { c.maxRetries = n } }

// WithRetryBaseDelay sets the initial back-off between retries inside `do()`.
// Subsequent retries multiply this by 3 (1s, 3s, 9s, ...). Independent from
// WithQueuePollInterval — they configure different layers (single-request
// retry vs async-watch tick).
func WithRetryBaseDelay(d time.Duration) Option {
	return func(c *httpClient) { c.retryBase = d }
}

// WithQueuePollInterval sets the watcher's tick cadence — how often the
// background goroutine pulls the recent-commands list from /service/queue.
// One queue call covers all currently-pending waiters; one info call covers
// all successful completions in the same tick.
func WithQueuePollInterval(d time.Duration) Option {
	return func(c *httpClient) { c.pollInterval = d }
}

// WithLogger sets the structured logger. Defaults to slog.Default().
func WithLogger(l *slog.Logger) Option { return func(c *httpClient) { c.log = l } }

// WithRecoveryParams overrides the create-recovery loop tuning. Defaults:
//   - grace:      30s  (give Kamatera time to register the server name)
//   - pollEvery:  15s  (each tick polls and emits a progress log)
//   - maxWait:    10m  (after this, assume the request never reached the API)
func WithRecoveryParams(grace, pollEvery, maxWait time.Duration) Option {
	return func(c *httpClient) {
		if grace > 0 {
			c.recoveryGrace = grace
		}
		if pollEvery > 0 {
			c.recoveryPollEvery = pollEvery
		}
		if maxWait > 0 {
			c.recoveryMax = maxWait
		}
	}
}

type httpClient struct {
	baseURL        string // cloudcli
	consoleBaseURL string // console.kamatera.com
	http           *http.Client
	creds          config.Credentials
	maxRetries     int
	retryBase      time.Duration
	pollInterval   time.Duration
	log            *slog.Logger

	recoveryGrace     time.Duration
	recoveryPollEvery time.Duration
	recoveryMax       time.Duration

	console *consoleClient
	watcher *cmdWatcher
}

// NewClient builds a Client. Credentials come from config; never log them.
func NewClient(creds config.Credentials, opts ...Option) Client {
	c := &httpClient{
		baseURL:        DefaultBaseURL,
		consoleBaseURL: DefaultConsoleBaseURL,
		// No client-level timeout: each call sets its own deadline via context.
		// A blanket http.Client.Timeout would force POST /service/server to
		// race against VM-creation latency (~2-3 min) and trigger spurious
		// "transport failure" recovery loops.
		http:              &http.Client{},
		creds:             creds,
		maxRetries:        3,
		retryBase:         1 * time.Second,
		pollInterval:      15 * time.Second,
		log:               slog.Default(),
		recoveryGrace:     30 * time.Second,
		recoveryPollEvery: 15 * time.Second,
		recoveryMax:       10 * time.Minute,
	}
	for _, o := range opts {
		o(c)
	}
	c.console = newConsoleClient(c.creds, c.http, c.log)
	c.console.baseURL = c.consoleBaseURL
	c.watcher = newCmdWatcher(c, c.pollInterval)
	return c
}

// CreateServer issues POST /service/server with NO retry on transport errors.
// On transport failure it spawns a recovery loop polling FindServerByName for
// up to recoveryMax. If the server is found, returns *ErrCreateRecovered with
// the resolved Server inside.
func (c *httpClient) CreateServer(ctx context.Context, req CreateServerRequest) (string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal create-server request: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/service/server", body, false /* not retryable */)
	if err == nil {
		id, decErr := decodeCommandID(resp)
		if decErr != nil {
			return "", fmt.Errorf("decode create-server response: %w", decErr)
		}
		return id, nil
	}
	// 4xx / 5xx: API answered, no recovery to do — the request either reached
	// Kamatera and was rejected (4xx) or the server itself is sad (5xx).
	if !IsTransportError(err) {
		return "", err
	}
	c.log.Warn("CreateServer transport failed; entering recovery — may take up to "+c.recoveryMax.String(),
		"name", req.Name, "err", err)
	server, recoveryErr := c.recoverByName(ctx, req.Name)
	if recoveryErr == nil {
		c.log.Info("CreateServer recovered after transport failure",
			"name", req.Name, "server_id", server.ID)
		return "", &ErrCreateRecovered{Server: server}
	}
	return "", fmt.Errorf("create transport failed and recovery did not find %q in %s: %w",
		req.Name, c.recoveryMax, err)
}

// recoverByName polls FindServerByName until either the server appears or
// recoveryMax elapses. 4xx errors abort early — the API is telling us the
// request shape is wrong, no amount of waiting will change that.
func (c *httpClient) recoverByName(ctx context.Context, name string) (Server, error) {
	if !sleepCtx(ctx, c.recoveryGrace) {
		return Server{}, ctx.Err()
	}
	start := time.Now()
	deadline := start.Add(c.recoveryMax)
	for {
		srv, err := c.FindServerByName(ctx, name)
		if err == nil {
			return srv, nil
		}
		if IsClientError(err) {
			// 4xx other than not-found: abort. ErrServerNotFound from FindServerByName
			// is a sentinel, not a APIError, so it's not classified as 4xx.
			return Server{}, fmt.Errorf("recovery aborted: %w", err)
		}
		// transport / 5xx / not-found: keep going.
		if time.Now().After(deadline) {
			return Server{}, fmt.Errorf("recovery timed out after %s", c.recoveryMax)
		}
		elapsed := time.Since(start).Truncate(time.Second)
		c.log.Info("CreateServer recovery still polling",
			"name", name, "elapsed", elapsed.String(), "max", c.recoveryMax.String())
		if !sleepCtx(ctx, c.recoveryPollEvery) {
			return Server{}, ctx.Err()
		}
	}
}

// TerminateServer is idempotent (a second terminate against an
// already-terminated server returns a deterministic error rather than
// provisioning new state), so it's safe to retry on transport / 5xx.
func (c *httpClient) TerminateServer(ctx context.Context, serverID string) (string, error) {
	if serverID == "" {
		return "", errors.New("serverID is empty")
	}
	body, _ := json.Marshal(map[string]any{"id": serverID, "force": true})
	resp, err := c.do(ctx, http.MethodPost, "/service/server/terminate", body, true)
	if err != nil {
		return "", err
	}
	id, err := decodeCommandID(resp)
	if err != nil {
		return "", fmt.Errorf("decode terminate-server response: %w", err)
	}
	return id, nil
}

// FindServerByName looks up a server by name through the console API's GET
// /servers endpoint, scanning the returned list client-side. Console doesn't
// expose a server-side name filter, but with our scale (≤ low-tens of
// servers) the per-call cost is negligible.
func (c *httpClient) FindServerByName(ctx context.Context, name string) (Server, error) {
	if name == "" {
		return Server{}, errors.New("name is empty")
	}
	servers, err := c.console.ListServers(ctx)
	if err != nil {
		return Server{}, err
	}
	for _, s := range servers {
		if s.Name == name {
			return s, nil
		}
	}
	return Server{}, &ServerNotFoundError{Name: name}
}

// WaitProvision delegates to the central watcher.
func (c *httpClient) WaitProvision(ctx context.Context, commandID, name string, timeout time.Duration) (Server, error) {
	return c.watcher.waitProvision(ctx, commandID, name, timeout)
}

// WaitTerminate delegates to the central watcher.
func (c *httpClient) WaitTerminate(ctx context.Context, commandID string, timeout time.Duration) error {
	return c.watcher.waitTerminate(ctx, commandID, timeout)
}

// do issues a single request with auth headers. When retryable is true, it
// retries up to maxRetries on 5xx and transport errors with exponential
// back-off (×3 each step). When retryable is false (POST /service/server) it
// makes ONE attempt only — see package doc for why.
//
// Non-2xx responses are returned as *APIError (typed); transport failures
// (timeout, DNS, TCP reset) are returned as raw errors. IsTransportError /
// IsClientError / IsServerError distinguish the cases without string parsing.
func (c *httpClient) do(ctx context.Context, method, path string, body []byte, retryable bool) ([]byte, error) {
	maxAttempts := 1
	if retryable {
		maxAttempts = c.maxRetries + 1
	}
	var lastErr error
	delay := c.retryBase
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
			delay *= 3
		}

		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("AuthClientId", c.creds.KamateraClientID())
		req.Header.Set("AuthSecret", c.creds.KamateraSecret())
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = redactErr(err)
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return respBody, nil
		}
		statusErr := &APIError{
			Method: method, Path: path, Code: resp.StatusCode, Body: string(respBody),
		}
		if statusErr.Client() {
			// 4xx: fail-fast, no point retrying.
			return nil, statusErr
		}
		// 5xx: keep last error for next attempt (or final return).
		lastErr = statusErr
	}
	if maxAttempts > 1 {
		return nil, fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
	}
	return nil, lastErr
}

// sleepCtx blocks for d unless the context is cancelled. Returns true if the
// full interval elapsed; false if the context cancelled during the wait.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// redactErr strips any auth header values that may have leaked into a
// transport error message.
func redactErr(err error) error {
	s := err.Error()
	for _, h := range []string{"AuthClientId", "AuthSecret"} {
		if strings.Contains(s, h+":") {
			s = strings.ReplaceAll(s, h+":", h+":<redacted>")
		}
	}
	return errors.New(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func decodeCommandID(body []byte) (string, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(body, &arr); err == nil && len(arr) > 0 {
		var s string
		if err := json.Unmarshal(arr[0], &s); err == nil && s != "" {
			return s, nil
		}
		var n json.Number
		if err := json.Unmarshal(arr[0], &n); err == nil {
			return n.String(), nil
		}
	}
	var s string
	if err := json.Unmarshal(body, &s); err == nil && s != "" {
		return s, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err == nil {
		for _, k := range []string{"commandIds", "commandIDs", "command_ids"} {
			if v, ok := obj[k]; ok {
				var list []string
				if err := json.Unmarshal(v, &list); err == nil && len(list) > 0 {
					return list[0], nil
				}
			}
		}
		if v, ok := obj["id"]; ok {
			var s string
			if err := json.Unmarshal(v, &s); err == nil && s != "" {
				return s, nil
			}
		}
	}
	return "", fmt.Errorf("no command id in response: %s", truncate(string(body), 200))
}

// decodeQueueStatuses expects an array — console API GET /queue always
// returns a JSON array (possibly empty). We do NOT accept single-object
// responses here because the only caller, ListQueue, always hits the list
// endpoint; a single-object body would be a real protocol error worth
// surfacing rather than a permissive fallback.
func decodeQueueStatuses(body []byte) ([]QueueStatus, error) {
	var arr []QueueStatus
	if err := json.Unmarshal(body, &arr); err != nil {
		return nil, fmt.Errorf("decode queue array: %w (body: %s)", err, truncate(string(body), 200))
	}
	return arr, nil
}
