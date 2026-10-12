package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/adapters/httpapi"
)

func TestServeHealthAndCancellation(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, listener, httpapi.NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil))
	}()
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://" + listener.Addr().String() + "/health")
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", response.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancellation failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP server did not finish after cancellation")
	}
	connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err == nil {
		if closeErr := connection.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		t.Fatal("listener remained open after shutdown")
	}
}

func TestServeReportsListenerFailure(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := Serve(ctx, listener, httpapi.NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)); err == nil {
		t.Fatal("closed listener was reported as success")
	}
}
