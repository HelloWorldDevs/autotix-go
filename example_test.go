package autotix_test

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	autotix "github.com/HelloWorldDevs/autotix-go"
)

func ExampleNew() {
	client, err := autotix.New(os.Getenv("AUTOTIX_WEBHOOK_TOKEN"),
		autotix.WithEnvironment("production"),
		autotix.WithRelease("v1.4.2"),
		autotix.WithProject("WEB"),
	)
	if err != nil {
		log.Fatal(err)
	}
	// Drain the queue before the process exits so nothing is lost.
	defer client.Close(context.Background())

	if err := doWork(); err != nil {
		client.Capture(err, autotix.WithTag("module", "billing"))
	}
}

func ExampleClient_Recover() {
	client, _ := autotix.New("your-org-webhook-token")
	defer client.Close(context.Background())

	go func() {
		// Report a panic in this goroutine and keep the process alive.
		defer client.Recover(autotix.WithTag("job", "nightly-report"))
		runNightlyReport()
	}()
}

func ExampleClient_Middleware() {
	client, _ := autotix.New("your-org-webhook-token", autotix.WithProject("API"))
	defer client.Close(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
		if err := placeOrder(r); err != nil {
			// Attach the request so the event carries the full URL.
			client.Capture(err, autotix.WithRequest(r))
			http.Error(w, "order failed", http.StatusInternalServerError)
			return
		}
	})

	// Panics in handlers are reported, then re-raised for your own recovery
	// middleware (or net/http's default) to handle.
	srv := &http.Server{Addr: ":8080", Handler: client.Middleware(mux)}
	_ = srv
}

func ExampleClient_Flush() {
	client, _ := autotix.New("your-org-webhook-token")
	client.CaptureMessage("deploy finished", autotix.WithLevel(autotix.LevelInfo))

	// In short-lived processes (CLIs, Lambdas, cron jobs) wait for delivery.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !client.Flush(ctx) {
		log.Println("autotix: some events were still in flight")
	}
}

func doWork() error                  { return errors.New("example") }
func runNightlyReport()              {}
func placeOrder(*http.Request) error { return nil }
