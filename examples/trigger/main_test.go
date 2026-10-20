package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// webhook records what the sample sends.
type webhook struct {
	status int
	mu     sync.Mutex
	tokens []string
	events []map[string]any
}

func (w *webhook) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var ev map[string]any
	_ = json.Unmarshal(body, &ev)
	w.mu.Lock()
	w.tokens = append(w.tokens, r.Header.Get("X-Webhook-Token"))
	w.events = append(w.events, ev)
	w.mu.Unlock()
	rw.WriteHeader(w.status)
}

// seen returns the tokens and events received so far.
func (w *webhook) seen() ([]string, []map[string]any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.tokens...), append([]map[string]any(nil), w.events...)
}

func serve(t *testing.T, status int) (*httptest.Server, *webhook) {
	t.Helper()
	w := &webhook{status: status}
	srv := httptest.NewServer(w)
	t.Cleanup(srv.Close)
	return srv, w
}

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestRunModes(t *testing.T) {
	cases := map[string]struct {
		message string
		level   string
	}{
		"test":  {"AutotixTestError: Autotix test error", "error"},
		"error": {"autotix sample: deliberate error from examples/trigger", "error"},
		"panic": {"panic: autotix sample: deliberate panic from examples/trigger", "fatal"},
	}
	for mode, want := range cases {
		t.Run(mode, func(t *testing.T) {
			srv, hook := serve(t, http.StatusOK)
			var stdout, stderr bytes.Buffer
			code := run([]string{"-mode", mode}, env(map[string]string{
				"AUTOTIX_WEBHOOK_TOKEN": "tok",
				"AUTOTIX_ENDPOINT":      srv.URL,
				"AUTOTIX_PROJECT":       "WEB",
			}), &stdout, &stderr)

			if code != 0 {
				t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), "sent=1 failed=0 dropped=0") {
				t.Errorf("stats missing from stdout: %s", stdout.String())
			}
			tokens, events := hook.seen()
			if len(events) != 1 {
				t.Fatalf("expected 1 event, got %d", len(events))
			}
			ev := events[0]
			if msg, _ := ev["message"].(string); !strings.HasPrefix(msg, want.message) {
				t.Errorf("message = %q, want prefix %q", msg, want.message)
			}
			if lvl, _ := ev["level"].(string); lvl != want.level {
				t.Errorf("level = %q, want %q", lvl, want.level)
			}
			details, _ := ev["details"].(map[string]any)
			if details["project_key"] != "WEB" {
				t.Errorf("project_key = %v", details["project_key"])
			}
			if stack, _ := details["stack_trace"].(string); !strings.Contains(stack, "main.") {
				t.Errorf("stack should include the sample's frames, got:\n%s", stack)
			}
			if tokens[0] != "tok" {
				t.Errorf("X-Webhook-Token = %q", tokens[0])
			}
		})
	}
}

func TestRunWithoutProjectSaysNoTicket(t *testing.T) {
	srv, _ := serve(t, http.StatusOK)
	var stdout, stderr bytes.Buffer
	code := run(nil, env(map[string]string{
		"AUTOTIX_WEBHOOK_TOKEN": "tok",
		"AUTOTIX_ENDPOINT":      srv.URL,
	}), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "without a ticket") {
		t.Errorf("stdout should explain that no project means no ticket: %s", stdout.String())
	}
}

func TestRunFailsWhenDeliveryFails(t *testing.T) {
	srv, _ := serve(t, http.StatusInternalServerError)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-mode", "error"}, env(map[string]string{
		"AUTOTIX_WEBHOOK_TOKEN": "tok",
		"AUTOTIX_ENDPOINT":      srv.URL,
	}), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "sent=0 failed=1") {
		t.Errorf("stdout: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "delivery failed") {
		t.Errorf("stderr: %s", stderr.String())
	}
}

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-h"}, env(nil), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "-mode") {
		t.Errorf("usage should list -mode:\n%s", stderr.String())
	}
}

func TestRunUsageErrors(t *testing.T) {
	cases := map[string]struct {
		args []string
		vars map[string]string
		want string
	}{
		"missing token": {nil, map[string]string{}, "AUTOTIX_WEBHOOK_TOKEN is not set"},
		"unknown mode":  {[]string{"-mode", "boom"}, map[string]string{"AUTOTIX_WEBHOOK_TOKEN": "tok"}, `unknown -mode "boom"`},
		"bad flag":      {[]string{"-nope"}, map[string]string{"AUTOTIX_WEBHOOK_TOKEN": "tok"}, "flag provided but not defined"},
		"bad endpoint": {nil, map[string]string{
			"AUTOTIX_WEBHOOK_TOKEN": "tok",
			"AUTOTIX_ENDPOINT":      "ftp://example.com",
		}, "endpoint must be an http(s) URL"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, env(tc.vars), &stdout, &stderr); code != 2 {
				t.Fatalf("exit %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr missing %q:\n%s", tc.want, stderr.String())
			}
		})
	}
}
