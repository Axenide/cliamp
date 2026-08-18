package tidal

import "testing"

func TestStreamURLRegistry(t *testing.T) {
	const u = "https://sp-pr-cf.audio.tidal.com/mediatracks/abc/0.flac"
	if IsStreamURL(u) {
		t.Fatalf("unregistered URL reported as stream URL")
	}
	registerStreamURL(u)
	if !IsStreamURL(u) {
		t.Fatalf("registered URL not recognized")
	}
	registerStreamURL("")
	if IsStreamURL("") {
		t.Fatalf("empty URL must not be registered")
	}
}
