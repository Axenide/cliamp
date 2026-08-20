package ytmusic

import (
	"testing"

	"github.com/bjarneo/cliamp/playlist"
)

func TestYTCacheRejectsDifferentAuthenticationScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cache := newYTCache("cookies:chrome")
	cache.setPlaylists([]playlistEntry{{ID: "private", Name: "Private"}})
	cache.setTracks("private", []playlist.Track{{Title: "Secret"}})
	saveSnapshot(cache.snapshot())

	matching := loadYTCache("cookies:chrome")
	if !matching.playlistsFresh() {
		t.Fatal("matching cache scope was not loaded")
	}
	for _, scope := range []string{"cookies:firefox", "cookies:chrome:Profile 2", "oauth:client-id"} {
		t.Run(scope, func(t *testing.T) {
			loaded := loadYTCache(scope)
			if loaded.playlistsFresh() || len(loaded.Tracks) != 0 {
				t.Fatalf("cache for %q reused entries from %q", scope, cache.Scope)
			}
			if loaded.Scope != scope {
				t.Fatalf("cache scope = %q, want %q", loaded.Scope, scope)
			}
		})
	}
}

func TestYTCacheRejectsLegacyUnscopedData(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	legacy := newYTCache("")
	legacy.setPlaylists([]playlistEntry{{ID: "old", Name: "Old"}})
	saveSnapshot(legacy.snapshot())

	loaded := loadYTCache("cookies:chrome")
	if loaded.playlistsFresh() {
		t.Fatal("scoped cache reused legacy unscoped playlists")
	}
}
