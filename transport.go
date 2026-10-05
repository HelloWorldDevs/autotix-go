package autotix

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
)

// maxResponseSnippet caps how much of a non-2xx body is kept for debug logs.
const maxResponseSnippet = 512

type transport interface {
	send(ctx context.Context, body []byte) (status int, snippet string, err error)
}

type httpTransport struct {
	client     *http.Client
	endpoint   string
	token      string
	hmacSecret string
	userAgent  string
}

func (t *httpTransport) send(ctx context.Context, body []byte) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", t.userAgent)

	// Auth: an HMAC secret signs the body; otherwise the token rides in a
	// header. Never both, since the server checks the token first.
	if t.hmacSecret != "" {
		mac := hmac.New(sha256.New, []byte(t.hmacSecret))
		mac.Write(body)
		req.Header.Set("X-Webhook-Signature", hex.EncodeToString(mac.Sum(nil)))
	} else if t.token != "" {
		req.Header.Set("X-Webhook-Token", t.token)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()

	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSnippet))
	// Drain a bounded amount more so the connection can be reused.
	_, _ = io.CopyN(io.Discard, resp.Body, 64<<10)
	return resp.StatusCode, string(snippet), nil
}
