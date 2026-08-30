package player

import (
	"reflect"
	"testing"
	"time"
)

type sequenceStreamer struct {
	samples [][2]float64
	pos     int
}

func (s *sequenceStreamer) Stream(dst [][2]float64) (int, bool) {
	n := copy(dst, s.samples[s.pos:])
	s.pos += n
	return n, s.pos < len(s.samples)
}

func (*sequenceStreamer) Err() error { return nil }

func TestTapPreservesStereoAndMonoMixAcrossWraparound(t *testing.T) {
	samples := [][2]float64{
		{1, -1},
		{0.2, 0.4},
		{0.3, 0.5},
		{0.4, 0.6},
		{0.5, 0.7},
		{0.6, 0.8},
	}
	tap := newTap(&sequenceStreamer{samples: samples}, 4, 44100, 0)

	for range 2 {
		out := make([][2]float64, 3)
		if n, _ := tap.Stream(out); n != len(out) {
			t.Fatalf("Stream() wrote %d frames, want %d", n, len(out))
		}
	}

	stereo := make([][2]float64, 4)
	if n := tap.StereoSamplesInto(stereo); n != len(stereo) {
		t.Fatalf("StereoSamplesInto() = %d, want %d", n, len(stereo))
	}
	wantStereo := samples[2:]
	if !reflect.DeepEqual(stereo, wantStereo) {
		t.Fatalf("StereoSamplesInto() = %v, want %v", stereo, wantStereo)
	}

	mono := make([]float64, 4)
	if n := tap.SamplesInto(mono); n != len(mono) {
		t.Fatalf("SamplesInto() = %d, want %d", n, len(mono))
	}
	wantMono := []float64{0.4, 0.5, 0.6, 0.7}
	if !reflect.DeepEqual(mono, wantMono) {
		t.Fatalf("SamplesInto() = %v, want %v", mono, wantMono)
	}
}

func TestTapWaveformSamplesAdvanceBetweenWrites(t *testing.T) {
	samples := make([][2]float64, 8)
	for i := range samples {
		samples[i] = [2]float64{float64(i), float64(i)}
	}

	now := time.Unix(1, 0)
	tap := newTap(&sequenceStreamer{samples: samples}, 16, 10, 8)
	tap.now = func() time.Time { return now }
	if n, _ := tap.Stream(make([][2]float64, len(samples))); n != len(samples) {
		t.Fatalf("Stream() wrote %d frames, want %d", n, len(samples))
	}

	buf := make([]float64, 2)
	now = now.Add(300 * time.Millisecond)
	if n := tap.WaveformSamplesInto(buf); n != len(buf) {
		t.Fatalf("WaveformSamplesInto() = %d, want %d", n, len(buf))
	}
	if want := []float64{1, 2}; !reflect.DeepEqual(buf, want) {
		t.Fatalf("WaveformSamplesInto() = %v, want %v", buf, want)
	}

	now = now.Add(400 * time.Millisecond)
	if n := tap.WaveformSamplesInto(buf); n != len(buf) {
		t.Fatalf("WaveformSamplesInto() = %d, want %d", n, len(buf))
	}
	if want := []float64{5, 6}; !reflect.DeepEqual(buf, want) {
		t.Fatalf("WaveformSamplesInto() = %v, want %v", buf, want)
	}
}

// TestTapWaveformAdvancesPastLastWriteSize covers the case that made raw
// visualizers stutter: the backend refills its buffer in bursts, so the last
// Stream call can be much shorter than the buffer it filled. Anchoring the read
// position to that call froze the waveform until the next burst — here, both
// reads would return the same frames.
func TestTapWaveformAdvancesPastLastWriteSize(t *testing.T) {
	samples := make([][2]float64, 8)
	for i := range samples {
		samples[i] = [2]float64{float64(i), float64(i)}
	}

	now := time.Unix(1, 0)
	// 10 Hz, an 8-frame backend buffer: 800 ms of audio in flight.
	tap := newTap(&sequenceStreamer{samples: samples}, 16, 10, 8)
	tap.now = func() time.Time { return now }

	// A long refill followed by a short tail, as a bursty backend does.
	if n, _ := tap.Stream(make([][2]float64, 6)); n != 6 {
		t.Fatalf("Stream() wrote %d frames, want 6", n)
	}
	if n, _ := tap.Stream(make([][2]float64, 2)); n != 2 {
		t.Fatalf("Stream() wrote %d frames, want 2", n)
	}

	buf := make([]float64, 2)
	now = now.Add(300 * time.Millisecond)
	if n := tap.WaveformSamplesInto(buf); n != len(buf) {
		t.Fatalf("WaveformSamplesInto() = %d, want %d", n, len(buf))
	}
	if want := []float64{1, 2}; !reflect.DeepEqual(buf, want) {
		t.Fatalf("WaveformSamplesInto() at +300ms = %v, want %v", buf, want)
	}

	// 400 ms later the window must have moved on, even though the last write
	// was only 2 frames long.
	now = now.Add(400 * time.Millisecond)
	if n := tap.WaveformSamplesInto(buf); n != len(buf) {
		t.Fatalf("WaveformSamplesInto() = %d, want %d", n, len(buf))
	}
	if want := []float64{5, 6}; !reflect.DeepEqual(buf, want) {
		t.Fatalf("WaveformSamplesInto() at +700ms = %v, want %v", buf, want)
	}
}

// TestTapWaveformDoesNotReadPastWritten guards the clamp: if the backend stalls,
// the window must stop at the newest written frame instead of walking into
// stale ring-buffer data.
func TestTapWaveformDoesNotReadPastWritten(t *testing.T) {
	samples := make([][2]float64, 8)
	for i := range samples {
		samples[i] = [2]float64{float64(i), float64(i)}
	}

	now := time.Unix(1, 0)
	tap := newTap(&sequenceStreamer{samples: samples}, 16, 10, 8)
	tap.now = func() time.Time { return now }
	if n, _ := tap.Stream(make([][2]float64, 8)); n != 8 {
		t.Fatalf("Stream() wrote %d frames, want 8", n)
	}

	buf := make([]float64, 2)
	now = now.Add(30 * time.Second)
	if n := tap.WaveformSamplesInto(buf); n != len(buf) {
		t.Fatalf("WaveformSamplesInto() = %d, want %d", n, len(buf))
	}
	if want := []float64{6, 7}; !reflect.DeepEqual(buf, want) {
		t.Fatalf("WaveformSamplesInto() after stall = %v, want %v", buf, want)
	}
}
