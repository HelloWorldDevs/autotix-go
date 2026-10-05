package autotix

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithRequestFillsRequestFields(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv)

	req := httptest.NewRequest(http.MethodPost, "/checkout?step=2", nil)
	req.Host = "shop.example.com"
	req.Header.Set("User-Agent", "Mozilla/5.0 test")
	c.Capture(errors.New("card declined"), WithRequest(req))
	flush(t, c)

	w := rec.wait(t, 1)[0].Wire
	if w.URL != "http://shop.example.com/checkout?step=2" {
		t.Errorf("url = %q", w.URL)
	}
	if w.Details.RequestMethod != "POST" {
		t.Errorf("request_method = %q", w.Details.RequestMethod)
	}
	if w.Details.UserAgent != "Mozilla/5.0 test" {
		t.Errorf("user_agent = %q", w.Details.UserAgent)
	}
}

func TestRequestURLSchemeDetection(t *testing.T) {
	plain := httptest.NewRequest(http.MethodGet, "/a", nil)
	plain.Host = "h"
	if got := requestURL(plain); got != "http://h/a" {
		t.Errorf("plain = %q", got)
	}

	tlsReq := httptest.NewRequest(http.MethodGet, "/a", nil)
	tlsReq.Host = "h"
	tlsReq.TLS = &tls.ConnectionState{}
	if got := requestURL(tlsReq); got != "https://h/a" {
		t.Errorf("tls = %q", got)
	}

	fwd := httptest.NewRequest(http.MethodGet, "/a", nil)
	fwd.Host = "h"
	fwd.Header.Set("X-Forwarded-Proto", "https")
	if got := requestURL(fwd); got != "https://h/a" {
		t.Errorf("forwarded = %q", got)
	}

	if got := requestURL(nil); got != "" {
		t.Errorf("nil request = %q", got)
	}
	noHost := httptest.NewRequest(http.MethodGet, "/a", nil)
	noHost.Host = ""
	if got := requestURL(noHost); got != "" {
		t.Errorf("no host should yield no URL (bare paths never route), got %q", got)
	}
}

func TestWithRequestNilIsNoop(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv)
	c.Capture(errors.New("x"), WithRequest(nil))
	flush(t, c)
	if got := rec.wait(t, 1)[0].Wire.URL; got != "" {
		t.Fatalf("url = %q", got)
	}
}

func TestMiddlewareCapturesPanicAndRepanics(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv)

	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("handler blew up")
	}))

	req := httptest.NewRequest(http.MethodGet, "/orders/7", nil)
	req.Host = "api.example.com"
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	if recovered != "handler blew up" {
		t.Fatalf("middleware must re-panic with the original value, got %v", recovered)
	}

	flush(t, c)
	w := rec.wait(t, 1)[0].Wire
	if w.Message != "panic: handler blew up" || w.Level != "fatal" {
		t.Errorf("event = %+v", w)
	}
	if w.URL != "http://api.example.com/orders/7" {
		t.Errorf("url = %q", w.URL)
	}
	if w.Details.RequestMethod != "GET" {
		t.Errorf("request_method = %q", w.Details.RequestMethod)
	}
}

func TestMiddlewareIgnoresErrAbortHandler(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv)
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	if recovered != http.ErrAbortHandler {
		t.Fatal("ErrAbortHandler must propagate")
	}
	flush(t, c)
	if rec.count() != 0 {
		t.Fatal("ErrAbortHandler is a sanctioned abort, not an error to report")
	}
}

func TestMiddlewarePassesThroughNormally(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv)
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusTeapot {
		t.Fatalf("status = %d", rr.Code)
	}
	flush(t, c)
	if rec.count() != 0 {
		t.Fatal("no panic, nothing to report")
	}
}
