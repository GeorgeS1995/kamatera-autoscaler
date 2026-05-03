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
	"sync"
	"time"

	"github.com/GeorgeS1995/kamatera-autoscaler/internal/config"
)

// DefaultConsoleBaseURL is the Kamatera console REST API endpoint
// (https://console.kamatera.com/service). It is a separate API from the
// cloudcli endpoint used for create/terminate; we keep them split because
// the two have different auth schemes (Bearer token vs header pair),
// different field shapes for server resources, and the console API has
// no `script-file` field for cloud-init injection.
const DefaultConsoleBaseURL = "https://console.kamatera.com/service"

// consoleClient queries Kamatera's queue endpoint on console.kamatera.com.
// The whole reason this lives in a separate file/type from httpClient is to
// avoid an N+1 problem in WaitProvision: the batched GET /queue here returns
// every active and recent command in one round-trip, so K concurrent waiters
// all share the same call rather than each issuing a per-id lookup.
//
// Auth is Bearer-token: POST /authenticate returns a token plus an `expires`
// unix-second timestamp. We cache it and refresh 30s before expiry to dodge
// mid-call invalidation; on a stray 401 we re-auth and retry once.
type consoleClient struct {
	baseURL    string
	creds      config.Credentials
	httpClient *http.Client
	log        *slog.Logger

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func newConsoleClient(creds config.Credentials, hc *http.Client, log *slog.Logger) *consoleClient {
	return &consoleClient{
		baseURL:    DefaultConsoleBaseURL,
		creds:      creds,
		httpClient: hc,
		log:        log,
	}
}

// authResponse is the shape of POST /authenticate.
type authResponse struct {
	Authentication string `json:"authentication"`
	Expires        int64  `json:"expires"` // unix seconds
}

// ensureToken acquires a fresh Bearer token if there isn't one or it's about
// to expire.
func (c *consoleClient) ensureToken(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Add(30*time.Second).Before(c.expiresAt) {
		return nil
	}
	body, _ := json.Marshal(map[string]string{
		"clientId": c.creds.KamateraClientID(),
		"secret":   c.creds.KamateraSecret(),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/authenticate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("authenticate: %w", redactErr(err))
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Method: "POST", Path: "/authenticate", Code: resp.StatusCode, Body: string(raw)}
	}
	var ar authResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return fmt.Errorf("decode authenticate response: %w", err)
	}
	if ar.Authentication == "" {
		return fmt.Errorf("authenticate: empty token in response")
	}
	c.token = ar.Authentication
	c.expiresAt = time.Unix(ar.Expires, 0)
	c.log.Debug("console api: token refreshed", "expires_at", c.expiresAt)
	return nil
}

// ListQueue returns the recent-commands list via GET /queue. Per Kamatera
// OpenAPI: "Lists active and recent commands on platform." Verified live to
// return a populated array (unlike the cloudcli /service/queue, which always
// returns []).
func (c *consoleClient) ListQueue(ctx context.Context) ([]QueueStatus, error) {
	body, err := c.bearerGet(ctx, "/queue")
	if err != nil {
		return nil, err
	}
	return decodeQueueStatuses(body)
}

// ListServers returns a brief list of servers via GET /servers. Per the
// OpenAPI each entry has id, datacenter, name, and power — enough for
// command-completion enrichment (matching command.server name to server.id).
//
// Replaces the cloudcli POST /service/server/info path, which has been
// returning intermittent 500 "Failed to run command method (serversInfo)"
// errors observed during e2e on 2026-05-02.
func (c *consoleClient) ListServers(ctx context.Context) ([]Server, error) {
	body, err := c.bearerGet(ctx, "/servers")
	if err != nil {
		return nil, err
	}
	var servers []Server
	if err := json.Unmarshal(body, &servers); err != nil {
		return nil, fmt.Errorf("decode /servers response: %w", err)
	}
	return servers, nil
}

// bearerGet does the GET with the cached token. On 401 it wipes the token,
// re-auths, and retries exactly once — that way an unexpectedly-rotated token
// doesn't surface as a transient watcher error.
func (c *consoleClient) bearerGet(ctx context.Context, path string) ([]byte, error) {
	if err := c.ensureToken(ctx); err != nil {
		return nil, err
	}
	body, err := c.bearerGetOnce(ctx, path)
	if err == nil {
		return body, nil
	}
	var ae *APIError
	if errors.As(err, &ae) && ae.Code == 401 {
		c.mu.Lock()
		c.token = ""
		c.mu.Unlock()
		if err := c.ensureToken(ctx); err != nil {
			return nil, err
		}
		return c.bearerGetOnce(ctx, path)
	}
	return nil, err
}

func (c *consoleClient) bearerGetOnce(ctx context.Context, path string) ([]byte, error) {
	c.mu.Lock()
	token := c.token
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kamatera: GET %s: %w", path, redactErr(err))
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{Method: "GET", Path: path, Code: resp.StatusCode, Body: string(raw)}
	}
	return raw, nil
}
