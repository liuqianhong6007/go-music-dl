package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/guohuiyuan/go-music-dl/core"
	"github.com/guohuiyuan/go-music-dl/internal/sourceapi"
)

func TestEmbeddedSourceAPIUsesWebSocketAndRequiresLocalToken(t *testing.T) {
	sourceServer, err := sourceapi.New(sourceapi.Config{
		TokenFile:     filepath.Join(t.TempDir(), "source.token"),
		SearchTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := newEngineWithQueueAndSource(
		StartOptions{DisableAuth: true},
		core.GetWebAuthSettings,
		nil,
		sourceServer.Handler(),
	)

	remote := httptest.NewRecorder()
	engine.ServeHTTP(remote, httptest.NewRequest(
		http.MethodGet,
		"http://music.test/internal/source-api/v1/health",
		nil,
	))
	if remote.Code != http.StatusNotFound {
		t.Fatalf("TCP-like source API request = %d, want 404", remote.Code)
	}

	localRequest := httptest.NewRequest(
		http.MethodGet,
		"http://music.test/internal/source-api/v1/health",
		nil,
	)
	localRequest = localRequest.WithContext(context.WithValue(localRequest.Context(), gatewayConnKey{}, true))
	local := httptest.NewRecorder()
	engine.ServeHTTP(local, localRequest)
	if local.Code != http.StatusOK {
		t.Fatalf("Unix source API health = %d, body=%s", local.Code, local.Body.String())
	}

	searchRequest := httptest.NewRequest(
		http.MethodGet,
		"http://music.test/internal/source-api/v1/search?keyword=x&sources=netease",
		nil,
	)
	searchRequest = searchRequest.WithContext(context.WithValue(searchRequest.Context(), gatewayConnKey{}, true))
	search := httptest.NewRecorder()
	engine.ServeHTTP(search, searchRequest)
	if search.Code != http.StatusUnauthorized {
		t.Fatalf("source API without token = %d, want 401", search.Code)
	}
}
