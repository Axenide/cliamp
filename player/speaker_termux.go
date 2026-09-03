//go:build termux

package player

import (
	"context"
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
	mu         sync.Mutex
	mixer      beep.Mixer
	client     *pulse.Client
	stream     *pulse.PlaybackStream
	sampleRate beep.SampleRate // remembered so Play can lazily recreate after errors
	bufferSize int
	started    atomic.Bool
	errored    atomic.Bool
	frameBuf   [][2]float64 // reused across callbacks; guarded by mu
}

func (t *termuxSpeaker) Init(sampleRate beep.SampleRate, bufferSize int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.initLocked(sampleRate, bufferSize)
}

// initLocked is Init's body. Caller must hold t.mu. Idempotent: a previous
// client/stream is closed first so the speaker can be reinitialized without
// leaking the PulseAudio socket. After it returns successfully, t.stream is
// non-nil and ready to be started by the next Play.
func (t *termuxSpeaker) initLocked(sampleRate beep.SampleRate, bufferSize int) error {
	t.closeLocked()

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
	t.sampleRate = sampleRate
	t.bufferSize = bufferSize
	t.frameBuf = make([][2]float64, bufferSize)
	t.started.Store(false)
	t.errored.Store(false)
	return nil
}

// closeLocked releases the current PulseAudio client and stream. Caller must
// hold t.mu. After it returns, t.stream and t.client are nil and the next
// Play will lazily recreate via initLocked.
func (t *termuxSpeaker) closeLocked() {
	if t.stream != nil {
		t.stream.Close()
		t.stream = nil
	}
	if t.client != nil {
		t.client.Close()
		t.client = nil
	}
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

	// Recovery path: if the previous stream was torn down (Init failure or
	// post-Start error) the next streamer is added straight away, so we
	// re-establish the PulseAudio connection lazily. If the daemon went
	// away after Start succeeded, Closed() reports serverLost; rebuild
	// before the user notices silence.
	if t.stream == nil && t.sampleRate != 0 {
		if err := t.initLocked(t.sampleRate, t.bufferSize); err != nil {
			debugPulseLog("Play: recovery init failed: %v", err)
			t.mu.Unlock()
			return
		}
	} else if t.stream != nil && t.stream.Closed() {
		t.closeLocked()
		t.started.Store(false)
		t.errored.Store(false)
		if err := t.initLocked(t.sampleRate, t.bufferSize); err != nil {
			debugPulseLog("Play: recovery init failed: %v", err)
			t.mu.Unlock()
			return
		}
	}

	t.mixer.Add(s...)
	stream := t.stream
	t.mu.Unlock()

	if stream != nil && t.started.CompareAndSwap(false, true) {
		go t.runStream(stream)
	}
}

// runStream calls Start(), which blocks until the PulseAudio daemon has
// begun requesting samples (the unbuffered "<-started" handoff inside
// PlaybackStream.Start). Operates exclusively on the snapshot passed by
// Play. If Start returns successfully and Error() reports a failure (for
// example the daemon disconnected during startup), the stream and client
// are released so the next Play can recreate them — otherwise started
// would stay set forever and the speaker would silently drop new audio.
func (t *termuxSpeaker) runStream(stream *pulse.PlaybackStream) {
	stream.Start()
	if err := stream.Error(); err != nil {
		debugPulseLog("stream error after Start: %v", err)
		t.errored.Store(true)
		t.mu.Lock()
		if t.stream == stream {
			t.closeLocked()
			t.started.Store(false)
		}
		t.mu.Unlock()
	}
}

// Clear mirrors gopxl/beep/v2/speaker.Clear: it empties the mixer and leaves
// the PulseAudio stream running. The stream does NOT need to be restarted
// after a Clear — the next Play just adds fresh streamers to the mixer and
// the audio callback resumes pulling from it. Critically, Clear must not
// reset started while a runStream goroutine may still be blocked inside
// PlaybackStream.Start; doing so would let a subsequent Play launch a
// second runStream on the same stream and deadlock on the unbuffered
// started notification (jfreymuth/pulse only emits one per stream).
func (t *termuxSpeaker) Clear() {
	t.mu.Lock()
	t.mixer.Clear()
	t.mu.Unlock()
}

func (t *termuxSpeaker) Lock()   { t.mu.Lock() }
func (t *termuxSpeaker) Unlock() { t.mu.Unlock() }

func (t *termuxSpeaker) Suspend() error {
	if t.errored.Load() || !t.started.Load() {
		return nil
	}
	if t.stream == nil {
		return nil
	}
	t.stream.Pause()
	return nil
}

func (t *termuxSpeaker) Resume() error {
	if t.errored.Load() || !t.started.Load() {
		return nil
	}
	if t.stream == nil {
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
		// Cap each sleep to the remaining deadline so we never overshoot.
		remaining := deadline.Sub(nowFunc())
		if remaining <= 0 {
			return ""
		}
		sleepFor := backoff
		if sleepFor > remaining {
			sleepFor = remaining
		}
		sleepFunc(sleepFor)
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

// pulseaudioSpawnTimeout bounds how long trySpawnPulseaudioImpl will wait
// for `pulseaudio --start` to return. Without this, a stalled daemon
// could block Player.New indefinitely. 5s is generous (a healthy
// daemon completes the spawn in well under a second).
const pulseaudioSpawnTimeout = 5 * time.Second

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
	ctx, cancel := context.WithTimeout(context.Background(), pulseaudioSpawnTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--start")
	if err := cmd.Run(); err != nil {
		debugPulseLog("pulseaudio --start failed: %v", err)
		return false
	}
	debugPulseLog("pulseaudio --start succeeded")
	return true
}

func init() { backend = &termuxSpeaker{} }
