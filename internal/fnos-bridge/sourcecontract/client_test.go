package sourcecontract

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClientUsesUnixSocketAndToken(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "source-contract-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "source.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(TokenHeader) != "test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/health":
			_, _ = io.WriteString(w, `{"ok":true}`)
		case "/v1/search":
			_ = json.NewEncoder(w).Encode([]Track{{ID: "1", Source: "netease", Name: "Song"}})
		case "/v1/stream":
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Header().Set("Content-Range", "bytes 2-5/10")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "2345")
		case "/v1/download":
			if r.Method == http.MethodPost {
				_, _ = io.WriteString(w, `{"id":"download-1","state":{"status":"waiting","percent":5}}`)
			} else {
				_, _ = io.WriteString(w, `{"deleted":true}`)
			}
		case "/v1/download/status":
			_, _ = io.WriteString(w, `{"status":"ready","percent":100}`)
		case "/v1/lyric":
			_, _ = io.WriteString(w, `{"lyric":"[00:00.00]Hello"}`)
		case "/v1/cover":
			w.Header().Set("Content-Type", "image/png")
			_, _ = io.WriteString(w, "cover")
		default:
			http.NotFound(w, r)
		}
	})}
	done := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(done)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	})

	client := NewClientWithPrefix(socket, "test-token", "")
	defer client.Close()
	ctx := context.Background()
	if err := client.Health(ctx); err != nil {
		t.Fatal(err)
	}
	tracks, err := client.Search(ctx, "Song", []string{"netease"}, 10)
	if err != nil || len(tracks) != 1 || tracks[0].ID != "1" {
		t.Fatalf("search = %#v, %v", tracks, err)
	}
	stream, err := client.Stream(ctx, tracks[0], "bytes=2-5", false)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(stream.Body)
	_ = stream.Body.Close()
	if stream.StatusCode != http.StatusPartialContent || string(body) != "2345" {
		t.Fatalf("stream = %d %q", stream.StatusCode, body)
	}
	download, err := client.EnqueueDownload(ctx, tracks[0])
	if err != nil || download.ID != "download-1" || download.State.Status != "waiting" {
		t.Fatalf("download = %#v, %v", download, err)
	}
	state, ok, err := client.DownloadStatus(ctx, download.ID)
	if err != nil || !ok || state.Status != "ready" {
		t.Fatalf("status = %#v %v %v", state, ok, err)
	}
	if err := client.ForgetDownload(ctx, download.ID); err != nil {
		t.Fatal(err)
	}
	lyric, err := client.Lyric(ctx, tracks[0])
	if err != nil || !strings.Contains(lyric, "Hello") {
		t.Fatalf("lyric = %q, %v", lyric, err)
	}
	cover, mime, err := client.Cover(ctx, tracks[0])
	if err != nil || string(cover) != "cover" || mime != "image/png" {
		t.Fatalf("cover = %q %q %v", cover, mime, err)
	}
}
