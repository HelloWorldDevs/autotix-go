package autotix

import (
	"errors"
	"net/http"
	"strings"
)

// WithRequest fills the event's URL, method, and user agent from an incoming
// request so Autotix can route it by URL. A nil request is ignored.
func WithRequest(r *http.Request) EventOption {
	return eventOption(func(e *Event) {
		if r == nil {
			return
		}
		if u := requestURL(r); u != "" {
			e.URL = u
		}
		e.RequestMethod = r.Method
		e.UserAgent = r.UserAgent()
	})
}

// Middleware reports panics raised by next, then re-panics so the recovery
// you already have (your own middleware or net/http's default) keeps
// working. Mount it closest to your handlers, inside any recovery
// middleware, so it sees the panic first. http.ErrAbortHandler is passed
// through without being reported.
func (c *Client) Middleware(next http.Handler) http.Handler {
	if c == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if !isAbortHandler(rec) {
					c.CapturePanic(rec, WithRequest(r))
				}
				panic(rec)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func isAbortHandler(rec any) bool {
	err, ok := rec.(error)
	return ok && errors.Is(err, http.ErrAbortHandler)
}

// requestURL rebuilds the full URL of an incoming request. It returns an
// empty string when there is no host, because a bare path never matches a
// project's URL patterns and would only mislead.
func requestURL(r *http.Request) string {
	if r == nil || r.URL == nil || r.Host == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := forwardedProto(r); proto != "" {
		scheme = proto
	}
	return scheme + "://" + r.Host + r.URL.RequestURI()
}

// forwardedProto honors the first X-Forwarded-Proto value when it is a
// plain http or https, as set by load balancers that terminate TLS.
func forwardedProto(r *http.Request) string {
	raw := r.Header.Get("X-Forwarded-Proto")
	if raw == "" {
		return ""
	}
	first, _, _ := strings.Cut(raw, ",")
	switch p := strings.ToLower(strings.TrimSpace(first)); p {
	case "http", "https":
		return p
	}
	return ""
}
