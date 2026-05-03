package kamatera

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GeorgeS1995/kamatera-autoscaler/internal/config"
)

func testCreds() config.Credentials {
	return setCredsViaEnv(config.Credentials{})
}

// setCredsViaEnv builds a config.Credentials by routing through the env-loader,
// so the unexported fields are populated the same way as in production.
func setCredsViaEnv(_ config.Credentials) config.Credentials {
	withEnv := func(k, v string, body func()) {
		old, ok := lookupEnv(k)
		setEnv(k, v)
		defer func() {
			if ok {
				setEnv(k, old)
			} else {
				unsetEnv(k)
			}
		}()
		body()
	}
	var c config.Credentials
	withEnv("KAMATERA_API_CLIENT_ID", "test-client", func() {
		withEnv("KAMATERA_API_SECRET", "test-secret", func() {
			withEnv("AUTOSCALER_JOIN_TOKEN", "tok", func() {
				withEnv("SSH_PUB_KEY", "ssh-ed25519 AAAA", func() {
					var err error
					c, err = config.LoadCredentials()
					if err != nil {
						panic(err)
					}
				})
			})
		})
	})
	return c
}

func TestCreateServer_HappyPath_AssertHeadersAndBody(t *testing.T) {
	creds := testCreds()
	var seenAuthClient, seenAuthSecret string
	var seenBody CreateServerRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuthClient = r.Header.Get("AuthClientId")
		seenAuthSecret = r.Header.Get("AuthSecret")
		if r.URL.Path != "/service/server" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &seenBody)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`["cmd-123"]`))
	}))
	defer srv.Close()

	c := NewClient(creds, WithBaseURL(srv.URL), WithMaxRetries(0))
	id, err := c.CreateServer(context.Background(), CreateServerRequest{
		Name: "node-a", Datacenter: "EU-FR", Image: "ubuntu_server_24.04_64-bit",
		CPU: "2B", RAM: 2048, Disk: "size=20", Network: "name=wan,ip=auto",
		BillingCycle: "hourly", PowerOn: "yes",
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if id != "cmd-123" {
		t.Errorf("commandID = %q, want cmd-123", id)
	}
	if seenAuthClient != "test-client" || seenAuthSecret != "test-secret" {
		t.Errorf("auth headers wrong: %q / %q", seenAuthClient, seenAuthSecret)
	}
	if seenBody.CPU != "2B" || seenBody.Datacenter != "EU-FR" {
		t.Errorf("unexpected body: %+v", seenBody)
	}
}

func TestCreateServer_NoRetryOn5xx(t *testing.T) {
	// Non-idempotent create must NOT retry — that's the whole reason for the
	// recovery loop. Verify that a 5xx fails after exactly one attempt.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(503)
	}))
	defer srv.Close()

	c := NewClient(testCreds(), WithBaseURL(srv.URL))
	_, err := c.CreateServer(context.Background(), CreateServerRequest{Name: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls = %d, want 1 (no retry on POST /service/server)", got)
	}
	if !IsServerError(err) {
		t.Errorf("expected ServerError, got %v", err)
	}
}

func TestCreateServer_ClientError_NoRetry_Returns4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
	}))
	defer srv.Close()

	c := NewClient(testCreds(), WithBaseURL(srv.URL))
	_, err := c.CreateServer(context.Background(), CreateServerRequest{Name: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
	if !IsClientError(err) {
		t.Errorf("expected ClientError, got %v", err)
	}
}

func TestCreateServer_TransportFailure_RecoversWhenServerExists(t *testing.T) {
	// Simulate POST /service/server hanging at transport layer, then the
	// recovery loop finding the server name via console GET /servers.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fakeAuthHandler(w, r) {
			return
		}
		switch {
		case r.URL.Path == "/service/server" && r.Method == http.MethodPost:
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("hijacker not supported")
			}
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		case r.Method == http.MethodGet && r.URL.Path == "/servers":
			_, _ = w.Write([]byte(`[{"id":"sid-42","name":"recovered-node"}]`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(testCreds(),
		WithBaseURL(srv.URL), WithConsoleBaseURL(srv.URL),
		WithRecoveryParams(time.Millisecond, time.Millisecond, 200*time.Millisecond))
	_, err := c.CreateServer(context.Background(), CreateServerRequest{Name: "recovered-node"})
	if err == nil {
		t.Fatal("expected ErrCreateRecovered, got nil")
	}
	var rec *ErrCreateRecovered
	if !errors.As(err, &rec) {
		t.Fatalf("expected *ErrCreateRecovered, got %T: %v", err, err)
	}
	if rec.Server.ID != "sid-42" {
		t.Errorf("recovered server ID = %q, want sid-42", rec.Server.ID)
	}
}

func TestCreateServer_TransportFailure_NotFound_ReturnsOriginal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fakeAuthHandler(w, r) {
			return
		}
		switch {
		case r.URL.Path == "/service/server" && r.Method == http.MethodPost:
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("hijacker not supported")
			}
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		case r.Method == http.MethodGet && r.URL.Path == "/servers":
			_, _ = w.Write([]byte(`[]`)) // never appears
		}
	}))
	defer srv.Close()

	c := NewClient(testCreds(),
		WithBaseURL(srv.URL), WithConsoleBaseURL(srv.URL),
		WithRecoveryParams(time.Millisecond, 5*time.Millisecond, 50*time.Millisecond))
	_, err := c.CreateServer(context.Background(), CreateServerRequest{Name: "phantom"})
	if err == nil {
		t.Fatal("expected error")
	}
	var rec *ErrCreateRecovered
	if errors.As(err, &rec) {
		t.Errorf("should NOT recover when server is not found, got %v", err)
	}
}

func TestTerminate_RetriesOn5xxThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`["term-1"]`))
	}))
	defer srv.Close()

	c := NewClient(testCreds(), WithBaseURL(srv.URL),
		WithMaxRetries(3), WithRetryBaseDelay(time.Millisecond))
	id, err := c.TerminateServer(context.Background(), "42")
	if err != nil {
		t.Fatalf("TerminateServer: %v", err)
	}
	if id != "term-1" {
		t.Errorf("id = %q", id)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestTerminate_RejectsEmptyID(t *testing.T) {
	c := NewClient(testCreds())
	if _, err := c.TerminateServer(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty id")
	}
}

func TestFindServerByName_HappyAndNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fakeAuthHandler(w, r) {
			return
		}
		_, _ = w.Write([]byte(`[{"id":"1","name":"alpha"},{"id":"2","name":"beta"}]`))
	}))
	defer srv.Close()
	c := NewClient(testCreds(),
		WithBaseURL(srv.URL), WithConsoleBaseURL(srv.URL), WithMaxRetries(0))
	got, err := c.FindServerByName(context.Background(), "beta")
	if err != nil {
		t.Fatalf("FindServerByName: %v", err)
	}
	if got.ID != "2" {
		t.Errorf("ID = %q", got.ID)
	}
	_, err = c.FindServerByName(context.Background(), "missing")
	if !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("expected ErrServerNotFound, got %v", err)
	}
	var snf *ServerNotFoundError
	if !errors.As(err, &snf) || snf.Name != "missing" {
		t.Errorf("expected typed ServerNotFoundError with name 'missing', got %v", err)
	}
}

// fakeAuthHandler responds to POST /authenticate with a fake bearer token.
// Mounted by tests that exercise watcher paths (which now go through
// console.kamatera.com auth). Returns true if it handled the request.
func fakeAuthHandler(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/authenticate" {
		return false
	}
	expires := time.Now().Add(time.Hour).Unix()
	fmt.Fprintf(w, `{"authentication":"fake-token","expires":%d}`, expires)
	return true
}

func TestWaitProvision_BatchesViaQueueAndInfo(t *testing.T) {
	// Verify the watcher path: one GET /queue per tick (console API), one
	// POST /service/server/info (cloudcli) to enrich after terminal-success.
	var queueCalls, infoCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fakeAuthHandler(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/queue":
			n := atomic.AddInt32(&queueCalls, 1)
			if n == 1 {
				_, _ = w.Write([]byte(`[{"id":42,"status":"progress","server":"node-a"}]`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":42,"status":"complete","server":"node-a"}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/servers":
			atomic.AddInt32(&infoCalls, 1)
			_, _ = w.Write([]byte(`[{"id":"sid-42","name":"node-a","power":"on"}]`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(testCreds(),
		WithBaseURL(srv.URL), WithConsoleBaseURL(srv.URL),
		WithQueuePollInterval(20*time.Millisecond))
	got, err := c.WaitProvision(context.Background(), "42", "node-a", 5*time.Second)
	if err != nil {
		t.Fatalf("WaitProvision: %v", err)
	}
	if got.ID != "sid-42" || got.Name != "node-a" {
		t.Errorf("server: %+v", got)
	}
	if atomic.LoadInt32(&queueCalls) < 2 {
		t.Errorf("expected >=2 queue calls, got %d", queueCalls)
	}
	if got := atomic.LoadInt32(&infoCalls); got != 1 {
		t.Errorf("info calls = %d, want 1 (single batched enrichment)", got)
	}
}

func TestWaitProvision_TerminalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fakeAuthHandler(w, r) {
			return
		}
		_, _ = w.Write([]byte(`[{"id":42,"status":"error","server":"node-a","log":"oops"}]`))
	}))
	defer srv.Close()
	c := NewClient(testCreds(),
		WithBaseURL(srv.URL), WithConsoleBaseURL(srv.URL),
		WithQueuePollInterval(time.Millisecond))
	_, err := c.WaitProvision(context.Background(), "42", "node-a", time.Second)
	if err == nil || !strings.Contains(err.Error(), "error") {
		t.Fatalf("expected terminal error, got %v", err)
	}
}

func TestWaitProvision_CtxCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fakeAuthHandler(w, r) {
			return
		}
		_, _ = w.Write([]byte(`[{"id":42,"status":"progress","server":"node-a"}]`))
	}))
	defer srv.Close()
	c := NewClient(testCreds(),
		WithBaseURL(srv.URL), WithConsoleBaseURL(srv.URL),
		WithQueuePollInterval(50*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err := c.WaitProvision(ctx, "42", "node-a", 5*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestWaitProvision_BatchesMultipleConcurrent(t *testing.T) {
	var queueCalls, infoCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fakeAuthHandler(w, r) {
			return
		}
		switch r.URL.Path {
		case "/queue":
			atomic.AddInt32(&queueCalls, 1)
			_, _ = w.Write([]byte(`[
				{"id":1,"status":"complete","server":"node-1"},
				{"id":2,"status":"complete","server":"node-2"},
				{"id":3,"status":"complete","server":"node-3"}
			]`))
		case "/servers":
			atomic.AddInt32(&infoCalls, 1)
			_, _ = w.Write([]byte(`[
				{"id":"sid-1","name":"node-1"},
				{"id":"sid-2","name":"node-2"},
				{"id":"sid-3","name":"node-3"}
			]`))
		}
	}))
	defer srv.Close()

	c := NewClient(testCreds(),
		WithBaseURL(srv.URL), WithConsoleBaseURL(srv.URL),
		WithQueuePollInterval(20*time.Millisecond))
	type result struct {
		s   Server
		err error
	}
	results := make(chan result, 3)
	for i, name := range []string{"node-1", "node-2", "node-3"} {
		go func(cmd, name string) {
			s, err := c.WaitProvision(context.Background(), cmd, name, 5*time.Second)
			results <- result{s, err}
		}(map[int]string{0: "1", 1: "2", 2: "3"}[i], name)
	}
	for i := 0; i < 3; i++ {
		r := <-results
		if r.err != nil {
			t.Errorf("WaitProvision: %v", r.err)
		}
		if r.s.ID == "" {
			t.Errorf("empty server ID")
		}
	}
	if atomic.LoadInt32(&queueCalls) > 3 {
		t.Errorf("queue calls = %d (excessive)", queueCalls)
	}
	if atomic.LoadInt32(&infoCalls) > 2 {
		t.Errorf("info calls = %d, want ≤2 (batched)", infoCalls)
	}
}

func TestWaitTerminate_NoEnrichmentCall(t *testing.T) {
	var infoCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fakeAuthHandler(w, r) {
			return
		}
		switch r.URL.Path {
		case "/queue":
			_, _ = w.Write([]byte(`[{"id":99,"status":"complete","server":"node-a","description":"Terminate Server"}]`))
		case "/servers":
			atomic.AddInt32(&infoCalls, 1)
		}
	}))
	defer srv.Close()
	c := NewClient(testCreds(),
		WithBaseURL(srv.URL), WithConsoleBaseURL(srv.URL),
		WithQueuePollInterval(time.Millisecond))
	if err := c.WaitTerminate(context.Background(), "99", time.Second); err != nil {
		t.Fatalf("WaitTerminate: %v", err)
	}
	if got := atomic.LoadInt32(&infoCalls); got != 0 {
		t.Errorf("expected 0 info calls for terminate, got %d", got)
	}
}

func TestDecodeCommandID_Variants(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`["abc"]`, "abc"},
		{`[42]`, "42"},
		{`"single"`, "single"},
		{`{"commandIds":["xyz"]}`, "xyz"},
		{`{"id":"obj"}`, "obj"},
	}
	for _, tc := range cases {
		got, err := decodeCommandID([]byte(tc.body))
		if err != nil {
			t.Errorf("body %q: %v", tc.body, err)
		}
		if got != tc.want {
			t.Errorf("body %q: got %q want %q", tc.body, got, tc.want)
		}
	}
	if _, err := decodeCommandID([]byte(`{"unrelated":"x"}`)); err == nil {
		t.Error("expected error for body with no command id")
	}
}

func TestRedactErr(t *testing.T) {
	got := redactErr(errors.New("dial tcp: AuthSecret: foobar")).Error()
	if !strings.Contains(got, "<redacted>") {
		t.Errorf("error not redacted: %s", got)
	}
}

func TestErrorClassifiers(t *testing.T) {
	if IsTransportError(nil) {
		t.Error("IsTransportError(nil) must be false")
	}
	if IsClientError(nil) {
		t.Error("IsClientError(nil) must be false")
	}
	transportErr := errors.New("dial tcp: i/o timeout")
	if !IsTransportError(transportErr) {
		t.Error("transport error not classified as transport")
	}
	if IsClientError(transportErr) || IsServerError(transportErr) {
		t.Error("transport error misclassified as HTTP")
	}

	clientErr := &APIError{Method: "POST", Path: "/x", Code: 400}
	if !IsClientError(clientErr) {
		t.Error("400 not classified as client")
	}
	if IsServerError(clientErr) || IsTransportError(clientErr) {
		t.Error("400 misclassified")
	}

	serverErr := &APIError{Method: "GET", Path: "/y", Code: 503}
	if !IsServerError(serverErr) {
		t.Error("503 not classified as server")
	}
}
