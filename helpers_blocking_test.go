package autotix

import (
	"net/http/httptest"
	"testing"
)

// newBlockingServer serves rec without registering srv.Close as a cleanup:
// a blocked handler would make Close hang, so callers unblock rec first and
// then let the process tear the listener down.
func newBlockingServer(t *testing.T, rec *recorder) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(rec)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv
}
