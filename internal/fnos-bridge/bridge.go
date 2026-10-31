package fnosbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/guohuiyuan/go-music-dl/internal/fnos-bridge/sourcecontract"
)

const sourceTag = "[外部] "

var transparentPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
	0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9c, 0x63, 0xf8, 0xcf, 0xc0, 0xf0,
	0x1f, 0x00, 0x05, 0x05, 0x02, 0x00, 0x5f, 0xc8,
	0xe7, 0x41, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45,
	0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

type BridgeConfig struct {
	Upstream      string
	Source        sourcecontract.Source
	Mappings      *MappingStore
	Sources       []string
	MaxOnline     int
	SearchTimeout time.Duration
	Version       string
}

// Bridge implements the fnOS music API subset needed for online search and
// playback. Every unhandled request is forwarded to the official backend.
type Bridge struct {
	upstream      string
	source        sourcecontract.Source
	mappings      *MappingStore
	sources       []string
	maxOnline     int
	searchTimeout time.Duration
	transport     *http.Transport
	proxy         *httputil.ReverseProxy
	handler       http.Handler
	version       string
	bindingMu     sync.Mutex
	bindingJobs   map[string]*bindingJob
	fallbackMu    sync.Mutex
	taskMu        sync.Mutex
	activeTasks   map[string]activeDownload
	favoriteMu    sync.Mutex
	favoriteRetry map[string]int64
}

func NewBridge(config BridgeConfig) (*Bridge, error) {
	upstream := strings.TrimSpace(config.Upstream)
	if upstream == "" {
		return nil, fmt.Errorf("official upstream Unix socket is required")
	}
	if config.Source == nil {
		return nil, fmt.Errorf("go-music-dl source API client is required")
	}
	if config.Mappings == nil {
		config.Mappings = NewMappingStore("")
	}
	if config.MaxOnline <= 0 {
		config.MaxOnline = 30
	}
	if config.SearchTimeout <= 0 {
		config.SearchTimeout = 12 * time.Second
	}
	if len(config.Sources) == 0 {
		config.Sources = []string{"netease", "qq", "kugou", "kuwo", "migu"}
	}

	transport := &http.Transport{
		Proxy:                 nil,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
	}
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", upstream)
	}

	target := &url.URL{Scheme: "http", Host: "unix"}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("fnOS music bridge upstream error for %s: %v", r.URL.Path, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"code": -1,
			"msg":  "official music backend unavailable",
			"data": nil,
		})
	}

	bridge := &Bridge{
		upstream:      upstream,
		source:        config.Source,
		mappings:      config.Mappings,
		sources:       append([]string(nil), config.Sources...),
		maxOnline:     config.MaxOnline,
		searchTimeout: config.SearchTimeout,
		transport:     transport,
		proxy:         proxy,
		version:       config.Version,
		bindingJobs:   make(map[string]*bindingJob),
		activeTasks:   make(map[string]activeDownload),
		favoriteRetry: make(map[string]int64),
	}
	bridge.handler = bridge.routes()
	return bridge, nil
}

func (b *Bridge) Handler() http.Handler {
	return b.handler
}

func (b *Bridge) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/_ext/fnos-go-music-dl/healthz", b.handleHealth)
	mux.HandleFunc(bridgeDesktopPath, b.handleDesktopStatus)
	mux.HandleFunc(bridgeDesktopPath+"/", b.handleDesktopStatus)
	mux.HandleFunc("/music/api/v1/search/track", b.handleSearch)
	mux.HandleFunc("/music/api/v1/track/stream", b.handleStream)
	mux.HandleFunc("/music/api/v1/track/stream/", b.handleStream)
	mux.HandleFunc("/music/api/v1/track/metadata", b.handleMetadata)
	mux.HandleFunc("/music/api/v1/track/metadata/", b.handleMetadata)
	mux.HandleFunc("/music/api/v1/track/audio-info", b.handleMetadata)
	mux.HandleFunc("/music/api/v1/lyric/list", b.handleLyrics)
	mux.HandleFunc("/music/api/v1/lyric/list/", b.handleLyrics)
	mux.HandleFunc("/music/api/v1/track/lyrics", b.handleLyrics)
	mux.HandleFunc("/music/api/v1/track/lyrics/", b.handleLyrics)
	mux.HandleFunc("/music/api/v1/detail/lyrics/", b.handleLyrics)
	mux.HandleFunc("/music/api/v1/static/cover", b.handleCover)
	mux.HandleFunc("/music/api/v1/static/cover/", b.handleCover)
	mux.HandleFunc("/music/api/v1/download/track", b.handleDownloadTrack)
	mux.HandleFunc("/music/api/v1/download/track/detail", b.handleDownloadDetail)
	mux.HandleFunc("/music/api/v1/download/track/transcode/prepare", b.handleDownloadPrepare)
	mux.HandleFunc("/music/api/v1/download/track/transcode/status", b.handleDownloadStatus)
	mux.HandleFunc("/music/api/v1/download/track/transcode/file", b.handleDownloadFile)
	mux.HandleFunc("/music/api/v1/download/track/transcode/file/", b.handleDownloadFile)
	mux.HandleFunc("/music/api/v1/download/track/transcode/delete", b.handleDownloadDelete)
	mux.HandleFunc("/music/api/v1/favorite-track/create", b.handleFavoriteCreate)
	mux.HandleFunc("/music/api/v1/favorite-track/delete", b.handleFavoriteDelete)
	mux.HandleFunc("/music/api/v1/favorite-track/list", b.handleFavoriteList)
	mux.HandleFunc("/music/api/v1/task/list", b.handleTaskList)
	mux.HandleFunc("/", b.handleUpstream)
	return mux
}

func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.handler.ServeHTTP(w, r)
}

func (b *Bridge) Close() {
	if b != nil && b.transport != nil {
		b.transport.CloseIdleConnections()
	}
}

func (b *Bridge) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"app":     "go-music-dl-fnos-bridge",
		"version": b.version,
		"sources": b.sources,
	})
}

func (b *Bridge) handleUpstream(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(strings.ToLower(r.URL.Path), "download") {
		log.Printf("fnOS download passthrough: %s %s keys=%s guid=%s quality=%s",
			r.Method, r.URL.Path, strings.Join(sortedQueryKeys(r.URL.Query()), ","),
			requestGUID(r), r.URL.Query().Get("quality"))
	}
	b.proxy.ServeHTTP(w, r)
}

func (b *Bridge) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		b.handleUpstream(w, r)
		return
	}
	keyword := requestKeyword(r)
	page := queryInt(r, "page", 1)
	size := queryInt(r, "size", 50)
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 50
	}
	if size > 200 {
		size = 200
	}
	if keyword == "" || page > 1 {
		b.handleUpstream(w, r)
		return
	}

	upstreamResponse, body, err := b.fetchUpstreamJSON(r, b.searchTimeout+5*time.Second)
	if err != nil {
		log.Printf("fnOS music local search failed: %v", err)
		b.handleUpstream(w, r)
		return
	}
	if upstreamResponse.StatusCode != http.StatusOK {
		writeHTTPResponse(w, upstreamResponse, body)
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeHTTPResponse(w, upstreamResponse, body)
		return
	}
	if code, ok := numberValue(payload["code"]); !ok || int(code) != 0 {
		writeHTTPResponse(w, upstreamResponse, body)
		return
	}
	data, ok := payload["data"].(map[string]any)
	if !ok {
		writeHTTPResponse(w, upstreamResponse, body)
		return
	}
	localList, _ := data["list"].([]any)
	localTotal := integerValue(data["total"], len(localList))
	if localTotal < len(localList) {
		localTotal = len(localList)
	}

	ctx, cancel := context.WithTimeout(r.Context(), b.searchTimeout)
	defer cancel()
	tracks, searchErr := b.source.Search(ctx, keyword, b.sources, b.maxOnline)
	if searchErr != nil && len(tracks) == 0 {
		log.Printf("fnOS music online search %q: %v", keyword, searchErr)
	}

	localKeys := make(map[string]struct{}, len(localList))
	for _, item := range localList {
		if key := trackKey(item); key != "" {
			localKeys[key] = struct{}{}
		}
	}
	onlineList := make([]any, 0, len(tracks))
	for _, track := range tracks {
		if strings.TrimSpace(track.ID) == "" || strings.TrimSpace(track.Source) == "" {
			continue
		}
		if key := onlineKeyFromTrack(track); key != "" {
			if _, exists := localKeys[key]; exists {
				continue
			}
			localKeys[key] = struct{}{}
		}
		record := b.mappings.Remember(track)
		onlineList = append(onlineList, b.trackVO(record))
	}
	if len(onlineList) > 0 {
		if err := b.mappings.Save(); err != nil {
			log.Printf("fnOS GUID map save failed: %v", err)
		}
	}

	mergedList := append(append(make([]any, 0, len(localList)+len(onlineList)), localList...), onlineList...)
	data["list"] = mergedList
	data["total"] = localTotal + len(onlineList)
	payload["code"] = 0
	payload["msg"] = "OK"
	writeJSON(w, http.StatusOK, payload)
}

func (b *Bridge) handleMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		b.handleUpstream(w, r)
		return
	}
	record, ok := b.recordForRequest(r)
	if !ok {
		b.handleUpstream(w, r)
		return
	}
	vo := b.trackVO(record)
	payload := map[string]any{
		"code": 0,
		"msg":  "ok",
		"data": vo,
	}
	vo["track"] = cloneMapWithout(vo, "track")
	writeJSON(w, http.StatusOK, payload)
}

func (b *Bridge) handleLyrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		b.handleUpstream(w, r)
		return
	}
	record, ok := b.recordForRequest(r)
	if !ok {
		b.handleUpstream(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), b.searchTimeout)
	defer cancel()
	text, err := b.source.Lyric(ctx, record.track())
	if err != nil {
		log.Printf("fnOS lyric fetch failed for %s/%s: %v", record.Source, record.ID, err)
	}
	text = strings.TrimSpace(text)
	if strings.Contains(r.URL.Path, "/lyric/list") {
		list := make([]any, 0, 1)
		preferred := ""
		if text != "" {
			lyricGUID := record.GUID + ":lyric"
			preferred = lyricGUID
			now := time.Now().Unix()
			list = append(list, map[string]any{
				"guid":      lyricGUID,
				"content":   text,
				"source":    2,
				"isLRC":     true,
				"offset":    0,
				"createdAt": now,
				"updatedAt": now,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"code": 0, "msg": "ok",
			"data": map[string]any{"list": list, "preferred": preferred},
		})
		return
	}
	if text == "" {
		writeJSON(w, http.StatusOK, map[string]any{"code": 0, "msg": "ok", "data": map[string]any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code": 0,
		"msg":  "ok",
		"data": map[string]any{"guid": record.GUID, "lyric": text},
	})
}

func (b *Bridge) handleCover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		b.handleUpstream(w, r)
		return
	}
	record, ok := b.recordForRequest(r)
	if !ok {
		b.handleUpstream(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), b.searchTimeout)
	defer cancel()
	data, mime, err := b.source.Cover(ctx, record.track())
	if err != nil {
		log.Printf("fnOS cover fetch failed for %s/%s: %v", record.Source, record.ID, err)
	}
	if len(data) == 0 {
		data, mime = transparentPNG, "image/png"
	}
	if strings.TrimSpace(mime) == "" {
		mime = http.DetectContentType(data)
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func (b *Bridge) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		b.handleUpstream(w, r)
		return
	}
	record, ok := b.recordForRequest(r)
	if !ok {
		b.handleUpstream(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), b.searchTimeout)
	defer cancel()
	result, resolved, err := b.streamWithFallback(
		ctx, record, strings.TrimSpace(r.Header.Get("Range")), r.Method == http.MethodHead,
	)
	if err != nil {
		log.Printf("fnOS source stream failed for %s/%s: %v", record.Source, record.ID, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"code": -1, "msg": "online source unavailable", "data": nil,
		})
		return
	}
	defer result.Body.Close()
	resolved = b.syncStreamTrackExtension(resolved, result)
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if value := result.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-MusicDL-Source", resolved.Source)
	w.WriteHeader(result.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, result.Body)
	}
}

func (b *Bridge) syncStreamTrackExtension(record Record, result *sourcecontract.StreamResult) Record {
	ext := audioExtensionFromContentType(result.Header.Get("Content-Type"))
	if ext == "" || ext == record.Ext {
		return record
	}
	track := record.track()
	track.Ext = ext
	updated, ok := b.mappings.RebindSource(record.GUID, track)
	if !ok {
		return record
	}
	if err := b.mappings.Save(); err != nil {
		log.Printf("fnOS stream metadata map save failed: %v", err)
	}
	return updated
}

func (b *Bridge) streamWithFallback(
	ctx context.Context,
	record Record,
	rangeHeader string,
	head bool,
) (*sourcecontract.StreamResult, Record, error) {
	result, primaryErr := b.source.Stream(ctx, record.track(), rangeHeader, head)
	if primaryErr == nil {
		return result, record, nil
	}
	if ctx.Err() != nil || strings.TrimSpace(record.Title) == "" {
		return nil, record, primaryErr
	}
	log.Printf("fnOS source stream unavailable for %s/%s, searching fallback: %v",
		record.Source, record.ID, primaryErr)

	// A failed player request can arrive concurrently (for example, a seek
	// retry). Serialize fallback resolution and reuse a binding completed by
	// another request.
	primarySource := record.Source
	primaryID := record.ID
	b.fallbackMu.Lock()
	defer b.fallbackMu.Unlock()
	if current, ok := b.mappings.Get(record.GUID); ok {
		record = current
	}
	if record.Source != primarySource || record.ID != primaryID {
		if result, err := b.source.Stream(ctx, record.track(), rangeHeader, head); err == nil {
			return result, record, nil
		}
	}

	for _, candidate := range b.streamFallbackCandidates(ctx, record) {
		if ctx.Err() != nil {
			break
		}
		result, err := b.source.Stream(ctx, candidate.track(), rangeHeader, head)
		if err != nil {
			log.Printf("fnOS fallback stream failed for %s/%s: %v", candidate.Source, candidate.ID, err)
			continue
		}
		if ext := audioExtensionFromContentType(result.Header.Get("Content-Type")); ext != "" {
			candidate.Ext = ext
		}
		rebound, ok := b.mappings.RebindSource(record.GUID, candidate.track())
		if !ok {
			_ = result.Body.Close()
			continue
		}
		if err := b.mappings.Save(); err != nil {
			log.Printf("fnOS GUID fallback map save failed: %v", err)
		}
		log.Printf("fnOS source stream switched %s/%s -> %s/%s (guid=%s)",
			primarySource, primaryID, candidate.Source, candidate.ID, record.GUID)
		return result, rebound, nil
	}
	return nil, record, fmt.Errorf("source unavailable after fallback: %w", primaryErr)
}

type fallbackSearchResult struct {
	source string
	tracks []sourcecontract.Track
}

type fallbackCandidate struct {
	track        sourcecontract.Track
	score        float64
	durationDiff int
}

const fallbackSearchBudget = 6 * time.Second

func (b *Bridge) streamFallbackCandidates(ctx context.Context, record Record) []Record {
	keyword := strings.TrimSpace(record.Title)
	if artist := strings.TrimSpace(record.Artist); artist != "" {
		keyword += " " + artist
	}
	sources := make([]string, 0, len(b.sources))
	seenSources := make(map[string]struct{}, len(b.sources))
	for _, source := range b.sources {
		source = strings.TrimSpace(source)
		if source == "" || source == record.Source {
			continue
		}
		if _, exists := seenSources[source]; exists {
			continue
		}
		seenSources[source] = struct{}{}
		sources = append(sources, source)
	}
	if keyword == "" || len(sources) == 0 {
		return nil
	}

	searchCtx, cancelSearch := context.WithTimeout(ctx, fallbackSearchBudget)
	defer cancelSearch()
	results := make(chan fallbackSearchResult, len(sources))
	for _, source := range sources {
		source := source
		go func() {
			tracks, err := b.source.Search(searchCtx, keyword, []string{source}, b.maxOnline)
			if err != nil {
				log.Printf("fnOS fallback search failed for %s: %v", source, err)
			}
			results <- fallbackSearchResult{source: source, tracks: tracks}
		}()
	}

	candidates := make([]fallbackCandidate, 0)
	seenTracks := make(map[string]struct{})
collectResults:
	for remaining := len(sources); remaining > 0; remaining-- {
		select {
		case <-searchCtx.Done():
			break collectResults
		case result := <-results:
			for _, track := range result.tracks {
				if strings.TrimSpace(track.Source) == "" {
					track.Source = result.source
				}
				if track.Source != result.source || strings.TrimSpace(track.ID) == "" {
					continue
				}
				key := track.Source + "\x00" + track.ID
				if _, exists := seenTracks[key]; exists {
					continue
				}
				seenTracks[key] = struct{}{}
				score := fallbackTrackSimilarity(record, track)
				if score < 0.9 || !fallbackDurationClose(record.Duration, track.Duration) {
					continue
				}
				durationDiff := 0
				if record.Duration > 0 && track.Duration > 0 {
					durationDiff = absInt(record.Duration - track.Duration)
				}
				candidates = append(candidates, fallbackCandidate{
					track: track, score: score, durationDiff: durationDiff,
				})
			}
		}
	}
	if ctx.Err() != nil {
		return nil
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].durationDiff < candidates[j].durationDiff
		}
		return candidates[i].score > candidates[j].score
	})
	const maxFallbackAttempts = 8
	if len(candidates) > maxFallbackAttempts {
		candidates = candidates[:maxFallbackAttempts]
	}
	records := make([]Record, 0, len(candidates))
	for _, candidate := range candidates {
		records = append(records, Record{
			GUID:     record.GUID,
			Source:   candidate.track.Source,
			ID:       candidate.track.ID,
			Title:    candidate.track.Name,
			Artist:   candidate.track.Artist,
			Album:    candidate.track.Album,
			Cover:    candidate.track.Cover,
			Ext:      candidate.track.Ext,
			Duration: candidate.track.Duration,
			Size:     candidate.track.Size,
			Bitrate:  candidate.track.Bitrate,
		})
	}
	return records
}

func fallbackTrackSimilarity(record Record, track sourcecontract.Track) float64 {
	nameScore := normalizedSimilarity(record.Title, track.Name)
	if nameScore == 0 {
		return 0
	}
	artistScore := normalizedSimilarity(record.Artist, track.Artist)
	if normalizedTrackText(record.Artist) == "" || normalizedTrackText(track.Artist) == "" {
		return nameScore
	}
	return nameScore*0.7 + artistScore*0.3
}

func fallbackDurationClose(original int, candidate int) bool {
	if original <= 0 || candidate <= 0 {
		return true
	}
	diff := absInt(original - candidate)
	if diff <= 10 {
		return true
	}
	allowed := int(float64(original) * 0.15)
	if allowed < 10 {
		allowed = 10
	}
	return diff <= allowed
}

func audioExtensionFromContentType(contentType string) string {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		return ""
	}
	switch mediaType {
	case "audio/mpeg", "audio/mp3", "audio/x-mp3":
		return "mp3"
	case "audio/flac", "audio/x-flac":
		return "flac"
	case "audio/ogg", "application/ogg":
		return "ogg"
	case "audio/mp4", "audio/x-m4a", "audio/aac", "audio/aacp":
		return "m4a"
	case "audio/x-ms-wma", "audio/wma", "video/x-ms-asf", "application/vnd.ms-asf":
		return "wma"
	default:
		return ""
	}
}

func normalizedSimilarity(left, right string) float64 {
	left = normalizedTrackText(left)
	right = normalizedTrackText(right)
	if left == "" || right == "" {
		return 0
	}
	if left == right {
		return 1
	}
	leftRunes := []rune(left)
	rightRunes := []rune(right)
	maxLen := len(leftRunes)
	if len(rightRunes) > maxLen {
		maxLen = len(rightRunes)
	}
	distance := runeEditDistance(leftRunes, rightRunes)
	if distance >= maxLen {
		return 0
	}
	return 1 - float64(distance)/float64(maxLen)
}

func normalizedTrackText(value string) string {
	var normalized strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Han, r) {
			normalized.WriteRune(r)
		}
	}
	return normalized.String()
}

func runeEditDistance(left, right []rune) int {
	if len(left) == 0 {
		return len(right)
	}
	if len(right) == 0 {
		return len(left)
	}
	previous := make([]int, len(right)+1)
	current := make([]int, len(right)+1)
	for i := range previous {
		previous[i] = i
	}
	for i, leftRune := range left {
		current[0] = i + 1
		for j, rightRune := range right {
			cost := 1
			if leftRune == rightRune {
				cost = 0
			}
			current[j+1] = min(
				current[j]+1,
				previous[j+1]+1,
				previous[j]+cost,
			)
		}
		previous, current = current, previous
	}
	return previous[len(right)]
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func (b *Bridge) handleDownloadPrepare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		b.handleUpstream(w, r)
		return
	}
	body := readSmallRequestBody(w, r)
	rawGUID := requestGUID(r)
	if rawGUID == "" {
		rawGUID = bodyGUID(body)
	}
	record, ok := b.recordForDownloadRequest(r, body)
	if !ok {
		log.Printf("fnOS download prepare passthrough: guid=%s matched=false", rawGUID)
		b.handleUpstream(w, r)
		return
	}
	log.Printf("fnOS NAS download prepare: guid=%s source=%s id=%s title=%q",
		record.GUID, record.Source, record.ID, record.Title)
	ctx, cancel := context.WithTimeout(r.Context(), b.searchTimeout)
	defer cancel()
	result, err := b.source.EnqueueDownload(ctx, record.track())
	if err != nil {
		log.Printf("fnOS NAS download enqueue failed for %s/%s: %v", record.Source, record.ID, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"code": -1,
			"msg":  "NAS download enqueue failed",
			"data": map[string]any{
				"status": "failed", "errno": "enqueue_failed",
				"errmsg": err.Error(), "downloadId": "",
			},
		})
		return
	}
	b.ensureBinding(record, result.ID, copyHeaders(r.Header), "")
	writeJSON(w, http.StatusOK, map[string]any{
		"code": 0,
		"msg":  "",
		"data": map[string]any{
			"status": "success", "errno": "", "errmsg": "",
			"downloadId": result.ID,
		},
	})
}

func (b *Bridge) handleDownloadTrack(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		b.handleUpstream(w, r)
		return
	}
	body := readSmallRequestBody(w, r)
	record, ok := b.recordForDownloadRequest(r, body)
	if !ok {
		b.handleUpstream(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), b.searchTimeout)
	defer cancel()
	result, err := b.source.EnqueueDownload(ctx, record.track())
	if err != nil {
		log.Printf("fnOS direct NAS download enqueue failed for %s/%s: %v", record.Source, record.ID, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"code": -1, "msg": "NAS download enqueue failed", "data": nil,
		})
		return
	}
	log.Printf("fnOS direct NAS download accepted: guid=%s source=%s id=%s state=%s downloadId=%s",
		record.GUID, record.Source, record.ID, result.State.Status, result.ID)
	b.ensureBinding(record, result.ID, copyHeaders(r.Header), "")
	if r.Method == http.MethodGet {
		b.serveDownloadWhenReady(w, r, result.ID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code": 0,
		"msg":  "",
		"data": map[string]any{
			"status":     "success",
			"downloadId": result.ID,
			"errno":      "",
			"errmsg":     "",
		},
	})
}

func (b *Bridge) serveDownloadWhenReady(w http.ResponseWriter, r *http.Request, downloadID string) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	log.Printf("fnOS waiting for NAS download before device response: downloadId=%s", downloadID)
	for {
		statusCtx, statusCancel := context.WithTimeout(ctx, 5*time.Second)
		state, ok, err := b.source.DownloadStatus(statusCtx, downloadID)
		statusCancel()
		if err != nil {
			log.Printf("fnOS device download status failed: downloadId=%s error=%v", downloadID, err)
		} else if !ok {
			log.Printf("fnOS device download disappeared: downloadId=%s", downloadID)
			writeJSON(w, http.StatusNotFound, map[string]any{
				"code": -1, "msg": "NAS download task not found", "data": nil,
			})
			return
		} else {
			switch state.Status {
			case "ready":
				b.serveNASDownloadFile(w, r, downloadID, state.Path)
				return
			case "failed":
				writeJSON(w, http.StatusBadGateway, map[string]any{
					"code": -1, "msg": "NAS download failed",
					"data": map[string]any{
						"status": "failed", "errmsg": state.Error, "downloadId": downloadID,
					},
				})
				return
			}
		}

		select {
		case <-ctx.Done():
			writeJSON(w, http.StatusGatewayTimeout, map[string]any{
				"code": -1, "msg": "NAS download timed out",
				"data": map[string]any{"downloadId": downloadID},
			})
			return
		case <-ticker.C:
		}
	}
}

func (b *Bridge) handleDownloadDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		b.handleUpstream(w, r)
		return
	}
	body := readSmallRequestBody(w, r)
	record, ok := b.recordForDownloadRequest(r, body)
	if !ok {
		b.handleUpstream(w, r)
		return
	}
	vo := b.trackVO(record)
	vo["downloaded"] = true
	vo["downloadStatus"] = "completed"
	vo["track"] = cloneMapWithout(vo, "track")
	writeJSON(w, http.StatusOK, map[string]any{
		"code": 0,
		"msg":  "",
		"data": vo,
	})
}

func (b *Bridge) handleDownloadStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		b.handleUpstream(w, r)
		return
	}
	body := readSmallRequestBody(w, r)
	downloadID := downloadIDFromRequest(r, body)
	if downloadID == "" {
		b.handleUpstream(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), b.searchTimeout)
	defer cancel()
	state, ok, err := b.source.DownloadStatus(ctx, downloadID)
	if err != nil {
		log.Printf("fnOS download status source API error: downloadId=%s error=%v", downloadID, err)
		b.handleUpstream(w, r)
		return
	}
	if !ok {
		log.Printf("fnOS download status passthrough: downloadId=%s known=false", downloadID)
		b.handleUpstream(w, r)
		return
	}
	log.Printf("fnOS NAS download status: downloadId=%s state=%s", downloadID, state.Status)
	writeJSON(w, http.StatusOK, map[string]any{
		"code": 0,
		"msg":  "",
		"data": map[string]any{
			"status": state.Status, "errno": "",
			"errmsg": state.Error, "downloadId": downloadID,
			"percent": state.Percent,
		},
	})
}

func (b *Bridge) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		b.handleUpstream(w, r)
		return
	}
	body := readSmallRequestBody(w, r)
	downloadID := downloadIDFromRequest(r, body)
	if downloadID == "" {
		b.handleUpstream(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), b.searchTimeout)
	defer cancel()
	state, ok, err := b.source.DownloadStatus(ctx, downloadID)
	if err != nil {
		log.Printf("fnOS download file source API error: downloadId=%s error=%v", downloadID, err)
		b.handleUpstream(w, r)
		return
	}
	if !ok {
		log.Printf("fnOS download file passthrough: downloadId=%s known=false", downloadID)
		b.handleUpstream(w, r)
		return
	}
	switch state.Status {
	case "ready":
		b.serveNASDownloadFile(w, r, downloadID, state.Path)
	case "failed":
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"code": -1, "msg": "NAS download failed",
			"data": map[string]any{"status": "failed", "errmsg": state.Error},
		})
	default:
		writeJSON(w, http.StatusConflict, map[string]any{
			"code": -1, "msg": "NAS download not ready",
			"data": map[string]any{"status": state.Status, "percent": state.Percent},
		})
	}
}

func (b *Bridge) serveNASDownloadFile(w http.ResponseWriter, r *http.Request, downloadID, filePath string) {
	filePath = strings.TrimSpace(filePath)
	if filePath == "" || !filepath.IsAbs(filePath) {
		log.Printf("fnOS download file unavailable: downloadId=%s path=%q", downloadID, filePath)
		writeJSON(w, http.StatusNotFound, map[string]any{
			"code": -1, "msg": "NAS download file not found", "data": nil,
		})
		return
	}
	file, err := os.Open(filePath)
	if err != nil {
		log.Printf("fnOS download file open failed: downloadId=%s path=%s error=%v", downloadID, filePath, err)
		writeJSON(w, http.StatusNotFound, map[string]any{
			"code": -1, "msg": "NAS download file not found", "data": nil,
		})
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		log.Printf("fnOS download file stat failed: downloadId=%s path=%s error=%v", downloadID, filePath, err)
		writeJSON(w, http.StatusNotFound, map[string]any{
			"code": -1, "msg": "NAS download file not found", "data": nil,
		})
		return
	}

	filename := filepath.Base(filePath)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("Cache-Control", "no-store")
	log.Printf("fnOS serving NAS download to device: downloadId=%s path=%s size=%d", downloadID, filePath, info.Size())
	http.ServeContent(w, r, filename, info.ModTime(), file)
}

func (b *Bridge) handleDownloadDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodDelete {
		b.handleUpstream(w, r)
		return
	}
	body := readSmallRequestBody(w, r)
	downloadID := downloadIDFromRequest(r, body)
	if downloadID == "" {
		b.handleUpstream(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), b.searchTimeout)
	defer cancel()
	_, ok, err := b.source.DownloadStatus(ctx, downloadID)
	if err != nil {
		log.Printf("fnOS download delete source API error: downloadId=%s error=%v", downloadID, err)
		b.handleUpstream(w, r)
		return
	}
	if !ok {
		log.Printf("fnOS download delete passthrough: downloadId=%s known=false", downloadID)
		b.handleUpstream(w, r)
		return
	}
	if err := b.source.ForgetDownload(ctx, downloadID); err != nil {
		log.Printf("fnOS download delete failed: downloadId=%s error=%v", downloadID, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code": 0, "msg": "",
		"data": map[string]any{"downloadId": downloadID, "deleted": true},
	})
}

func (b *Bridge) fetchUpstreamJSON(r *http.Request, timeout time.Duration) (*http.Response, []byte, error) {
	target := "http://unix" + r.URL.RequestURI()
	request, err := http.NewRequestWithContext(r.Context(), r.Method, target, nil)
	if err != nil {
		return nil, nil, err
	}
	request.Header = cloneHeader(r.Header)
	request.Header.Set("Accept-Encoding", "identity")
	request.Host = r.Host
	client := &http.Client{Transport: b.transport, Timeout: timeout}
	response, err := client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return response, nil, err
	}
	return response, body, nil
}

func (b *Bridge) recordForRequest(r *http.Request) (Record, bool) {
	guid := requestGUID(r)
	if guid == "" {
		return Record{}, false
	}
	return b.mappings.Get(guid)
}

func (b *Bridge) trackVO(record Record) map[string]any {
	title := strings.TrimSpace(record.Title)
	if title == "" {
		title = "Unknown"
	}
	artist := strings.TrimSpace(record.Artist)
	if artist == "" {
		artist = "Unknown"
	}
	album := strings.TrimSpace(record.Album)
	if album == "" {
		album = "Unknown"
	}
	ext := normalizeExt(record.Ext)
	durationMS := record.Duration * 1000
	artistGUID := FakeChildGUID(record.GUID, "artist:"+artist)
	albumGUID := FakeChildGUID(record.GUID, "album:"+album)
	artists := []any{map[string]any{"name": artist, "guid": artistGUID}}
	coverURL := "/music/api/v1/static/cover?guid=" + url.QueryEscape(record.GUID) + "&coverId=" + url.QueryEscape(record.GUID)
	bitrate := record.Bitrate * 1000
	if bitrate <= 0 {
		bitrate = 320000
	}
	bitDepth := any(nil)
	if ext == "flac" || ext == "wav" || ext == "aiff" {
		bitDepth = 16
	}
	audioSpec := map[string]any{
		"path":       fmt.Sprintf("online/go-music-dl/%s/%s.%s", record.Source, record.GUID, ext),
		"format":     ext,
		"codec":      ext,
		"container":  ext,
		"duration":   durationMS,
		"size":       record.Size,
		"channel":    2,
		"sampleRate": 44100,
		"bitrate":    bitrate,
	}
	if bitDepth != nil {
		audioSpec["bitDepth"] = bitDepth
	}
	albumObj := map[string]any{
		"name":    album,
		"guid":    albumGUID,
		"artists": artists,
		"coverId": record.GUID,
	}
	return map[string]any{
		"guid":         record.GUID,
		"id":           record.GUID,
		"title":        sourceTag + title,
		"name":         sourceTag + title,
		"artist":       artist,
		"artists":      artists,
		"album":        albumObj,
		"albumName":    album,
		"audioSpec":    audioSpec,
		"duration":     durationMS,
		"duration_ms":  durationMS,
		"durationMs":   durationMS,
		"codec":        ext,
		"codecName":    ext,
		"format":       ext,
		"ext":          ext,
		"size":         record.Size,
		"coverId":      record.GUID,
		"coverUrl":     coverURL,
		"coverURL":     coverURL,
		"source":       "go-music-dl:" + record.Source,
		"is_online":    true,
		"isFavorite":   false,
		"isCue":        false,
		"hasLyric":     true,
		"genres":       []any{},
		"accessStatus": 0,
	}
}

func requestKeyword(r *http.Request) string {
	for _, key := range []string{"q", "keyword", "query"} {
		if value := strings.TrimSpace(r.URL.Query().Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func requestGUID(r *http.Request) string {
	query := r.URL.Query()
	for _, key := range []string{"guid", "trackGUID", "trackGuid", "trackId", "id", "coverId"} {
		if value := query.Get(key); value != "" {
			return normalizeGUID(value)
		}
	}
	path := strings.TrimRight(r.URL.Path, "/")
	if slash := strings.LastIndex(path, "/"); slash >= 0 {
		return normalizeGUID(path[slash+1:])
	}
	return ""
}

func readSmallRequestBody(w http.ResponseWriter, r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read request body", http.StatusBadRequest)
		return nil
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return body
}

func downloadIDFromRequest(r *http.Request, body []byte) string {
	query := r.URL.Query()
	for _, key := range []string{"downloadId", "downloadID", "taskGuid", "taskId", "id"} {
		if value := strings.TrimSpace(query.Get(key)); value != "" {
			return value
		}
	}
	if len(body) == 0 {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	for _, key := range []string{"downloadId", "downloadID", "taskGuid", "taskId", "id"} {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func bodyGUID(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	for _, key := range []string{"guid", "trackGUID", "trackGuid", "trackId", "id"} {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return normalizeGUID(value)
		}
	}
	return ""
}

func (b *Bridge) recordForDownloadRequest(r *http.Request, body []byte) (Record, bool) {
	if record, ok := b.recordForRequest(r); ok {
		return record, true
	}
	if guid := bodyGUID(body); guid != "" {
		if record, found := b.mappings.Get(guid); found {
			return record, true
		}
	}
	if len(body) == 0 {
		return Record{}, false
	}
	return Record{}, false
}

func queryInt(r *http.Request, key string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get(key)))
	if err != nil {
		return fallback
	}
	return value
}

func sortedQueryKeys(values url.Values) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func trackKey(item any) string {
	object, ok := item.(map[string]any)
	if !ok {
		return ""
	}
	title, _ := object["title"].(string)
	if title == "" {
		title, _ = object["name"].(string)
	}
	artist, _ := object["artist"].(string)
	if artist == "" {
		if artists, ok := object["artists"].([]any); ok {
			names := make([]string, 0, len(artists))
			for _, value := range artists {
				if person, ok := value.(map[string]any); ok {
					if name, _ := person["name"].(string); strings.TrimSpace(name) != "" {
						names = append(names, name)
					}
				}
			}
			artist = strings.Join(names, "/")
		}
	}
	return normalizedText(title) + "\x00" + normalizedText(artist)
}

func onlineKeyFromTrack(track sourcecontract.Track) string {
	return normalizedText(track.Name) + "\x00" + normalizedText(track.Artist)
}

func normalizedText(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func integerValue(value any, fallback int) int {
	number, ok := numberValue(value)
	if !ok {
		return fallback
	}
	return int(number)
}

func numberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	default:
		return 0, false
	}
}

func cloneMapWithout(source map[string]any, key string) map[string]any {
	copy := make(map[string]any, len(source))
	for name, value := range source {
		if name != key {
			copy[name] = value
		}
	}
	return copy
}

func cloneHeader(source http.Header) http.Header {
	header := make(http.Header, len(source))
	for key, values := range source {
		if strings.EqualFold(key, "Connection") || strings.EqualFold(key, "Proxy-Connection") {
			continue
		}
		header[key] = append([]string(nil), values...)
	}
	return header
}

func writeHTTPResponse(w http.ResponseWriter, response *http.Response, body []byte) {
	for key, values := range response.Header {
		if strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Transfer-Encoding") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(response.StatusCode)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	var buffer bytes.Buffer
	if err := json.NewEncoder(&buffer).Encode(payload); err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(buffer.Len()))
	w.WriteHeader(status)
	_, _ = w.Write(buffer.Bytes())
}
