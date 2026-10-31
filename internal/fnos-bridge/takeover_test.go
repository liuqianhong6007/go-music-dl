package fnosbridge

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSocketTakeoverIsReversible(t *testing.T) {
	if testing.Short() {
		t.Skip("exercises live Unix socket takeover")
	}
	dir := shortSocketDir(t)
	target := filepath.Join(dir, "trim_music.socket")
	upstream := filepath.Join(dir, "trim_music_upstream.socket")

	officialListener, err := net.Listen("unix", target)
	if err != nil {
		t.Fatalf("listen official socket: %v", err)
	}
	officialServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "official")
	})}
	officialDone := make(chan struct{})
	go func() {
		_ = officialServer.Serve(officialListener)
		close(officialDone)
	}()
	t.Cleanup(func() {
		_ = officialServer.Close()
		select {
		case <-officialDone:
		case <-time.After(time.Second):
		}
	})

	takeover, err := TakeOver(target, upstream, 0o600)
	if err != nil {
		t.Fatalf("TakeOver: %v", err)
	}
	bridgeServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "bridge")
	})}
	bridgeDone := make(chan struct{})
	go func() {
		_ = bridgeServer.Serve(takeover.Listener)
		close(bridgeDone)
	}()

	if got := requestUnixSocket(t, target, "/"); got != "bridge" {
		t.Fatalf("target before restore = %q, want bridge", got)
	}
	if got := requestUnixSocket(t, upstream, "/"); got != "official" {
		t.Fatalf("upstream = %q, want official", got)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("target socket mode = %v, %v", info.Mode().Perm(), err)
	}

	_ = bridgeServer.Close()
	select {
	case <-bridgeDone:
	case <-time.After(time.Second):
		t.Fatal("bridge server did not stop")
	}
	if err := takeover.Close(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := requestUnixSocket(t, target, "/"); got != "official" {
		t.Fatalf("target after restore = %q, want official", got)
	}
	if _, err := os.Lstat(upstream); !os.IsNotExist(err) {
		t.Fatalf("upstream still exists after restore: %v", err)
	}
}

func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fnos-socket-")
	if err != nil {
		t.Fatalf("create short socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func requestUnixSocket(t *testing.T, socketPath, path string) string {
	t.Helper()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	response, err := client.Get("http://localhost" + path)
	if err != nil {
		t.Fatalf("request %s: %v", socketPath, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s: %v", socketPath, err)
	}
	return strings.TrimSpace(string(body))
}
