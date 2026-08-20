package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/bjarneo/cliamp/playlist"
)

func TestCanceledSpotAlbumResponseIsIgnored(t *testing.T) {
	tests := []struct {
		name   string
		cancel func(*Model)
	}{
		{
			name: "close overlay",
			cancel: func(m *Model) {
				m.closeSpotSearch()
			},
		},
		{
			name: "back to input",
			cancel: func(m *Model) {
				m.handleSpotSearchResultsKey(tea.KeyPressMsg{Code: tea.KeyEscape})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{
				playlist: playlist.New(),
				spotSearch: spotSearchState{
					visible:      true,
					screen:       spotSearchResults,
					albumLoading: true,
				},
			}
			const gen = 7
			m.requests.spotAlbum = gen

			tt.cancel(&m)
			if m.requests.spotAlbum == gen {
				t.Fatal("album request generation was not invalidated")
			}

			updated, cmd := m.Update(spotAlbumTracksMsg{
				gen:    gen,
				action: spotAlbumPlay,
				album:  albumResult("Late Album"),
				tracks: []playlist.Track{{Title: "Late Track"}},
			})
			m = updated.(Model)
			if cmd != nil {
				t.Fatal("stale album response returned a command")
			}
			if m.playlist.Len() != 0 {
				t.Fatalf("playlist length = %d after stale response, want 0", m.playlist.Len())
			}
		})
	}
}
