package sourceapi

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/guohuiyuan/go-music-dl/core"
	"github.com/guohuiyuan/go-music-dl/internal/fnos-bridge/sourcecontract"
	"github.com/guohuiyuan/music-lib/model"
)

func searchTracks(ctx context.Context, keyword string, sources []string, limit int) ([]sourcecontract.Track, error) {
	type result struct {
		index int
		songs []model.Song
	}
	type searchEntry struct {
		source string
		fn     core.SearchFunc
	}
	searches := make([]searchEntry, 0, len(sources))
	for _, source := range sources {
		source = strings.TrimSpace(source)
		if source == "" || source == "local" {
			continue
		}
		if fn := core.GetSearchFunc(source); fn != nil {
			searches = append(searches, searchEntry{source: source, fn: fn})
		}
	}
	if len(searches) == 0 {
		return nil, fmt.Errorf("no supported sources")
	}

	results := make(chan result, len(searches))
	for index, search := range searches {
		index, search := index, search
		go func() {
			songs, err := search.fn(keyword)
			if err != nil {
				results <- result{index: index}
				return
			}
			for i := range songs {
				if songs[i].Source == "" {
					songs[i].Source = search.source
				}
			}
			results <- result{index: index, songs: songs}
		}()
	}

	ordered := make([][]model.Song, len(searches))
	for remaining := len(searches); remaining > 0; remaining-- {
		select {
		case <-ctx.Done():
			return flattenTracks(ordered, limit), ctx.Err()
		case result := <-results:
			ordered[result.index] = result.songs
		}
	}
	return flattenTracks(ordered, limit), nil
}

func flattenTracks(results [][]model.Song, limit int) []sourcecontract.Track {
	seen := make(map[string]struct{})
	tracks := make([]sourcecontract.Track, 0, limit)
	for _, sourceSongs := range results {
		for _, song := range sourceSongs {
			if strings.TrimSpace(song.ID) == "" || strings.TrimSpace(song.Source) == "" {
				continue
			}
			key := normalizedText(song.Name) + "\x00" + normalizedText(song.Artist)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			tracks = append(tracks, trackFromSong(song))
			if len(tracks) >= limit {
				return tracks
			}
		}
	}
	return tracks
}

func normalizedText(value string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.Is(unicode.Cf, r):
			return -1
		default:
			return unicode.ToLower(r)
		}
	}, strings.TrimSpace(value))
}

func trackFromSong(song model.Song) sourcecontract.Track {
	return sourcecontract.Track{
		ID:       song.ID,
		Source:   song.Source,
		Name:     song.Name,
		Artist:   song.Artist,
		Album:    song.Album,
		Duration: song.Duration,
		Size:     song.Size,
		Bitrate:  song.Bitrate,
		Ext:      song.Ext,
		Cover:    song.Cover,
	}
}

func musicSong(track sourcecontract.Track) model.Song {
	return model.Song{
		ID:       track.ID,
		Source:   track.Source,
		Name:     track.Name,
		Artist:   track.Artist,
		Album:    track.Album,
		Duration: track.Duration,
		Size:     track.Size,
		Bitrate:  track.Bitrate,
		Ext:      track.Ext,
		Cover:    track.Cover,
	}
}

func trackFromRequest(r *http.Request) sourcecontract.Track {
	query := r.URL.Query()
	duration, _ := strconv.Atoi(query.Get("duration"))
	size, _ := strconv.ParseInt(query.Get("size"), 10, 64)
	bitrate, _ := strconv.Atoi(query.Get("bitrate"))
	return sourcecontract.Track{
		ID:       strings.TrimSpace(query.Get("id")),
		Source:   strings.TrimSpace(query.Get("source")),
		Name:     strings.TrimSpace(query.Get("name")),
		Artist:   strings.TrimSpace(query.Get("artist")),
		Album:    strings.TrimSpace(query.Get("album")),
		Duration: duration,
		Size:     size,
		Bitrate:  bitrate,
		Ext:      strings.TrimSpace(query.Get("ext")),
		Cover:    strings.TrimSpace(query.Get("cover")),
	}
}
