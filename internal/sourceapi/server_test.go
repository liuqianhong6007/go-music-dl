package sourceapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestServerGeneratesTokenAndProtectsAPI(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "source.token")
	server, err := New(Config{
		TokenFile:     tokenFile,
		SearchTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(server.token) < 32 {
		t.Fatalf("generated token is unexpectedly short")
	}

	handler := server.Handler()
	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "http://source/v1/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health = %d", health.Code)
	}
	search := httptest.NewRecorder()
	handler.ServeHTTP(search, httptest.NewRequest(http.MethodGet, "http://source/v1/search?keyword=x&sources=netease", nil))
	if search.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated search = %d", search.Code)
	}
}
