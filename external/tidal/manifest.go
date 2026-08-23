package tidal

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Audio quality values accepted by the playbackinfo endpoint. Any paid Tidal
// subscription includes all four tiers (the HiFi/HiFi Plus split ended in
// 2024).
const (
	qualityLow      = "LOW"             // 96 kbps AAC
	qualityHigh     = "HIGH"            // 320 kbps AAC
	qualityLossless = "LOSSLESS"        // FLAC 16-bit/44.1kHz
	qualityHiRes    = "HI_RES_LOSSLESS" // FLAC 24-bit up to 192kHz (DASH)
)

// normalizeQuality maps a [tidal] config quality string to a Tidal
// audioquality value. ok is false when s is not a recognized quality name.
func normalizeQuality(s string) (quality string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "low":
		return qualityLow, true
	case "high":
		return qualityHigh, true
	case "", "lossless":
		return qualityLossless, true
	case "hires", "hi_res", "hi-res", "hi_res_lossless", "max":
		return qualityHiRes, true
	default:
		return "", false
	}
}

// errDASHManifest signals that playbackinfo returned a segmented MPEG-DASH
// manifest, which the player pipeline cannot stream directly. Tidal delivers
// HI_RES_LOSSLESS via DASH; resolveStreamURL falls back to LOSSLESS (plain
// FLAC over HTTP) when it sees this.
var errDASHManifest = errors.New("tidal: DASH manifest not supported")

// btsManifest is Tidal's "basic track stream" manifest: a JSON document with
// direct CDN URLs, delivered base64-encoded in the playbackinfo response for
// LOW/HIGH/LOSSLESS qualities.
type btsManifest struct {
	MimeType       string   `json:"mimeType"`
	Codecs         string   `json:"codecs"`
	EncryptionType string   `json:"encryptionType"`
	URLs           []string `json:"urls"`
}

// streamURLFromManifest extracts a direct stream URL from a playbackinfo
// response. Returns errDASHManifest for DASH manifests (hi-res tiers).
func streamURLFromManifest(pi apiPlaybackInfo) (string, error) {
	switch {
	case strings.Contains(pi.ManifestMimeType, "dash+xml"):
		return "", errDASHManifest
	case strings.Contains(pi.ManifestMimeType, "vnd.tidal.bts"):
	default:
		return "", fmt.Errorf("tidal: unsupported manifest type %q", pi.ManifestMimeType)
	}

	raw, err := base64.StdEncoding.DecodeString(pi.Manifest)
	if err != nil {
		return "", fmt.Errorf("tidal: decode manifest: %w", err)
	}
	var m btsManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("tidal: parse manifest: %w", err)
	}
	if m.EncryptionType != "" && m.EncryptionType != "NONE" {
		return "", fmt.Errorf("tidal: stream is encrypted (%s), not supported", m.EncryptionType)
	}
	if len(m.URLs) == 0 || m.URLs[0] == "" {
		return "", errors.New("tidal: manifest contains no stream URL")
	}
	return m.URLs[0], nil
}

// resolveStreamURL fetches playback info at the requested quality and extracts
// a direct stream URL. When HI_RES_LOSSLESS comes back as a DASH manifest, it
// re-requests at LOSSLESS so hi-res users still get CD-quality FLAC. The
// quality that actually produced the URL is returned so callers can stop
// requesting hi-res once it proves undeliverable.
func resolveStreamURL(quality string, fetch func(quality string) (apiPlaybackInfo, error)) (u, usedQuality string, err error) {
	pi, err := fetch(quality)
	if err != nil {
		return "", "", err
	}
	u, err = streamURLFromManifest(pi)
	if errors.Is(err, errDASHManifest) && quality == qualityHiRes {
		pi, err = fetch(qualityLossless)
		if err != nil {
			return "", "", err
		}
		u, err = streamURLFromManifest(pi)
		return u, qualityLossless, err
	}
	return u, quality, err
}
