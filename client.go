package autotix

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Version is the SDK version, sent in the User-Agent and in each event.
const Version = "0.1.0"

// Defaults for New. Override them with the matching With option.
const (
	// DefaultEndpoint is the production server-side ingest endpoint.
	DefaultEndpoint = "https://app.autotix.io/api/webhook/error"
	// DefaultTimeout bounds each delivery request. The webhook endpoint
	// creates the ticket before it answers, so it is slower than a plain
	// ingest call; this matches the PHP integrations.
	DefaultTimeout = 15 * time.Second
	// DefaultQueueSize is how many events may wait for delivery.
	DefaultQueueSize = 100
	// DefaultWorkers is how many deliveries run at once.
	DefaultWorkers = 2
)

// Stats counts delivery outcomes since the client was created.
type Stats struct {
	// Sent is the number of events the server accepted with a 2xx.
	Sent int64
	// Failed is the number of events that got a non-2xx or a network error.
	Failed int64
	// Dropped is the number of events discarded because the queue was full
	// or the client was already closed.
	Dropped int64
}

// Client delivers events to Autotix. Create one with New and share it
// across goroutines; all methods are safe for concurrent use.
type Client struct {
	cfg       config
	transport transport
	log       *slog.Logger
	envURL    string // where ReportEnvironment posts

	// bgCtx scopes background work that is not an event (the startup
	// environment report); Close cancels it.
	bgCtx      context.Context
	bgCancel   context.CancelFunc
	background sync.WaitGroup

	mu     sync.RWMutex // guards closed and the queue's close
	closed bool
	queue  chan *Event

	workers sync.WaitGroup
	pending atomic.Int64 // queued plus in-flight events

	sent    atomic.Int64
	failed  atomic.Int64
	dropped atomic.Int64
}

// New creates a client authenticated with the org's webhook token (from
// Settings in the Autotix dashboard). The token is a server secret; keep it
// in configuration, never in client code. New returns an error when no
// credentials are given or an option is invalid.
func New(token string, opts ...Option) (*Client, error) {
	cfg := defaultConfig()
	cfg.token = token
	for _, o := range opts {
		if o != nil {
			o.applyClient(&cfg)
		}
	}
	if cfg.token == "" && cfg.hmacSecret == "" {
		cfg.errs = append(cfg.errs, ErrMissingCredentials)
	}
	if err := validateEndpoint(cfg.endpoint); err != nil {
		cfg.errs = append(cfg.errs, err)
	}
	if len(cfg.errs) > 0 {
		return nil, errors.Join(cfg.errs...)
	}
	if !cfg.gitSet {
		cfg.git = DetectGitContext()
	}
	if cfg.serverName == "" {
		cfg.serverName, _ = os.Hostname()
	}

	c := &Client{
		cfg: cfg,
		log: cfg.logger,
		transport: &httpTransport{
			client:     cfg.httpClient,
			endpoint:   cfg.endpoint,
			token:      cfg.token,
			hmacSecret: cfg.hmacSecret,
			userAgent:  "autotix-go/" + Version,
		},
		queue:  make(chan *Event, cfg.queueSize),
		envURL: envReportURL(cfg.endpoint),
	}
	c.bgCtx, c.bgCancel = context.WithCancel(context.Background())
	for i := 0; i < cfg.workers; i++ {
		c.workers.Add(1)
		go c.worker()
	}
	c.startEnvironmentReport()
	return c, nil
}

func validateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("autotix: invalid endpoint %q: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("autotix: endpoint must be an http(s) URL, got %q", raw)
	}
	return nil
}

// Capture reports an error with the stack of the calling goroutine. A nil
// error is ignored. The call returns immediately; delivery happens in the
// background.
func (c *Client) Capture(err error, opts ...EventOption) {
	if c == nil || err == nil {
		return
	}
	ev := c.newEvent(err.Error(), LevelError, CaptureStack(0), fmt.Sprintf("%T", err))
	c.enqueue(ev, opts, false)
}

// CaptureMessage reports a message that is not an error, at LevelInfo
// unless WithLevel says otherwise. No stack trace is attached, so identical
// messages dedupe together on the server.
func (c *Client) CaptureMessage(message string, opts ...EventOption) {
	if c == nil {
		return
	}
	ev := c.newEvent(message, LevelInfo, "", "")
	c.enqueue(ev, opts, false)
}

// CapturePanic reports a recovered panic value at LevelFatal. Call it from
// your own deferred recover when you want to decide what happens next, for
// example to re-panic:
//
//	defer func() {
//		if r := recover(); r != nil {
//			client.CapturePanic(r)
//			panic(r)
//		}
//	}()
func (c *Client) CapturePanic(recovered any, opts ...EventOption) {
	if c == nil || recovered == nil {
		return
	}
	ev := c.newEvent(panicMessage(recovered), LevelFatal, CaptureStack(0), fmt.Sprintf("%T", recovered))
	c.enqueue(ev, opts, false)
}

// Recover is meant to be deferred directly: it recovers a panic in the
// current goroutine, reports it, and lets the goroutine end normally. It
// must be the deferred call itself (defer client.Recover()), not called
// from inside another deferred function, or recover has no effect.
//
// The panic is swallowed even on a nil client, so disabling Autotix never
// changes how your program behaves. Use CapturePanic to re-panic instead.
func (c *Client) Recover(opts ...EventOption) {
	if rec := recover(); rec != nil {
		c.CapturePanic(rec, opts...)
	}
}

// TriggerTestError sends one clearly labeled event through the normal
// pipeline to verify the setup end to end. It bypasses sampling and tags the
// event autotix_test=true so it is easy to filter out later.
func (c *Client) TriggerTestError(opts ...EventOption) {
	if c == nil {
		return
	}
	msg := "Autotix test error (" + time.Now().UTC().Format(time.RFC3339) + ")"
	ev := c.newEvent("AutotixTestError: "+msg, LevelError, CaptureStack(0), "AutotixTestError")
	ev.Tags["autotix_test"] = "true"
	c.enqueue(ev, opts, true)
}

// Flush waits until every queued and in-flight event has been delivered or
// has failed. It returns false when ctx ends first. Call it before a
// short-lived process exits.
func (c *Client) Flush(ctx context.Context) bool {
	if c == nil || c.pending.Load() == 0 {
		return true
	}
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return c.pending.Load() == 0
		case <-tick.C:
			if c.pending.Load() == 0 {
				return true
			}
		}
	}
}

// Close stops accepting events, delivers what is already queued, and stops
// the workers. It also waits for the startup environment report if one is
// still in flight. It returns false when ctx ends first, and cancels that
// report then. Calling Close more than once is safe.
func (c *Client) Close(ctx context.Context) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.queue)
	}
	c.mu.Unlock()

	done := make(chan struct{})
	go func() {
		c.workers.Wait()
		c.background.Wait()
		close(done)
	}()
	select {
	case <-done:
		c.bgCancel()
		return true
	case <-ctx.Done():
		c.bgCancel()
		return false
	}
}

// Stats returns delivery counters.
func (c *Client) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	return Stats{
		Sent:    c.sent.Load(),
		Failed:  c.failed.Load(),
		Dropped: c.dropped.Load(),
	}
}

// --- internals --------------------------------------------------------------

func (c *Client) newEvent(message string, level Level, stack, errType string) *Event {
	ev := &Event{
		Message:     message,
		Level:       level,
		StackTrace:  stack,
		ErrorType:   errType,
		Project:     c.cfg.project,
		Environment: c.cfg.environment,
		Release:     c.cfg.release,
		ServerName:  c.cfg.serverName,
		Timestamp:   time.Now(),
		Tags:        make(map[string]string, len(c.cfg.tags)),
	}
	for k, v := range c.cfg.tags {
		ev.Tags[k] = v
	}
	if c.cfg.git != nil {
		git := *c.cfg.git
		ev.Git = &git
	}
	return ev
}

func (c *Client) enqueue(ev *Event, opts []EventOption, skipSampling bool) {
	for _, o := range opts {
		if o != nil {
			o.applyEvent(ev)
		}
	}
	if !skipSampling && c.cfg.sampleRate < 1 && rand.Float64() >= c.cfg.sampleRate {
		return
	}
	if c.cfg.beforeSend != nil {
		if ev = c.runBeforeSend(ev); ev == nil {
			return
		}
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		c.dropped.Add(1)
		return
	}
	c.pending.Add(1)
	select {
	case c.queue <- ev:
	default:
		c.pending.Add(-1)
		c.dropped.Add(1)
		c.log.Debug("Autotix: queue full, event dropped")
	}
}

// runBeforeSend shields delivery from a buggy hook: a panic inside it is
// logged and the event goes out as it was.
func (c *Client) runBeforeSend(ev *Event) (out *Event) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Warn("Autotix: beforeSend hook panicked, sending event unchanged", "panic", fmt.Sprint(r))
			out = ev
		}
	}()
	return c.cfg.beforeSend(ev)
}

func (c *Client) worker() {
	defer c.workers.Done()
	for ev := range c.queue {
		c.deliver(ev)
		c.pending.Add(-1)
	}
}

func (c *Client) deliver(ev *Event) {
	body, err := encodePayload(ev.toPayload())
	if err != nil {
		c.failed.Add(1)
		c.log.Warn("Autotix delivery failed", "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.timeout)
	defer cancel()
	status, snippet, err := c.transport.send(ctx, body)

	switch {
	case err != nil:
		c.failed.Add(1)
		c.log.Warn("Autotix delivery failed", "error", err, "endpoint", c.cfg.endpoint)
	case status < 200 || status >= 300:
		c.failed.Add(1)
		// The body only reaches debug level: it can carry detail that
		// should not land in a warning stream.
		c.log.Warn("Autotix delivery failed", "status", status, "endpoint", c.cfg.endpoint)
		c.log.Debug("Autotix: non-2xx response body", "status", status, "body", snippet)
	default:
		c.sent.Add(1)
		c.log.Debug("Autotix: delivered", "status", status, "level", string(ev.Level), "message", truncate(ev.Message, 120))
	}
}

func panicMessage(recovered any) string {
	if err, ok := recovered.(error); ok {
		return "panic: " + err.Error()
	}
	return fmt.Sprintf("panic: %v", recovered)
}
