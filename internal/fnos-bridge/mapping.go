package fnosbridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/guohuiyuan/go-music-dl/internal/fnos-bridge/sourcecontract"
)

const (
	maxMappingRecords  = 20000
	mappingFileVersion = 2
)

// Record is the durable join between a synthetic fnOS GUID and a go-music-dl
// source item. fnOS only accepts 32-hex track IDs, while source IDs are opaque.
type Record struct {
	GUID         string `json:"guid"`
	Source       string `json:"source"`
	ID           string `json:"id"`
	Title        string `json:"title"`
	Artist       string `json:"artist"`
	Album        string `json:"album"`
	Cover        string `json:"cover,omitempty"`
	Ext          string `json:"ext,omitempty"`
	Duration     int    `json:"duration,omitempty"`
	Size         int64  `json:"size,omitempty"`
	Bitrate      int    `json:"bitrate,omitempty"`
	OfficialGUID string `json:"official_guid,omitempty"`
	// FavoritePending is retained for state files written before per-user
	// favorite intents were introduced. New writes use Favorites.
	FavoritePending bool                      `json:"favorite_pending,omitempty"`
	Favorites       map[string]FavoriteIntent `json:"favorites,omitempty"`
	BoundAt         int64                     `json:"bound_at,omitempty"`
	UpdatedAt       int64                     `json:"updated_at"`
}

type FavoriteIntent struct {
	Status    string `json:"status"`
	UpdatedAt int64  `json:"updated_at"`
	LastError string `json:"last_error,omitempty"`
}

type mappingFile struct {
	Version int               `json:"version"`
	Items   map[string]Record `json:"items"`
}

// MappingStore persists synthetic GUIDs so restarts do not invalidate IDs held
// by the fnOS client, favorites or play history.
type MappingStore struct {
	mu      sync.RWMutex
	path    string
	records map[string]Record
}

func NewMappingStore(path string) *MappingStore {
	return &MappingStore{path: strings.TrimSpace(path), records: make(map[string]Record)}
}

func (s *MappingStore) Load() error {
	if s == nil || s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read fnOS GUID map: %w", err)
	}
	var file mappingFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("decode fnOS GUID map: %w", err)
	}
	records := make(map[string]Record, len(file.Items))
	for guid, record := range file.Items {
		if len(guid) != 32 || record.Source == "" || record.ID == "" {
			continue
		}
		record.GUID = guid
		records[guid] = record
	}
	s.mu.Lock()
	s.records = records
	s.mu.Unlock()
	return nil
}

func (s *MappingStore) Remember(song sourcecontract.Track) Record {
	guid := FakeGUID(song.Source, song.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, hadExisting := s.records[guid]
	record := Record{
		GUID:      guid,
		Source:    strings.TrimSpace(song.Source),
		ID:        strings.TrimSpace(song.ID),
		Title:     strings.TrimSpace(song.Name),
		Artist:    strings.TrimSpace(song.Artist),
		Album:     strings.TrimSpace(song.Album),
		Cover:     strings.TrimSpace(song.Cover),
		Ext:       normalizeExt(song.Ext),
		Duration:  song.Duration,
		Size:      song.Size,
		Bitrate:   song.Bitrate,
		UpdatedAt: time.Now().Unix(),
	}
	if hadExisting {
		record.OfficialGUID = existing.OfficialGUID
		record.FavoritePending = existing.FavoritePending
		record.Favorites = existing.Favorites
		record.BoundAt = existing.BoundAt
	}
	s.records[record.GUID] = record
	s.pruneLocked()
	return record
}

// RebindSource keeps the fnOS-facing GUID stable while replacing the source
// track behind it after an automatic playback fallback.
func (s *MappingStore) RebindSource(guid string, song sourcecontract.Track) (Record, bool) {
	guid = normalizeGUID(guid)
	source := strings.TrimSpace(song.Source)
	id := strings.TrimSpace(song.ID)
	if guid == "" || source == "" || id == "" {
		return Record{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[guid]
	if !ok {
		return Record{}, false
	}
	record.Source = source
	record.ID = id
	record.Title = strings.TrimSpace(song.Name)
	record.Artist = strings.TrimSpace(song.Artist)
	record.Album = strings.TrimSpace(song.Album)
	record.Cover = strings.TrimSpace(song.Cover)
	record.Ext = normalizeExt(song.Ext)
	record.Duration = song.Duration
	record.Size = song.Size
	record.Bitrate = song.Bitrate
	record.UpdatedAt = time.Now().Unix()
	s.records[guid] = record
	return record, true
}

func (s *MappingStore) BindOfficial(guid, officialGUID string) (Record, bool) {
	guid = normalizeGUID(guid)
	officialGUID = normalizeGUID(officialGUID)
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[guid]
	if !ok || officialGUID == "" {
		return Record{}, false
	}
	record.OfficialGUID = officialGUID
	record.BoundAt = time.Now().Unix()
	record.UpdatedAt = record.BoundAt
	s.records[guid] = record
	return record, true
}

func (s *MappingStore) SetFavoriteState(guid, userGUID, status, lastError string) (Record, bool) {
	guid = normalizeGUID(guid)
	userGUID = normalizeUserGUID(userGUID)
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[guid]
	if !ok || userGUID == "" {
		return Record{}, false
	}
	now := time.Now().Unix()
	favorites := make(map[string]FavoriteIntent, len(record.Favorites)+1)
	for user, intent := range record.Favorites {
		favorites[user] = intent
	}
	status = strings.TrimSpace(status)
	if status == "" || status == "bound" {
		delete(favorites, userGUID)
	} else {
		previous := favorites[userGUID]
		previous.Status = status
		previous.UpdatedAt = now
		previous.LastError = strings.TrimSpace(lastError)
		favorites[userGUID] = previous
	}
	record.Favorites = favorites
	record.FavoritePending = false
	record.UpdatedAt = now
	s.records[guid] = record
	return record, true
}

func (s *MappingStore) PendingFavorites(userGUID string) []Record {
	userGUID = normalizeUserGUID(userGUID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	records := make([]Record, 0)
	for _, record := range s.records {
		if record.FavoritePending || pendingFavoriteForUser(record, userGUID) {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].UpdatedAt > records[j].UpdatedAt })
	return records
}

func (s *MappingStore) ClaimLegacyFavorite(guid, userGUID string) (Record, bool) {
	guid = normalizeGUID(guid)
	userGUID = normalizeUserGUID(userGUID)
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[guid]
	if !ok || !record.FavoritePending || userGUID == "" {
		return record, ok
	}
	favorites := make(map[string]FavoriteIntent, len(record.Favorites)+1)
	for user, intent := range record.Favorites {
		favorites[user] = intent
	}
	now := time.Now().Unix()
	favorites[userGUID] = FavoriteIntent{Status: "pending", UpdatedAt: now}
	record.Favorites = favorites
	record.FavoritePending = false
	record.UpdatedAt = now
	s.records[guid] = record
	return record, true
}

func (s *MappingStore) ClaimLegacyFavoriteUser(targetUserGUID string) bool {
	targetUserGUID = normalizeUserGUID(targetUserGUID)
	if targetUserGUID == "" || !strings.HasPrefix(targetUserGUID, "gateway:") {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	legacyUsers := make(map[string]struct{})
	for _, record := range s.records {
		for user := range record.Favorites {
			if user != "" && !strings.HasPrefix(user, "gateway:") {
				legacyUsers[user] = struct{}{}
			}
		}
	}
	if len(legacyUsers) != 1 {
		return false
	}
	var sourceUser string
	for user := range legacyUsers {
		sourceUser = user
	}
	changed := false
	now := time.Now().Unix()
	for guid, record := range s.records {
		intent, ok := record.Favorites[sourceUser]
		if !ok {
			continue
		}
		favorites := make(map[string]FavoriteIntent, len(record.Favorites))
		for user, value := range record.Favorites {
			favorites[user] = value
		}
		delete(favorites, sourceUser)
		if _, exists := favorites[targetUserGUID]; !exists {
			favorites[targetUserGUID] = intent
		}
		record.Favorites = favorites
		record.UpdatedAt = now
		s.records[guid] = record
		changed = true
	}
	return changed
}

func pendingFavoriteForUser(record Record, userGUID string) bool {
	intent, ok := record.Favorites[userGUID]
	return ok && (intent.Status == "pending" || intent.Status == "failed")
}

func hasPendingFavorite(record Record) bool {
	if record.FavoritePending {
		return true
	}
	for _, intent := range record.Favorites {
		if intent.Status == "pending" || intent.Status == "failed" {
			return true
		}
	}
	return false
}

func normalizeUserGUID(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func (r Record) track() sourcecontract.Track {
	return sourcecontract.Track{
		ID: r.ID, Source: r.Source, Name: r.Title, Artist: r.Artist,
		Album: r.Album, Duration: r.Duration, Size: r.Size,
		Bitrate: r.Bitrate, Ext: r.Ext, Cover: r.Cover,
	}
}

func (s *MappingStore) Get(guid string) (Record, bool) {
	if s == nil {
		return Record{}, false
	}
	guid = normalizeGUID(guid)
	s.mu.RLock()
	record, ok := s.records[guid]
	s.mu.RUnlock()
	return record, ok
}

func (s *MappingStore) Save() error {
	if s == nil || s.path == "" {
		return nil
	}
	s.mu.Lock()
	file := mappingFile{Version: mappingFileVersion, Items: make(map[string]Record, len(s.records))}
	for guid, record := range s.records {
		file.Items[guid] = record
	}
	s.mu.Unlock()

	data, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("encode fnOS GUID map: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create fnOS GUID map directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".guid-map-*.tmp")
	if err != nil {
		return fmt.Errorf("create fnOS GUID map temp file: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write fnOS GUID map: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync fnOS GUID map: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close fnOS GUID map: %w", err)
	}
	if err := os.Rename(tempName, s.path); err != nil {
		return fmt.Errorf("replace fnOS GUID map: %w", err)
	}
	return nil
}

func (s *MappingStore) pruneLocked() {
	if len(s.records) <= maxMappingRecords {
		return
	}
	type item struct {
		guid    string
		at      int64
		pending bool
	}
	items := make([]item, 0, len(s.records))
	for guid, record := range s.records {
		items = append(items, item{guid: guid, at: record.UpdatedAt, pending: hasPendingFavorite(record)})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].pending != items[j].pending {
			return !items[i].pending
		}
		return items[i].at < items[j].at
	})
	for _, candidate := range items[:len(items)-maxMappingRecords] {
		delete(s.records, candidate.guid)
	}
}

func FakeGUID(source, id string) string {
	sum := sha256.Sum256([]byte("go-music-dl::fnos-music::" + strings.TrimSpace(source) + "::" + strings.TrimSpace(id)))
	return hex.EncodeToString(sum[:16])
}

func FakeChildGUID(guid, child string) string {
	sum := sha256.Sum256([]byte("go-music-dl::fnos-child::" + normalizeGUID(guid) + "::" + strings.TrimSpace(child)))
	return hex.EncodeToString(sum[:16])
}

func normalizeGUID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "track_")
	if slash := strings.LastIndex(value, "/"); slash >= 0 {
		value = value[slash+1:]
	}
	for _, suffix := range []string{".jpg", ".jpeg", ".png", ".webp", ".gif"} {
		value = strings.TrimSuffix(value, suffix)
	}
	return value
}

func normalizeExt(value string) string {
	value = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(value), "."))
	switch value {
	case "mpeg", "mp3":
		return "mp3"
	case "mp4", "aac", "m4a":
		return "m4a"
	case "wave", "wav":
		return "wav"
	case "vorbis", "ogg":
		return "ogg"
	case "flac", "opus", "wma", "ape", "aiff":
		return value
	default:
		if value == "" {
			return "mp3"
		}
		return value
	}
}
