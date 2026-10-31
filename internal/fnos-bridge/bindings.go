package fnosbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	favoriteRetryThrottle = time.Minute
	favoriteRetryTimeout  = 2 * time.Minute
	favoriteRetryBatch    = 5
	gatewayUserIDHeader   = "X-Trim-Userid"
)

type bindingJob struct {
	record          Record
	downloadID      string
	headers         http.Header
	favoriteHeaders map[string]http.Header
}

func (b *Bridge) ensureBinding(record Record, downloadID string, headers http.Header, favoriteUser string) {
	b.bindingMu.Lock()
	job := b.bindingJobs[record.GUID]
	if job == nil {
		job = &bindingJob{
			record:          record,
			favoriteHeaders: make(map[string]http.Header),
		}
		b.bindingJobs[record.GUID] = job
		go b.runBinding(job)
	}
	if downloadID != "" {
		job.downloadID = downloadID
	}
	if len(headers) > 0 {
		job.headers = headers
	}
	favoriteUser = normalizeUserGUID(favoriteUser)
	if favoriteUser != "" {
		job.favoriteHeaders[favoriteUser] = cloneHeader(headers)
	}
	b.bindingMu.Unlock()
	if downloadID != "" {
		b.rememberActiveDownload(record, downloadID)
	}
}

func (b *Bridge) runBinding(job *bindingJob) {
	defer func() {
		b.bindingMu.Lock()
		delete(b.bindingJobs, job.record.GUID)
		b.bindingMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	if !b.waitForNASDownload(ctx, job) {
		b.clearFavoriteAfterDownloadFailure(job, "NAS download failed")
		return
	}

	if current, ok := b.mappings.Get(job.record.GUID); ok {
		job.record = current
	}
	officialGUID := strings.TrimSpace(job.record.OfficialGUID)
	if officialGUID == "" {
		deadline := time.Now().Add(3 * time.Minute)
		for time.Now().Before(deadline) {
			found, err := b.findOfficialGUID(ctx, job.record, job.headers)
			if err != nil {
				log.Printf("official binding search failed for %s/%s: %v", job.record.Source, job.record.ID, err)
			} else if found != "" {
				officialGUID = found
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
		if officialGUID == "" {
			log.Printf("official binding timed out for %s/%s", job.record.Source, job.record.ID)
			return
		}
		record, ok := b.mappings.BindOfficial(job.record.GUID, officialGUID)
		if !ok {
			return
		}
		job.record = record
		_ = b.mappings.Save()
		log.Printf("bound online track %s to official %s", job.record.GUID, officialGUID)
	}

	b.bindingMu.Lock()
	favoriteHeaders := make(map[string]http.Header, len(job.favoriteHeaders))
	for user, headers := range job.favoriteHeaders {
		favoriteHeaders[user] = cloneHeader(headers)
	}
	b.bindingMu.Unlock()
	for user, headers := range favoriteHeaders {
		if err := b.writeOfficialFavorite(ctx, "/music/api/v1/favorite-track/create", headers, officialGUID); err != nil {
			log.Printf("bind official favorite failed for %s user=%s: %v", job.record.GUID, user, err)
			if record, ok := b.mappings.SetFavoriteState(job.record.GUID, user, "failed", err.Error()); ok {
				job.record = record
				_ = b.mappings.Save()
			}
			continue
		}
		if record, ok := b.mappings.SetFavoriteState(job.record.GUID, user, "", ""); ok {
			job.record = record
			_ = b.mappings.Save()
		}
		log.Printf("official favorite completed for %s user=%s -> %s", job.record.GUID, user, officialGUID)
	}
}

func (b *Bridge) clearFavoriteAfterDownloadFailure(job *bindingJob, reason string) {
	b.bindingMu.Lock()
	users := make([]string, 0, len(job.favoriteHeaders))
	for user := range job.favoriteHeaders {
		users = append(users, user)
	}
	b.bindingMu.Unlock()
	for _, user := range users {
		if _, ok := b.mappings.SetFavoriteState(job.record.GUID, user, "", ""); ok {
			_ = b.mappings.Save()
			log.Printf("removed favorite after %s for %s user=%s", reason, job.record.GUID, user)
		}
	}
}

func (b *Bridge) waitForNASDownload(ctx context.Context, job *bindingJob) bool {
	deadline := time.Now().Add(8 * time.Minute)
	for time.Now().Before(deadline) {
		b.bindingMu.Lock()
		downloadID := job.downloadID
		b.bindingMu.Unlock()
		if downloadID == "" {
			return false
		}
		state, ok, err := b.source.DownloadStatus(ctx, downloadID)
		if err == nil && ok {
			switch state.Status {
			case "ready":
				return true
			case "failed":
				log.Printf("NAS download failed before official binding for %s/%s: %s",
					job.record.Source, job.record.ID, state.Error)
				return false
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
	return false
}

func (b *Bridge) findOfficialGUID(ctx context.Context, record Record, headers http.Header) (string, error) {
	values := url.Values{}
	values.Set("q", strings.TrimSpace(record.Title+" "+record.Artist))
	values.Set("page", "1")
	values.Set("size", "50")
	status, body, err := b.officialJSON(ctx, http.MethodGet, "/music/api/v1/search/track?"+values.Encode(), headers, nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("official search returned %d", status)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	if code, ok := numberValue(payload["code"]); !ok || int(code) != 0 {
		return "", fmt.Errorf("official search rejected")
	}
	data, _ := payload["data"].(map[string]any)
	list, _ := data["list"].([]any)
	want := normalizedText(record.Title) + "\x00" + normalizedText(record.Artist)
	for _, item := range list {
		object, _ := item.(map[string]any)
		guid, _ := object["guid"].(string)
		if guid != "" && trackKey(item) == want {
			return normalizeGUID(guid), nil
		}
	}
	return "", nil
}

func (b *Bridge) officialJSON(ctx context.Context, method, path string, headers http.Header, body []byte) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	request.Header = cloneHeader(headers)
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Del("Content-Length")
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := (&http.Client{Transport: b.transport, Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	return response.StatusCode, data, err
}

func (b *Bridge) officialUser(ctx context.Context, headers http.Header) (string, bool) {
	status, body, err := b.officialJSON(ctx, http.MethodGet, "/music/api/v1/user/me", headers, nil)
	if err != nil || status != http.StatusOK {
		return "", false
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return "", false
	}
	code, _ := numberValue(payload["code"])
	data, _ := payload["data"].(map[string]any)
	guid, _ := data["guid"].(string)
	guid = normalizeUserGUID(guid)
	return guid, int(code) == 0 && guid != ""
}

func favoriteUserFromHeaders(headers http.Header) (string, bool) {
	uid := normalizeUserGUID(headers.Get(gatewayUserIDHeader))
	if uid == "" {
		return "", false
	}
	return "gateway:" + uid, true
}

func (b *Bridge) favoriteUser(ctx context.Context, headers http.Header) (string, bool) {
	if userGUID, ok := favoriteUserFromHeaders(headers); ok {
		return userGUID, true
	}
	return b.officialUser(ctx, headers)
}

func (b *Bridge) writeOfficialFavorite(ctx context.Context, path string, headers http.Header, officialGUID string) error {
	body, _ := json.Marshal(map[string]string{"trackGUID": officialGUID})
	status, responseBody, err := b.officialJSON(ctx, http.MethodPost, path, headers, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("official favorite returned %d", status)
	}
	var payload map[string]any
	if json.Unmarshal(responseBody, &payload) != nil {
		return fmt.Errorf("invalid official favorite response")
	}
	code, _ := numberValue(payload["code"])
	if int(code) != 0 {
		return fmt.Errorf("official favorite rejected: %s", strings.TrimSpace(fmt.Sprint(payload["msg"])))
	}
	return nil
}

func (b *Bridge) handleFavoriteCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		b.handleUpstream(w, r)
		return
	}
	body := readSmallRequestBody(w, r)
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	rawGUID, _ := payload["trackGUID"].(string)
	record, ok := b.mappings.Get(normalizeGUID(rawGUID))
	if !ok {
		b.handleUpstream(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	headers := copyHeaders(r.Header)
	userGUID, authenticated := b.favoriteUser(ctx, headers)
	if !authenticated {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"code": 99999, "msg": "INVALID TOKEN", "data": nil})
		return
	}
	record, _ = b.mappings.SetFavoriteState(record.GUID, userGUID, "pending", "")
	_ = b.mappings.Save()
	if record.OfficialGUID != "" {
		if err := b.writeOfficialFavorite(ctx, "/music/api/v1/favorite-track/create", headers, record.OfficialGUID); err == nil {
			record, _ = b.mappings.SetFavoriteState(record.GUID, userGUID, "", "")
			_ = b.mappings.Save()
		} else {
			record, _ = b.mappings.SetFavoriteState(record.GUID, userGUID, "failed", err.Error())
			_ = b.mappings.Save()
		}
	} else {
		result, err := b.source.EnqueueDownload(ctx, record.track())
		if err == nil {
			b.ensureBinding(record, result.ID, headers, userGUID)
		} else {
			record, _ = b.mappings.SetFavoriteState(record.GUID, userGUID, "", "")
			_ = b.mappings.Save()
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"code": -1, "msg": "NAS download enqueue failed", "data": nil,
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "msg": "", "data": nil})
}

func (b *Bridge) handleFavoriteDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		b.handleUpstream(w, r)
		return
	}
	body := readSmallRequestBody(w, r)
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	rawGUID, _ := payload["trackGUID"].(string)
	record, ok := b.mappings.Get(normalizeGUID(rawGUID))
	if !ok {
		b.handleUpstream(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	headers := copyHeaders(r.Header)
	userGUID, authenticated := b.favoriteUser(ctx, headers)
	if !authenticated {
		b.handleUpstream(w, r)
		return
	}
	record, _ = b.mappings.SetFavoriteState(record.GUID, userGUID, "", "")
	_ = b.mappings.Save()
	if record.OfficialGUID != "" {
		_ = b.writeOfficialFavorite(ctx, "/music/api/v1/favorite-track/delete", headers, record.OfficialGUID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "msg": "", "data": nil})
}

func (b *Bridge) handleFavoriteList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		b.handleUpstream(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	headers := copyHeaders(r.Header)
	status, body, err := b.officialJSON(ctx, http.MethodGet, r.URL.RequestURI(), headers, nil)
	if err != nil || status != http.StatusOK {
		b.handleUpstream(w, r)
		return
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		b.handleUpstream(w, r)
		return
	}
	if code, ok := numberValue(payload["code"]); !ok || int(code) != 0 {
		writeJSON(w, http.StatusOK, payload)
		return
	}
	userGUID, authenticated := b.favoriteUser(ctx, headers)
	if !authenticated {
		// The list request itself is authenticated and already succeeded. A
		// failed secondary identity probe must not blank an otherwise valid
		// official list.
		writeJSON(w, http.StatusOK, payload)
		return
	}
	if b.mappings.ClaimLegacyFavoriteUser(userGUID) {
		_ = b.mappings.Save()
	}
	data, _ := payload["data"].(map[string]any)
	list, _ := data["list"].([]any)
	originalLen := len(list)
	existing := make(map[string]struct{}, len(list))
	changed := false
	for _, item := range list {
		if object, ok := item.(map[string]any); ok {
			if guid, _ := object["guid"].(string); guid != "" {
				existing[normalizeGUID(guid)] = struct{}{}
			}
			object["isFavorite"] = true
		}
	}
	pending := b.mappings.PendingFavorites(userGUID)
	for _, record := range pending {
		if record.FavoritePending {
			if claimed, ok := b.mappings.ClaimLegacyFavorite(record.GUID, userGUID); ok {
				record = claimed
				changed = true
			}
		}
		if record.OfficialGUID != "" {
			if _, ok := existing[record.OfficialGUID]; ok {
				_, _ = b.mappings.SetFavoriteState(record.GUID, userGUID, "", "")
				changed = true
				continue
			}
		}
		if _, ok := existing[record.GUID]; ok {
			continue
		}
		vo := b.trackVO(record)
		vo["isFavorite"] = true
		list = append(list, vo)
	}
	if data == nil {
		data = map[string]any{}
	}
	data["list"] = list
	if total, ok := numberValue(data["total"]); ok {
		data["total"] = int(total) + len(list) - originalLen
	}
	payload["data"] = data
	if changed {
		_ = b.mappings.Save()
	}
	b.scheduleFavoriteRetries(pending, userGUID, headers)
	writeJSON(w, http.StatusOK, payload)
}

func (b *Bridge) scheduleFavoriteRetries(records []Record, userGUID string, headers http.Header) {
	userGUID = normalizeUserGUID(userGUID)
	if userGUID == "" || len(records) == 0 {
		return
	}
	now := time.Now().Unix()
	selected := make([]Record, 0, favoriteRetryBatch)
	b.favoriteMu.Lock()
	if len(b.favoriteRetry) > 2000 {
		for key, at := range b.favoriteRetry {
			if now-at > int64((24*time.Hour)/time.Second) {
				delete(b.favoriteRetry, key)
			}
		}
	}
	for _, record := range records {
		if len(selected) >= favoriteRetryBatch {
			break
		}
		key := userGUID + "\x00" + record.GUID
		if previous := b.favoriteRetry[key]; now-previous < int64(favoriteRetryThrottle/time.Second) {
			continue
		}
		b.favoriteRetry[key] = now
		selected = append(selected, record)
	}
	b.favoriteMu.Unlock()
	for _, record := range selected {
		record := record
		go b.retryFavoriteBinding(record, userGUID, cloneHeader(headers))
	}
}

func (b *Bridge) retryFavoriteBinding(record Record, userGUID string, headers http.Header) {
	ctx, cancel := context.WithTimeout(context.Background(), favoriteRetryTimeout)
	defer cancel()

	current, ok := b.mappings.Get(record.GUID)
	if !ok || !pendingFavoriteForUser(current, userGUID) {
		return
	}
	officialGUID := strings.TrimSpace(current.OfficialGUID)
	if officialGUID == "" {
		deadline := time.Now().Add(favoriteRetryTimeout)
		for officialGUID == "" && time.Now().Before(deadline) {
			found, err := b.findOfficialGUID(ctx, current, headers)
			if err != nil {
				log.Printf("favorite retry search failed for %s user=%s: %v", current.GUID, userGUID, err)
			} else if found != "" {
				officialGUID = found
				if bound, ok := b.mappings.BindOfficial(current.GUID, officialGUID); ok {
					current = bound
					_ = b.mappings.Save()
				}
				break
			}
			select {
			case <-ctx.Done():
				break
			case <-time.After(3 * time.Second):
			}
		}
	}
	if officialGUID == "" {
		if _, ok := b.mappings.SetFavoriteState(current.GUID, userGUID, "failed", "official track not found yet"); ok {
			_ = b.mappings.Save()
		}
		return
	}
	if err := b.writeOfficialFavorite(ctx, "/music/api/v1/favorite-track/create", headers, officialGUID); err != nil {
		log.Printf("favorite retry write failed for %s user=%s: %v", current.GUID, userGUID, err)
		if _, ok := b.mappings.SetFavoriteState(current.GUID, userGUID, "failed", err.Error()); ok {
			_ = b.mappings.Save()
		}
		return
	}
	if _, ok := b.mappings.SetFavoriteState(current.GUID, userGUID, "", ""); ok {
		_ = b.mappings.Save()
	}
	log.Printf("official favorite completed for %s user=%s -> %s", current.GUID, userGUID, officialGUID)
}

func copyHeaders(source http.Header) http.Header {
	return cloneHeader(source)
}
