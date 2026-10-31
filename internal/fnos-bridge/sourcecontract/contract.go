package sourcecontract

import (
	"context"
	"io"
	"net/http"
)

type Track struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Name     string `json:"name"`
	Artist   string `json:"artist"`
	Album    string `json:"album"`
	Duration int    `json:"duration"`
	Size     int64  `json:"size"`
	Bitrate  int    `json:"bitrate"`
	Ext      string `json:"ext"`
	Cover    string `json:"cover"`
}

type DownloadState struct {
	Status  string `json:"status"`
	Percent int    `json:"percent"`
	Error   string `json:"error,omitempty"`
	Path    string `json:"path,omitempty"`
}

type DownloadResult struct {
	ID    string        `json:"id"`
	State DownloadState `json:"state"`
}

type StreamResult struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
}

type Source interface {
	Health(ctx context.Context) error
	Search(ctx context.Context, keyword string, sources []string, limit int) ([]Track, error)
	Stream(ctx context.Context, track Track, rangeHeader string, head bool) (*StreamResult, error)
	EnqueueDownload(ctx context.Context, track Track) (DownloadResult, error)
	DownloadStatus(ctx context.Context, id string) (DownloadState, bool, error)
	ForgetDownload(ctx context.Context, id string) error
	Lyric(ctx context.Context, track Track) (string, error)
	Cover(ctx context.Context, track Track) ([]byte, string, error)
}
