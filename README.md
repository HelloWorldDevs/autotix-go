# Autotix Go SDK

Ship Go errors and panics to [Autotix](https://autotix.io). Autotix turns them into Jira or GitHub tickets and, when enabled, opens AI-generated fix PRs.

- **Zero dependencies.** Standard library only.
- **Never blocks, never throws.** Delivery runs in the background on a bounded queue. A failed request is logged and counted, not returned.
- **Panics covered.** `defer client.Recover()` for goroutines, `client.Middleware` for `net/http`.
- **PR previews.** Errors inside a Tugboat preview are posted to the pull request instead of opening a ticket.

## Install

```bash
go get github.com/HelloWorldDevs/autotix-go
```

Requires Go 1.22 or newer.

## Quick start

```go
import autotix "github.com/HelloWorldDevs/autotix-go"

client, err := autotix.New(os.Getenv("AUTOTIX_WEBHOOK_TOKEN"),
    autotix.WithEnvironment("production"),
    autotix.WithRelease("v1.4.2"),
    autotix.WithProject("WEB"), // route errors here; see "How errors reach a project"
)
if err != nil {
    log.Fatal(err)
}
defer client.Close(context.Background()) // deliver anything still queued

if err := doWork(); err != nil {
    client.Capture(err, autotix.WithTag("module", "billing"))
}
```

`Capture` records the stack of the calling goroutine, so call it where you handle the error.

## Where do I get a token?

In the Autotix dashboard open **Settings** and copy your org **webhook token**. It is a server secret. Keep it in configuration or the environment, never in client code.

## Panics

Report a panic in a goroutine and let the goroutine end normally:

```go
go func() {
    defer client.Recover(autotix.WithTag("job", "nightly-report"))
    runNightlyReport()
}()
```

`Recover` must be the deferred call itself. To re-panic after reporting, recover yourself and call `CapturePanic`:

```go
defer func() {
    if r := recover(); r != nil {
        client.CapturePanic(r)
        panic(r)
    }
}()
```

## net/http

```go
mux := http.NewServeMux()
mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
    if err := placeOrder(r); err != nil {
        client.Capture(err, autotix.WithRequest(r)) // attaches the full URL
        http.Error(w, "order failed", http.StatusInternalServerError)
        return
    }
})

srv := &http.Server{Addr: ":8080", Handler: client.Middleware(mux)}
```

`Middleware` reports panics raised by handlers, then re-panics so your existing recovery middleware (or net/http's default) runs as before. Mount it closest to your handlers so it sees the panic first. It works the same with chi, gorilla/mux, and any other router that takes an `http.Handler`.

## Short-lived processes

CLIs, cron jobs, and Lambdas exit before background delivery finishes unless you wait:

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
client.Flush(ctx) // or client.Close(ctx) on shutdown
```

## Verify your setup

```go
client.TriggerTestError()
client.Flush(context.Background())
```

This sends an `AutotixTestError: …` event tagged `autotix_test=true`, bypasses sampling, and should show up in **Webhook Logs** within a few seconds.

## How errors reach a project

Autotix decides which project (Jira project or GitHub repo) each error belongs to in this order:

1. **A pinned project.** `WithProject("WEB")` on the client, or on one event. Pinned events skip URL matching.
2. **URL matching.** Otherwise the event's URL is tested against each project's URL patterns. `WithRequest(r)` attaches the full request URL; `WithURL` sets one by hand. A bare path matches nothing.

Server code with no request, such as workers and cron jobs, should always pin a project.

## Vulnerability scanning

Autotix can check the modules compiled into your service against the [OSV.dev](https://osv.dev) advisory database and list matches under **Vulnerabilities** in the dashboard. Turn it on by naming the service's public URL:

```go
client, err := autotix.New(os.Getenv("AUTOTIX_WEBHOOK_TOKEN"),
    autotix.WithProject("API"),
    autotix.WithSiteURL("https://api.example.com"),
)
```

With `WithSiteURL` set, `New` sends one report in the background: the Go version (checked as the standard library, `stdlib`) and every dependency module from `runtime/debug.ReadBuildInfo`, with versions as compiled into the binary. A replaced module is reported as its replacement; one replaced by a local directory has no version to check and is left out. Only module paths and versions are sent, never source code. A binary's modules cannot change while it runs, so the SDK reports once per process start; Autotix re-checks the stored list against new advisories every day.

- **Enable the feature.** Vulnerability scanning has to be turned on for the project. Until it is, Autotix accepts the report without storing it and the SDK logs the reason at Info.
- **One site URL per service.** Autotix keys each inventory on the URL's scheme, host, and port, so services that share a host overwrite each other's report.
- **Project.** The report joins the project pinned with `WithProject`. Without one it is matched to a project by the site URL, like an error's URL.
- **Token required.** The report authenticates with the webhook token. A client with only an HMAC secret skips it (logged at Debug).
- **Fail soft.** A failed report is logged at Warn and never affects error delivery. A rate-limited one (Autotix accepts 30 reports a minute per org, which many replicas starting together can exceed) is logged at Info, since the replica that got through reported the same modules. `Close` waits for a report still in flight and cancels it when its context ends.
- **Fixes.** "Fix it" on a finding files a ticket. Automatic `go.mod` bump PRs are not available yet.

CLIs and tests can report on demand and read the answer:

```go
client, _ := autotix.New(token,
    autotix.WithSiteURL("https://api.example.com"),
    autotix.WithEnvironmentReportOnStart(false), // report below instead of at startup
)
res, err := client.ReportEnvironment(ctx)
switch {
case err != nil:
    log.Printf("environment report failed: %v", err)
case !res.Stored:
    log.Printf("environment report not stored: %s", res.Reason)
default:
    log.Printf("reported %d packages (changed: %v)", res.Packages, res.Changed)
}
```

## Configuration

```go
autotix.New(token,
    autotix.WithEnvironment("production"),
    autotix.WithRelease("v1.2.3"),
    autotix.WithProject("WEB"),
    autotix.WithTags(map[string]string{"service": "api"}),
    autotix.WithServerName("web-1"),            // default: os.Hostname()
    autotix.WithSampleRate(1.0),                // 0..1, fraction of events to ship
    autotix.WithBeforeSend(func(e *autotix.Event) *autotix.Event {
        if strings.Contains(e.Message, "context canceled") {
            return nil // drop
        }
        return e
    }),
    autotix.WithTimeout(15*time.Second),        // per request
    autotix.WithQueueSize(100),                 // events waiting for delivery
    autotix.WithWorkers(2),                     // concurrent deliveries
    autotix.WithHTTPClient(&http.Client{}),     // proxies, custom transports
    autotix.WithLogger(slog.Default()),         // failures log at Warn
    autotix.WithEndpoint("https://app.autotix.io/api/webhook/error"), // self-hosted
    autotix.WithHMACSecret("shared-secret"),    // sign bodies instead of sending the token
    autotix.WithSiteURL("https://api.example.com"), // turns on vulnerability scanning
    autotix.WithEnvironmentReportOnStart(true), // default; false to call ReportEnvironment yourself
)
```

Per-event options for `Capture`, `CaptureMessage`, `CapturePanic`, and `Recover`:

| Option | Effect |
| --- | --- |
| `WithLevel(autotix.LevelFatal)` | `fatal`, `error`, `warning`, `info`, `debug` |
| `WithTag(k, v)` / `WithTags(m)` | Searchable tags. Per-event tags override client defaults. |
| `WithExtra(k, v)` | Non-searchable detail, such as a request id. |
| `WithUser(autotix.User{ID: "u_42"})` | The affected user. |
| `WithURL(u)` | Full URL for project routing. |
| `WithRequest(r)` | URL, method, and user agent from an `*http.Request`. |
| `WithProject(key)` | Route this one event to a project. |
| `WithStackTrace(s)` | Replace the captured stack. |

## Behavior and limits

- **Failures never reach your code.** Network errors and non-2xx responses are logged at Warn and counted in `client.Stats()`.
- **Bounded queue.** When the queue is full new events are dropped, so an error storm cannot grow memory. Autotix dedupes repeats server-side anyway.
- **A nil `*Client` is safe.** Every method is a no-op, so you can leave the client unset where Autotix is disabled.
- **Messages dedupe by text.** `CaptureMessage` sends no stack trace, so identical messages collapse into one ticket.
- **PR previews.** `AUTOTIX_GIT_PROVIDER` and `AUTOTIX_GIT_PR`, or Tugboat's `TUGBOAT_GITHUB_PR`, are detected at startup and sent as `details.git`.

## Development

```bash
go test -race -cover ./...
go vet ./...
```

## Source

This repository is a read-only mirror, split from the Autotix monorepo. Issues and PRs are welcome here and will be applied upstream.

## License

MIT
