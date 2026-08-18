package tidal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// testClient returns a client with a valid-looking token pointed at srv.
func testClient(srv *httptest.Server) *client {
	c := newClient("id", "secret")
	c.baseURL = srv.URL + "/"
	c.http = srv.Client()
	c.accessToken = "token"
	c.tokenType = "Bearer"
	c.refreshToken = "refresh"
	c.expiresAt = time.Now().Add(time.Hour)
	c.countryCode = "US"
	c.userID = "42"
	return c
}

func TestFetchListPagination(t *testing.T) {
	// 150 favorite artists: page 1 full (100), page 2 partial (50).
	total := 150
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("countryCode"); got != "US" {
			t.Errorf("countryCode = %q, want US", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Errorf("Authorization = %q", got)
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		var items []map[string]any
		for i := offset; i < offset+limit && i < total; i++ {
			items = append(items, map[string]any{
				"item": map[string]any{"id": i, "name": fmt.Sprintf("artist-%d", i)},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":              items,
			"totalNumberOfItems": total,
		})
	}))
	defer srv.Close()

	c := testClient(srv)
	artists, err := c.favoriteArtists(context.Background())
	if err != nil {
		t.Fatalf("favoriteArtists: %v", err)
	}
	if len(artists) != total {
		t.Fatalf("got %d artists, want %d", len(artists), total)
	}
	if artists[0].Name != "artist-0" || artists[total-1].Name != "artist-149" {
		t.Errorf("unexpected boundary items: %q, %q", artists[0].Name, artists[total-1].Name)
	}
}

func TestFetchListMaxItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		items := make([]map[string]any, limit)
		for i := range items {
			items[i] = map[string]any{"item": map[string]any{"id": i, "title": "t"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":              items,
			"totalNumberOfItems": 10000,
		})
	}))
	defer srv.Close()

	c := testClient(srv)
	tracks, err := c.favoriteTracks(context.Background(), 250)
	if err != nil {
		t.Fatalf("favoriteTracks: %v", err)
	}
	if len(tracks) != 250 {
		t.Errorf("got %d tracks, want cap of 250", len(tracks))
	}
}

func TestDoRequestRefreshesOn401(t *testing.T) {
	// The refresh path persists rotated tokens; keep the write away from the
	// user's real credentials file.
	t.Setenv("CLIAMP_CONFIG_DIR", t.TempDir())

	var apiCalls, tokenCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"status":401}`)
			return
		}
		fmt.Fprint(w, `{"sessionId":"s","countryCode":"NO","userId":7}`)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		fmt.Fprint(w, `{"access_token":"fresh","token_type":"Bearer","expires_in":3600}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := testClient(srv)
	c.tokenURL = srv.URL + "/token"
	if err := c.loadSession(context.Background()); err != nil {
		t.Fatalf("loadSession: %v", err)
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Errorf("token refreshes = %d, want 1", got)
	}
	if got := apiCalls.Load(); got != 2 {
		t.Errorf("api calls = %d, want 2 (401 then retry)", got)
	}
	if c.countryCode != "NO" {
		t.Errorf("countryCode = %q, want NO", c.countryCode)
	}
}
