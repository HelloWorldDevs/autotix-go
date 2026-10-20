// Command trigger sends one event to Autotix to check a Go setup end to end.
//
//	AUTOTIX_WEBHOOK_TOKEN=... go run ./examples/trigger -mode test
//
// Modes:
//
//	test   client.TriggerTestError(): a labeled AutotixTestError event
//	error  client.Capture(err): an ordinary error with its stack
//	panic  a deliberate panic caught by a deferred client.Recover()
//
// Environment:
//
//	AUTOTIX_WEBHOOK_TOKEN  required, the org webhook token from Settings
//	AUTOTIX_ENDPOINT       optional, overrides the ingest endpoint
//	AUTOTIX_PROJECT        optional, the project key to route the event to
//
// The command waits for delivery, prints the client's Stats, and exits 1 if
// the event was not delivered (2 for a usage error). Every mode sends a real
// event: one that reaches a project opens a real ticket. See README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	autotix "github.com/HelloWorldDevs/autotix-go"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run is main without the process exit, so it can be tested. It returns the
// exit code.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("trigger", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", "test", "what to send: test, error, or panic")
	wait := fs.Duration("timeout", 15*time.Second, "how long to wait for delivery")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	switch *mode {
	case "test", "error", "panic":
	default:
		fmt.Fprintf(stderr, "unknown -mode %q: use test, error, or panic\n", *mode)
		return 2
	}

	token := getenv("AUTOTIX_WEBHOOK_TOKEN")
	if token == "" {
		fmt.Fprintln(stderr, "AUTOTIX_WEBHOOK_TOKEN is not set. Copy the org webhook token from Settings in the Autotix dashboard.")
		return 2
	}

	opts := []autotix.Option{
		autotix.WithEnvironment("sample"),
		autotix.WithTag("sample", "examples/trigger"),
		// Failed deliveries are reported below; keep the SDK's own log quiet
		// unless it has something to warn about.
		autotix.WithLogger(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))),
	}
	endpoint := autotix.DefaultEndpoint
	if e := getenv("AUTOTIX_ENDPOINT"); e != "" {
		endpoint = e
		opts = append(opts, autotix.WithEndpoint(e))
	}
	project := getenv("AUTOTIX_PROJECT")
	if project != "" {
		opts = append(opts, autotix.WithProject(project))
	}

	client, err := autotix.New(token, opts...)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	fmt.Fprintf(stdout, "Sending a %q event to %s", *mode, endpoint)
	if project != "" {
		fmt.Fprintf(stdout, " (project %s)", project)
	} else {
		fmt.Fprint(stdout, " (no AUTOTIX_PROJECT: it will show in Webhook Logs without a ticket)")
	}
	fmt.Fprintln(stdout)

	switch *mode {
	case "test":
		client.TriggerTestError()
	case "error":
		client.Capture(errors.New("autotix sample: deliberate error from examples/trigger"))
	case "panic":
		panicAndRecover(client)
	}

	// Delivery is asynchronous: wait for it before reading Stats, then shut
	// the client down.
	ctx, cancel := context.WithTimeout(context.Background(), *wait)
	defer cancel()
	flushed := client.Flush(ctx)
	client.Close(ctx)

	stats := client.Stats()
	fmt.Fprintf(stdout, "sent=%d failed=%d dropped=%d\n", stats.Sent, stats.Failed, stats.Dropped)

	switch {
	case !flushed:
		fmt.Fprintf(stderr, "delivery did not finish within %s\n", *wait)
		return 1
	case stats.Failed > 0 || stats.Dropped > 0 || stats.Sent == 0:
		fmt.Fprintln(stderr, "delivery failed: the warning above has the HTTP status or network error")
		return 1
	}
	fmt.Fprintln(stdout, "Delivered. Check Webhook Logs in the Autotix dashboard.")
	return 0
}

// panicAndRecover panics on purpose. The deferred Recover reports the panic
// with this goroutine's stack and lets the function return normally.
func panicAndRecover(client *autotix.Client) {
	defer client.Recover(autotix.WithTag("mode", "panic"))
	panic("autotix sample: deliberate panic from examples/trigger")
}
