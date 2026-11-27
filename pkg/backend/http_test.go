package backend

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestHTTPBackendCheckReusesConnection guards against closing an unread body,
// which makes net/http drop the keep-alive connection on every poll.
func TestHTTPBackendCheckReusesConnection(t *testing.T) {
	var connections atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "health response")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)

	b, err := New(&Config{Type: HTTP, Address: server.URL, Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if healthy, err := b.Check(context.Background()); !healthy || err != nil {
			t.Fatalf("Check() = %t, %v; want healthy", healthy, err)
		}
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("health checks used %d connections, want 1", got)
	}
}
