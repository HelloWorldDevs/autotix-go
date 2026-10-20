# Trigger sample

A minimal program that sends one event to Autotix, waits for delivery, and prints the result. Use it to check a token, an endpoint, or project routing without wiring the SDK into your own code first.

> **These are real events.** Every mode sends a real event to your org. When the event reaches a project (`AUTOTIX_PROJECT` is set), Autotix opens a real ticket in that project's Jira or GitHub, attempts a fix PR if autofix is on, and tickets are billed once your trial ends. Point `AUTOTIX_PROJECT` at a scratch project if you want to see the full ticket flow. Without it the event has no URL to match a project, so it is recorded in **Webhook Logs** as "No mapped URL found" and no ticket is opened: a safe way to check the token and connectivity.

## Run it

From the module root (`packages/go-sdk` in the Autotix monorepo, the repository root in the `autotix-go` mirror):

```bash
export AUTOTIX_WEBHOOK_TOKEN=...   # Settings > webhook token in the Autotix dashboard
go run ./examples/trigger -mode test
```

```
Sending a "test" event to https://app.autotix.io/api/webhook/error (no AUTOTIX_PROJECT: it will show in Webhook Logs without a ticket)
sent=1 failed=0 dropped=0
Delivered. Check Webhook Logs in the Autotix dashboard.
```

Once a release that includes this sample is tagged, you can also run it without cloning:

```bash
go run github.com/HelloWorldDevs/autotix-go/examples/trigger@latest -mode test
```

## Modes

| `-mode` | What it sends |
| --- | --- |
| `test` (default) | `client.TriggerTestError()`: an `AutotixTestError` event tagged `autotix_test=true`. |
| `error` | `client.Capture(errors.New(...))`: an ordinary error with the caller's stack. |
| `panic` | A deliberate panic recovered by `defer client.Recover()`, sent at level `fatal`. |

`-timeout` (default `15s`) bounds the wait for delivery.

Autotix dedupes repeats of the same error, so running a mode again usually adds an occurrence to the existing ticket instead of opening a new one.

## Environment

| Variable | |
| --- | --- |
| `AUTOTIX_WEBHOOK_TOKEN` | Required. The org webhook token from **Settings** in the Autotix dashboard. It is a server secret. |
| `AUTOTIX_ENDPOINT` | Optional. Overrides the ingest endpoint, for a staging or self-hosted Autotix. |
| `AUTOTIX_PROJECT` | Optional. The project key (a Jira key or `owner/repo`) to route the event to. |

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Delivered: Autotix answered with a 2xx. |
| `1` | Not delivered: a non-2xx answer, a network error, or no answer within `-timeout`. The SDK's warning on stderr has the details. |
| `2` | Usage error: missing token, unknown mode, or an invalid endpoint. |
