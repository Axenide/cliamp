// Package audiobookshelf implements a cliamp provider for an Audiobookshelf
// server, exposing its audiobooks and podcasts as playlists.
package audiobookshelf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var defaultHTTPClient = &http.Client{Timeout: 30 * time.Second}

const maxResponseBody = 10 << 20

// Client speaks to an Audiobookshelf server over its HTTP API.
type Client struct {
	baseURL    string
	user       string
	password   string
	libraries  []string
	httpClient *http.Client

	// mu guards the lazily-populated fields below, which are read and written
	// from concurrent tea.Cmd goroutines. It is never held across network I/O.
	mu           sync.Mutex
	token        string
	libraryCache []Library
}

// NewClient returns a Client for the given server URL and credentials. When
// libraries is non-empty, only libraries with those names are visible.
func NewClient(baseURL, token, user, password string, libraries []string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		user:       user,
		password:   password,
		libraries:  libraries,
		httpClient: defaultHTTPClient,
	}
}

// SetHTTPClient overrides the HTTP client used for requests. Mainly for tests
// that inject a custom transport.
func (c *Client) SetHTTPClient(hc *http.Client) { c.httpClient = hc }

// ClearCache discards the cached library list.
func (c *Client) ClearCache() {
	c.mu.Lock()
	c.libraryCache = nil
	c.mu.Unlock()
}

// Ping checks that the server is reachable and the credentials are accepted.
func (c *Client) Ping() error {
	_, err := c.Libraries()
	return err
}

// Libraries returns the libraries visible to the user, filtered by the
// configured name list. Results are cached after the first successful call.
func (c *Client) Libraries() ([]Library, error) {
	c.mu.Lock()
	cached := c.libraryCache
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}

	var resp librariesResponse
	if err := c.get("/api/libraries", nil, &resp); err != nil {
		return nil, err
	}

	out := make([]Library, 0, len(resp.Libraries))
	for _, lib := range resp.Libraries {
		if c.allowed(lib.Name) {
			out = append(out, lib)
		}
	}

	c.mu.Lock()
	c.libraryCache = out
	c.mu.Unlock()
	return out, nil
}

func (c *Client) allowed(name string) bool {
	if len(c.libraries) == 0 {
		return true
	}
	for _, want := range c.libraries {
		if strings.EqualFold(strings.TrimSpace(want), name) {
			return true
		}
	}
	return false
}

// StreamURL returns an authenticated audio URL for one file of an item.
func (c *Client) StreamURL(itemID, ino string) string {
	_ = c.ensureAuth()
	u := c.baseURL + "/api/items/" + url.PathEscape(itemID) + "/file/" + url.PathEscape(ino)
	if token := c.authToken(); token != "" {
		u += "?" + url.Values{"token": {token}}.Encode()
	}
	return u
}

// IsStreamURL reports whether the URL is an Audiobookshelf item-file endpoint.
// Used by the player to route these URLs through the buffered ffmpeg pipeline.
func IsStreamURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	p := strings.ToLower(u.Path)
	return strings.Contains(p, "/api/items/") && strings.Contains(p, "/file/")
}

func (c *Client) authToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

func (c *Client) get(p string, params url.Values, out any) error {
	if err := c.ensureAuth(); err != nil {
		return err
	}

	u := c.baseURL + p
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("audiobookshelf: %s: %w", p, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.authToken())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("audiobookshelf: %s: %w", p, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("audiobookshelf: %s: http status %s", p, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("audiobookshelf: %s: %w", p, err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("audiobookshelf: %s: %w", p, err)
	}
	return nil
}

func (c *Client) postJSON(p string, payload any) error {
	if err := c.ensureAuth(); err != nil {
		return err
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("audiobookshelf: %s: %w", p, err)
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+p, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("audiobookshelf: %s: %w", p, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.authToken())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("audiobookshelf: %s: %w", p, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("audiobookshelf: %s: http status %s", p, resp.Status)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
	return nil
}

func (c *Client) ensureAuth() error {
	c.mu.Lock()
	have := c.token != ""
	c.mu.Unlock()
	if have {
		return nil
	}
	if c.user == "" || c.password == "" {
		return fmt.Errorf("audiobookshelf: missing token or user/password")
	}

	body, err := json.Marshal(map[string]string{"username": c.user, "password": c.password})
	if err != nil {
		return fmt.Errorf("audiobookshelf: login: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/login", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("audiobookshelf: login: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("audiobookshelf: login: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("audiobookshelf: login: http status %s", resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("audiobookshelf: login: %w", err)
	}

	var out loginResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return fmt.Errorf("audiobookshelf: login: %w", err)
	}
	token := out.User.Token
	if token == "" {
		token = out.AccessToken
	}
	if token == "" {
		return fmt.Errorf("audiobookshelf: login: missing token")
	}

	c.mu.Lock()
	c.token = token
	c.mu.Unlock()
	return nil
}
