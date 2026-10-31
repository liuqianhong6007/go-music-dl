package fnosbridge

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

const completedDownloadLinger = 15 * time.Second

type activeDownload struct {
	record     Record
	createdAt  int64
	finishedAt int64
}

func (b *Bridge) rememberActiveDownload(record Record, downloadID string) {
	downloadID = strings.TrimSpace(downloadID)
	if downloadID == "" {
		return
	}
	b.taskMu.Lock()
	defer b.taskMu.Unlock()
	if _, exists := b.activeTasks[downloadID]; exists {
		return
	}
	b.activeTasks[downloadID] = activeDownload{
		record:    record,
		createdAt: time.Now().UnixMilli(),
	}
}

func (b *Bridge) forgetActiveDownload(downloadID string) {
	downloadID = strings.TrimSpace(downloadID)
	if downloadID == "" {
		return
	}
	b.taskMu.Lock()
	delete(b.activeTasks, downloadID)
	b.taskMu.Unlock()
}

func (b *Bridge) activeDownloadSnapshot() map[string]activeDownload {
	b.taskMu.Lock()
	defer b.taskMu.Unlock()
	tasks := make(map[string]activeDownload, len(b.activeTasks))
	for id, task := range b.activeTasks {
		tasks[id] = task
	}
	return tasks
}

func (b *Bridge) markActiveDownloadFinished(downloadID string) activeDownload {
	b.taskMu.Lock()
	defer b.taskMu.Unlock()
	task := b.activeTasks[downloadID]
	if task.finishedAt == 0 {
		task.finishedAt = time.Now().UnixMilli()
		b.activeTasks[downloadID] = task
	}
	return task
}

func (b *Bridge) handleTaskList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		b.handleUpstream(w, r)
		return
	}

	response, body, err := b.fetchUpstreamJSON(r, b.searchTimeout+3*time.Second)
	if err != nil {
		log.Printf("fnOS task list upstream failed: %v", err)
		writeJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"msg":  "",
			"data": b.sourceTaskViews(r.Context()),
		})
		return
	}
	if response.StatusCode != http.StatusOK {
		writeHTTPResponse(w, response, body)
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeHTTPResponse(w, response, body)
		return
	}
	if code, ok := numberValue(payload["code"]); !ok || int(code) != 0 {
		writeHTTPResponse(w, response, body)
		return
	}

	official, container := officialTaskList(payload["data"])
	merged := append(official, b.sourceTaskViews(r.Context())...)
	switch container.(type) {
	case []any:
		payload["data"] = merged
	case map[string]any:
		typed := container.(map[string]any)
		typed["list"] = merged
		if _, exists := typed["total"]; exists {
			typed["total"] = len(merged)
		}
		payload["data"] = typed
	default:
		payload["data"] = merged
	}
	payload["code"] = 0
	writeJSON(w, http.StatusOK, payload)
}

func officialTaskList(data any) ([]any, any) {
	switch typed := data.(type) {
	case []any:
		return append([]any(nil), typed...), typed
	case map[string]any:
		list, _ := typed["list"].([]any)
		return append([]any(nil), list...), typed
	default:
		return nil, nil
	}
}

func (b *Bridge) sourceTaskViews(ctx context.Context) []any {
	active := b.activeDownloadSnapshot()
	ids := make([]string, 0, len(active))
	for id := range active {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left, right := active[ids[i]], active[ids[j]]
		if left.createdAt == right.createdAt {
			return ids[i] < ids[j]
		}
		return left.createdAt > right.createdAt
	})

	views := make([]any, 0, len(ids))
	for _, id := range ids {
		task := active[id]
		statusCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		state, ok, err := b.source.DownloadStatus(statusCtx, id)
		cancel()
		if err != nil {
			log.Printf("fnOS task list status failed: downloadId=%s error=%v", id, err)
			views = append(views, sourceTaskView(id, task, sourceTaskState{status: "waiting"}))
			continue
		}
		if !ok {
			b.forgetActiveDownload(id)
			continue
		}
		if state.Status == "ready" {
			finished := b.markActiveDownloadFinished(id)
			if time.Since(time.UnixMilli(finished.finishedAt)) >= completedDownloadLinger {
				b.forgetActiveDownload(id)
				continue
			}
			views = append(views, sourceTaskView(id, finished, sourceTaskState{status: "ready"}))
			continue
		}
		views = append(views, sourceTaskView(id, task, sourceTaskState{
			status: state.Status,
			err:    state.Error,
		}))
	}
	return views
}

type sourceTaskState struct {
	status string
	err    string
}

func sourceTaskView(id string, task activeDownload, state sourceTaskState) map[string]any {
	title := strings.TrimSpace(task.record.Title)
	if title == "" {
		title = "Unknown"
	}
	name := sourceTag + title
	if artist := strings.TrimSpace(task.record.Artist); artist != "" {
		name += " - " + artist
	}
	done := state.status == "ready" || state.status == "failed"
	successCount := 0
	failCount := 0
	doneAt := int64(0)
	errs := []any{}
	switch state.status {
	case "ready":
		successCount = 1
		doneAt = time.Now().UnixMilli()
	case "failed":
		failCount = 1
		doneAt = time.Now().UnixMilli()
		message := strings.TrimSpace(state.err)
		if message == "" {
			message = "download failed"
		}
		errs = append(errs, map[string]any{
			"code": 1, "msg": message, "name": title,
			"ext1": "", "ext2": "", "ext3": "",
		})
	}
	return map[string]any{
		"id":            id,
		"type":          "download",
		"name":          name,
		"total":         1,
		"successCount":  successCount,
		"failCount":     failCount,
		"done":          done,
		"cancelling":    false,
		"canceled":      false,
		"canceledCount": 0,
		"createdAt":     task.createdAt,
		"doneAt":        doneAt,
		"errs":          errs,
		"retryable":     false,
	}
}
