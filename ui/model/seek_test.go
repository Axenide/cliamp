package model

import (
	"testing"
	"time"

	"github.com/bjarneo/cliamp/playlist"
	"github.com/bjarneo/cliamp/ui"
)

func TestSetSeekStepLarge(t *testing.T) {
	t.Run("sets positive value", func(t *testing.T) {
		m := Model{}
		m.SetSeekStepLarge(45 * time.Second)
		if got, want := m.seekStepLarge, 45*time.Second; got != want {
			t.Fatalf("seekStepLarge = %v, want %v", got, want)
		}
	})

	t.Run("resets non-positive to default", func(t *testing.T) {
		tests := []time.Duration{0, -5 * time.Second}
		for _, in := range tests {
			m := Model{}
			m.SetSeekStepLarge(in)
			if got, want := m.seekStepLarge, 30*time.Second; got != want {
				t.Fatalf("SetSeekStepLarge(%v): seekStepLarge = %v, want %v", in, got, want)
			}
		}
	})

	t.Run("clamps too-small positive value", func(t *testing.T) {
		m := Model{}
		m.SetSeekStepLarge(5 * time.Second)
		if got, want := m.seekStepLarge, 6*time.Second; got != want {
			t.Fatalf("seekStepLarge = %v, want %v", got, want)
		}
	})
}

// streamSeekModel returns a model playing a seekable stream track, the shape
// that routes seeks through the debounce path.
func streamSeekModel(eng *playbackFakeEngine) Model {
	m := Model{player: eng, playlist: playlist.New()}
	m.setPlaybackTrack(playlist.Track{Path: "https://nav/stream", Stream: true})
	return m
}

func TestStreamSeekDebounceSumsPresses(t *testing.T) {
	eng := &playbackFakeEngine{playing: true, seekable: true, duration: time.Hour}
	m := streamSeekModel(eng)

	for range 3 {
		if cmd := m.doSeek(5 * time.Second); cmd != nil {
			t.Fatal("doSeek returned a command, want the seek debounced")
		}
	}
	if len(eng.seekCalls) != 0 {
		t.Fatalf("Seek calls during debounce = %d, want 0", len(eng.seekCalls))
	}

	cmd := m.tickSeek(time.Duration(seekDebounceTicks) * ui.TickFast)
	if cmd == nil {
		t.Fatal("tickSeek returned nil, want the summed seek to fire")
	}
	if msg := cmd(); msg.(seekTickMsg).target != 15*time.Second {
		t.Fatalf("seek target = %v, want 15s", msg.(seekTickMsg).target)
	}
	if len(eng.seekCalls) != 1 || eng.seekCalls[0] != 15*time.Second {
		t.Fatalf("Seek calls = %v, want one 15s seek", eng.seekCalls)
	}
}

// A decoder restart can outlast the debounce window. The seek queued meanwhile
// must wait for the running one, or the two restarts race and the older target
// wins.
func TestStreamSeekWaitsForRunningSeek(t *testing.T) {
	eng := &playbackFakeEngine{playing: true, seekable: true, duration: time.Hour}
	m := streamSeekModel(eng)

	m.doSeek(10 * time.Second)
	first := m.tickSeek(time.Duration(seekDebounceTicks) * ui.TickFast)
	if first == nil {
		t.Fatal("tickSeek returned nil, want the first seek to fire")
	}

	// Second burst lands while the first restart is still running.
	m.doSeek(20 * time.Second)
	if cmd := m.tickSeek(time.Duration(seekDebounceTicks) * ui.TickFast); cmd != nil {
		t.Fatal("tickSeek returned a command, want the seek deferred until the first completes")
	}
	if len(eng.seekCalls) != 0 {
		t.Fatalf("Seek calls before the first seek runs = %v, want none", eng.seekCalls)
	}

	msg := first()
	eng.position = 10 * time.Second // the first restart landed
	updated, cmd := m.Update(msg)
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("Update returned nil, want the deferred seek to fire")
	}
	cmd()

	if len(eng.seekCalls) != 2 {
		t.Fatalf("Seek calls = %v, want the first and the deferred seek", eng.seekCalls)
	}
	if got := eng.seekCalls[0] + eng.seekCalls[1]; got != 30*time.Second {
		t.Fatalf("final position = %v, want the latest target 30s", got)
	}
	if !m.seek.active {
		t.Fatal("seek.active = false, want the seek still marked active")
	}
}
