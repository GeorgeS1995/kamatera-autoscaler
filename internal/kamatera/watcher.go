package kamatera

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// cmdWatcher centralises async-command polling so K concurrent waiters cost
// 1 GET /queue + (at most) 1 GET /servers per tick, instead of K queue
// calls and K info calls.
//
// Lifecycle is lazy: the watcher goroutine starts on the first wait*() call
// and exits as soon as no commands are pending. A subsequent wait restarts
// it. This avoids a permanent idle goroutine in the controller.
//
// TODO: cmdWatcher reaches into client.console directly (w.client.console.…)
// for queue/server lookups — tight coupling to httpClient's internals. If a
// second watcher implementation ever appears (e.g. a different transport for
// long-poll), extract a small interface like
//
//	type queryAPI interface {
//	    ListQueue(ctx) ([]QueueStatus, error)
//	    ListServers(ctx) ([]Server, error)
//	}
//
// and pass it into newCmdWatcher. Not worth the abstraction yet — one
// watcher, one provider.
type cmdWatcher struct {
	mu      sync.Mutex
	pending map[string]*pendingCmd // commandID → state
	running bool

	client   *httpClient
	interval time.Duration
}

// pendingCmd is the watcher's internal record for one outstanding wait.
//
// enrichName is the only signal of "do server-info enrichment after terminal
// success" — set to a non-empty name for provisioning waits, left empty for
// terminate waits. The two callers (waitProvision / waitTerminate) set this
// field for their respective use cases; the watcher itself does not invent it.
type pendingCmd struct {
	commandID  string
	enrichName string
	ch         chan waitResult
	deadline   time.Time
}

type waitResult struct {
	server Server
	err    error
}

func newCmdWatcher(client *httpClient, interval time.Duration) *cmdWatcher {
	return &cmdWatcher{
		pending:  map[string]*pendingCmd{},
		client:   client,
		interval: interval,
	}
}

// waitProvision blocks until commandID reaches a terminal state and returns
// the fully-populated Server (with ID and IPs) for the given name.
func (w *cmdWatcher) waitProvision(ctx context.Context, commandID, name string, timeout time.Duration) (Server, error) {
	return w.register(ctx, &pendingCmd{
		commandID:  commandID,
		enrichName: name,
		ch:         make(chan waitResult, 1),
		deadline:   time.Now().Add(timeout),
	})
}

// waitTerminate blocks until commandID reaches a terminal state. The returned
// Server is always zero — it's only here to keep the result channel uniform.
func (w *cmdWatcher) waitTerminate(ctx context.Context, commandID string, timeout time.Duration) error {
	_, err := w.register(ctx, &pendingCmd{
		commandID: commandID,
		ch:        make(chan waitResult, 1),
		deadline:  time.Now().Add(timeout),
	})
	return err
}

func (w *cmdWatcher) register(ctx context.Context, cmd *pendingCmd) (Server, error) {
	if cmd.commandID == "" {
		// Defensive: in our code path decodeCommandID either returns a
		// non-empty id or an error, so the watcher should never receive an
		// empty id. Guard anyway — silently waiting forever for an empty
		// command id is the worst possible outcome.
		return Server{}, fmt.Errorf("kamatera: wait registered with empty commandID")
	}
	w.mu.Lock()
	if _, dup := w.pending[cmd.commandID]; dup {
		w.mu.Unlock()
		return Server{}, fmt.Errorf("kamatera: command %s is already being waited on", cmd.commandID)
	}
	w.pending[cmd.commandID] = cmd
	if !w.running {
		w.running = true
		go w.run()
	}
	w.mu.Unlock()

	select {
	case r := <-cmd.ch:
		return r.server, r.err
	case <-ctx.Done():
		w.cancel(cmd.commandID)
		return Server{}, ctx.Err()
	}
}

func (w *cmdWatcher) cancel(commandID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.pending, commandID)
}

func (w *cmdWatcher) run() {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		w.tick()

		w.mu.Lock()
		if len(w.pending) == 0 {
			w.running = false
			w.mu.Unlock()
			return
		}
		w.mu.Unlock()

		<-ticker.C
	}
}

func (w *cmdWatcher) tick() {
	// Per-tick context — independent from any waiter's ctx, since the
	// per-call ctx only governs how long that caller blocks, not how long
	// the watcher's own HTTP calls may take.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Snapshot pending under the lock; release before doing I/O.
	w.mu.Lock()
	snapshot := make([]*pendingCmd, 0, len(w.pending))
	pendingIDs := make([]string, 0, len(w.pending))
	for _, p := range w.pending {
		snapshot = append(snapshot, p)
		pendingIDs = append(pendingIDs, p.commandID)
	}
	w.mu.Unlock()

	if len(snapshot) == 0 {
		return
	}

	w.client.log.Debug("watcher tick start",
		"pending_count", len(snapshot), "pending_ids", pendingIDs)

	// One queue list covers all pending commands. Issued against
	// console.kamatera.com (different API from cloudcli) because cloudcli's
	// /service/queue endpoint always returns an empty array — see consoleClient.
	queue, err := w.client.console.ListQueue(ctx)
	if err != nil {
		w.client.log.Warn("watcher: queue list failed; will retry next tick", "err", err)
		w.expireDeadlines(snapshot)
		return
	}
	queueByID := make(map[string]QueueStatus, len(queue))
	queueIDs := make([]string, 0, len(queue))
	for _, q := range queue {
		queueByID[q.ID.String()] = q
		queueIDs = append(queueIDs, q.ID.String())
	}
	w.client.log.Debug("watcher: queue list response",
		"entries", len(queue), "ids", queueIDs)
	for _, p := range snapshot {
		if q, ok := queueByID[p.commandID]; ok {
			w.client.log.Debug("watcher: pending found in batch",
				"command_id", p.commandID, "status", string(q.Status),
				"is_terminal", q.Status.IsTerminal(), "is_failure", q.Status.IsFailure(),
				"server_name", q.Server)
		} else {
			w.client.log.Debug("watcher: pending NOT in batch list",
				"command_id", p.commandID)
		}
	}

	type completion struct {
		cmd    *pendingCmd
		result waitResult
	}
	var completed []completion
	var namesToEnrich []string
	now := time.Now()

	for _, p := range snapshot {
		if now.After(p.deadline) {
			completed = append(completed, completion{
				cmd:    p,
				result: waitResult{err: fmt.Errorf("kamatera: timeout waiting for command %s", p.commandID)},
			})
			continue
		}
		q, ok := queueByID[p.commandID]
		if !ok {
			// Not yet visible in the recent-commands window. Keep waiting.
			continue
		}
		if q.Status.IsFailure() {
			completed = append(completed, completion{
				cmd: p,
				result: waitResult{
					err: fmt.Errorf("kamatera: command %s ended %s: %s",
						p.commandID, q.Status, truncate(q.Log, 200)),
				},
			})
			continue
		}
		if q.Status.IsTerminal() {
			completed = append(completed, completion{
				cmd:    p,
				result: waitResult{server: Server{Name: q.Server}},
			})
			if p.enrichName != "" {
				namesToEnrich = append(namesToEnrich, p.enrichName)
			}
		}
	}

	// One info list batches enrichment for all successful provisions this tick.
	var serverByName map[string]Server
	if len(namesToEnrich) > 0 {
		servers, err := w.client.console.ListServers(ctx)
		if err != nil {
			w.client.log.Warn("watcher: server list failed; deferring enrichment", "err", err)
			// Defer enrichment: drop only the to-be-enriched completions, the
			// rest (failures and terminate-completions) still resolve this tick.
			completed = retainCompletions(completed, func(c completion) bool {
				return c.result.err != nil || c.cmd.enrichName == ""
			})
		} else {
			serverByName = make(map[string]Server, len(servers))
			for _, s := range servers {
				serverByName[s.Name] = s
			}
		}
	}

	// Apply enrichment to surviving completions.
	for i := range completed {
		c := &completed[i]
		if c.result.err != nil || c.cmd.enrichName == "" {
			continue
		}
		if srv, ok := serverByName[c.cmd.enrichName]; ok {
			c.result.server = srv
			continue
		}
		// Defensive: queue said terminal-success but the server isn't in the
		// info list. Could happen if the server was terminated concurrently
		// between the two API calls, or rarely on Kamatera-side state lag.
		c.result.err = fmt.Errorf("kamatera: command %s reached terminal state but server %q not in /service/server/info",
			c.cmd.commandID, c.cmd.enrichName)
	}

	// Deliver results, drop pending entries.
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range completed {
		if cur, ok := w.pending[c.cmd.commandID]; ok && cur == c.cmd {
			cur.ch <- c.result
			close(cur.ch)
			delete(w.pending, c.cmd.commandID)
		}
	}
}

// expireDeadlines fires timeouts on any pending commands past their deadline.
// Used when the queue list itself failed (we can't tell their status this
// tick) — without this, a watcher unable to reach the API would silently keep
// its callers blocked indefinitely.
func (w *cmdWatcher) expireDeadlines(snapshot []*pendingCmd) {
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range snapshot {
		if !now.After(p.deadline) {
			continue
		}
		if cur, ok := w.pending[p.commandID]; ok && cur == p {
			cur.ch <- waitResult{err: fmt.Errorf("kamatera: timeout waiting for command %s", p.commandID)}
			close(cur.ch)
			delete(w.pending, p.commandID)
		}
	}
}

func retainCompletions[T any](in []T, keep func(T) bool) []T {
	out := in[:0]
	for _, v := range in {
		if keep(v) {
			out = append(out, v)
		}
	}
	return out
}
