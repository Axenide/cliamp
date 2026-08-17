package audiobookshelf

import (
	"net/http"
	"testing"

	"github.com/bjarneo/cliamp/provider"
)

func mockProvider(fn roundTripFunc) *Provider {
	return newProvider(mockClient("tok", "", "", nil, fn))
}

const bookItemJSON = `{
	"id":"book-1",
	"mediaType":"book",
	"media":{
		"duration":7200,
		"numTracks":2,
		"metadata":{"title":"Mistborn","authorName":"Brandon Sanderson","publishedYear":"2006"},
		"chapters":[
			{"id":0,"start":0,"end":3600,"title":"Chapter One"},
			{"id":1,"start":3600,"end":5400,"title":"Chapter Two"},
			{"id":2,"start":5400,"end":7200,"title":"Chapter Three"}
		],
		"tracks":[
			{"index":1,"startOffset":0,"duration":3600,"ino":"111","metadata":{"filename":"part1.m4b"}},
			{"index":2,"startOffset":3600,"duration":3600,"ino":"222","metadata":{"filename":"part2.m4b"}}
		]
	}
}`

const podcastItemJSON = `{
	"id":"pod-1",
	"mediaType":"podcast",
	"media":{
		"numEpisodes":2,
		"metadata":{"title":"Darknet Diaries","author":"Jack Rhysider"},
		"episodes":[
			{"id":"ep-1","title":"Old One","index":1,"publishedAt":1000,"audioFile":{"ino":"901","duration":1800}},
			{"id":"ep-2","title":"New One","index":2,"publishedAt":2000,"audioFile":{"ino":"902","duration":2400}}
		]
	}
}`

func TestProviderName(t *testing.T) {
	p := mockProvider(func(req *http.Request) (*http.Response, error) {
		t.Fatal("no request expected")
		return nil, nil
	})
	if p.Name() != "Audiobookshelf" {
		t.Fatalf("Name() = %q, want Audiobookshelf", p.Name())
	}
}

func TestPlaylistsSectionsBooksAndPodcasts(t *testing.T) {
	p := mockProvider(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/libraries":
			return jsonResponse(`{"libraries":[
				{"id":"lib-b","name":"Audiobooks","mediaType":"book"},
				{"id":"lib-p","name":"Podcasts","mediaType":"podcast"}
			]}`), nil
		case "/api/libraries/lib-b/items":
			return jsonResponse(`{"total":1,"results":[` + bookItemJSON + `]}`), nil
		case "/api/libraries/lib-p/items":
			return jsonResponse(`{"total":1,"results":[` + podcastItemJSON + `]}`), nil
		default:
			t.Fatalf("unexpected path %s", req.URL.Path)
			return nil, nil
		}
	})

	lists, err := p.Playlists()
	if err != nil {
		t.Fatalf("Playlists() error: %v", err)
	}
	if len(lists) != 2 {
		t.Fatalf("got %d playlists, want 2", len(lists))
	}
	book := lists[0]
	if book.ID != "b:book-1" || book.Section != sectionAudiobooks {
		t.Fatalf("book playlist = %+v", book)
	}
	if book.Name != "Brandon Sanderson — Mistborn" {
		t.Fatalf("book name = %q", book.Name)
	}
	if book.TrackCount != 2 || book.DurationSecs != 7200 {
		t.Fatalf("book counts = %+v", book)
	}
	show := lists[1]
	if show.ID != "p:pod-1" || show.Section != sectionPodcasts {
		t.Fatalf("show playlist = %+v", show)
	}
	if show.Name != "Darknet Diaries" || show.TrackCount != 2 {
		t.Fatalf("show playlist = %+v", show)
	}
}

func TestPlaylistsEmptyCatalogCached(t *testing.T) {
	calls := 0
	p := mockProvider(func(req *http.Request) (*http.Response, error) {
		calls++
		switch req.URL.Path {
		case "/api/libraries":
			return jsonResponse(`{"libraries":[{"id":"lib-b","name":"Audiobooks","mediaType":"book"}]}`), nil
		case "/api/libraries/lib-b/items":
			return jsonResponse(`{"total":0,"results":[]}`), nil
		default:
			t.Fatalf("unexpected path %s", req.URL.Path)
			return nil, nil
		}
	})

	lists, err := p.Playlists()
	if err != nil {
		t.Fatalf("Playlists() error: %v", err)
	}
	if len(lists) != 0 {
		t.Fatalf("got %d playlists, want 0", len(lists))
	}

	if _, err := p.Playlists(); err != nil {
		t.Fatalf("cached Playlists() error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("http calls = %d, want 2 (libraries + items on the first call; second call must hit the cache)", calls)
	}
}

func TestTracksBookUsesChapterTitles(t *testing.T) {
	calls := 0
	p := mockProvider(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/items/book-1" {
			t.Fatalf("unexpected path %s", req.URL.Path)
		}
		calls++
		return jsonResponse(bookItemJSON), nil
	})

	tracks, err := p.Tracks("b:book-1")
	if err != nil {
		t.Fatalf("Tracks() error: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("got %d tracks, want 2", len(tracks))
	}

	first := tracks[0]
	if first.Title != "Chapter One" {
		t.Fatalf("first title = %q, want Chapter One (file covers one chapter)", first.Title)
	}
	if first.Artist != "Brandon Sanderson" || first.Album != "Mistborn" || first.Year != 2006 {
		t.Fatalf("first track = %+v", first)
	}
	if first.TrackNumber != 1 || first.DurationSecs != 3600 || !first.Stream {
		t.Fatalf("first track = %+v", first)
	}
	if first.Meta(provider.MetaAudiobookshelfID) != "book-1" {
		t.Fatalf("first meta id = %q", first.Meta(provider.MetaAudiobookshelfID))
	}
	if first.Meta(provider.MetaAudiobookshelfOffset) != "0" {
		t.Fatalf("first offset = %q, want 0", first.Meta(provider.MetaAudiobookshelfOffset))
	}
	if first.Meta(provider.MetaAudiobookshelfTotal) != "7200" {
		t.Fatalf("first total = %q, want 7200", first.Meta(provider.MetaAudiobookshelfTotal))
	}

	second := tracks[1]
	if second.Title != "part2.m4b" {
		t.Fatalf("second title = %q, want the filename (file spans two chapters)", second.Title)
	}
	if second.Meta(provider.MetaAudiobookshelfOffset) != "3600" {
		t.Fatalf("second offset = %q, want 3600", second.Meta(provider.MetaAudiobookshelfOffset))
	}

	if _, err := p.Tracks("b:book-1"); err != nil {
		t.Fatalf("cached Tracks() error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("http calls = %d, want 1 (second call must hit the cache)", calls)
	}
}

func TestTracksPodcastNewestFirst(t *testing.T) {
	p := mockProvider(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/items/pod-1" {
			t.Fatalf("unexpected path %s", req.URL.Path)
		}
		return jsonResponse(podcastItemJSON), nil
	})

	tracks, err := p.Tracks("p:pod-1")
	if err != nil {
		t.Fatalf("Tracks() error: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("got %d tracks, want 2", len(tracks))
	}
	if tracks[0].Title != "New One" || tracks[1].Title != "Old One" {
		t.Fatalf("episode order = %q, %q", tracks[0].Title, tracks[1].Title)
	}
	if tracks[0].Album != "Darknet Diaries" || tracks[0].Artist != "Jack Rhysider" {
		t.Fatalf("episode track = %+v", tracks[0])
	}
	if tracks[0].DurationSecs != 2400 {
		t.Fatalf("episode duration = %d, want 2400", tracks[0].DurationSecs)
	}
	if tracks[0].Meta(provider.MetaAudiobookshelfEpisode) != "ep-2" {
		t.Fatalf("episode meta = %q", tracks[0].Meta(provider.MetaAudiobookshelfEpisode))
	}
}

func TestTracksUnknownPlaylistID(t *testing.T) {
	p := mockProvider(func(req *http.Request) (*http.Response, error) {
		t.Fatal("no request expected")
		return nil, nil
	})
	if _, err := p.Tracks("x:nope"); err == nil {
		t.Fatal("Tracks() error = nil, want an error for an unprefixed id")
	}
}

func TestRefreshClearsCaches(t *testing.T) {
	calls := 0
	p := mockProvider(func(req *http.Request) (*http.Response, error) {
		calls++
		return jsonResponse(bookItemJSON), nil
	})

	if _, err := p.Tracks("b:book-1"); err != nil {
		t.Fatalf("Tracks() error: %v", err)
	}
	p.Refresh()
	if _, err := p.Tracks("b:book-1"); err != nil {
		t.Fatalf("Tracks() after Refresh error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("http calls = %d, want 2", calls)
	}
}
