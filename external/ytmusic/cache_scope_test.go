package ytmusic

import (
	"testing"

	"github.com/bjarneo/cliamp/playlist"
)

func TestYTCacheRejectsDifferentOAuthAccount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := saveCreds(&storedCreds{RefreshToken: "account-a"}); err != nil {
		t.Fatal(err)
	}
	scopeA := oauthCacheScope("client")
	cache := newYTCache(scopeA)
	cache.setPlaylists([]playlistEntry{{ID: "private", Name: "Private"}})
	cache.setTracks("private", []playlist.Track{{Title: "Secret"}})
	saveSnapshot(cache.snapshot())

	matching := loadYTCache(scopeA)
	if !matching.playlistsFresh() {
		t.Fatal("matching cache scope was not loaded")
	}

	if err := saveCreds(&storedCreds{RefreshToken: "account-b"}); err != nil {
		t.Fatal(err)
	}
	scopeB := oauthCacheScope("client")
	loaded := loadYTCache(scopeB)
	if loaded.playlistsFresh() || len(loaded.Tracks) != 0 {
		t.Fatal("OAuth cache reused entries after the stored account changed")
	}
	if scopeA == scopeB {
		t.Fatal("OAuth cache scope did not change with the refresh token")
	}
}

func TestYTCacheRejectsLegacyUnscopedData(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	legacy := newYTCache("")
	legacy.setPlaylists([]playlistEntry{{ID: "old", Name: "Old"}})
	saveSnapshot(legacy.snapshot())

	loaded := loadYTCache(oauthCacheScope("client"))
	if loaded.playlistsFresh() {
		t.Fatal("scoped cache reused legacy unscoped playlists")
	}
}
