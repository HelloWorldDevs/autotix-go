package autotix

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPerEventProjectOverridesClientDefault(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv, WithProject("WEB"))
	c.Capture(errors.New("routed elsewhere"), WithProject("API"))
	flush(t, c)
	if got := rec.wait(t, 1)[0].Wire.Details.ProjectKey; got != "API" {
		t.Fatalf("project_key = %q, want API", got)
	}
}

func TestPayloadDefaultsFieldsClearedByHook(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv, WithBeforeSend(func(e *Event) *Event {
		e.Level = ""
		e.Timestamp = time.Time{}
		return e
	}))
	c.Capture(errors.New("x"))
	flush(t, c)
	w := rec.wait(t, 1)[0].Wire
	if w.Level != "error" {
		t.Errorf("empty level should fall back to error, got %q", w.Level)
	}
	if w.Details.Timestamp == 0 {
		t.Error("zero timestamp should fall back to now")
	}
}

func TestTruncateRespectsRuneBoundaries(t *testing.T) {
	// "é" is two bytes; cutting at 3 bytes must not split the second rune.
	if got := truncate("aéé", 3); got != "aé" {
		t.Fatalf("truncate = %q, want %q", got, "aé")
	}
	if got := truncate("abc", 3); got != "abc" {
		t.Fatalf("truncate should keep strings at the limit, got %q", got)
	}
	if got := truncate("", 3); got != "" {
		t.Fatalf("truncate of empty = %q", got)
	}
}

func TestTagOptionsInitializeNilMaps(t *testing.T) {
	var e Event
	WithTag("a", "1").applyEvent(&e)
	WithTags(map[string]string{"b": "2"}).applyEvent(&e)
	WithExtra("k", 3).applyEvent(&e)
	if e.Tags["a"] != "1" || e.Tags["b"] != "2" || e.Extra["k"] != 3 {
		t.Fatalf("options did not populate maps: %+v", e)
	}
}

func TestForwardedProtoIgnoresUnknownSchemes(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Host = "h"
	r.Header.Set("X-Forwarded-Proto", "gopher")
	if got := requestURL(r); got != "http://h/x" {
		t.Fatalf("unknown forwarded proto must be ignored, got %q", got)
	}
	r.Header.Set("X-Forwarded-Proto", "HTTPS, http")
	if got := requestURL(r); got != "https://h/x" {
		t.Fatalf("first forwarded proto should win, case-insensitively, got %q", got)
	}
}

func TestUnencodableExtraCountsAsFailure(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv)
	c.Capture(errors.New("x"), WithExtra("ch", make(chan int)))
	flush(t, c)
	if s := c.Stats(); s.Failed != 1 || s.Sent != 0 {
		t.Fatalf("stats = %+v", s)
	}
	if rec.count() != 0 {
		t.Fatal("an unencodable event must not reach the server")
	}
}
