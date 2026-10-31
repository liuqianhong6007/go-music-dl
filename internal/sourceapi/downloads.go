package sourceapi

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"

	"github.com/guohuiyuan/go-music-dl/core"
	"github.com/guohuiyuan/go-music-dl/internal/fnos-bridge/sourcecontract"
	"github.com/guohuiyuan/music-lib/model"
)

type downloadEntry struct {
	RequestKey string
	State      sourcecontract.DownloadState
}

type downloadManager struct {
	queue *core.DownloadQueue
	mu    sync.Mutex
	tasks map[string]downloadEntry
}

func newDownloadManager(queue *core.DownloadQueue) (*downloadManager, error) {
	if queue == nil {
		return nil, fmt.Errorf("download queue is required")
	}
	return &downloadManager{queue: queue, tasks: make(map[string]downloadEntry)}, nil
}

func processDownload(input core.DownloadTaskInput) (core.DownloadTaskResult, error) {
	log.Printf("source API NAS download started: source=%s id=%s dir=%s",
		input.Song.Source, input.Song.ID, input.Directory)
	index, err := core.LoadDownloadDedupSet()
	if err != nil {
		return core.DownloadTaskResult{}, err
	}
	result, err := core.DownloadWithDedupCheckWithTemplate(
		&input.Song, input.Directory, input.Embed, input.Embed,
		input.FilenameTemplate, index,
	)
	if err != nil {
		log.Printf("source API NAS download failed: source=%s id=%s error=%v",
			input.Song.Source, input.Song.ID, err)
		return core.DownloadTaskResult{}, err
	}
	status := core.DownloadStatusSuccess
	if result.Skipped {
		status = core.DownloadStatusSkipped
	}
	log.Printf("source API NAS download finished: source=%s id=%s status=%s path=%s",
		input.Song.Source, input.Song.ID, status, result.SavedPath)
	return core.DownloadTaskResult{Status: status, Path: result.SavedPath, Warning: result.Warning}, nil
}

func (m *downloadManager) Enqueue(track sourcecontract.Track) (sourcecontract.DownloadResult, error) {
	if m == nil {
		return sourcecontract.DownloadResult{}, fmt.Errorf("download manager unavailable")
	}
	settings := core.GetWebSettings()
	directory := core.DownloadDirOrDefault(settings.DownloadDir)
	song := musicSong(track)
	index, err := core.LoadDownloadDedupSet()
	if err != nil {
		return sourcecontract.DownloadResult{}, err
	}
	if result, ok := existingDownloadResult("", directory, &song, index); ok {
		id, err := randomDownloadID()
		if err != nil {
			return sourcecontract.DownloadResult{}, err
		}
		result.ID = id
		m.set(id, downloadEntry{State: result.State})
		return result, nil
	}

	requestID, err := randomDownloadID()
	if err != nil {
		return sourcecontract.DownloadResult{}, err
	}
	input := core.DownloadTaskInput{
		Song: song, Directory: directory,
		FilenameTemplate: settings.DownloadFilenameTemplate,
		Embed:            settings.EmbedDownload,
	}
	added, err := m.queue.Enqueue(requestID, []core.DownloadTaskInput{input})
	if err != nil {
		return sourcecontract.DownloadResult{}, err
	}
	if added == 0 {
		task, activeErr := m.queue.ActiveTaskBySong(track.Source, track.ID, directory)
		if activeErr != nil {
			index, loadErr := core.LoadDownloadDedupSet()
			if loadErr == nil {
				if result, ok := existingDownloadResult(requestID, directory, &song, index); ok {
					m.set(requestID, downloadEntry{State: result.State})
					return result, nil
				}
			}
			return sourcecontract.DownloadResult{}, activeErr
		}
		requestID = strings.TrimSuffix(task.RequestKey, ":0")
	}
	result := sourcecontract.DownloadResult{
		ID: requestID, State: sourcecontract.DownloadState{Status: "waiting", Percent: 5},
	}
	m.set(requestID, downloadEntry{RequestKey: requestID + ":0", State: result.State})
	log.Printf("source API NAS download queued: id=%s source=%s track=%s dir=%s added=%d",
		requestID, track.Source, track.ID, directory, added)
	return result, nil
}

func existingDownloadResult(id, directory string, song *model.Song, index core.DownloadDedupIndex) (sourcecontract.DownloadResult, bool) {
	if !core.IsSongStillDownloaded(song, index, directory) {
		return sourcecontract.DownloadResult{}, false
	}
	relPath := core.SongFileRelPath(song, index)
	path := ""
	if relPath != "" {
		path = filepath.Join(core.DownloadDirOrDefault(directory), filepath.FromSlash(relPath))
	}
	return sourcecontract.DownloadResult{
		ID: id,
		State: sourcecontract.DownloadState{
			Status:  "ready",
			Percent: 100,
			Path:    path,
		},
	}, true
}

func (m *downloadManager) Status(id string) (sourcecontract.DownloadState, bool) {
	if m == nil {
		return sourcecontract.DownloadState{}, false
	}
	id = strings.TrimSpace(id)
	m.mu.Lock()
	entry, ok := m.tasks[id]
	m.mu.Unlock()
	if !ok {
		return sourcecontract.DownloadState{}, false
	}
	if entry.RequestKey == "" {
		return entry.State, true
	}
	task, err := m.queue.TaskByRequestKey(entry.RequestKey)
	if err != nil {
		state := sourcecontract.DownloadState{
			Status: "failed", Percent: entry.State.Percent, Error: "download task disappeared",
		}
		m.set(id, downloadEntry{State: state})
		return state, true
	}
	state := downloadState(task)
	m.set(id, downloadEntry{RequestKey: entry.RequestKey, State: state})
	return state, true
}

func (m *downloadManager) Forget(id string) bool {
	id = strings.TrimSpace(id)
	m.mu.Lock()
	_, ok := m.tasks[id]
	if ok {
		delete(m.tasks, id)
	}
	m.mu.Unlock()
	return ok
}

func (m *downloadManager) set(id string, entry downloadEntry) {
	m.mu.Lock()
	m.tasks[id] = entry
	m.mu.Unlock()
}

func downloadState(task core.DownloadTask) sourcecontract.DownloadState {
	state := sourcecontract.DownloadState{}
	switch task.Status {
	case core.TaskWaiting:
		state.Status, state.Percent = "waiting", 5
	case core.TaskDownloading:
		state.Status, state.Percent = "downloading", 50
	case core.DownloadStatusSuccess, core.DownloadStatusSkipped:
		state.Status, state.Percent, state.Path = "ready", 100, task.Path
	case core.TaskCanceled:
		state.Status, state.Percent, state.Error = "failed", 100, "download canceled"
	default:
		state.Status, state.Percent, state.Error = "failed", 100, strings.TrimSpace(task.Error)
		if state.Error == "" {
			state.Error = "download failed"
		}
	}
	return state
}

func randomDownloadID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
