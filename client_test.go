package autotix

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// --- construction -----------------------------------------------------------

func TestNewRequiresCredentials(t *testing.T) {
	if _, err := New(""); !errors.Is(err, ErrMissingCredentials) {
		t.Fatalf("expected ErrMissingCredentials, got %v", err)
	}
	// An HMAC secret is a credential on its own.
	c, err := New("", WithHMACSecret("s3cret"))
	if err != nil {
		t.Fatalf("hmac-only client should construct: %v", err)
	}
	c.Close(testCtx(t))
}

func TestNewValidatesOptions(t *testing.T) {
	cases := map[string]Option{
		"sample rate below 0": WithSampleRate(-0.1),
		"sample rate above 1": WithSampleRate(1.1),
		"bad endpoint":        WithEndpoint("::not a url"),
		"non-http endpoint":   WithEndpoint("ftp://example.com/x"),
		"zero timeout":        WithTimeout(0),
		"negative queue size": WithQueueSize(0),
		"zero workers":        WithWorkers(0),
		"nil http client":     WithHTTPClient(nil),
	}
	for name, opt := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New("tok", opt); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestDefaultEndpointIsProductionWebhook(t *testing.T) {
	if DefaultEndpoint != "https://app.autotix.io/api/webhook/error" {
		t.Fatalf("unexpected DefaultEndpoint %q", DefaultEndpoint)
	}
	c, err := New("tok")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(testCtx(t))
	if c.cfg.endpoint != DefaultEndpoint {
		t.Fatalf("client endpoint = %q", c.cfg.endpoint)
	}
}

// --- wire format ------------------------------------------------------------

func TestCaptureSendsGenericWebhookPayload(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv,
		WithEnvironment("production"),
		WithRelease("v1.2.3"),
		WithProject("WEB"),
		WithTags(map[string]string{"service": "api", "region": "us"}),
		WithServerName("web-1"),
	)

	before := time.Now().Unix()
	c.Capture(errors.New("boom"),
		WithTag("region", "eu"), // per-event wins over client default
		WithExtra("request_id", "abc-123"),
		WithUser(User{ID: "u_42", Email: "a@example.com"}),
		WithURL("https://example.com/checkout"),
	)
	flush(t, c)

	reqs := rec.wait(t, 1)
	got := reqs[0]

	if h := got.Header.Get("Content-Type"); h != "application/json" {
		t.Errorf("Content-Type = %q", h)
	}
	if h := got.Header.Get("X-Webhook-Token"); h != "test-token" {
		t.Errorf("X-Webhook-Token = %q", h)
	}
	if h := got.Header.Get("User-Agent"); h != "autotix-go/"+Version {
		t.Errorf("User-Agent = %q", h)
	}
	if got.Header.Get("X-Webhook-Signature") != "" {
		t.Error("token auth must not send a signature")
	}

	w := got.Wire
	if w.Source != "go" {
		t.Errorf("source = %q", w.Source)
	}
	if w.Level != "error" {
		t.Errorf("level = %q", w.Level)
	}
	if w.Message != "boom" {
		t.Errorf("message = %q", w.Message)
	}
	if w.URL != "https://example.com/checkout" {
		t.Errorf("url = %q", w.URL)
	}
	d := w.Details
	if d.ErrorType != "*errors.errorString" {
		t.Errorf("error_type = %q", d.ErrorType)
	}
	if !strings.Contains(d.StackTrace, "TestCaptureSendsGenericWebhookPayload") {
		t.Errorf("stack_trace should include the capture site, got:\n%s", d.StackTrace)
	}
	if d.Environment != "production" || d.Release != "v1.2.3" || d.ProjectKey != "WEB" {
		t.Errorf("environment/release/project_key = %q/%q/%q", d.Environment, d.Release, d.ProjectKey)
	}
	if d.Hostname != "web-1" {
		t.Errorf("hostname = %q", d.Hostname)
	}
	if d.GoVersion != runtime.Version() {
		t.Errorf("go_version = %q", d.GoVersion)
	}
	if d.SDKVersion != Version {
		t.Errorf("sdk_version = %q", d.SDKVersion)
	}
	if d.Timestamp < before {
		t.Errorf("timestamp %d < %d", d.Timestamp, before)
	}
	if d.Tags["service"] != "api" || d.Tags["region"] != "eu" {
		t.Errorf("tags = %v", d.Tags)
	}
	if d.Extra["request_id"] != "abc-123" {
		t.Errorf("extra = %v", d.Extra)
	}
	if d.User == nil || d.User.ID != "u_42" || d.User.Email != "a@example.com" {
		t.Errorf("user = %+v", d.User)
	}
}

func TestCaptureOmitsEmptyOptionalFields(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv, WithServerName("h"))
	c.Capture(errors.New("x"))
	flush(t, c)

	raw := rec.wait(t, 1)[0].Raw
	details, ok := raw["details"].(map[string]any)
	if !ok {
		t.Fatalf("details missing: %v", raw)
	}
	for _, key := range []string{"environment", "release", "project_key", "tags", "extra", "user", "git", "request_method", "user_agent"} {
		if _, present := details[key]; present {
			t.Errorf("details.%s should be omitted when unset", key)
		}
	}
	// `url` is always present (possibly empty) so the server's generic parser
	// sees the same shape the PHP modules send.
	if _, present := raw["url"]; !present {
		t.Error("url must always be present")
	}
}

func TestCaptureIgnoresNilError(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv)
	c.Capture(nil)
	flush(t, c)
	if rec.count() != 0 {
		t.Fatalf("nil error must not be sent, got %d requests", rec.count())
	}
}

func TestCaptureUsesDefaultHostname(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv)
	c.Capture(errors.New("x"))
	flush(t, c)
	host, _ := os.Hostname()
	if got := rec.wait(t, 1)[0].Wire.Details.Hostname; got != host {
		t.Fatalf("hostname = %q, want %q", got, host)
	}
}

func TestCaptureMessageDefaultsToInfoWithoutStack(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv)
	c.CaptureMessage("queue backlog is growing")
	c.CaptureMessage("disk almost full", WithLevel(LevelWarning))
	flush(t, c)

	reqs := rec.wait(t, 2)
	// Delivery order across workers is not guaranteed; index by message.
	byMsg := map[string]recordedRequest{}
	for _, r := range reqs {
		byMsg[r.Wire.Message] = r
	}
	info := byMsg["queue backlog is growing"]
	if info.Wire.Level != "info" {
		t.Errorf("default level = %q, want info", info.Wire.Level)
	}
	if _, present := info.Raw["details"].(map[string]any)["stack_trace"]; present {
		t.Error("messages should not carry a stack trace by default")
	}
	if byMsg["disk almost full"].Wire.Level != "warning" {
		t.Errorf("explicit level not honored")
	}
}

func TestWithStackTraceOverridesCapturedStack(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv)
	c.Capture(errors.New("x"), WithStackTrace("custom frame 1\ncustom frame 2"))
	flush(t, c)
	if got := rec.wait(t, 1)[0].Wire.Details.StackTrace; got != "custom frame 1\ncustom frame 2" {
		t.Fatalf("stack_trace = %q", got)
	}
}

func TestPayloadTruncatesOversizedFields(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv)
	c.Capture(errors.New(strings.Repeat("m", maxMessageLen+100)),
		WithStackTrace(strings.Repeat("s", maxStackLen+100)))
	flush(t, c)
	w := rec.wait(t, 1)[0].Wire
	if len(w.Message) != maxMessageLen {
		t.Errorf("message length = %d, want %d", len(w.Message), maxMessageLen)
	}
	if len(w.Details.StackTrace) != maxStackLen {
		t.Errorf("stack length = %d, want %d", len(w.Details.StackTrace), maxStackLen)
	}
}

func TestPayloadDoesNotHTMLEscape(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv)
	c.Capture(errors.New("a < b && c > d"))
	flush(t, c)
	body := rec.wait(t, 1)[0].Body
	if !bytes.Contains(body, []byte(`"a < b && c > d"`)) {
		t.Fatalf("body should keep angle brackets verbatim: %s", body)
	}
}

// --- panics -----------------------------------------------------------------

func TestCapturePanicReportsFatal(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv)
	c.CapturePanic("something exploded")
	c.CapturePanic(errors.New("typed"), WithTag("k", "v"))
	flush(t, c)

	byMsg := map[string]recordedRequest{}
	for _, r := range rec.wait(t, 2) {
		byMsg[r.Wire.Message] = r
	}
	s := byMsg["panic: something exploded"]
	if s.Wire.Level != "fatal" {
		t.Errorf("level = %q, want fatal", s.Wire.Level)
	}
	if s.Wire.Details.ErrorType != "string" {
		t.Errorf("error_type = %q", s.Wire.Details.ErrorType)
	}
	if s.Wire.Details.StackTrace == "" {
		t.Error("panic should carry a stack trace")
	}
	e := byMsg["panic: typed"]
	if e.Wire.Details.ErrorType != "*errors.errorString" || e.Wire.Details.Tags["k"] != "v" {
		t.Errorf("typed panic details = %+v", e.Wire.Details)
	}
}

func TestCapturePanicIgnoresNil(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv)
	c.CapturePanic(nil)
	flush(t, c)
	if rec.count() != 0 {
		t.Fatal("nil panic value must not be sent")
	}
}

func TestRecoverSwallowsAndReportsPanic(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv)

	func() {
		defer c.Recover(WithTag("job", "nightly"))
		panic("worker died")
	}()

	flush(t, c)
	w := rec.wait(t, 1)[0].Wire
	if w.Message != "panic: worker died" || w.Level != "fatal" || w.Details.Tags["job"] != "nightly" {
		t.Fatalf("unexpected panic event: %+v", w)
	}
	if !strings.Contains(w.Details.StackTrace, "TestRecoverSwallowsAndReportsPanic") {
		t.Fatalf("stack should include the panicking frame:\n%s", w.Details.StackTrace)
	}
}

func TestRecoverIsNoopWithoutPanic(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv)
	func() {
		defer c.Recover()
	}()
	flush(t, c)
	if rec.count() != 0 {
		t.Fatal("no panic, nothing should be sent")
	}
}

// --- nil client -------------------------------------------------------------

func TestNilClientIsNoop(t *testing.T) {
	var c *Client
	c.Capture(errors.New("x"))
	c.CaptureMessage("x")
	c.CapturePanic("x")
	func() {
		defer c.Recover()
		// no panic
	}()
	c.TriggerTestError()
	if !c.Flush(context.Background()) || !c.Close(context.Background()) {
		t.Fatal("nil client Flush/Close should report success")
	}
	if s := c.Stats(); s != (Stats{}) {
		t.Fatalf("nil client stats = %+v", s)
	}
	// Middleware must still pass requests through.
	called := false
	h := c.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	h.ServeHTTP(nil, nil)
	if !called {
		t.Fatal("nil client middleware should pass through")
	}
}

// --- sampling, hooks --------------------------------------------------------

func TestSampleRateZeroDropsEverything(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv, WithSampleRate(0))
	for i := 0; i < 20; i++ {
		c.Capture(errors.New("x"))
	}
	flush(t, c)
	if rec.count() != 0 {
		t.Fatalf("sampleRate 0 sent %d events", rec.count())
	}
}

func TestTriggerTestErrorBypassesSampling(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv, WithSampleRate(0))
	c.TriggerTestError(WithURL("https://example.com/"))
	flush(t, c)
	w := rec.wait(t, 1)[0].Wire
	if !strings.HasPrefix(w.Message, "AutotixTestError: ") {
		t.Errorf("message = %q", w.Message)
	}
	if w.Details.Tags["autotix_test"] != "true" {
		t.Errorf("tags = %v", w.Details.Tags)
	}
	if w.URL != "https://example.com/" {
		t.Errorf("url = %q", w.URL)
	}
	if w.Details.StackTrace == "" {
		t.Error("test error should carry a stack trace")
	}
}

func TestBeforeSendCanDropAndMutate(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv, WithBeforeSend(func(e *Event) *Event {
		if strings.Contains(e.Message, "noise") {
			return nil
		}
		e.Message = "rewritten: " + e.Message
		if e.Tags == nil {
			e.Tags = map[string]string{}
		}
		e.Tags["hooked"] = "yes"
		return e
	}))
	c.Capture(errors.New("noise from a scanner"))
	c.Capture(errors.New("real"))
	flush(t, c)

	reqs := rec.wait(t, 1)
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
	if reqs[0].Wire.Message != "rewritten: real" || reqs[0].Wire.Details.Tags["hooked"] != "yes" {
		t.Fatalf("beforeSend result not used: %+v", reqs[0].Wire)
	}
}

func TestBeforeSendPanicStillDelivers(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv, WithBeforeSend(func(e *Event) *Event {
		panic("buggy hook")
	}))
	c.Capture(errors.New("still arrives"))
	flush(t, c)
	if got := rec.wait(t, 1)[0].Wire.Message; got != "still arrives" {
		t.Fatalf("message = %q", got)
	}
}

// --- auth -------------------------------------------------------------------

func TestHMACSecretSignsBodyInsteadOfSendingToken(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv, WithHMACSecret("shared-secret"))
	c.Capture(errors.New("signed"))
	flush(t, c)

	got := rec.wait(t, 1)[0]
	if got.Header.Get("X-Webhook-Token") != "" {
		t.Error("hmac auth must not send the token header")
	}
	mac := hmac.New(sha256.New, []byte("shared-secret"))
	mac.Write(got.Body)
	want := hex.EncodeToString(mac.Sum(nil))
	if sig := got.Header.Get("X-Webhook-Signature"); sig != want {
		t.Errorf("X-Webhook-Signature = %q, want %q", sig, want)
	}
}

// --- delivery outcomes ------------------------------------------------------

func TestStatsCountSentAndFailed(t *testing.T) {
	okSrv, _ := newServer(t, http.StatusOK)
	c := newClient(t, okSrv)
	c.Capture(errors.New("a"))
	c.Capture(errors.New("b"))
	flush(t, c)
	if s := c.Stats(); s.Sent != 2 || s.Failed != 0 || s.Dropped != 0 {
		t.Fatalf("stats after success = %+v", s)
	}

	failSrv, _ := newServer(t, http.StatusUnauthorized)
	f := newClient(t, failSrv)
	f.Capture(errors.New("a"))
	flush(t, f)
	if s := f.Stats(); s.Sent != 0 || s.Failed != 1 {
		t.Fatalf("stats after 401 = %+v", s)
	}
}

func TestNetworkErrorIsCountedNotRaised(t *testing.T) {
	srv, _ := newServer(t, http.StatusOK)
	url := srv.URL
	srv.Close() // connection refused from here on
	c, err := New("tok", WithEndpoint(url), WithTimeout(time.Second), WithLogger(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(testCtx(t))
	c.Capture(errors.New("unreachable"))
	flush(t, c)
	if s := c.Stats(); s.Failed != 1 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestTimeoutIsEnforced(t *testing.T) {
	rec := newRecorder(http.StatusOK)
	rec.block = make(chan struct{})
	srv := newBlockingServer(t, rec)
	defer close(rec.block)

	c, err := New("tok", WithEndpoint(srv.URL), WithTimeout(50*time.Millisecond), WithLogger(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	c.Capture(errors.New("slow"))
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !c.Flush(ctx) {
		t.Fatal("flush should complete once the request times out")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout not enforced, took %s", elapsed)
	}
	if s := c.Stats(); s.Failed != 1 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestQueueOverflowDropsInsteadOfBlocking(t *testing.T) {
	rec := newRecorder(http.StatusOK)
	rec.block = make(chan struct{})
	srv := newBlockingServer(t, rec)

	c, err := New("tok", WithEndpoint(srv.URL), WithQueueSize(1), WithWorkers(1), WithLogger(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	// Worker takes one and blocks on the server; the queue holds one more;
	// everything after that must be dropped immediately.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			c.Capture(errors.New("storm"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Capture blocked on a full queue")
	}
	// Give the worker a moment to dequeue so the count is deterministic.
	time.Sleep(50 * time.Millisecond)
	if s := c.Stats(); s.Dropped < 7 {
		t.Fatalf("expected most events dropped, stats = %+v", s)
	}
	close(rec.block)
	c.Close(testCtx(t))
}

func TestFlushReturnsFalseOnTimeout(t *testing.T) {
	rec := newRecorder(http.StatusOK)
	rec.block = make(chan struct{})
	srv := newBlockingServer(t, rec)

	c, err := New("tok", WithEndpoint(srv.URL), WithTimeout(5*time.Second), WithLogger(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	c.Capture(errors.New("stuck"))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if c.Flush(ctx) {
		t.Fatal("Flush should report false when the context expires first")
	}
	close(rec.block)
	c.Close(testCtx(t))
}

func TestFlushWithNothingPendingIsImmediate(t *testing.T) {
	srv, _ := newServer(t, http.StatusOK)
	c := newClient(t, srv)
	if !c.Flush(testCtx(t)) {
		t.Fatal("nothing pending should flush true")
	}
}

func TestCloseStopsAcceptingAndIsIdempotent(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c, err := New("tok", WithEndpoint(srv.URL), WithLogger(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	c.Capture(errors.New("before close"))
	if !c.Close(testCtx(t)) {
		t.Fatal("close should drain and return true")
	}
	rec.wait(t, 1)

	c.Capture(errors.New("after close"))
	c.CaptureMessage("after close")
	if !c.Close(testCtx(t)) {
		t.Fatal("second close should be a successful no-op")
	}
	if rec.count() != 1 {
		t.Fatalf("events after Close must be dropped, got %d requests", rec.count())
	}
}

func TestConcurrentCaptureIsSafe(t *testing.T) {
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv, WithQueueSize(1000), WithWorkers(4))
	done := make(chan struct{})
	for g := 0; g < 8; g++ {
		go func(g int) {
			for i := 0; i < 25; i++ {
				c.Capture(errors.New("concurrent"), WithTag("g", string(rune('a'+g))))
			}
			done <- struct{}{}
		}(g)
	}
	for g := 0; g < 8; g++ {
		<-done
	}
	flush(t, c)
	if n := rec.count(); n != 200 {
		t.Fatalf("expected 200 deliveries, got %d (stats %+v)", n, c.Stats())
	}
}

// --- logging ----------------------------------------------------------------

func TestNon2xxLogsAWarning(t *testing.T) {
	srv, _ := newServer(t, http.StatusInternalServerError)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	c, err := New("tok", WithEndpoint(srv.URL), WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	c.Capture(errors.New("x"))
	c.Close(testCtx(t))
	out := buf.String()
	if !strings.Contains(out, "Autotix delivery failed") || !strings.Contains(out, "status=500") {
		t.Fatalf("expected a warning with the status, got: %s", out)
	}
	if strings.Contains(out, `{"success":true}`) {
		t.Fatal("response body must not be logged at warn level")
	}
}

func TestSuccessIsSilentAtInfoLevel(t *testing.T) {
	srv, _ := newServer(t, http.StatusOK)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	c, err := New("tok", WithEndpoint(srv.URL), WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	c.Capture(errors.New("x"))
	c.Close(testCtx(t))
	if buf.Len() != 0 {
		t.Fatalf("successful delivery should not log above debug, got: %s", buf.String())
	}
}
