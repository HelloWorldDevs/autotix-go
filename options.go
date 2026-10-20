package autotix

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// Option configures a Client. Pass options to New.
type Option interface {
	applyClient(*config)
}

// EventOption adjusts a single event. Pass options to Capture,
// CaptureMessage, CapturePanic, Recover, or TriggerTestError.
type EventOption interface {
	applyEvent(*Event)
}

// DualOption is accepted by both New (as a default for every event) and the
// capture methods (for one event). WithProject, WithTag, and WithTags are
// dual options.
type DualOption interface {
	Option
	EventOption
}

type config struct {
	token       string
	hmacSecret  string
	endpoint    string
	timeout     time.Duration
	httpClient  *http.Client
	logger      *slog.Logger
	environment string
	release     string
	project     string
	serverName  string
	tags        map[string]string
	sampleRate  float64
	beforeSend  func(*Event) *Event
	queueSize   int
	workers     int
	git         *GitContext
	gitSet      bool
	siteURL     string
	// envReportOnStart sends one environment report from New when siteURL
	// is set.
	envReportOnStart bool
	// readBuildInfo is debug.ReadBuildInfo, swappable in tests.
	readBuildInfo func() (*debug.BuildInfo, bool)
	errs          []error
}

func defaultConfig() config {
	return config{
		endpoint:   DefaultEndpoint,
		timeout:    DefaultTimeout,
		httpClient: &http.Client{},
		logger:     slog.Default(),
		tags:       map[string]string{},
		sampleRate: 1,
		queueSize:  DefaultQueueSize,
		workers:    DefaultWorkers,

		envReportOnStart: true,
		readBuildInfo:    debug.ReadBuildInfo,
	}
}

func (c *config) fail(format string, args ...any) {
	c.errs = append(c.errs, fmt.Errorf("autotix: "+format, args...))
}

type clientOption func(*config)

func (f clientOption) applyClient(c *config) { f(c) }

type eventOption func(*Event)

func (f eventOption) applyEvent(e *Event) { f(e) }

// --- client options ---------------------------------------------------------

// WithEndpoint overrides the ingest URL. Use it for a self-hosted Autotix or
// a staging environment. Defaults to DefaultEndpoint.
func WithEndpoint(url string) Option {
	return clientOption(func(c *config) { c.endpoint = url })
}

// WithTimeout bounds each delivery request. Defaults to DefaultTimeout.
func WithTimeout(d time.Duration) Option {
	return clientOption(func(c *config) {
		if d <= 0 {
			c.fail("timeout must be positive, got %s", d)
			return
		}
		c.timeout = d
	})
}

// WithHTTPClient sends requests through the given client, for custom
// transports, proxies, or tests.
func WithHTTPClient(hc *http.Client) Option {
	return clientOption(func(c *config) {
		if hc == nil {
			c.fail("http client must not be nil")
			return
		}
		c.httpClient = hc
	})
}

// WithLogger routes the SDK's own log lines. Failed deliveries log at Warn,
// successful ones at Debug. Defaults to slog.Default. Pass a logger backed
// by io.Discard to silence the SDK.
func WithLogger(l *slog.Logger) Option {
	return clientOption(func(c *config) {
		if l != nil {
			c.logger = l
		}
	})
}

// WithEnvironment tags every event with a deployment environment such as
// "production" or "staging".
func WithEnvironment(env string) Option {
	return clientOption(func(c *config) { c.environment = env })
}

// WithRelease tags every event with an app version or commit SHA.
func WithRelease(release string) Option {
	return clientOption(func(c *config) { c.release = release })
}

// WithServerName overrides the hostname reported with each event. Defaults
// to os.Hostname.
func WithServerName(name string) Option {
	return clientOption(func(c *config) { c.serverName = name })
}

// WithSampleRate ships only the given fraction of events, from 0 (none) to
// 1 (all, the default). Sampling runs before any network work, so dropped
// events cost nothing. TriggerTestError always bypasses it.
func WithSampleRate(rate float64) Option {
	return clientOption(func(c *config) {
		if rate < 0 || rate > 1 {
			c.fail("sample rate must be between 0 and 1, got %v", rate)
			return
		}
		c.sampleRate = rate
	})
}

// WithBeforeSend runs fn on every event right before it is queued. Return
// nil to drop the event. A panic inside fn is recovered and the event is
// sent as it was.
func WithBeforeSend(fn func(*Event) *Event) Option {
	return clientOption(func(c *config) { c.beforeSend = fn })
}

// WithQueueSize bounds how many events can wait for delivery. When the queue
// is full new events are dropped and counted in Stats. Defaults to
// DefaultQueueSize.
func WithQueueSize(n int) Option {
	return clientOption(func(c *config) {
		if n < 1 {
			c.fail("queue size must be at least 1, got %d", n)
			return
		}
		c.queueSize = n
	})
}

// WithWorkers sets how many deliveries run concurrently. Defaults to
// DefaultWorkers.
func WithWorkers(n int) Option {
	return clientOption(func(c *config) {
		if n < 1 {
			c.fail("workers must be at least 1, got %d", n)
			return
		}
		c.workers = n
	})
}

// WithHMACSecret signs each request body with HMAC-SHA256 and sends the hex
// digest in X-Webhook-Signature instead of sending the token. When set, the
// token passed to New may be empty.
func WithHMACSecret(secret string) Option {
	return clientOption(func(c *config) { c.hmacSecret = secret })
}

// WithGitContext pins the pull request context attached to every event,
// replacing what DetectGitContext would find in the environment.
func WithGitContext(g GitContext) Option {
	return clientOption(func(c *config) {
		c.git = &g
		c.gitSet = true
	})
}

// WithSiteURL names the public URL of this service, such as
// "https://api.example.com", and turns on environment reporting for
// Vulnerability Scanning: New sends the Go version and the modules compiled
// into the binary once, in the background, and Autotix checks them against
// the OSV.dev advisory database. Without a site URL nothing is reported.
//
// Autotix keys each inventory on the URL's scheme, host, and port, so give
// every service its own host or port; services that share one overwrite each
// other's report. Reporting needs the webhook token, not only an HMAC secret.
func WithSiteURL(url string) Option {
	return clientOption(func(c *config) {
		if url == "" {
			c.siteURL = ""
			return
		}
		if err := validateSiteURL(url); err != nil {
			c.errs = append(c.errs, err)
			return
		}
		c.siteURL = url
	})
}

// WithEnvironmentReportOnStart controls the background environment report
// New sends when WithSiteURL is set. It defaults to true. Turn it off in a
// CLI or test that calls ReportEnvironment itself, so the inventory is not
// sent twice.
func WithEnvironmentReportOnStart(enabled bool) Option {
	return clientOption(func(c *config) { c.envReportOnStart = enabled })
}

// --- dual options -----------------------------------------------------------

type projectOption string

func (p projectOption) applyClient(c *config) { c.project = string(p) }
func (p projectOption) applyEvent(e *Event)   { e.Project = string(p) }

// WithProject routes events to a project key (a Jira key or owner/repo)
// instead of matching on the event URL. Passed to New it applies to every
// event; passed to a capture method it applies to that event. Background
// jobs, queue consumers, and CLIs should always pin a project because their
// errors carry no URL to match.
func WithProject(key string) DualOption {
	return projectOption(key)
}

type tagsOption map[string]string

func (t tagsOption) applyClient(c *config) {
	for k, v := range t {
		c.tags[k] = v
	}
}

func (t tagsOption) applyEvent(e *Event) {
	if e.Tags == nil {
		e.Tags = make(map[string]string, len(t))
	}
	for k, v := range t {
		e.Tags[k] = v
	}
}

// WithTag attaches one searchable tag. Per-event tags override client
// defaults with the same key.
func WithTag(key, value string) DualOption {
	return tagsOption{key: value}
}

// WithTags attaches several searchable tags at once.
func WithTags(tags map[string]string) DualOption {
	return tagsOption(tags)
}

// --- event options ----------------------------------------------------------

// WithLevel sets the event severity.
func WithLevel(l Level) EventOption {
	return eventOption(func(e *Event) { e.Level = l })
}

// WithExtra attaches one non-searchable detail, such as a request id.
func WithExtra(key string, value any) EventOption {
	return eventOption(func(e *Event) {
		if e.Extra == nil {
			e.Extra = map[string]any{}
		}
		e.Extra[key] = value
	})
}

// WithUser attaches the affected user.
func WithUser(u User) EventOption {
	return eventOption(func(e *Event) { e.User = &u })
}

// WithURL sets the full URL the error belongs to. Send the whole URL
// including scheme and host; a bare path matches no project.
func WithURL(url string) EventOption {
	return eventOption(func(e *Event) { e.URL = url })
}

// WithStackTrace replaces the stack the SDK captured, for errors that carry
// their own trace.
func WithStackTrace(stack string) EventOption {
	return eventOption(func(e *Event) { e.StackTrace = stack })
}

// ErrMissingCredentials is returned by New when neither a token nor an HMAC
// secret was supplied.
var ErrMissingCredentials = errors.New("autotix: a webhook token or HMAC secret is required")
