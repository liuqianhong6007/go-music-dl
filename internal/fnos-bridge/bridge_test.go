package fnosbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guohuiyuan/go-music-dl/internal/fnos-bridge/sourcecontract"
)

type stubSource struct {
	mu         sync.Mutex
	tracks     []sourcecontract.Track
	lyric      string
	cover      []byte
	stream     string
	streamByID map[string]string
	streamErr  map[string]error
	enqueued   int
	enqueueErr error
	state      sourcecontract.DownloadState
	forgot     string
}

func (s *stubSource) Health(context.Context) error { return nil }

func (s *stubSource) Search(context.Context, string, []string, int) ([]sourcecontract.Track, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sourcecontract.Track(nil), s.tracks...), nil
}

func (s *stubSource) Stream(_ context.Context, track sourcecontract.Track, _ string, _ bool) (*sourcecontract.StreamResult, error) {
	s.mu.Lock()
	streamErr := s.streamErr[track.ID]
	body := s.stream
	if configured, ok := s.streamByID[track.ID]; ok {
		body = configured
	}
	s.mu.Unlock()
	if streamErr != nil {
		return nil, streamErr
	}
	header := make(http.Header)
	header.Set("Content-Type", "audio/mpeg")
	if body == "" {
		body = "2345"
	}
	header.Set("Content-Length", fmt.Sprint(len(body)))
	header.Set("Accept-Ranges", "bytes")
	return &sourcecontract.StreamResult{
		StatusCode: http.StatusPartialContent,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (s *stubSource) EnqueueDownload(context.Context, sourcecontract.Track) (sourcecontract.DownloadResult, error) {
	s.enqueued++
	if s.enqueueErr != nil {
		return sourcecontract.DownloadResult{}, s.enqueueErr
	}
	return sourcecontract.DownloadResult{
		ID: "0123456789abcdef0123456789abcdef", State: s.state,
	}, nil
}

func (s *stubSource) DownloadStatus(_ context.Context, id string) (sourcecontract.DownloadState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, id == "0123456789abcdef0123456789abcdef", nil
}

func (s *stubSource) ForgetDownload(_ context.Context, id string) error {
	s.forgot = id
	return nil
}

func (s *stubSource) Lyric(context.Context, sourcecontract.Track) (string, error) {
	return s.lyric, nil
}

func (s *stubSource) Cover(context.Context, sourcecontract.Track) ([]byte, string, error) {
	return s.cover, "image/png", nil
}

func TestBridgeSearchMetadataLyricsCoverAndRangeStream(t *testing.T) {
	socketDir := shortSocketDir(t)
	upstream := filepath.Join(socketDir, "official.socket")
	stopOfficial := startUnixTestServer(t, upstream, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/music/api/v1/search/track" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"list":[{"guid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","title":"Song","artists":[{"name":"Artist"}],"total":1}],"total":1}}`)
	}))
	defer stopOfficial()

	mappings := NewMappingStore(filepath.Join(t.TempDir(), "guid-map.json"))
	source := &stubSource{
		tracks: []sourcecontract.Track{{
			ID: "online-1", Source: "netease", Name: "Online Song", Artist: "Artist",
			Album: "Album", Ext: "mp3", Duration: 10, Size: 10,
		}},
		lyric: "[00:00.00]Hello",
		cover: []byte("cover-bytes"),
	}
	bridge, err := NewBridge(BridgeConfig{
		Upstream:      upstream,
		Source:        source,
		Mappings:      mappings,
		Sources:       []string{"netease"},
		MaxOnline:     5,
		SearchTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer bridge.Close()

	search := httptest.NewRecorder()
	bridge.ServeHTTP(search, httptest.NewRequest(http.MethodGet, "http://music.local/music/api/v1/search/track?q=Song&size=50", nil))
	if search.Code != http.StatusOK {
		t.Fatalf("search status = %d, body=%s", search.Code, search.Body.String())
	}
	var searchPayload struct {
		Code int `json:"code"`
		Data struct {
			List  []map[string]any `json:"list"`
			Total int              `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(search.Body.Bytes(), &searchPayload); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	if searchPayload.Code != 0 || len(searchPayload.Data.List) != 2 || searchPayload.Data.Total != 2 {
		t.Fatalf("search payload = %#v", searchPayload)
	}
	online := searchPayload.Data.List[1]
	guid, _ := online["guid"].(string)
	if len(guid) != 32 || !strings.HasPrefix(online["title"].(string), "[外部] ") {
		t.Fatalf("online track VO = %#v", online)
	}
	if _, ok := mappings.Get(guid); !ok {
		t.Fatalf("GUID %s not persisted in memory", guid)
	}

	metadata := httptest.NewRecorder()
	bridge.ServeHTTP(metadata, httptest.NewRequest(http.MethodGet, "http://music.local/music/api/v1/track/metadata?guid="+guid, nil))
	if metadata.Code != http.StatusOK || !strings.Contains(metadata.Body.String(), `"track"`) {
		t.Fatalf("metadata = %d %s", metadata.Code, metadata.Body.String())
	}

	lyrics := httptest.NewRecorder()
	bridge.ServeHTTP(lyrics, httptest.NewRequest(http.MethodGet, "http://music.local/music/api/v1/lyric/list?guid="+guid, nil))
	if lyrics.Code != http.StatusOK || !strings.Contains(lyrics.Body.String(), "Hello") {
		t.Fatalf("lyrics = %d %s", lyrics.Code, lyrics.Body.String())
	}

	cover := httptest.NewRecorder()
	bridge.ServeHTTP(cover, httptest.NewRequest(http.MethodGet, "http://music.local/music/api/v1/static/cover?guid="+guid, nil))
	if cover.Code != http.StatusOK || cover.Body.String() != "cover-bytes" {
		t.Fatalf("cover = %d %q", cover.Code, cover.Body.String())
	}

	streamRequest := httptest.NewRequest(http.MethodGet, "http://music.local/music/api/v1/track/stream?guid="+guid, nil)
	streamRequest.Header.Set("Range", "bytes=2-5")
	stream := httptest.NewRecorder()
	bridge.ServeHTTP(stream, streamRequest)
	if stream.Code != http.StatusPartialContent || stream.Body.String() != "2345" {
		t.Fatalf("range stream = %d %q headers=%v", stream.Code, stream.Body.String(), stream.Header())
	}
}

func TestBridgeDesktopStatusPage(t *testing.T) {
	bridge, err := NewBridge(BridgeConfig{
		Upstream: filepath.Join(t.TempDir(), "unused.socket"),
		Source:   &stubSource{},
		Mappings: NewMappingStore(""),
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer bridge.Close()

	page := httptest.NewRecorder()
	bridge.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "http://music.local"+bridgeDesktopPath, nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "音乐下载桥接") {
		t.Fatalf("desktop page = %d %s", page.Code, page.Body.String())
	}

	health := httptest.NewRecorder()
	bridge.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "http://music.local"+bridgeDesktopPath+"/healthz", nil))
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"ok":true`) {
		t.Fatalf("desktop health = %d %s", health.Code, health.Body.String())
	}
}

func TestBridgeStreamAutoSwitchesUnavailableSource(t *testing.T) {
	mappings := NewMappingStore("")
	record := mappings.Remember(sourcecontract.Track{
		ID: "primary-1", Source: "netease", Name: "Online Song", Artist: "Artist",
		Album: "Album", Ext: "mp3", Duration: 180, Size: 4096,
	})
	source := &stubSource{
		tracks: []sourcecontract.Track{
			{
				ID: "primary-1", Source: "netease", Name: "Online Song", Artist: "Artist",
				Album: "Album", Ext: "mp3", Duration: 180, Size: 4096,
			},
			{
				ID: "fallback-1", Source: "qq", Name: "Online Song", Artist: "Artist",
				Album: "Album", Ext: "flac", Duration: 181, Size: 8192,
			},
		},
		streamByID: map[string]string{"fallback-1": "fallback-audio"},
		streamErr:  map[string]error{"primary-1": errors.New("source unavailable")},
	}
	bridge, err := NewBridge(BridgeConfig{
		Upstream:      filepath.Join(t.TempDir(), "unused.socket"),
		Source:        source,
		Mappings:      mappings,
		Sources:       []string{"netease", "qq"},
		MaxOnline:     8,
		SearchTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer bridge.Close()

	stream := httptest.NewRecorder()
	bridge.ServeHTTP(stream, httptest.NewRequest(
		http.MethodGet,
		"http://music.local/music/api/v1/track/stream?guid="+record.GUID,
		nil,
	))
	if stream.Code != http.StatusPartialContent || stream.Body.String() != "fallback-audio" {
		t.Fatalf("fallback stream = %d %q", stream.Code, stream.Body.String())
	}
	if got := stream.Header().Get("X-MusicDL-Source"); got != "qq" {
		t.Fatalf("fallback source header = %q, want qq", got)
	}
	rebound, ok := mappings.Get(record.GUID)
	if !ok || rebound.Source != "qq" || rebound.ID != "fallback-1" || rebound.Ext != "mp3" {
		t.Fatalf("rebound mapping = %#v, ok=%v", rebound, ok)
	}
	if rebound.GUID != record.GUID {
		t.Fatalf("fallback changed GUID from %s to %s", record.GUID, rebound.GUID)
	}
}

func TestBridgeStreamRejectsUnrelatedFallback(t *testing.T) {
	mappings := NewMappingStore("")
	record := mappings.Remember(sourcecontract.Track{
		ID: "primary-1", Source: "netease", Name: "Online Song", Artist: "Artist", Duration: 180,
	})
	source := &stubSource{
		tracks: []sourcecontract.Track{{
			ID: "other-1", Source: "qq", Name: "Different Song", Artist: "Other Artist", Duration: 180,
		}},
		streamErr: map[string]error{"primary-1": errors.New("source unavailable")},
	}
	bridge, err := NewBridge(BridgeConfig{
		Upstream:      filepath.Join(t.TempDir(), "unused.socket"),
		Source:        source,
		Mappings:      mappings,
		Sources:       []string{"netease", "qq"},
		SearchTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer bridge.Close()

	stream := httptest.NewRecorder()
	bridge.ServeHTTP(stream, httptest.NewRequest(
		http.MethodGet,
		"http://music.local/music/api/v1/track/stream?guid="+record.GUID,
		nil,
	))
	if stream.Code != http.StatusBadGateway {
		t.Fatalf("unrelated fallback status = %d, body=%s", stream.Code, stream.Body.String())
	}
	current, ok := mappings.Get(record.GUID)
	if !ok || current.Source != "netease" || current.ID != "primary-1" {
		t.Fatalf("mapping changed after rejected fallback: %#v, ok=%v", current, ok)
	}
}

func TestBridgeStreamUpdatesExtensionFromContentType(t *testing.T) {
	mappings := NewMappingStore("")
	record := mappings.Remember(sourcecontract.Track{
		ID: "primary-1", Source: "netease", Name: "Online Song", Artist: "Artist",
		Ext: "flac", Duration: 180,
	})
	bridge, err := NewBridge(BridgeConfig{
		Upstream: filepath.Join(t.TempDir(), "unused.socket"),
		Source:   &stubSource{},
		Mappings: mappings,
		Sources:  []string{"netease"},
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer bridge.Close()

	stream := httptest.NewRecorder()
	bridge.ServeHTTP(stream, httptest.NewRequest(
		http.MethodGet,
		"http://music.local/music/api/v1/track/stream?guid="+record.GUID,
		nil,
	))
	if stream.Code != http.StatusPartialContent {
		t.Fatalf("stream status = %d, body=%s", stream.Code, stream.Body.String())
	}
	current, ok := mappings.Get(record.GUID)
	if !ok || current.Ext != "mp3" {
		t.Fatalf("stream extension = %#v, ok=%v", current, ok)
	}
}

func TestBridgeRoutesExternalDownloadToNASOnly(t *testing.T) {
	socketDir := shortSocketDir(t)
	upstream := filepath.Join(socketDir, "official.socket")
	stopOfficial := startUnixTestServer(t, upstream, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"official","data":{"status":"success","downloadId":"official-id"}}`)
	}))
	defer stopOfficial()

	mappings := NewMappingStore(filepath.Join(t.TempDir(), "guid-map.json"))
	record := mappings.Remember(sourcecontract.Track{
		ID: "song-1", Source: "netease", Name: "Song", Artist: "Artist",
	})
	downloadPath := filepath.Join(t.TempDir(), "Artist - Song.mp3")
	if err := os.WriteFile(downloadPath, []byte("complete-audio"), 0o644); err != nil {
		t.Fatalf("write download fixture: %v", err)
	}
	source := &stubSource{state: sourcecontract.DownloadState{Status: "ready", Percent: 100, Path: downloadPath}}
	bridge, err := NewBridge(BridgeConfig{
		Upstream:      upstream,
		Source:        source,
		Mappings:      mappings,
		Sources:       []string{"netease"},
		SearchTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer bridge.Close()

	prepareBody := strings.NewReader(`{"trackGUID":"` + record.GUID + `","quality":"original"}`)
	prepare := httptest.NewRecorder()
	bridge.ServeHTTP(prepare, httptest.NewRequest(http.MethodPost, "http://music.local/music/api/v1/download/track/transcode/prepare", prepareBody))
	if prepare.Code != http.StatusOK || !strings.Contains(prepare.Body.String(), `"status":"success"`) {
		t.Fatalf("prepare = %d %s", prepare.Code, prepare.Body.String())
	}
	if source.enqueued != 1 {
		t.Fatalf("external prepare did not enqueue NAS download")
	}

	direct := httptest.NewRecorder()
	bridge.ServeHTTP(direct, httptest.NewRequest(http.MethodGet, "http://music.local/music/api/v1/download/track?trackGUID="+record.GUID, nil))
	if direct.Code != http.StatusOK || direct.Body.String() != "complete-audio" {
		t.Fatalf("direct download = %d %s", direct.Code, direct.Body.String())
	}
	if source.enqueued != 2 {
		t.Fatalf("direct external download did not enqueue NAS download")
	}

	detail := httptest.NewRecorder()
	bridge.ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "http://music.local/music/api/v1/download/track/detail?trackGUID="+record.GUID, nil))
	if detail.Code != http.StatusOK ||
		!strings.Contains(detail.Body.String(), `"downloaded":true`) ||
		!strings.Contains(detail.Body.String(), `"code":0`) {
		t.Fatalf("download detail = %d %s", detail.Code, detail.Body.String())
	}

	const officialGUID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, ok := mappings.BindOfficial(record.GUID, officialGUID); !ok {
		t.Fatal("failed to bind official GUID")
	}
	redirect := httptest.NewRecorder()
	bridge.ServeHTTP(redirect, httptest.NewRequest(
		http.MethodGet,
		"http://music.local/music/api/v1/download/track?trackGUID="+record.GUID,
		nil,
	))
	if redirect.Code != http.StatusOK || redirect.Body.String() != "complete-audio" {
		t.Fatalf("bound download = %d %s", redirect.Code, redirect.Body.String())
	}

	status := httptest.NewRecorder()
	bridge.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "http://music.local/music/api/v1/download/track/transcode/status?downloadId=0123456789abcdef0123456789abcdef", nil))
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"status":"ready"`) {
		t.Fatalf("status = %d %s", status.Code, status.Body.String())
	}

	file := httptest.NewRecorder()
	bridge.ServeHTTP(file, httptest.NewRequest(http.MethodGet, "http://music.local/music/api/v1/download/track/transcode/file?downloadId=0123456789abcdef0123456789abcdef", nil))
	if file.Code != http.StatusOK || file.Body.String() != "complete-audio" ||
		file.Header().Get("Content-Length") != "14" ||
		!strings.Contains(file.Header().Get("Content-Disposition"), "Artist - Song.mp3") {
		t.Fatalf("file = %d body=%q headers=%v", file.Code, file.Body.String(), file.Header())
	}

	rangeRequest := httptest.NewRequest(http.MethodGet, "http://music.local/music/api/v1/download/track/transcode/file?downloadId=0123456789abcdef0123456789abcdef", nil)
	rangeRequest.Header.Set("Range", "bytes=0-3")
	rangeFile := httptest.NewRecorder()
	bridge.ServeHTTP(rangeFile, rangeRequest)
	if rangeFile.Code != http.StatusPartialContent || rangeFile.Body.String() != "comp" {
		t.Fatalf("range file = %d body=%q headers=%v", rangeFile.Code, rangeFile.Body.String(), rangeFile.Header())
	}

	del := httptest.NewRecorder()
	bridge.ServeHTTP(del, httptest.NewRequest(http.MethodPost, "http://music.local/music/api/v1/download/track/transcode/delete", strings.NewReader(`{"downloadId":"0123456789abcdef0123456789abcdef"}`)))
	if del.Code != http.StatusOK || source.forgot != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("delete = %d forgot=%q", del.Code, source.forgot)
	}

	local := httptest.NewRecorder()
	bridge.ServeHTTP(local, httptest.NewRequest(http.MethodPost, "http://music.local/music/api/v1/download/track/transcode/prepare", strings.NewReader(`{"trackGUID":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)))
	if local.Code != http.StatusOK || !strings.Contains(local.Body.String(), `"msg":"official"`) {
		t.Fatalf("local prepare was not passed through: %d %s", local.Code, local.Body.String())
	}
	if source.enqueued != 3 {
		t.Fatalf("local prepare incorrectly enqueued NAS download")
	}
}

func TestBridgeDirectDownloadWaitsForNAS(t *testing.T) {
	socketDir := shortSocketDir(t)
	upstream := filepath.Join(socketDir, "official.socket")
	stopOfficial := startUnixTestServer(t, upstream, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer stopOfficial()

	mappings := NewMappingStore(filepath.Join(t.TempDir(), "guid-map.json"))
	record := mappings.Remember(sourcecontract.Track{
		ID: "song-1", Source: "netease", Name: "Song", Artist: "Artist",
	})
	downloadPath := filepath.Join(t.TempDir(), "Artist - Song.mp3")
	if err := os.WriteFile(downloadPath, []byte("complete-audio"), 0o644); err != nil {
		t.Fatalf("write download fixture: %v", err)
	}
	source := &stubSource{state: sourcecontract.DownloadState{Status: "waiting", Percent: 5}}
	bridge, err := NewBridge(BridgeConfig{
		Upstream:      upstream,
		Source:        source,
		Mappings:      mappings,
		Sources:       []string{"netease"},
		SearchTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer bridge.Close()

	go func() {
		time.Sleep(50 * time.Millisecond)
		source.mu.Lock()
		source.state = sourcecontract.DownloadState{Status: "ready", Percent: 100, Path: downloadPath}
		source.mu.Unlock()
	}()
	response := httptest.NewRecorder()
	bridge.ServeHTTP(response, httptest.NewRequest(
		http.MethodGet,
		"http://music.local/music/api/v1/download/track?trackGUID="+record.GUID,
		nil,
	))
	if response.Code != http.StatusOK || response.Body.String() != "complete-audio" {
		t.Fatalf("waiting direct download = %d body=%q", response.Code, response.Body.String())
	}
}

func TestBridgeTaskListMergesSourceDownload(t *testing.T) {
	socketDir := shortSocketDir(t)
	upstream := filepath.Join(socketDir, "official.socket")
	stopOfficial := startUnixTestServer(t, upstream, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/music/api/v1/task/list" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"","data":[{"id":"official-task","type":"fileScan","name":"Scan","total":2,"successCount":1,"failCount":0,"done":false,"cancelling":false,"canceled":false,"canceledCount":0,"createdAt":1,"doneAt":0}]}`)
	}))
	defer stopOfficial()

	mappings := NewMappingStore(filepath.Join(t.TempDir(), "guid-map.json"))
	record := mappings.Remember(sourcecontract.Track{
		ID: "song-1", Source: "netease", Name: "Song", Artist: "Artist",
	})
	source := &stubSource{state: sourcecontract.DownloadState{Status: "downloading", Percent: 50}}
	bridge, err := NewBridge(BridgeConfig{
		Upstream:      upstream,
		Source:        source,
		Mappings:      mappings,
		Sources:       []string{"netease"},
		SearchTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer bridge.Close()

	download := httptest.NewRecorder()
	bridge.ServeHTTP(download, httptest.NewRequest(
		http.MethodPost,
		"http://music.local/music/api/v1/download/track?trackGUID="+record.GUID,
		nil,
	))
	if download.Code != http.StatusOK {
		t.Fatalf("download = %d %s", download.Code, download.Body.String())
	}

	list := httptest.NewRecorder()
	bridge.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "http://music.local/music/api/v1/task/list", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("task list = %d %s", list.Code, list.Body.String())
	}
	var payload struct {
		Code int `json:"code"`
		Data []struct {
			ID    string `json:"id"`
			Type  string `json:"type"`
			Name  string `json:"name"`
			Done  bool   `json:"done"`
			Total int    `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode task list: %v", err)
	}
	if payload.Code != 0 || len(payload.Data) != 2 {
		t.Fatalf("task list payload = %s", list.Body.String())
	}
	if payload.Data[0].ID != "official-task" || payload.Data[1].ID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("task order = %#v", payload.Data)
	}
	if payload.Data[1].Type != "download" || payload.Data[1].Done || payload.Data[1].Total != 1 ||
		!strings.Contains(payload.Data[1].Name, "Song") {
		t.Fatalf("source task = %#v", payload.Data[1])
	}

	source.mu.Lock()
	source.state = sourcecontract.DownloadState{Status: "ready", Percent: 100}
	source.mu.Unlock()
	completed := httptest.NewRecorder()
	bridge.ServeHTTP(completed, httptest.NewRequest(http.MethodGet, "http://music.local/music/api/v1/task/list", nil))
	if completed.Code != http.StatusOK ||
		!strings.Contains(completed.Body.String(), `"id":"0123456789abcdef0123456789abcdef"`) ||
		!strings.Contains(completed.Body.String(), `"done":true`) ||
		!strings.Contains(completed.Body.String(), `"successCount":1`) {
		t.Fatalf("completed source task was not shown once: %d %s", completed.Code, completed.Body.String())
	}

	bridge.taskMu.Lock()
	bridge.activeTasks["0123456789abcdef0123456789abcdef"] = activeDownload{
		record:     record,
		createdAt:  time.Now().Add(-time.Minute).UnixMilli(),
		finishedAt: time.Now().Add(-completedDownloadLinger).UnixMilli(),
	}
	bridge.taskMu.Unlock()
	expired := httptest.NewRecorder()
	bridge.ServeHTTP(expired, httptest.NewRequest(http.MethodGet, "http://music.local/music/api/v1/task/list", nil))
	if expired.Code != http.StatusOK || strings.Contains(expired.Body.String(), `"id":"0123456789abcdef0123456789abcdef"`) {
		t.Fatalf("expired completed task remained in active list: %d %s", expired.Code, expired.Body.String())
	}
}

func TestFavoriteListRetriesBoundIntentPerUserWithoutDownload(t *testing.T) {
	socketDir := shortSocketDir(t)
	upstream := filepath.Join(socketDir, "official.socket")
	const officialGUID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	var (
		mu          sync.Mutex
		writes      []string
		userMECalls int
	)
	stopOfficial := startUnixTestServer(t, upstream, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/music/api/v1/favorite-track/list":
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"list":[],"total":0,"sort":"favoriteAt,desc"}}`)
		case "/music/api/v1/user/me":
			mu.Lock()
			userMECalls++
			mu.Unlock()
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"guid":"user-a"}}`)
		case "/music/api/v1/favorite-track/create":
			var payload map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			mu.Lock()
			writes = append(writes, payload["trackGUID"])
			mu.Unlock()
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":null}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer stopOfficial()

	mappings := NewMappingStore(filepath.Join(t.TempDir(), "guid-map.json"))
	record := mappings.Remember(sourcecontract.Track{ID: "song-1", Source: "netease", Name: "Song", Artist: "Artist"})
	_, _ = mappings.BindOfficial(record.GUID, officialGUID)
	_, _ = mappings.SetFavoriteState(record.GUID, "gateway:1000", "pending", "")
	_, _ = mappings.SetFavoriteState(record.GUID, "gateway:2000", "pending", "")
	source := &stubSource{state: sourcecontract.DownloadState{Status: "ready", Percent: 100}}
	bridge, err := NewBridge(BridgeConfig{
		Upstream: upstream, Source: source, Mappings: mappings,
		Sources: []string{"netease"}, SearchTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer bridge.Close()

	request := httptest.NewRequest(http.MethodGet, "http://music.local/music/api/v1/favorite-track/list", nil)
	request.Header.Set("Cookie", "music-token=test")
	request.Header.Set(gatewayUserIDHeader, "1000")
	response := httptest.NewRecorder()
	bridge.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), record.GUID) ||
		!strings.Contains(response.Body.String(), `"isFavorite":true`) {
		t.Fatalf("favorite list = %d %s", response.Code, response.Body.String())
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, _ := mappings.Get(record.GUID)
		if !pendingFavoriteForUser(current, "gateway:1000") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, _ := mappings.Get(record.GUID)
	if pendingFavoriteForUser(current, "gateway:1000") {
		t.Fatal("gateway user 1000 pending favorite was not cleared")
	}
	if !pendingFavoriteForUser(current, "gateway:2000") {
		t.Fatal("retrying gateway user 1000 cleared gateway user 2000 favorite")
	}
	mu.Lock()
	gotWrites := append([]string(nil), writes...)
	gotUserMECalls := userMECalls
	mu.Unlock()
	if len(gotWrites) != 1 || gotWrites[0] != officialGUID {
		t.Fatalf("official favorite writes = %v", gotWrites)
	}
	if source.enqueued != 0 {
		t.Fatalf("favorite list unexpectedly triggered downloads: %d", source.enqueued)
	}
	if gotUserMECalls != 0 {
		t.Fatalf("favorite list consumed auth ticket with %d /user/me probes", gotUserMECalls)
	}
}

func TestFavoriteRemovedWhenNASDownloadFails(t *testing.T) {
	socketDir := shortSocketDir(t)
	upstream := filepath.Join(socketDir, "official.socket")
	stopOfficial := startUnixTestServer(t, upstream, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer stopOfficial()

	mappings := NewMappingStore(filepath.Join(t.TempDir(), "guid-map.json"))
	record := mappings.Remember(sourcecontract.Track{ID: "song-failed", Source: "kugou", Name: "Song", Artist: "Artist"})
	source := &stubSource{state: sourcecontract.DownloadState{Status: "failed", Percent: 100, Error: "source unavailable"}}
	bridge, err := NewBridge(BridgeConfig{
		Upstream: upstream, Source: source, Mappings: mappings,
		Sources: []string{"kugou"}, SearchTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer bridge.Close()

	request := httptest.NewRequest(
		http.MethodPost,
		"http://music.local/music/api/v1/favorite-track/create",
		strings.NewReader(`{"trackGUID":"`+record.GUID+`"}`),
	)
	request.Header.Set(gatewayUserIDHeader, "1000")
	response := httptest.NewRecorder()
	bridge.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("favorite create = %d %s", response.Code, response.Body.String())
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, _ := mappings.Get(record.GUID)
		if !pendingFavoriteForUser(current, "gateway:1000") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, _ := mappings.Get(record.GUID)
	t.Fatalf("failed NAS download left favorite pending: %#v", current.Favorites)
}

func TestFavoriteRemovedWhenNASEnqueueFails(t *testing.T) {
	socketDir := shortSocketDir(t)
	upstream := filepath.Join(socketDir, "official.socket")
	stopOfficial := startUnixTestServer(t, upstream, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer stopOfficial()

	mappings := NewMappingStore(filepath.Join(t.TempDir(), "guid-map.json"))
	record := mappings.Remember(sourcecontract.Track{ID: "song-enqueue-failed", Source: "kugou", Name: "Song", Artist: "Artist"})
	source := &stubSource{enqueueErr: errors.New("queue unavailable")}
	bridge, err := NewBridge(BridgeConfig{
		Upstream: upstream, Source: source, Mappings: mappings,
		Sources: []string{"kugou"}, SearchTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer bridge.Close()

	request := httptest.NewRequest(
		http.MethodPost,
		"http://music.local/music/api/v1/favorite-track/create",
		strings.NewReader(`{"trackGUID":"`+record.GUID+`"}`),
	)
	request.Header.Set(gatewayUserIDHeader, "1000")
	response := httptest.NewRecorder()
	bridge.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), `"code":-1`) {
		t.Fatalf("favorite enqueue failure = %d %s", response.Code, response.Body.String())
	}
	current, _ := mappings.Get(record.GUID)
	if pendingFavoriteForUser(current, "gateway:1000") {
		t.Fatalf("enqueue failure left favorite pending: %#v", current.Favorites)
	}
}

func startUnixTestServer(t *testing.T, path string, handler http.Handler) func() {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	server := &http.Server{Handler: handler}
	done := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(done)
	}()
	return func() {
		_ = server.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		_ = os.Remove(path)
	}
}
