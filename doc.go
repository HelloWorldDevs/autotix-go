// Package autotix ships Go errors and panics to Autotix, which turns them
// into Jira or GitHub tickets and, when enabled, AI-generated fix PRs.
//
// The package has no dependencies outside the standard library. Create one
// Client per process, keep it for the lifetime of the process, and Close it
// on shutdown so queued events are delivered:
//
//	client, err := autotix.New(os.Getenv("AUTOTIX_WEBHOOK_TOKEN"),
//		autotix.WithEnvironment("production"),
//		autotix.WithProject("WEB"),
//	)
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer client.Close(context.Background())
//
//	if err := doWork(); err != nil {
//		client.Capture(err)
//	}
//
// Delivery is asynchronous and bounded. Capture never blocks the caller and
// never returns an error: a full queue drops the event, a failed request is
// logged and counted in Stats. Error tracking that can take down the app it
// watches is a footgun, so the SDK fails soft everywhere.
//
// A nil *Client is safe to use. Every method is a no-op, which lets an app
// leave the client unset in environments where Autotix is disabled.
package autotix
