package autotix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- helpers ----------------------------------------------------------------

// envRequest is one request the fake environment endpoint saw.
type envRequest struct {
	Method string
	Path   string
	Header http.Header
	Report envReport
}

// envServer stands in for /api/telemetry/env.
type envServer struct {
	mu     sync.Mutex
	reqs   []envRequest
	status int
	body   string
	// When non-nil, each request waits until it is closed or the client
	// gives up on the request.
	block chan struct{}
}

func (s *envServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.block != nil {
		select {
		case <-s.block:
		case <-r.Context().Done():
			return
		}
	}
	raw, _ := io.ReadAll(r.Body)
	req := envRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone()}
	_ = json.Unmarshal(raw, &req.Report)
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	w.WriteHeader(s.status)
	_, _ = io.WriteString(w, s.body)
}

func (s *envServer) requests() []envRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]envRequest(nil), s.reqs...)
}

func (s *envServer) wait(t *testing.T, n int) []envRequest {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if reqs := s.requests(); len(reqs) >= n {
			return reqs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected at least %d environment report(s), got %d", n, len(s.requests()))
	return nil
}

const storedBody = `{"stored":true,"changed":true,"nextCheckDays":30}`

func newEnvServer(t *testing.T, status int, body string) (*httptest.Server, *envServer) {
	t.Helper()
	es := &envServer{status: status, body: body}
	srv := httptest.NewServer(es)
	t.Cleanup(srv.Close)
	return srv, es
}

// syncBuffer is a log sink that the background report goroutine can write
// to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func debugLogger(buf *syncBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func withBuildInfo(info *debug.BuildInfo) Option {
	return clientOption(func(c *config) {
		c.readBuildInfo = func() (*debug.BuildInfo, bool) { return info, info != nil }
	})
}

func fakeBuildInfo() *debug.BuildInfo {
	return &debug.BuildInfo{
		GoVersion: "go1.22.3",
		Main:      debug.Module{Path: "example.com/app", Version: "(devel)"},
		Deps: []*debug.Module{
			{Path: "golang.org/x/net", Version: "v0.17.0"},
			{Path: "github.com/docker/docker", Version: "v20.10.0+incompatible"},
			{Path: "golang.org/x/text", Version: "v0.0.0-20230101000000-abcdefabcdef"},
			// Replaced by a fork: the fork is the code that runs.
			{Path: "github.com/orig/lib", Version: "v1.0.0",
				Replace: &debug.Module{Path: "github.com/fork/lib", Version: "v1.0.1"}},
			// Replaced by a local directory: nothing to look up.
			{Path: "github.com/local/lib", Version: "v1.2.0",
				Replace: &debug.Module{Path: "../lib"}},
			{Path: "example.com/devel", Version: "(devel)"},
			nil,
			{Path: "example.com/app", Version: "v1.0.0"}, // the main module
		},
	}
}

func newEnvClient(t *testing.T, srv *httptest.Server, opts ...Option) *Client {
	t.Helper()
	base := []Option{
		WithEndpoint(srv.URL + "/api/webhook/error"),
		WithTimeout(2 * time.Second),
		WithLogger(discardLogger()),
		WithSiteURL("https://api.example.com"),
		WithEnvironmentReportOnStart(false),
		withBuildInfo(fakeBuildInfo()),
	}
	c, err := New("test-token", append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close(testCtx(t)) })
	return c
}

// --- report building --------------------------------------------------------

func TestOSVGoVersion(t *testing.T) {
	cases := map[string]string{
		"go1.22.3":                "1.22.3",
		"go1.20":                  "1.20.0",
		"go1.22rc1":               "1.22.0-rc.1",
		"go1.21beta2":             "1.21.0-beta.2",
		"go1.22.0 X:boringcrypto": "1.22.0",
		"devel go1.23-abc123 Tue": "",
		"(devel)":                 "",
		"1.22.3":                  "",
		"":                        "",
	}
	for in, want := range cases {
		if got := osvGoVersion(in); got != want {
			t.Errorf("osvGoVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModulePackages(t *testing.T) {
	got := modulePackages(fakeBuildInfo())
	want := []envPackage{
		{"golang.org/x/net", "0.17.0", "Go"},
		{"github.com/docker/docker", "20.10.0+incompatible", "Go"},
		{"golang.org/x/text", "0.0.0-20230101000000-abcdefabcdef", "Go"},
		{"github.com/fork/lib", "1.0.1", "Go"},
	}
	if len(got) != len(want) {
		t.Fatalf("modulePackages = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("package %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	if pkgs := modulePackages(nil); pkgs == nil || len(pkgs) != 0 {
		t.Errorf("modulePackages(nil) = %#v, want an empty non-nil slice", pkgs)
	}
}

func TestBuildEnvReport(t *testing.T) {
	r := buildEnvReport("https://api.example.com", "WEB", fakeBuildInfo(), "go1.22.3")
	if r.Platform != "go" || r.PlatformVersion != "1.22.3" || r.Runtime != "go 1.22.3" {
		t.Errorf("platform/version/runtime = %q/%q/%q", r.Platform, r.PlatformVersion, r.Runtime)
	}
	if r.SiteURL != "https://api.example.com" || r.ProjectKey != "WEB" {
		t.Errorf("siteUrl/projectKey = %q/%q", r.SiteURL, r.ProjectKey)
	}
	var names []string
	for _, p := range r.Packages {
		names = append(names, p.Name+"@"+p.Version)
		if p.Ecosystem != "Go" {
			t.Errorf("%s ecosystem = %q", p.Name, p.Ecosystem)
		}
	}
	want := "github.com/docker/docker@20.10.0+incompatible github.com/fork/lib@1.0.1 " +
		"golang.org/x/net@0.17.0 golang.org/x/text@0.0.0-20230101000000-abcdefabcdef stdlib@1.22.3"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("packages (sorted) =\n %s\nwant\n %s", got, want)
	}
}

func TestBuildEnvReportDevelToolchain(t *testing.T) {
	r := buildEnvReport("https://api.example.com", "", nil, "devel go1.23-abc123")
	if r.PlatformVersion != "devel go1.23-abc123" || r.Runtime != "go devel go1.23-abc123" {
		t.Errorf("platformVersion/runtime = %q/%q", r.PlatformVersion, r.Runtime)
	}
	if len(r.Packages) != 0 {
		t.Errorf("a devel toolchain has no stdlib release to report, got %+v", r.Packages)
	}
	body, _ := json.Marshal(r)
	if !strings.Contains(string(body), `"packages":[]`) {
		t.Errorf("packages must encode as an empty array, got %s", body)
	}
	if strings.Contains(string(body), "projectKey") {
		t.Errorf("an unset project must be omitted, got %s", body)
	}
}

func TestEnvReportURL(t *testing.T) {
	cases := map[string]string{
		"https://app.autotix.io/api/webhook/error":           "https://app.autotix.io/api/telemetry/env",
		"https://x.example.com/autotix/api/webhook/error/":   "https://x.example.com/autotix/api/telemetry/env",
		"http://127.0.0.1:8080":                              "http://127.0.0.1:8080/api/telemetry/env",
		"https://x.example.com/custom/ingest?token=abc#frag": "https://x.example.com/api/telemetry/env",
		"::not a url": "",
	}
	for in, want := range cases {
		if got := envReportURL(in); got != want {
			t.Errorf("envReportURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWithSiteURLValidates(t *testing.T) {
	for _, bad := range []string{"ftp://example.com", "example.com", "::nope"} {
		if _, err := New("tok", WithSiteURL(bad)); err == nil {
			t.Errorf("WithSiteURL(%q) should fail", bad)
		}
	}
	c, err := New("tok", WithSiteURL(""), WithEnvironmentReportOnStart(false))
	if err != nil {
		t.Fatalf("an empty site URL leaves reporting off: %v", err)
	}
	c.Close(testCtx(t))
}

// --- ReportEnvironment ------------------------------------------------------

func TestReportEnvironmentSendsReport(t *testing.T) {
	srv, es := newEnvServer(t, http.StatusOK, storedBody)
	c := newEnvClient(t, srv, WithProject("WEB"))

	res, err := c.ReportEnvironment(context.Background())
	if err != nil {
		t.Fatalf("ReportEnvironment: %v", err)
	}
	if res.Status != 200 || !res.Stored || !res.Changed || res.NextCheckDays != 30 {
		t.Errorf("result = %+v", res)
	}

	reqs := es.requests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
	got := reqs[0]
	if got.Method != http.MethodPost || got.Path != "/api/telemetry/env" {
		t.Errorf("%s %s", got.Method, got.Path)
	}
	if h := got.Header.Get("X-Webhook-Token"); h != "test-token" {
		t.Errorf("X-Webhook-Token = %q", h)
	}
	if h := got.Header.Get("Content-Type"); h != "application/json" {
		t.Errorf("Content-Type = %q", h)
	}
	if h := got.Header.Get("User-Agent"); h != "autotix-go/"+Version {
		t.Errorf("User-Agent = %q", h)
	}

	rep := got.Report
	if rep.SiteURL != "https://api.example.com" || rep.Platform != "go" || rep.ProjectKey != "WEB" {
		t.Errorf("siteUrl/platform/projectKey = %q/%q/%q", rep.SiteURL, rep.Platform, rep.ProjectKey)
	}
	// The real toolchain version, in OSV's form.
	goVer := osvGoVersion(runtime.Version())
	if goVer != "" && (rep.PlatformVersion != goVer || rep.Runtime != "go "+goVer) {
		t.Errorf("platformVersion/runtime = %q/%q, want %q", rep.PlatformVersion, rep.Runtime, goVer)
	}
	wantPkgs := 4
	if goVer != "" {
		wantPkgs++ // stdlib
	}
	if len(rep.Packages) != wantPkgs || res.Packages != wantPkgs {
		t.Errorf("sent %d packages (result says %d), want %d: %+v", len(rep.Packages), res.Packages, wantPkgs, rep.Packages)
	}
	foundStdlib := false
	for _, p := range rep.Packages {
		if p.Name == "stdlib" {
			foundStdlib = p.Version == goVer && p.Ecosystem == "Go"
		}
	}
	if goVer != "" && !foundStdlib {
		t.Errorf("stdlib@%s missing from %+v", goVer, rep.Packages)
	}
}

func TestReportEnvironmentNotStored(t *testing.T) {
	srv, _ := newEnvServer(t, http.StatusOK,
		`{"stored":false,"reason":"vuln_scan feature is not enabled for this project.","nextCheckDays":90}`)
	c := newEnvClient(t, srv)

	res, err := c.ReportEnvironment(context.Background())
	if err != nil {
		t.Fatalf("a declined report is not an error: %v", err)
	}
	if res.Stored || res.NextCheckDays != 90 || !strings.Contains(res.Reason, "not enabled") {
		t.Errorf("result = %+v", res)
	}
}

func TestReportEnvironmentRejected(t *testing.T) {
	srv, _ := newEnvServer(t, http.StatusBadRequest, `{"error":"siteUrl must be http(s)"}`)
	c := newEnvClient(t, srv)
	res, err := c.ReportEnvironment(context.Background())
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "siteUrl must be http(s)") {
		t.Fatalf("err = %v", err)
	}
	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d", res.Status)
	}

	srv2, _ := newEnvServer(t, http.StatusBadGateway, "<html>bad gateway</html>")
	c2 := newEnvClient(t, srv2)
	res, err = c2.ReportEnvironment(context.Background())
	if err == nil || !strings.HasSuffix(err.Error(), "status 502") {
		t.Fatalf("err = %v", err)
	}
	if res.Status != http.StatusBadGateway || res.Stored {
		t.Errorf("result = %+v", res)
	}
}

func TestReportEnvironmentNetworkError(t *testing.T) {
	srv, _ := newEnvServer(t, http.StatusOK, storedBody)
	c := newEnvClient(t, srv)
	srv.Close() // connection refused from here on

	res, err := c.ReportEnvironment(context.Background())
	if err == nil {
		t.Fatal("expected a network error")
	}
	if res.Status != 0 {
		t.Errorf("status = %d", res.Status)
	}
}

func TestReportEnvironmentTimeout(t *testing.T) {
	es := &envServer{status: http.StatusOK, body: storedBody, block: make(chan struct{})}
	srv := newBlockingEnvServer(t, es)
	c := newEnvClient(t, srv, WithTimeout(50*time.Millisecond))

	start := time.Now()
	_, err := c.ReportEnvironment(context.Background())
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("the client timeout was not applied (%s)", time.Since(start))
	}
	close(es.block)
}

func TestReportEnvironmentPreconditions(t *testing.T) {
	srv, es := newEnvServer(t, http.StatusOK, storedBody)

	noSite, err := New("tok", WithEndpoint(srv.URL), WithLogger(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	defer noSite.Close(testCtx(t))
	if _, err := noSite.ReportEnvironment(context.Background()); !errors.Is(err, ErrNoSiteURL) {
		t.Errorf("without a site URL: err = %v", err)
	}

	hmacOnly, err := New("", WithHMACSecret("s3cret"), WithEndpoint(srv.URL),
		WithSiteURL("https://api.example.com"), WithLogger(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	defer hmacOnly.Close(testCtx(t))
	if _, err := hmacOnly.ReportEnvironment(context.Background()); !errors.Is(err, ErrEnvironmentNeedsToken) {
		t.Errorf("HMAC only: err = %v", err)
	}

	var nilClient *Client
	if res, err := nilClient.ReportEnvironment(context.Background()); err != nil || res != (EnvironmentResult{}) {
		t.Errorf("nil client: %+v, %v", res, err)
	}

	if n := len(es.requests()); n != 0 {
		t.Errorf("no request should be made, got %d", n)
	}
}

// --- the startup report -----------------------------------------------------

func TestNewReportsEnvironmentOnce(t *testing.T) {
	srv, es := newEnvServer(t, http.StatusOK, storedBody)
	logs := &syncBuffer{}
	c := newEnvClient(t, srv, WithEnvironmentReportOnStart(true), WithLogger(debugLogger(logs)))

	es.wait(t, 1)
	if !c.Close(testCtx(t)) {
		t.Fatal("Close timed out")
	}
	if n := len(es.requests()); n != 1 {
		t.Errorf("expected exactly one startup report, got %d", n)
	}
	if !strings.Contains(logs.String(), "environment reported") {
		t.Errorf("expected a debug line for the report, got:\n%s", logs.String())
	}
}

func TestStartupReportSkippedWithoutSiteURLOrToken(t *testing.T) {
	srv, es := newEnvServer(t, http.StatusOK, storedBody)

	logs := &syncBuffer{}
	c, err := New("tok", WithEndpoint(srv.URL), WithLogger(debugLogger(logs)))
	if err != nil {
		t.Fatal(err)
	}
	c.Close(testCtx(t))
	if !strings.Contains(logs.String(), "environment report skipped") ||
		!strings.Contains(logs.String(), "WithSiteURL") {
		t.Errorf("expected a debug line naming the missing site URL, got:\n%s", logs.String())
	}

	hmacLogs := &syncBuffer{}
	h, err := New("", WithHMACSecret("s3cret"), WithEndpoint(srv.URL),
		WithSiteURL("https://api.example.com"), WithLogger(debugLogger(hmacLogs)))
	if err != nil {
		t.Fatal(err)
	}
	h.Close(testCtx(t))
	if !strings.Contains(hmacLogs.String(), "needs the webhook token") {
		t.Errorf("expected a debug line naming the missing token, got:\n%s", hmacLogs.String())
	}

	if n := len(es.requests()); n != 0 {
		t.Errorf("no report should be sent, got %d", n)
	}
}

func TestStartupReportLogsOutcome(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   string
	}{
		"rejected":   {http.StatusUnauthorized, `{"error":"Invalid token"}`, "level=WARN msg=\"Autotix: environment report failed\""},
		"not stored": {http.StatusOK, `{"stored":false,"reason":"off"}`, "level=INFO msg=\"Autotix: environment report not stored\" reason=off"},
		"rate limited": {http.StatusTooManyRequests, `{"error":"Rate limit exceeded"}`,
			"level=INFO msg=\"Autotix: environment report rate limited, skipped until the next start\""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv, es := newEnvServer(t, tc.status, tc.body)
			logs := &syncBuffer{}
			c := newEnvClient(t, srv, WithEnvironmentReportOnStart(true), WithLogger(debugLogger(logs)))
			es.wait(t, 1)
			c.Close(testCtx(t))
			if !strings.Contains(logs.String(), tc.want) {
				t.Errorf("logs missing %q:\n%s", tc.want, logs.String())
			}
		})
	}
}

func TestStartupReportRecoversPanic(t *testing.T) {
	srv, es := newEnvServer(t, http.StatusOK, storedBody)
	logs := &syncBuffer{}
	c := newEnvClient(t, srv,
		WithEnvironmentReportOnStart(true),
		WithLogger(debugLogger(logs)),
		clientOption(func(c *config) {
			c.readBuildInfo = func() (*debug.BuildInfo, bool) { panic("no build info for you") }
		}),
	)
	if !c.Close(testCtx(t)) {
		t.Fatal("Close timed out")
	}
	if !strings.Contains(logs.String(), "environment report panicked") {
		t.Errorf("expected the panic to be logged, got:\n%s", logs.String())
	}
	if n := len(es.requests()); n != 0 {
		t.Errorf("no report should be sent, got %d", n)
	}
}

func TestCloseWaitsForStartupReport(t *testing.T) {
	es := &envServer{status: http.StatusOK, body: storedBody, block: make(chan struct{})}
	srv := newBlockingEnvServer(t, es)
	c := newEnvClient(t, srv, WithEnvironmentReportOnStart(true))

	go func() {
		time.Sleep(100 * time.Millisecond)
		close(es.block)
	}()
	if !c.Close(testCtx(t)) {
		t.Fatal("Close should wait for the in-flight report")
	}
	if n := len(es.requests()); n != 1 {
		t.Errorf("the report should have completed before Close returned, got %d", n)
	}
}

func TestCloseCancelsStartupReportWhenCtxEnds(t *testing.T) {
	es := &envServer{status: http.StatusOK, body: storedBody, block: make(chan struct{})}
	srv := newBlockingEnvServer(t, es)
	logs := &syncBuffer{}
	c := newEnvClient(t, srv,
		WithEnvironmentReportOnStart(true),
		WithTimeout(10*time.Second),
		WithLogger(debugLogger(logs)),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if c.Close(ctx) {
		t.Fatal("Close should report that ctx ended first")
	}

	// The report is canceled rather than left running for the full timeout.
	done := make(chan struct{})
	go func() {
		c.background.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the startup report was not canceled")
	}
	if !strings.Contains(logs.String(), "canceled by Close") {
		t.Errorf("expected a cancel debug line, got:\n%s", logs.String())
	}
	close(es.block)
	if n := len(es.requests()); n != 0 {
		t.Errorf("a canceled report must not be recorded, got %d", n)
	}
}

// newBlockingEnvServer serves es with connections torn down at cleanup, so a
// blocked handler cannot hang srv.Close.
func newBlockingEnvServer(t *testing.T, es *envServer) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(es)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv
}
