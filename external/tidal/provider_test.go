package tidal

import (
	"encoding/json"
	"testing"
)

func TestNewNormalizesQualityAndCredentials(t *testing.T) {
	tests := []struct {
		name         string
		quality      string
		clientID     string
		clientSecret string
		wantQuality  string
		wantID       string
		wantSecret   string
	}{
		{
			name:        "defaults",
			wantQuality: qualityLossless,
			wantID:      fallbackClientID,
			wantSecret:  fallbackClientSecret,
		},
		{
			name:        "invalid quality falls back to lossless",
			quality:     "ultra",
			wantQuality: qualityLossless,
			wantID:      fallbackClientID,
			wantSecret:  fallbackClientSecret,
		},
		{
			name:         "custom credentials",
			quality:      "hires",
			clientID:     "my-id",
			clientSecret: "my-secret",
			wantQuality:  qualityHiRes,
			wantID:       "my-id",
			wantSecret:   "my-secret",
		},
		{
			name:        "custom id without secret uses id as secret",
			quality:     "high",
			clientID:    "solo-id",
			wantQuality: qualityHigh,
			wantID:      "solo-id",
			wantSecret:  "solo-id",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New(tt.quality, tt.clientID, tt.clientSecret)
			if p.quality != tt.wantQuality {
				t.Errorf("quality = %q, want %q", p.quality, tt.wantQuality)
			}
			if p.clientID != tt.wantID {
				t.Errorf("clientID = %q, want %q", p.clientID, tt.wantID)
			}
			if p.clientSecret != tt.wantSecret {
				t.Errorf("clientSecret = %q, want %q", p.clientSecret, tt.wantSecret)
			}
		})
	}
}

func TestTrackArtist(t *testing.T) {
	album := &apiAlbum{Artist: apiArtist{Name: "Album Artist"}}
	tests := []struct {
		name  string
		track apiTrack
		album *apiAlbum
		want  string
	}{
		{
			name:  "main artist",
			track: apiTrack{Artist: apiArtist{Name: "Main"}, Artists: []apiArtist{{Name: "First"}}},
			want:  "Main",
		},
		{
			name:  "first of artists list",
			track: apiTrack{Artists: []apiArtist{{Name: "First"}, {Name: "Second"}}},
			want:  "First",
		},
		{
			name:  "album artist fallback",
			track: apiTrack{},
			album: album,
			want:  "Album Artist",
		},
		{
			name:  "no artist anywhere",
			track: apiTrack{},
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := trackArtist(tt.track, tt.album); got != tt.want {
				t.Errorf("trackArtist = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAlbumInfo(t *testing.T) {
	a := apiAlbum{
		ID:             json.Number("123"),
		Title:          "Album",
		NumberOfTracks: 12,
		ReleaseDate:    "2021-01-02",
		Artist:         apiArtist{ID: json.Number("7"), Name: "Artist"},
	}
	got := albumInfo(a)
	if got.ID != "123" || got.Name != "Album" || got.Artist != "Artist" ||
		got.ArtistID != "7" || got.Year != 2021 || got.TrackCount != 12 {
		t.Errorf("albumInfo = %+v", got)
	}
}
