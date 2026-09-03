//go:build termux

package player

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopxl/beep/v2"
	"github.com/jfreymuth/pulse"
)

// CLIAMP_DEBUG_PULSE=1 prints diagnostic information about the audio
// backend selection. Useful for diagnosing Termux installs where
// PulseAudio isn't running or the runtime socket is in an unexpected
// location. Disabled by default because the TUI cannot tolerate stray
// stderr writes.
const debugPulseEnv = "CLIAMP_DEBUG_PULSE"

func debugPulseLog(format string, args ...any) {
	if os.Getenv(debugPulseEnv) == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "[cliamp:pulse] "+format+"\n", args...)
}

// termuxSpeaker drives a beep.Mixer through a PulseAudio playback stream
// using jfreymuth/pulse (pure-Go, no CGO, no ALSA). The PulseAudio daemon
// is expected to be running on Termux with a usable sink (e.g. OpenSL_ES).
type termuxSpeaker struct {
	mu       sync.Mutex
	mixer    beep.Mixer
	client   *pulse.Client
	stream   *pulse.PlaybackStream
	started  atomic.Bool
	errored  atomic.Bool
	frameBuf [][2]float64 // reused across callbacks; guarded by mu
}

func (t *termuxSpeaker) Init(sampleRate beep.SampleRate, bufferSize int) error {
	client, err := pulse.NewClient(t.newClientOptions()...)
	if err != nil {
		debugPulseLog("pulse.NewClient failed: %v", err)
		return err
	}
	debugPulseLog("pulse.NewClient succeeded")
	stream, err := client.NewPlayback(
		pulse.Float32Reader(t.fillSamples),
		pulse.PlaybackStereo,
		pulse.PlaybackSampleRate(int(sampleRate)),
		pulse.PlaybackLatency(float64(bufferSize)/float64(sampleRate)),
		pulse.PlaybackMediaName("cliamp"),
	)
	if err != nil {
		client.Close()
		return err
	}
	t.client = client
	t.stream = stream
	t.frameBuf = make([][2]float64, bufferSize)
	return nil
}

func (t *termuxSpeaker) newClientOptions() []pulse.ClientOption {
	opts := []pulse.ClientOption{pulse.ClientApplicationName("cliamp")}
	if server := pulseServerOption(); server != "" {
		opts = append(opts, pulse.ClientServerString(server))
	}
	return opts
}

func (t *termuxSpeaker) Play(s ...beep.Streamer) {
	t.mu.Lock()
	t.mixer.Add(s...)
	t.mu.Unlock()

	if t.started.CompareAndSwap(false, true) {
		go t.runStream()
	}
}

// runStream calls Start() which blocks until the daemon has begun requesting
// samples. There is no server-side close supervision: if PulseAudio dies
// mid-session the user has to restart cliamp.
func (t *termuxSpeaker) runStream() {
	t.stream.Start()
	if err := t.stream.Error(); err != nil {
		t.errored.Store(true)
	}
}

func (t *termuxSpeaker) Clear() {
	t.mu.Lock()
	t.mixer.Clear()
	stream := t.stream
	client := t.client
	t.stream = nil
	t.client = nil
	t.mu.Unlock()

	// SpeakerClear is invoked exactly once per Player lifetime
	// (from Player.Close). Release the PulseAudio resources now so the
	// daemon doesn't keep our sink input registered.
	if stream != nil {
		stream.Close()
	}
	if client != nil {
		client.Close()
	}
}

func (t *termuxSpeaker) Lock()   { t.mu.Lock() }
func (t *termuxSpeaker) Unlock() { t.mu.Unlock() }

func (t *termuxSpeaker) Suspend() error {
	if t.errored.Load() || !t.started.Load() {
		return nil
	}
	t.stream.Pause()
	return nil
}

func (t *termuxSpeaker) Resume() error {
	if t.errored.Load() || !t.started.Load() {
		return nil
	}
	t.stream.Resume()
	return nil
}

// fillSamples is invoked by PulseAudio's internal goroutine when the daemon
// needs more bytes. It pulls stereo frames out of beep.Mixer under our mutex
// and writes them as interleaved float32, matching the Float32Reader format
// registered in Init.
func (t *termuxSpeaker) fillSamples(buf []float32) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	frames := len(buf) / 2
	if cap(t.frameBuf) < frames {
		t.frameBuf = make([][2]float64, frames)
	}
	frameSlice := t.frameBuf[:frames]
	n, _ := t.mixer.Stream(frameSlice)
	for i := 0; i < n; i++ {
		l := frameSlice[i][0]
		r := frameSlice[i][1]
		if l > 1 {
			l = 1
		} else if l < -1 {
			l = -1
		}
		if r > 1 {
			r = 1
		} else if r < -1 {
			r = -1
		}
		buf[i*2+0] = float32(l)
		buf[i*2+1] = float32(r)
	}
	return n * 2, nil
}

// pulseServerOption returns the explicit server string to pass to
// pulse.ClientServerString, or "" when jfreymuth/pulse should use the
// user's PULSE_SERVER or its built-in defaultServerStrings fallback.
//
// Priority order:
//  1. PULSE_SERVER env var (any value) → "" (respect user setting).
//  2. Termux-friendly base dirs → "unix:<absolute path>".
//  3. Nothing → "" (let jfreymuth/pulse fail with a clear error).
func pulseServerOption() string {
	if v, ok := os.LookupEnv("PULSE_SERVER"); ok {
		debugPulseLog("PULSE_SERVER=%q set; deferring to user configuration", v)
		return ""
	}
	if addr := discoverPulseSocket(); addr != "" {
		debugPulseLog("discovered pulse socket: %q", addr)
		return "unix:" + addr
	}
	debugPulseLog("no pulse socket found")
	return ""
}

// Discovery budget. PulseAudio creates its runtime socket asynchronously,
// so cliamp may boot a few milliseconds before the daemon does. The retry
// wrapper polls with exponential backoff up to discoveryTotalDeadline. If
// the socket exists on the first attempt we return immediately — no fixed
// sleep is added.
const (
	discoveryTotalDeadline  = 500 * time.Millisecond
	discoveryInitialBackoff = 25 * time.Millisecond
	discoveryMaxBackoff     = 100 * time.Millisecond
)

// nowFunc and sleepFunc are swapped in tests so retry behavior can be
// exercised without real waits.
var (
	nowFunc   = time.Now
	sleepFunc = time.Sleep
)

// discoverPulseSocket locates the PulseAudio daemon's Unix socket on Termux
// without invoking pactl, hardcoding the random runtime-dir suffix, or
// falling back to TCP. Returns the absolute socket path, or "" if not
// found within the discovery deadline.
//
// The retry is needed because PulseAudio creates its runtime dir and
// socket asynchronously after spawning. On a slow Termux boot we may
// read the directory before the socket appears; the wrapper waits up
// to discoveryTotalDeadline with exponential backoff.
//
// If the socket is still not visible, we try the libpulse "autospawn"
// fallback: invoke `pulseaudio --start` (exactly what libpulse's
// autospawn does) so the daemon creates the runtime dir + socket,
// then retry the discovery once more. We do not extend the per-phase
// deadline or chain additional retries; one spawn attempt is enough.
func discoverPulseSocket() string {
	debugPulseLog("PULSE_SERVER unset; bases=%v", pulseSocketBases())

	if addr := discoverPulseSocketWithProbe(discoverPulseSocketOnce); addr != "" {
		return addr
	}
	if !trySpawnPulseaudio() {
		return ""
	}
	return discoverPulseSocketWithProbe(discoverPulseSocketOnce)
}

// discoverPulseSocketWithProbe retries probe up to discoveryTotalDeadline
// with exponential backoff (capped). Returns the first non-empty result.
// Used directly by tests with a controllable probe.
func discoverPulseSocketWithProbe(probe func() string) string {
	deadline := nowFunc().Add(discoveryTotalDeadline)
	backoff := discoveryInitialBackoff
	for {
		if addr := probe(); addr != "" {
			return addr
		}
		if !nowFunc().Before(deadline) {
			return ""
		}
		sleepFunc(backoff)
		backoff *= 2
		if backoff > discoveryMaxBackoff {
			backoff = discoveryMaxBackoff
		}
	}
}

// discoverPulseSocketOnce makes a single attempt to find the socket,
// without any retry. Returns "" if no socket is currently visible.
//
// Strategy:
//  1. Try the canonical path pulse/native in each candidate base directory.
//  2. Fall back to scanning each base directory for any subdirectory whose
//     name starts with "pulse" and which contains a Unix socket named
//     "native". We use os.ReadDir + os.Stat directly because filepath.Glob
//     is unreliable on some Android/Termux filesystems.
func discoverPulseSocketOnce() string {
	for _, base := range pulseSocketBases() {
		if path := filepath.Join(base, "pulse", "native"); isUnixSocket(path) {
			return path
		}
	}
	for _, base := range pulseSocketBases() {
		if path := findPulseSocketInDir(base); path != "" {
			return path
		}
	}
	return ""
}

// findPulseSocketInDir scans base for a Unix socket at <subdir>/native
// where subdir's name starts with "pulse".
func findPulseSocketInDir(base string) string {
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "pulse") {
			continue
		}
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(base, name, "native")
		info, statErr := os.Stat(candidate)
		if statErr != nil {
			continue
		}
		if info.Mode()&os.ModeSocket != 0 {
			return candidate
		}
	}
	return ""
}

// pulseSocketBases returns the directories where the PulseAudio runtime
// socket might live, in priority order. De-duplicated.
func pulseSocketBases() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}

	// Standard freedesktop spec.
	add(os.Getenv("XDG_RUNTIME_DIR"))

	// POSIX fallback. Termux sets $TMPDIR but not $XDG_RUNTIME_DIR, so
	// pulseaudio uses $TMPDIR for its runtime dir.
	add(os.Getenv("TMPDIR"))

	// Termux-only fallback: $PREFIX/tmp. The PREFIX env var contains
	// "com.termux" on real Termux installs; we restrict the fallback to
	// that case so unrelated Linux installs aren't pointlessly probed.
	if prefix := os.Getenv("PREFIX"); strings.Contains(prefix, "com.termux") {
		add(filepath.Join(prefix, "tmp"))
	}

	return out
}

func isUnixSocket(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSocket != 0
}

// spawnPulseaudioFn is the autospawn trigger. Production calls
// trySpawnPulseaudioImpl; tests can swap it for a deterministic stub.
var spawnPulseaudioFn = trySpawnPulseaudioImpl

// trySpawnPulseaudio invokes `pulseaudio --start`, the same way libpulse's
// autospawn does (see pulseaudio/src/pulse/context.c context_autospawn).
// The parent returns 0 once the daemon has forked and bound its socket.
// We do NOT pass --exit-idle-time=-1 (libpulse doesn't either); the
// daemon persists according to PulseAudio's standard lifecycle rules.
func trySpawnPulseaudio() bool {
	return spawnPulseaudioFn()
}

func trySpawnPulseaudioImpl() bool {
	bin, err := exec.LookPath("pulseaudio")
	if err != nil {
		debugPulseLog("pulseaudio not in PATH: %v", err)
		return false
	}
	cmd := exec.Command(bin, "--start")
	if err := cmd.Run(); err != nil {
		debugPulseLog("pulseaudio --start failed: %v", err)
		return false
	}
	debugPulseLog("pulseaudio --start succeeded")
	return true
}

func init() { backend = &termuxSpeaker{} }
