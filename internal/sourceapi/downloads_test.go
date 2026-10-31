package sourceapi

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/guohuiyuan/go-music-dl/core"
	"github.com/guohuiyuan/music-lib/model"
)

func TestExistingDownloadResultIncludesNASPath(t *testing.T) {
	directory := t.TempDir()
	filename := "Artist - Song.mp3"
	path := filepath.Join(directory, filename)
	if err := os.WriteFile(path, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	song := &model.Song{Name: "Song", Artist: "Artist"}
	index := core.DownloadDedupIndex{core.SongKey(song): filename}

	result, ok := existingDownloadResult("download-id", directory, song, index)
	if !ok {
		t.Fatal("existing download was not recognized")
	}
	if result.State.Status != "ready" || result.State.Percent != 100 || result.State.Path != path {
		t.Fatalf("existing download result = %#v", result)
	}
}
