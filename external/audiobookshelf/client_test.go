package audiobookshelf

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}

func statusResponse(code int, status string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Status:     status,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewBufferString("")),
	}
}

func mockClient(token, user, password string, libraries []string, fn roundTripFunc) *Client {
	c := NewClient("https://abs.example.com", token, user, password, libraries)
	c.SetHTTPClient(&http.Client{Transport: fn})
	return c
}

const librariesJSON = `{"libraries":[
	{"id":"lib-b","name":"Audiobooks","mediaType":"book"},
	{"id":"lib-p","name":"Podcasts","mediaType":"podcast"},
	{"id":"lib-x","name":"Kids","mediaType":"book"}
]}`

func TestLibrariesSendsBearerToken(t *testing.T) {
	var gotAuth string
	c := mockClient("tok", "", "", nil, func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/libraries" {
			t.Fatalf("unexpected path %s", req.URL.Path)
		}
		gotAuth = req.Header.Get("Authorization")
		return jsonResponse(librariesJSON), nil
	})

	libs, err := c.Libraries()
	if err != nil {
		t.Fatalf("Libraries() error: %v", err)
	}
	if len(libs) != 3 {
		t.Fatalf("got %d libraries, want 3", len(libs))
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("Authorization = %q, want Bearer tok", gotAuth)
	}
}

func TestLibrariesFilteredByName(t *testing.T) {
	c := mockClient("tok", "", "", []string{"podcasts"}, func(req *http.Request) (*http.Response, error) {
		return jsonResponse(librariesJSON), nil
	})

	libs, err := c.Libraries()
	if err != nil {
		t.Fatalf("Libraries() error: %v", err)
	}
	if len(libs) != 1 || libs[0].ID != "lib-p" {
		t.Fatalf("libs = %+v, want only lib-p", libs)
	}
}

func TestLibrariesCached(t *testing.T) {
	calls := 0
	c := mockClient("tok", "", "", nil, func(req *http.Request) (*http.Response, error) {
		calls++
		return jsonResponse(librariesJSON), nil
	})

	if _, err := c.Libraries(); err != nil {
		t.Fatalf("first Libraries() error: %v", err)
	}
	if _, err := c.Libraries(); err != nil {
		t.Fatalf("second Libraries() error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("http calls = %d, want 1", calls)
	}

	c.ClearCache()
	if _, err := c.Libraries(); err != nil {
		t.Fatalf("third Libraries() error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("http calls after ClearCache = %d, want 2", calls)
	}
}

func TestLazyLogin(t *testing.T) {
	var loginBody string
	c := mockClient("", "listener", "secret", nil, func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/login":
			data, _ := io.ReadAll(req.Body)
			loginBody = string(data)
			return jsonResponse(`{"user":{"id":"u-1","token":"logged-in"}}`), nil
		case "/api/libraries":
			if got := req.Header.Get("Authorization"); got != "Bearer logged-in" {
				t.Fatalf("Authorization = %q, want Bearer logged-in", got)
			}
			return jsonResponse(librariesJSON), nil
		default:
			t.Fatalf("unexpected path %s", req.URL.Path)
			return nil, nil
		}
	})

	if err := c.Ping(); err != nil {
		t.Fatalf("Ping() error: %v", err)
	}
	if !strings.Contains(loginBody, `"username":"listener"`) || !strings.Contains(loginBody, `"password":"secret"`) {
		t.Fatalf("login body = %s", loginBody)
	}
}

func TestLoginAcceptsAccessToken(t *testing.T) {
	c := mockClient("", "listener", "secret", nil, func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/login":
			return jsonResponse(`{"user":{"id":"u-1"},"accessToken":"newer"}`), nil
		case "/api/libraries":
			if got := req.Header.Get("Authorization"); got != "Bearer newer" {
				t.Fatalf("Authorization = %q, want Bearer newer", got)
			}
			return jsonResponse(librariesJSON), nil
		default:
			t.Fatalf("unexpected path %s", req.URL.Path)
			return nil, nil
		}
	})

	if err := c.Ping(); err != nil {
		t.Fatalf("Ping() error: %v", err)
	}
}

func TestMissingCredentials(t *testing.T) {
	c := mockClient("", "", "", nil, func(req *http.Request) (*http.Response, error) {
		t.Fatal("no request should be made")
		return nil, nil
	})

	err := c.Ping()
	if err == nil || !strings.Contains(err.Error(), "audiobookshelf:") {
		t.Fatalf("Ping() error = %v, want audiobookshelf-prefixed error", err)
	}
}

func TestGetHTTPError(t *testing.T) {
	c := mockClient("tok", "", "", nil, func(req *http.Request) (*http.Response, error) {
		return statusResponse(http.StatusUnauthorized, "401 Unauthorized"), nil
	})

	err := c.Ping()
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("Ping() error = %v, want 401", err)
	}
}

func TestStreamURL(t *testing.T) {
	c := mockClient("tok", "", "", nil, func(req *http.Request) (*http.Response, error) {
		t.Fatal("no request should be made")
		return nil, nil
	})

	got := c.StreamURL("item-1", "12345")
	want := "https://abs.example.com/api/items/item-1/file/12345?token=tok"
	if got != want {
		t.Fatalf("StreamURL() = %q, want %q", got, want)
	}
}

func TestIsStreamURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{name: "item file", url: "https://abs.example.com/api/items/item-1/file/123?token=t", want: true},
		{name: "cover", url: "https://abs.example.com/api/items/item-1/cover", want: false},
		{name: "other host path", url: "https://music.example.com/rest/stream", want: false},
		{name: "not a url", url: "::::", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsStreamURL(tt.url); got != tt.want {
				t.Fatalf("IsStreamURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}
