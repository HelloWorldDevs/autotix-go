package autotix

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// recordedRequest is one request the test server saw, decoded both ways so
// tests can make typed assertions (wire) and key-presence assertions (raw).
type recordedRequest struct {
	Header http.Header
	Body   []byte
	Wire   payload
	Raw    map[string]any
}

// recorder is an in-memory stand-in for the Autotix webhook endpoint.
type recorder struct {
	mu     sync.Mutex
	reqs   []recordedRequest
	status int
	// When non-nil, every request blocks until the channel is closed.
	block chan struct{}
}

func newRecorder(status int) *recorder {
	return &recorder{status: status}
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if r.block != nil {
		<-r.block
	}
	body, _ := io.ReadAll(req.Body)
	rec := recordedRequest{Header: req.Header.Clone(), Body: body}
	_ = json.Unmarshal(body, &rec.Wire)
	_ = json.Unmarshal(body, &rec.Raw)
	r.mu.Lock()
	r.reqs = append(r.reqs, rec)
	r.mu.Unlock()
	w.WriteHeader(r.status)
	_, _ = w.Write([]byte(`{"success":true}`))
}

// wait polls until the recorder has seen at least n requests.
func (r *recorder) wait(t *testing.T, n int) []recordedRequest {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		if len(r.reqs) >= n {
			out := append([]recordedRequest(nil), r.reqs...)
			r.mu.Unlock()
			return out
		}
		r.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t.Fatalf("expected at least %d request(s), got %d", n, len(r.reqs))
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func newServer(t *testing.T, status int) (*httptest.Server, *recorder) {
	t.Helper()
	rec := newRecorder(status)
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	return srv, rec
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newClient builds a client pointed at srv with quiet logging and a short
// timeout. Extra options are applied after the defaults so tests can override.
func newClient(t *testing.T, srv *httptest.Server, opts ...Option) *Client {
	t.Helper()
	base := []Option{
		WithEndpoint(srv.URL),
		WithTimeout(2 * time.Second),
		WithLogger(discardLogger()),
	}
	c, err := New("test-token", append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close(testCtx(t)) })
	return c
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func flush(t *testing.T, c *Client) {
	t.Helper()
	if !c.Flush(testCtx(t)) {
		t.Fatal("Flush timed out")
	}
}
