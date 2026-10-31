package sourceapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/guohuiyuan/go-music-dl/core"
	"github.com/guohuiyuan/go-music-dl/internal/fnos-bridge/sourcecontract"
)

type Config struct {
	TokenFile     string
	SearchTimeout time.Duration
}

type Server struct {
	config    Config
	token     string
	downloads *downloadManager
}

func New(config Config) (*Server, error) {
	if config.SearchTimeout <= 0 {
		config.SearchTimeout = 12 * time.Second
	}
	token, err := loadOrCreateToken(config.TokenFile)
	if err != nil {
		return nil, err
	}
	return &Server{config: config, token: token}, nil
}

// PrepareDownloads attaches the source API to the Web application's already
// started download queue.
func (s *Server) PrepareDownloads(queue *core.DownloadQueue) error {
	if s.downloads != nil {
		return nil
	}
	manager, err := newDownloadManager(queue)
	if err != nil {
		return err
	}
	s.downloads = manager
	return nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", s.handleHealth)
	mux.Handle("/v1/search", s.auth(http.HandlerFunc(s.handleSearch)))
	mux.Handle("/v1/stream", s.auth(http.HandlerFunc(s.handleStream)))
	mux.Handle("/v1/download", s.auth(http.HandlerFunc(s.handleDownload)))
	mux.Handle("/v1/download/status", s.auth(http.HandlerFunc(s.handleDownloadStatus)))
	mux.Handle("/v1/lyric", s.auth(http.HandlerFunc(s.handleLyric)))
	mux.Handle("/v1/cover", s.auth(http.HandlerFunc(s.handleCover)))
	return mux
}

func loadOrCreateToken(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("source API token file is required")
	}
	if data, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(data))
		if len(token) >= 32 {
			return token, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(random[:])
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".source-token-*.tmp")
	if err != nil {
		return "", err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return "", err
	}
	if _, err := temp.WriteString(token); err != nil {
		_ = temp.Close()
		return "", err
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(name, path); err != nil {
		return "", err
	}
	return token, nil
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := r.Header.Get(sourcecontract.TokenHeader)
		if s.token == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": core.AppVersion})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	keyword := strings.TrimSpace(r.URL.Query().Get("keyword"))
	sources := splitCSV(r.URL.Query().Get("sources"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if keyword == "" || len(sources) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "keyword and sources are required"})
		return
	}
	if limit <= 0 {
		limit = 30
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.config.SearchTimeout)
	defer cancel()
	tracks, err := searchTracks(ctx, keyword, sources, limit)
	if err != nil && len(tracks) == 0 {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, tracks)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	track := trackFromRequest(r)
	song := musicSong(track)
	downloadFn := core.GetDownloadFunc(track.Source)
	if track.ID == "" || downloadFn == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported track"})
		return
	}
	streamURL, err := downloadFn(&song)
	if err != nil || strings.TrimSpace(streamURL) == "" {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "source unavailable"})
		return
	}
	rangeHeader := strings.TrimSpace(r.Header.Get("Range"))
	fetch, handled, err := core.NewSourceRangeFetch(streamURL, track.Source, rangeHeader)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if handled {
		contentType := strings.TrimSpace(fetch.ContentType)
		if contentType == "" {
			contentType = core.AudioMimeByExt(track.Ext)
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.FormatInt(fetch.ContentLength, 10))
		if fetch.ContentRange != "" {
			w.Header().Set("Content-Range", fetch.ContentRange)
		}
		w.WriteHeader(fetch.StatusCode)
		if r.Method != http.MethodHead {
			_ = fetch.WriteTo(w)
		}
		return
	}

	method := r.Method
	if method == http.MethodHead {
		method = http.MethodGet
		rangeHeader = "bytes=0-0"
	}
	request, err := core.BuildSourceRequest(method, streamURL, track.Source, rangeHeader)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream request error"})
		return
	}
	response, err := (&http.Client{Timeout: 2 * time.Minute}).Do(request)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream stream error"})
		return
	}
	defer response.Body.Close()
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, response.Body)
	}
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var track sourcecontract.Track
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&track); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid track"})
			return
		}
		result, err := s.downloads.Enqueue(track)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)
	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" || !s.downloads.Forget(id) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "download not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleDownloadStatus(w http.ResponseWriter, r *http.Request) {
	state, ok := s.downloads.Status(strings.TrimSpace(r.URL.Query().Get("id")))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "download not found"})
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleLyric(w http.ResponseWriter, r *http.Request) {
	track := trackFromRequest(r)
	fn := core.GetLyricFunc(track.Source)
	if fn == nil {
		writeJSON(w, http.StatusOK, map[string]any{"lyric": ""})
		return
	}
	song := musicSong(track)
	lyric, err := fn(&song)
	if err != nil {
		log.Printf("source API lyric error for %s/%s: %v", track.Source, track.ID, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"lyric": lyric})
}

func (s *Server) handleCover(w http.ResponseWriter, r *http.Request) {
	track := trackFromRequest(r)
	if strings.TrimSpace(track.Cover) == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	data, mime, err := core.FetchBytesWithMime(track.Cover, track.Source)
	if err != nil || len(data) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if strings.TrimSpace(mime) == "" {
		mime = http.DetectContentType(data)
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func splitCSV(raw string) []string {
	var out []string
	for _, value := range strings.Split(raw, ",") {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}
