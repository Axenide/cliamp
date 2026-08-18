package tidal

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bjarneo/cliamp/applog"
	"github.com/bjarneo/cliamp/playlist"
	"github.com/bjarneo/cliamp/provider"
)

// Compile-time interface checks.
var (
	_ playlist.Provider         = (*TidalProvider)(nil)
	_ playlist.Authenticator    = (*TidalProvider)(nil)
	_ playlist.Refresher        = (*TidalProvider)(nil)
	_ provider.Searcher         = (*TidalProvider)(nil)
	_ provider.ArtistBrowser    = (*TidalProvider)(nil)
	_ provider.AlbumBrowser     = (*TidalProvider)(nil)
	_ provider.AlbumTrackLoader = (*TidalProvider)(nil)
	_ provider.Closer           = (*TidalProvider)(nil)
)

// favoriteTracksID is the synthetic playlist ID for the user's favorite tracks.
const favoriteTracksID = "favorites/tracks"

// favoriteTracksLimit caps the synthetic Favorite Tracks list. Each track
// costs one playbackinfo call to resolve a (short-lived) stream URL, so
// resolving an unbounded library would be slow and wasteful. Matches Qobuz.
const favoriteTracksLimit = 500

// resolveConcurrency bounds how many playbackinfo calls run in parallel when
// resolving a playlist's streaming URLs.
const resolveConcurrency = 8

// albumSortTypes is the static sort list for Tidal album browsing. The private
// API has no global catalog listing, so browsing surfaces favorite albums.
var albumSortTypes = []provider.SortType{
	{ID: "favorites", Label: "Favorite Albums"},
}

// TidalProvider implements playlist.Provider backed by Tidal's private client
// API. Streaming URLs are resolved per track via playbackinfopostpaywall and
// routed through the player's buffered pipeline (see stream.go).
type TidalProvider struct {
	quality      string // normalized Tidal audioquality value
	clientID     string
	clientSecret string

	// hiresFallback latches once a HI_RES_LOSSLESS request comes back as a
	// DASH manifest, so later tracks skip the doomed hi-res round-trip and
	// request LOSSLESS directly.
	hiresFallback atomic.Bool

	mu         sync.Mutex
	client     *client
	authCancel context.CancelFunc

	listCache  []playlist.PlaylistInfo
	trackCache map[string][]playlist.Track
}

// New creates a TidalProvider. Authentication is deferred until the user first
// selects the provider. quality is a [tidal] config quality name (see
// NormalizeQuality); unrecognized values fall back to lossless. Empty client
// credentials fall back to the built-in pair.
func New(quality, clientID, clientSecret string) *TidalProvider {
	q, ok := normalizeQuality(quality)
	if !ok {
		applog.UserError("tidal: unknown quality %q, using \"lossless\" (valid: low, high, lossless, hires)", quality)
		q = qualityLossless
	}
	if clientID == "" {
		clientID, clientSecret = fallbackClientID, fallbackClientSecret
	} else if clientSecret == "" {
		// Matches python-tidal: a client without a secret uses its ID as one.
		clientSecret = clientID
	}
	return &TidalProvider{
		quality:      q,
		clientID:     clientID,
		clientSecret: clientSecret,
		trackCache:   make(map[string][]playlist.Track),
	}
}

func (p *TidalProvider) Name() string { return "Tidal" }

// ensureClient builds an authenticated client from stored credentials only
// (no browser). Returns playlist.ErrNeedsAuth if interactive sign-in is needed.
func (p *TidalProvider) ensureClient() (*client, error) {
	p.mu.Lock()
	if p.client != nil {
		c := p.client
		p.mu.Unlock()
		return c, nil
	}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := newClientSilent(ctx)
	if err != nil {
		applog.Debug("tidal: silent auth failed, prompting sign-in: %v", err)
		return nil, playlist.ErrNeedsAuth
	}

	p.mu.Lock()
	p.client = c
	p.mu.Unlock()
	return c, nil
}

// Authenticate runs the interactive device-flow sign-in (shows a
// link.tidal.com URL, waits for approval). Implements playlist.Authenticator.
func (p *TidalProvider) Authenticate() error {
	p.mu.Lock()
	if p.client != nil {
		p.mu.Unlock()
		return nil
	}
	if p.authCancel != nil {
		p.authCancel()
		p.authCancel = nil
	}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	p.mu.Lock()
	p.authCancel = cancel
	p.mu.Unlock()

	c, err := newClientInteractive(ctx, p.clientID, p.clientSecret)

	p.mu.Lock()
	p.authCancel = nil
	p.mu.Unlock()
	cancel()

	if err != nil {
		return err
	}
	p.mu.Lock()
	p.client = c
	p.mu.Unlock()
	return nil
}

// Close cancels any in-progress sign-in. Implements provider.Closer.
func (p *TidalProvider) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.authCancel != nil {
		p.authCancel()
		p.authCancel = nil
	}
}

// Refresh clears cached playlists and tracks so the next call re-fetches and
// re-resolves streaming URLs (which expire). Implements playlist.Refresher.
func (p *TidalProvider) Refresh() {
	p.mu.Lock()
	p.listCache = nil
	p.trackCache = make(map[string][]playlist.Track)
	p.mu.Unlock()
}

// Playlists returns the user's Tidal playlists plus a synthetic Favorite
// Tracks entry.
func (p *TidalProvider) Playlists() ([]playlist.PlaylistInfo, error) {
	c, err := p.ensureClient()
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	if p.listCache != nil {
		cached := slices.Clone(p.listCache)
		p.mu.Unlock()
		return cached, nil
	}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pls, err := c.userPlaylists(ctx)
	if err != nil {
		return nil, err
	}

	lists := []playlist.PlaylistInfo{
		{
			ID:      favoriteTracksID,
			Name:    "Favorite Tracks",
			Section: "Library",
		},
	}
	for _, pl := range pls {
		lists = append(lists, playlist.PlaylistInfo{
			ID:           pl.UUID,
			Name:         pl.Title,
			TrackCount:   pl.NumberOfTracks,
			DurationSecs: pl.Duration,
			Section:      "Your playlists",
		})
	}

	p.mu.Lock()
	p.listCache = lists
	p.mu.Unlock()
	return slices.Clone(lists), nil
}

// Tracks returns the tracks of a playlist (or the synthetic Favorite Tracks
// entry), each with a resolved streaming URL.
func (p *TidalProvider) Tracks(playlistID string) ([]playlist.Track, error) {
	c, err := p.ensureClient()
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	if cached, ok := p.trackCache[playlistID]; ok {
		tracks := slices.Clone(cached)
		p.mu.Unlock()
		return tracks, nil
	}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var apiTracks []apiTrack
	if playlistID == favoriteTracksID {
		apiTracks, err = c.favoriteTracks(ctx, favoriteTracksLimit)
	} else {
		apiTracks, err = c.playlistTracks(ctx, playlistID)
	}
	if err != nil {
		return nil, err
	}

	tracks := p.resolveTracks(ctx, c, apiTracks, nil)

	p.mu.Lock()
	p.trackCache[playlistID] = tracks
	p.mu.Unlock()
	return slices.Clone(tracks), nil
}

// SearchTracks searches the Tidal catalog. Implements provider.Searcher.
func (p *TidalProvider) SearchTracks(ctx context.Context, query string, limit int) ([]playlist.Track, error) {
	c, err := p.ensureClient()
	if err != nil {
		return nil, err
	}
	apiTracks, err := c.searchTracks(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	return p.resolveTracks(ctx, c, apiTracks, nil), nil
}

// Artists returns the user's favorite artists. Implements provider.ArtistBrowser.
func (p *TidalProvider) Artists() ([]provider.ArtistInfo, error) {
	c, err := p.ensureClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	artists, err := c.favoriteArtists(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]provider.ArtistInfo, 0, len(artists))
	for _, a := range artists {
		out = append(out, provider.ArtistInfo{
			ID:   a.ID.String(),
			Name: a.Name,
		})
	}
	return out, nil
}

// ArtistAlbums returns the albums of an artist. Implements provider.ArtistBrowser.
func (p *TidalProvider) ArtistAlbums(artistID string) ([]provider.AlbumInfo, error) {
	c, err := p.ensureClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	albums, err := c.artistAlbums(ctx, artistID)
	if err != nil {
		return nil, err
	}
	out := make([]provider.AlbumInfo, 0, len(albums))
	for _, a := range albums {
		out = append(out, albumInfo(a))
	}
	return out, nil
}

// AlbumList returns the user's favorite albums (the private API has no global
// album catalog to browse). Implements provider.AlbumBrowser.
func (p *TidalProvider) AlbumList(_ string, offset, size int) ([]provider.AlbumInfo, error) {
	c, err := p.ensureClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	albums, err := c.favoriteAlbums(ctx, offset, size)
	if err != nil {
		return nil, err
	}
	out := make([]provider.AlbumInfo, 0, len(albums))
	for _, a := range albums {
		out = append(out, albumInfo(a))
	}
	return out, nil
}

func (p *TidalProvider) AlbumSortTypes() []provider.SortType { return albumSortTypes }

func (p *TidalProvider) DefaultAlbumSort() string { return "favorites" }

// AlbumTracks returns the tracks of an album. Implements provider.AlbumTrackLoader.
func (p *TidalProvider) AlbumTracks(albumID string) ([]playlist.Track, error) {
	c, err := p.ensureClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// The album metadata (used only as a fallback for track fields) and the
	// track list are independent round-trips; fetch them concurrently.
	var (
		album     apiAlbum
		albumErr  error
		tracks    []apiTrack
		tracksErr error
		wg        sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		album, albumErr = c.albumGet(ctx, albumID)
	}()
	go func() {
		defer wg.Done()
		tracks, tracksErr = c.albumTracks(ctx, albumID)
	}()
	wg.Wait()
	if albumErr != nil {
		return nil, albumErr
	}
	if tracksErr != nil {
		return nil, tracksErr
	}
	return p.resolveTracks(ctx, c, tracks, &album), nil
}

// resolveTracks converts API tracks into playable tracks, resolving a signed
// streaming URL for each in parallel. albumFallback supplies album metadata
// for tracks that lack it (albums/{id}/tracks nests tracks without an album
// field). Tracks that are not streamable or fail URL resolution are returned
// as unplayable.
func (p *TidalProvider) resolveTracks(ctx context.Context, c *client, in []apiTrack, albumFallback *apiAlbum) []playlist.Track {
	out := make([]playlist.Track, len(in))
	sem := make(chan struct{}, resolveConcurrency)
	var wg sync.WaitGroup

	for i := range in {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			out[idx] = p.buildTrack(ctx, c, in[idx], albumFallback)
		}(i)
	}
	wg.Wait()
	return out
}

// buildTrack maps a single API track to a playlist.Track, resolving its stream
// URL unless the track is not streamable.
func (p *TidalProvider) buildTrack(ctx context.Context, c *client, t apiTrack, albumFallback *apiAlbum) playlist.Track {
	album := t.Album
	if album == nil {
		album = albumFallback
	}

	track := playlist.Track{
		Title:        t.Title,
		Artist:       trackArtist(t, album),
		TrackNumber:  t.TrackNumber,
		DurationSecs: t.Duration,
		Stream:       true,
		ProviderMeta: map[string]string{provider.MetaTidalID: t.ID.String()},
	}
	if album != nil {
		track.Album = album.Title
		track.Year = provider.YearFromDate(album.ReleaseDate)
	}

	if !t.AllowStreaming || !t.StreamReady {
		track.Unplayable = true
		return track
	}

	quality := p.quality
	if quality == qualityHiRes && p.hiresFallback.Load() {
		quality = qualityLossless
	}
	u, used, err := resolveStreamURL(quality, func(q string) (apiPlaybackInfo, error) {
		return c.playbackInfo(ctx, t.ID.String(), q)
	})
	if err != nil {
		applog.Debug("tidal: resolve stream url for track %s: %v", t.ID.String(), err)
		track.Unplayable = true
		return track
	}
	if quality == qualityHiRes && used != qualityHiRes && p.hiresFallback.CompareAndSwap(false, true) {
		applog.Info("tidal: hi-res streams are DASH-delivered (unsupported); using lossless for this session")
	}
	registerStreamURL(u)
	track.Path = u
	return track
}

// trackArtist picks the best available artist name for a track.
func trackArtist(t apiTrack, album *apiAlbum) string {
	if t.Artist.Name != "" {
		return t.Artist.Name
	}
	if len(t.Artists) > 0 && t.Artists[0].Name != "" {
		return t.Artists[0].Name
	}
	if album != nil {
		return album.Artist.Name
	}
	return ""
}

// albumInfo maps a Tidal album to provider.AlbumInfo.
func albumInfo(a apiAlbum) provider.AlbumInfo {
	return provider.AlbumInfo{
		ID:         a.ID.String(),
		Name:       a.Title,
		Artist:     a.Artist.Name,
		ArtistID:   a.Artist.ID.String(),
		Year:       provider.YearFromDate(a.ReleaseDate),
		TrackCount: a.NumberOfTracks,
	}
}
