package fnosbridge

import (
	"path/filepath"
	"testing"

	"github.com/guohuiyuan/go-music-dl/internal/fnos-bridge/sourcecontract"
)

func TestMappingStorePersistsStableGUIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "guid-map.json")
	store := NewMappingStore(path)
	song := sourcecontract.Track{
		ID: "12345", Source: "netease", Name: "Song", Artist: "Artist",
		Album: "Album", Ext: "flac", Duration: 215, Cover: "https://example.test/cover.jpg",
	}
	record := store.Remember(song)
	if len(record.GUID) != 32 {
		t.Fatalf("GUID length = %d, want 32", len(record.GUID))
	}
	if record.GUID != FakeGUID(song.Source, song.ID) {
		t.Fatalf("GUID is not deterministic")
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded := NewMappingStore(path)
	if err := loaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := loaded.Get(record.GUID)
	if !ok {
		t.Fatalf("persisted GUID %s not found", record.GUID)
	}
	if got.ID != song.ID || got.Source != song.Source || got.Title != song.Name {
		t.Fatalf("loaded record = %#v", got)
	}
	bound, ok := loaded.BindOfficial(record.GUID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if !ok || bound.OfficialGUID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("bind official = %#v, %v", bound, ok)
	}
	bound, ok = loaded.SetFavoriteState(record.GUID, "user-a", "pending", "")
	if !ok || !pendingFavoriteForUser(bound, "user-a") {
		t.Fatalf("favorite pending = %#v, %v", bound, ok)
	}
	refreshed := loaded.Remember(song)
	if refreshed.OfficialGUID != bound.OfficialGUID || !pendingFavoriteForUser(refreshed, "user-a") {
		t.Fatalf("Remember lost durable binding: %#v", refreshed)
	}
}

func TestFavoriteIntentIsIsolatedByUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guid-map.json")
	store := NewMappingStore(path)
	record := store.Remember(sourcecontract.Track{ID: "1", Source: "netease", Name: "Song", Artist: "Artist"})
	if _, ok := store.SetFavoriteState(record.GUID, "user-a", "pending", ""); !ok {
		t.Fatal("set user-a favorite")
	}
	if _, ok := store.SetFavoriteState(record.GUID, "user-b", "failed", "auth expired"); !ok {
		t.Fatal("set user-b favorite")
	}
	if got := store.PendingFavorites("user-a"); len(got) != 1 || !pendingFavoriteForUser(got[0], "user-a") {
		t.Fatalf("user-a favorites = %#v", got)
	}
	if got := store.PendingFavorites("user-b"); len(got) != 1 || !pendingFavoriteForUser(got[0], "user-b") {
		t.Fatalf("user-b favorites = %#v", got)
	}
	if _, ok := store.SetFavoriteState(record.GUID, "user-a", "", ""); !ok {
		t.Fatal("clear user-a favorite")
	}
	if got := store.PendingFavorites("user-a"); len(got) != 0 {
		t.Fatalf("user-a favorite was not cleared: %#v", got)
	}
	if got := store.PendingFavorites("user-b"); len(got) != 1 {
		t.Fatalf("clearing user-a affected user-b: %#v", got)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	loaded := NewMappingStore(path)
	if err := loaded.Load(); err != nil {
		t.Fatal(err)
	}
	if got := loaded.PendingFavorites("user-b"); len(got) != 1 {
		t.Fatalf("per-user favorite intent was not persisted: %#v", got)
	}
}

func TestLegacyFavoriteUserMigration(t *testing.T) {
	store := NewMappingStore("")
	first := store.Remember(sourcecontract.Track{ID: "1", Source: "netease", Name: "First", Artist: "Artist"})
	second := store.Remember(sourcecontract.Track{ID: "2", Source: "netease", Name: "Second", Artist: "Artist"})
	_, _ = store.SetFavoriteState(first.GUID, "official-user", "pending", "")
	_, _ = store.SetFavoriteState(second.GUID, "official-user", "failed", "retry")

	if !store.ClaimLegacyFavoriteUser("gateway:1000") {
		t.Fatal("legacy favorite user was not migrated")
	}
	if got := store.PendingFavorites("gateway:1000"); len(got) != 2 {
		t.Fatalf("migrated favorites = %#v", got)
	}
	if got := store.PendingFavorites("official-user"); len(got) != 0 {
		t.Fatalf("legacy favorites still visible: %#v", got)
	}
}

func TestNormalizeGUIDAcceptsCoverVariants(t *testing.T) {
	const guid = "0123456789abcdef0123456789abcdef"
	for _, input := range []string{guid, "track_" + guid, "prefix/" + guid, guid + ".jpg"} {
		if got := normalizeGUID(input); got != guid {
			t.Fatalf("normalizeGUID(%q) = %q, want %q", input, got, guid)
		}
	}
}
