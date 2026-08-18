package tidal

import (
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
)

func TestNormalizeQuality(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"low", qualityLow, true},
		{"high", qualityHigh, true},
		{"lossless", qualityLossless, true},
		{"", qualityLossless, true},
		{"LOSSLESS", qualityLossless, true},
		{"  lossless  ", qualityLossless, true},
		{"hires", qualityHiRes, true},
		{"hi_res", qualityHiRes, true},
		{"hi-res", qualityHiRes, true},
		{"hi_res_lossless", qualityHiRes, true},
		{"max", qualityHiRes, true},
		{"320", "", false},
		{"flac", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := normalizeQuality(tt.in)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("normalizeQuality(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func btsBase64(jsonBody string) string {
	return base64.StdEncoding.EncodeToString([]byte(jsonBody))
}

func TestStreamURLFromManifest(t *testing.T) {
	tests := []struct {
		name    string
		pi      apiPlaybackInfo
		want    string // expected URL; empty (with nil wantErr) = any error
		wantErr error  // sentinel to match
	}{
		{
			name: "bts flac",
			pi: apiPlaybackInfo{
				ManifestMimeType: "application/vnd.tidal.bts",
				Manifest:         btsBase64(`{"mimeType":"audio/flac","codecs":"flac","encryptionType":"NONE","urls":["https://cdn.tidal.com/x.flac"]}`),
			},
			want: "https://cdn.tidal.com/x.flac",
		},
		{
			name: "bts empty encryption type",
			pi: apiPlaybackInfo{
				ManifestMimeType: "application/vnd.tidal.bts",
				Manifest:         btsBase64(`{"urls":["https://cdn.tidal.com/y.m4a"]}`),
			},
			want: "https://cdn.tidal.com/y.m4a",
		},
		{
			name: "dash manifest",
			pi: apiPlaybackInfo{
				ManifestMimeType: "application/dash+xml",
				Manifest:         btsBase64(`<MPD></MPD>`),
			},
			wantErr: errDASHManifest,
		},
		{
			name: "encrypted stream",
			pi: apiPlaybackInfo{
				ManifestMimeType: "application/vnd.tidal.bts",
				Manifest:         btsBase64(`{"encryptionType":"OLD_AES","urls":["https://cdn.tidal.com/z"]}`),
			},
		},
		{
			name: "unknown mime type",
			pi:   apiPlaybackInfo{ManifestMimeType: "application/vnd.tidal.emu"},
		},
		{
			name: "invalid base64",
			pi: apiPlaybackInfo{
				ManifestMimeType: "application/vnd.tidal.bts",
				Manifest:         "!!!not-base64!!!",
			},
		},
		{
			name: "no urls",
			pi: apiPlaybackInfo{
				ManifestMimeType: "application/vnd.tidal.bts",
				Manifest:         btsBase64(`{"encryptionType":"NONE","urls":[]}`),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := streamURLFromManifest(tt.pi)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("err = %v, want %v", err, tt.wantErr)
				}
			case tt.want == "":
				if err == nil {
					t.Errorf("expected error, got url %q", got)
				}
			default:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tt.want {
					t.Errorf("url = %q, want %q", got, tt.want)
				}
			}
		})
	}
}

func TestResolveStreamURL(t *testing.T) {
	bts := func(u string) apiPlaybackInfo {
		return apiPlaybackInfo{
			ManifestMimeType: "application/vnd.tidal.bts",
			Manifest:         btsBase64(fmt.Sprintf(`{"encryptionType":"NONE","urls":[%q]}`, u)),
		}
	}
	dash := apiPlaybackInfo{ManifestMimeType: "application/dash+xml", Manifest: btsBase64(`<MPD/>`)}

	t.Run("lossless direct", func(t *testing.T) {
		var qualities []string
		u, used, err := resolveStreamURL(qualityLossless, func(q string) (apiPlaybackInfo, error) {
			qualities = append(qualities, q)
			return bts("https://cdn/a.flac"), nil
		})
		if err != nil || u != "https://cdn/a.flac" || used != qualityLossless {
			t.Fatalf("got (%q, %q, %v)", u, used, err)
		}
		if len(qualities) != 1 || qualities[0] != qualityLossless {
			t.Errorf("fetched qualities = %v", qualities)
		}
	})

	t.Run("hires falls back to lossless on dash", func(t *testing.T) {
		var qualities []string
		u, used, err := resolveStreamURL(qualityHiRes, func(q string) (apiPlaybackInfo, error) {
			qualities = append(qualities, q)
			if q == qualityHiRes {
				return dash, nil
			}
			return bts("https://cdn/cd.flac"), nil
		})
		if err != nil || u != "https://cdn/cd.flac" {
			t.Fatalf("got (%q, %v)", u, err)
		}
		if used != qualityLossless {
			t.Errorf("used quality = %q, want %q", used, qualityLossless)
		}
		want := []string{qualityHiRes, qualityLossless}
		if len(qualities) != 2 || qualities[0] != want[0] || qualities[1] != want[1] {
			t.Errorf("fetched qualities = %v, want %v", qualities, want)
		}
	})

	t.Run("lossless dash does not fall back", func(t *testing.T) {
		_, _, err := resolveStreamURL(qualityLossless, func(q string) (apiPlaybackInfo, error) {
			return dash, nil
		})
		if !errors.Is(err, errDASHManifest) {
			t.Errorf("err = %v, want errDASHManifest", err)
		}
	})

	t.Run("fetch error propagates", func(t *testing.T) {
		wantErr := errors.New("boom")
		_, _, err := resolveStreamURL(qualityHiRes, func(q string) (apiPlaybackInfo, error) {
			return apiPlaybackInfo{}, wantErr
		})
		if !errors.Is(err, wantErr) {
			t.Errorf("err = %v, want %v", err, wantErr)
		}
	})
}
