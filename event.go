package autotix

import (
	"bytes"
	"encoding/json"
	"runtime"
	"time"
	"unicode/utf8"
)

// Source is the webhook `source` value every event carries. The Autotix
// dashboard labels Go events with it and dedup keys include it.
const Source = "go"

// Level is the severity of an event.
type Level string

// Severity levels, from most to least severe.
const (
	LevelFatal   Level = "fatal"
	LevelError   Level = "error"
	LevelWarning Level = "warning"
	LevelInfo    Level = "info"
	LevelDebug   Level = "debug"
)

// User identifies the person affected by an event. Send only what you need.
type User struct {
	ID       string `json:"id,omitempty"`
	Email    string `json:"email,omitempty"`
	Username string `json:"username,omitempty"`
}

// GitContext names the pull request an error was observed inside, so Autotix
// can comment on the PR instead of opening a ticket. It is detected from the
// environment at startup (see DetectGitContext) or set with WithGitContext.
type GitContext struct {
	// Provider is the lowercase host name: "github", "gitlab", "bitbucket".
	// Autotix acts on GitHub today and ignores the rest.
	Provider string `json:"provider"`
	// PR is the pull or merge request number.
	PR int `json:"pr"`
	// Ref is the branch name, informational only.
	Ref string `json:"ref,omitempty"`
	// Preview is a label for the preview environment, such as a Tugboat
	// preview name.
	Preview string `json:"preview,omitempty"`
}

// Event is one error as the SDK sees it before encoding. A WithBeforeSend
// hook receives a pointer to it and may change any field or drop it.
type Event struct {
	// Message is the error text. Required.
	Message string
	// Level defaults to LevelError for errors, LevelInfo for messages, and
	// LevelFatal for panics.
	Level Level
	// StackTrace is formatted like a Go panic trace: a function name line
	// followed by a tab-indented file:line.
	StackTrace string
	// ErrorType is the Go type of the captured value, such as "*fs.PathError".
	ErrorType string
	// URL is the full request URL the error belongs to. Autotix matches it
	// against each project's URL patterns when no Project is pinned.
	URL string
	// RequestMethod and UserAgent are filled by WithRequest.
	RequestMethod string
	UserAgent     string
	// Project pins the event to a project key (a Jira key or owner/repo),
	// skipping URL matching. Background jobs should always set one.
	Project     string
	Environment string
	Release     string
	// ServerName is the reporting host, os.Hostname by default.
	ServerName string
	Timestamp  time.Time
	// Tags are searchable key/value pairs.
	Tags map[string]string
	// Extra holds any other detail worth attaching.
	Extra map[string]any
	User  *User
	Git   *GitContext
}

// Caps mirror the server's normalizer so a huge message never gets a
// request rejected outright.
const (
	maxMessageLen = 4_000
	maxStackLen   = 16_000
)

// payload is the generic webhook wire format, the same shape the PHP
// modules send to /api/webhook/error.
type payload struct {
	Source  string  `json:"source"`
	Level   string  `json:"level"`
	Message string  `json:"message"`
	URL     string  `json:"url"`
	Details details `json:"details"`
}

type details struct {
	StackTrace    string            `json:"stack_trace,omitempty"`
	ErrorType     string            `json:"error_type,omitempty"`
	Environment   string            `json:"environment,omitempty"`
	Release       string            `json:"release,omitempty"`
	ProjectKey    string            `json:"project_key,omitempty"`
	RequestMethod string            `json:"request_method,omitempty"`
	UserAgent     string            `json:"user_agent,omitempty"`
	Hostname      string            `json:"hostname,omitempty"`
	GoVersion     string            `json:"go_version,omitempty"`
	SDKVersion    string            `json:"sdk_version,omitempty"`
	Timestamp     int64             `json:"timestamp"`
	Tags          map[string]string `json:"tags,omitempty"`
	Extra         map[string]any    `json:"extra,omitempty"`
	User          *User             `json:"user,omitempty"`
	Git           *GitContext       `json:"git,omitempty"`
}

func (e *Event) toPayload() payload {
	level := e.Level
	if level == "" {
		level = LevelError
	}
	ts := e.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	return payload{
		Source:  Source,
		Level:   string(level),
		Message: truncate(e.Message, maxMessageLen),
		URL:     e.URL,
		Details: details{
			StackTrace:    truncate(e.StackTrace, maxStackLen),
			ErrorType:     e.ErrorType,
			Environment:   e.Environment,
			Release:       e.Release,
			ProjectKey:    e.Project,
			RequestMethod: e.RequestMethod,
			UserAgent:     e.UserAgent,
			Hostname:      e.ServerName,
			GoVersion:     runtime.Version(),
			SDKVersion:    Version,
			Timestamp:     ts.Unix(),
			Tags:          e.Tags,
			Extra:         e.Extra,
			User:          e.User,
			Git:           e.Git,
		},
	}
}

// encodePayload marshals without HTML escaping so "<", ">" and "&" in error
// text arrive verbatim, matching the PHP SDK's JSON_UNESCAPED_SLASHES output.
func encodePayload(p payload) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// truncate caps s at max bytes without splitting a multi-byte rune.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
