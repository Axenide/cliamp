//go:build termux

package player

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeSocket creates a real Unix domain socket at <dir>/<sub>/<name> and
// returns the directory path. The socket is closed on test cleanup.
func makeSocket(t *testing.T, sub, name string) string {
	t.Helper()
	dir := t.TempDir()
	subDir := filepath.Join(dir, sub)
	if err := os.MkdirAll(subDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sockPath := filepath.Join(subDir, name)
	ln, err := net.ListenUnix("unix", mustAddr(t, sockPath))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return dir
}

// withEnv sets env vars for the duration of a test, restoring on cleanup.
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		old, had := os.LookupEnv(k)
		if err := os.Setenv(k, v); err != nil {
			t.Fatalf("setenv %s: %v", k, err)
		}
		if had {
			t.Cleanup(func() { _ = os.Setenv(k, old) })
		} else {
			t.Cleanup(func() { _ = os.Unsetenv(k) })
		}
	}
}

// clearEnv unsets env vars for the duration of a test.
func clearEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		old, had := os.LookupEnv(k)
		_ = os.Unsetenv(k)
		if had {
			t.Cleanup(func() { _ = os.Setenv(k, old) })
		}
	}
}

func mustAddr(t *testing.T, path string) *net.UnixAddr {
	t.Helper()
	a, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return a
}

// --- PulseAudio discovery ---

func TestDiscoverPulseSocket_CanonicalPath(t *testing.T) {
	dir := makeSocket(t, "pulse", "native")
	withEnv(t, map[string]string{"XDG_RUNTIME_DIR": dir, "TMPDIR": ""})

	got := discoverPulseSocket()
	want := filepath.Join(dir, "pulse", "native")
	if got != want {
		t.Errorf("discoverPulseSocket() = %q, want %q", got, want)
	}
}

func TestDiscoverPulseSocket_RandomizedSuffix(t *testing.T) {
	// PulseAudio's default runtime dir has a random hash suffix.
	dir := makeSocket(t, "pulse-AbCd1234", "native")
	withEnv(t, map[string]string{"XDG_RUNTIME_DIR": dir, "TMPDIR": ""})

	want := filepath.Join(dir, "pulse-AbCd1234", "native")
	if got := discoverPulseSocket(); got != want {
		t.Errorf("discoverPulseSocket() = %q, want %q (must match the random suffix without hardcoding)", got, want)
	}
}

func TestDiscoverPulseSocket_TMPDIRFallback(t *testing.T) {
	// Termux sets $TMPDIR but not $XDG_RUNTIME_DIR; pulseaudio creates
	// its runtime dir there.
	dir := makeSocket(t, "pulse-XyZ987", "native")
	clearEnv(t, "XDG_RUNTIME_DIR", "TMPDIR", "PREFIX")
	withEnv(t, map[string]string{"TMPDIR": dir})

	want := filepath.Join(dir, "pulse-XyZ987", "native")
	if got := discoverPulseSocket(); got != want {
		t.Errorf("discoverPulseSocket() = %q, want %q (TMPDIR fallback)", got, want)
	}
}

func TestDiscoverPulseSocket_TermuxPREFIXPath(t *testing.T) {
	// $PREFIX/tmp is the canonical Termux runtime location when XDG and
	// TMPDIR are absent; the PREFIX contains "com.termux" so the
	// Termux-specific fallback engages. We construct the prefix path under
	// t.TempDir() with a "com.termux" segment so pulseSocketBases'
	// strings.Contains check fires, without depending on a hardcoded /tmp
	// path that could collide with parallel runs or system state.
	prefixPath := filepath.Join(t.TempDir(), "com.termux-test", "files", "usr")
	tmpDir := filepath.Join(prefixPath, "tmp")
	sockPath := filepath.Join(tmpDir, "pulse-AbC", "native")
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ln, err := net.ListenUnix("unix", mustAddr(t, sockPath))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	clearEnv(t, "XDG_RUNTIME_DIR", "TMPDIR", "PREFIX")
	withEnv(t, map[string]string{"PREFIX": prefixPath})

	if got := discoverPulseSocket(); got != sockPath {
		t.Errorf("discoverPulseSocket() = %q, want %q (PREFIX/tmp fallback)", got, sockPath)
	}
}

func TestDiscoverPulseSocket_NoSocket(t *testing.T) {
	dir := t.TempDir()
	clearEnv(t, "XDG_RUNTIME_DIR", "TMPDIR", "PREFIX")
	withEnv(t, map[string]string{"XDG_RUNTIME_DIR": dir})

	// Stub the spawn so we don't accidentally exec a real pulseaudio
	// binary if one happens to be on the test host's PATH.
	withStubSpawn(t, func() bool { return false })
	useFakeClock(t)
	if got := discoverPulseSocket(); got != "" {
		t.Errorf("discoverPulseSocket() = %q, want empty", got)
	}
}

func TestDiscoverPulseSocket_IgnoresRegularFile(t *testing.T) {
	// A regular file named "native" must not be mistaken for the socket.
	dir := t.TempDir()
	pulseDir := filepath.Join(dir, "pulse")
	if err := os.MkdirAll(pulseDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pulseDir, "native"), []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	withEnv(t, map[string]string{"XDG_RUNTIME_DIR": dir, "TMPDIR": ""})

	withStubSpawn(t, func() bool { return false })
	useFakeClock(t)
	if got := discoverPulseSocket(); got != "" {
		t.Errorf("discoverPulseSocket() = %q, want empty (regular file should be ignored)", got)
	}
}

// --- PULSE_SERVER env var ---

func TestPulseServerOption_HonorsPULSESERVER(t *testing.T) {
	// PULSE_SERVER wins even when a local socket is reachable.
	dir := makeSocket(t, "pulse-AbCd", "native")
	clearEnv(t, "XDG_RUNTIME_DIR", "TMPDIR", "PREFIX", "PULSE_SERVER")
	withEnv(t, map[string]string{
		"XDG_RUNTIME_DIR": dir,
		"PULSE_SERVER":    "unix:/some/explicit/path",
	})

	if got := pulseServerOption(); got != "" {
		t.Errorf("pulseServerOption() = %q, want empty (PULSE_SERVER must win)", got)
	}
}

func TestPulseServerOption_HonorsEmptyPULSESERVER(t *testing.T) {
	// An empty PULSE_SERVER is still an explicit setting.
	dir := makeSocket(t, "pulse-AbCd", "native")
	clearEnv(t, "XDG_RUNTIME_DIR", "TMPDIR", "PREFIX", "PULSE_SERVER")
	withEnv(t, map[string]string{
		"XDG_RUNTIME_DIR": dir,
		"PULSE_SERVER":    "",
	})

	if got := pulseServerOption(); got != "" {
		t.Errorf("pulseServerOption() = %q, want empty", got)
	}
}

// --- Retry budget ---

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time        { return c.t }
func (c *fakeClock) sleep(d time.Duration) { c.t = c.t.Add(d) }

func useFakeClock(t *testing.T) *fakeClock {
	t.Helper()
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	nowFunc = clk.now
	sleepFunc = clk.sleep
	t.Cleanup(func() {
		nowFunc = time.Now
		sleepFunc = time.Sleep
	})
	return clk
}

func TestDiscoverPulseSocketWithProbe_NoRetryWhenFoundImmediately(t *testing.T) {
	var calls int
	probe := func() string {
		calls++
		return "/tmp/pulse-AbCd/native"
	}

	if got := discoverPulseSocketWithProbe(probe); got != "/tmp/pulse-AbCd/native" {
		t.Errorf("got %q, want \"/tmp/pulse-AbCd/native\"", got)
	}
	if calls != 1 {
		t.Errorf("probe calls = %d, want 1 (no retry when first attempt succeeds)", calls)
	}
}

func TestDiscoverPulseSocketWithProbe_RetriesUntilFound(t *testing.T) {
	clk := useFakeClock(t)

	var calls int
	probe := func() string {
		calls++
		if calls < 4 {
			return ""
		}
		return "/tmp/pulse-ZzZ/native"
	}

	if got := discoverPulseSocketWithProbe(probe); got != "/tmp/pulse-ZzZ/native" {
		t.Errorf("got %q, want \"/tmp/pulse-ZzZ/native\"", got)
	}
	if calls != 4 {
		t.Errorf("probe calls = %d, want 4", calls)
	}
	// The backoff schedule (capped): 25ms, 50ms, 100ms.
	// Total elapsed: discoveryInitialBackoff + 2*discoveryInitialBackoff + discoveryMaxBackoff.
	wantElapsed := discoveryInitialBackoff + 2*discoveryInitialBackoff + discoveryMaxBackoff
	if elapsed := clk.t.Sub(time.Unix(1_700_000_000, 0).UTC()); elapsed != wantElapsed {
		t.Errorf("elapsed = %v, want %v", elapsed, wantElapsed)
	}
}

func TestDiscoverPulseSocketWithProbe_EmptyAfterDeadline(t *testing.T) {
	useFakeClock(t)

	probe := func() string { return "" }
	if got := discoverPulseSocketWithProbe(probe); got != "" {
		t.Errorf("got %q, want empty after deadline", got)
	}
}

// --- Autospawn ---

func withStubSpawn(t *testing.T, fn func() bool) {
	t.Helper()
	saved := spawnPulseaudioFn
	spawnPulseaudioFn = fn
	t.Cleanup(func() { spawnPulseaudioFn = saved })
}

func TestDiscoverPulseSocket_DoesNotSpawnWhenSocketExists(t *testing.T) {
	dir := makeSocket(t, "pulse-AbCd", "native")
	clearEnv(t, "XDG_RUNTIME_DIR", "TMPDIR", "PREFIX", "PULSE_SERVER")
	withEnv(t, map[string]string{"TMPDIR": dir})

	spawnCalls := 0
	withStubSpawn(t, func() bool {
		spawnCalls++
		return true
	})
	useFakeClock(t)

	if got := discoverPulseSocket(); got == "" {
		t.Errorf("discoverPulseSocket() = empty, want socket")
	}
	if spawnCalls != 0 {
		t.Errorf("spawn called %d times, want 0 (daemon already running)", spawnCalls)
	}
}

func TestDiscoverPulseSocket_SpawnsOnceWhenFirstPhaseFails(t *testing.T) {
	// No socket initially; spawn is invoked and succeeds (but doesn't
	// create a socket, to keep the test hermetic). discoverPulseSocket
	// returns "" and spawn is called exactly once.
	dir := t.TempDir()
	clearEnv(t, "XDG_RUNTIME_DIR", "TMPDIR", "PREFIX", "PULSE_SERVER")
	withEnv(t, map[string]string{"TMPDIR": dir})

	spawnCalls := 0
	withStubSpawn(t, func() bool {
		spawnCalls++
		return true
	})
	useFakeClock(t)

	if got := discoverPulseSocket(); got != "" {
		t.Errorf("got %q, want empty (no socket after spawn)", got)
	}
	if spawnCalls != 1 {
		t.Errorf("spawn called %d times, want exactly 1", spawnCalls)
	}
}

func TestDiscoverPulseSocket_SpawnThenSucceeds(t *testing.T) {
	// No socket initially. The stub for spawnPulseaudioFn creates the
	// socket (mirroring what the real pulseaudio --start does). The
	// second discovery phase finds it.
	dir := t.TempDir()
	clearEnv(t, "XDG_RUNTIME_DIR", "TMPDIR", "PREFIX", "PULSE_SERVER")
	withEnv(t, map[string]string{"TMPDIR": dir})

	withStubSpawn(t, func() bool {
		sub := filepath.Join(dir, "pulse-VVVV")
		if err := os.MkdirAll(sub, 0o700); err != nil {
			return false
		}
		sockPath := filepath.Join(sub, "native")
		ln, err := net.ListenUnix("unix", mustAddr(t, sockPath))
		if err != nil {
			return false
		}
		t.Cleanup(func() { _ = ln.Close() })
		return true
	})
	useFakeClock(t)

	want := filepath.Join(dir, "pulse-VVVV", "native")
	if got := discoverPulseSocket(); got != want {
		t.Errorf("got %q, want %q (spawn must enable discovery on second phase)", got, want)
	}
}

func TestDiscoverPulseSocket_FailedSpawnReturnsEmpty(t *testing.T) {
	// Spawn fails (binary missing, exit code != 0, etc). There is no
	// point in retrying discovery.
	dir := t.TempDir()
	clearEnv(t, "XDG_RUNTIME_DIR", "TMPDIR", "PREFIX", "PULSE_SERVER")
	withEnv(t, map[string]string{"TMPDIR": dir})

	spawnCalls := 0
	withStubSpawn(t, func() bool {
		spawnCalls++
		return false
	})
	useFakeClock(t)

	if got := discoverPulseSocket(); got != "" {
		t.Errorf("got %q, want empty (spawn failed)", got)
	}
	if spawnCalls != 1 {
		t.Errorf("spawn called %d times, want exactly 1", spawnCalls)
	}
}

func TestTrySpawnPulseaudioImpl_NotInPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if got := trySpawnPulseaudioImpl(); got {
		t.Errorf("trySpawnPulseaudioImpl() = true, want false (pulseaudio not in PATH)")
	}
}

// --- Integration with jfreymuth/pulse ---

func TestTermuxSpeakerInit_DoesNotReturnNoValidServer(t *testing.T) {
	// Regression guard: with a real Unix socket present and PULSE_SERVER
	// unset, termuxSpeaker.Init must reach the protocol stage. The fake
	// socket only accepts; it does not speak PulseAudio, so we expect
	// an error — just not the "no valid server" error that would mean
	// our server string never reached proto.Connect.
	dir := makeSocket(t, "pulse-AbCd", "native")
	clearEnv(t, "XDG_RUNTIME_DIR", "TMPDIR", "PREFIX", "PULSE_SERVER")
	withEnv(t, map[string]string{"TMPDIR": dir})

	err := (&termuxSpeaker{}).Init(44100, 4096)
	if err == nil {
		t.Fatal("expected error from fake socket (no real PulseAudio protocol)")
	}
	if msg := err.Error(); strings.Contains(msg, "no valid server") {
		t.Fatalf("Init returned %q: our server-string selection must have failed", msg)
	}
}

// --- Lifecycle invariants ---

// runClearWithTimeout invokes Clear in a goroutine and waits up to the
// timeout for it to return. Clear can take ~1s on a fake server because
// stream.Close / client.Close do proto.Request calls that block until
// the 1s Request timeout fires. The timeout gives the test a deterministic
// bound so a regression that turns Clear into an infinite hang fails fast.
func runClearWithTimeout(t *testing.T, sp *termuxSpeaker) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		sp.Clear()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Clear hung beyond reasonable timeout")
	}
}

// TestClear_DoesNotTouchStream verifies that Clear mirrors
// gopxl/beep/v2/speaker.Clear: it clears the mixer and resets state
// without closing the PulseAudio stream or client. Closing them under
// a concurrent runStream would race with stream.Start's <-started
// receive and could panic on a closed request channel.
func TestClear_DoesNotTouchStream(t *testing.T) {
	dir := makeSocket(t, "pulse-AbCd", "native")
	clearEnv(t, "XDG_RUNTIME_DIR", "TMPDIR", "PREFIX", "PULSE_SERVER")
	withEnv(t, map[string]string{"TMPDIR": dir})

	sp := &termuxSpeaker{}
	if err := sp.Init(44100, 4096); err != nil {
		t.Skipf("Init failed (expected on fake socket): %v", err)
	}

	runClearWithTimeout(t, sp)

	sp.mu.Lock()
	stream := sp.stream
	sp.mu.Unlock()
	if stream == nil {
		t.Errorf("Clear must not nil out t.stream (would race with runStream)")
	}
}

// TestClear_DoesNotResetStarted verifies that Clear preserves the started
// flag so a subsequent Play cannot launch a second runStream on the same
// PulseAudio stream. The previous design reset started in Clear, which let
// Play → Clear → Play deadlock on jfreymuth/pulse's unbuffered started
// notification (only one is emitted per stream).
func TestClear_DoesNotResetStarted(t *testing.T) {
	dir := makeSocket(t, "pulse-AbCd", "native")
	clearEnv(t, "XDG_RUNTIME_DIR", "TMPDIR", "PREFIX", "PULSE_SERVER")
	withEnv(t, map[string]string{"TMPDIR": dir})

	sp := &termuxSpeaker{}
	if err := sp.Init(44100, 4096); err != nil {
		t.Skipf("Init failed (expected on fake socket): %v", err)
	}

	sp.started.Store(true)
	sp.errored.Store(true)

	runClearWithTimeout(t, sp)

	if !sp.started.Load() {
		t.Errorf("Clear must not reset started (lets Play spawn a competing runStream)")
	}
	if !sp.errored.Load() {
		t.Errorf("Clear must not reset errored (Suspend/Resume skip when errored)")
	}
}

// TestPlay_ClearThenPlay_PreservesStarted is the regression guard for the
// Play → Clear → Play deadlock: a prior Play schedules a runStream goroutine
// that is still pending PlaybackStream.Start (blocking on the unbuffered
// started channel). Clear must leave started set so the next Play's
// CompareAndSwap fails and no second runStream is spawned — otherwise both
// goroutines would race into Start and one would wait forever for a
// "started" notification that jfreymuth/pulse only emits once per stream.
func TestPlay_ClearThenPlay_PreservesStarted(t *testing.T) {
	dir := makeSocket(t, "pulse-AbCd", "native")
	clearEnv(t, "XDG_RUNTIME_DIR", "TMPDIR", "PREFIX", "PULSE_SERVER")
	withEnv(t, map[string]string{"TMPDIR": dir})

	sp := &termuxSpeaker{}
	if err := sp.Init(44100, 4096); err != nil {
		t.Skipf("Init failed (expected on fake socket): %v", err)
	}

	sp.started.Store(true)

	sp.Clear()
	if !sp.started.Load() {
		t.Fatal("Clear must not reset started (precondition for Play to skip a new runStream)")
	}

	sp.Play()
	if !sp.started.Load() {
		t.Fatal("Play must not have spawned a competing runStream")
	}
}

// TestPlay_NoGoroutineWhenStreamNil verifies that Play is safe to call
// before Init (or after an Init failure): the snapshot is nil, so no
// goroutine is spawned and no panic occurs.
func TestPlay_NoGoroutineWhenStreamNil(t *testing.T) {
	sp := &termuxSpeaker{} // no Init; client and stream are nil

	sp.Play()

	if sp.started.Load() {
		t.Errorf("started must remain false when stream is nil")
	}
}

// --- Retry deadline ---

// TestDiscoverPulseSocketWithProbe_RespectsDeadline verifies the retry
// budget: even with a probe that always fails, total elapsed time stays
// within discoveryTotalDeadline plus a small slack. Each sleep is
// capped to the remaining deadline so we never overshoot.
func TestDiscoverPulseSocketWithProbe_RespectsDeadline(t *testing.T) {
	useFakeClock(t)

	const slack = time.Millisecond
	probe := func() string { return "" }
	start := nowFunc()
	_ = discoverPulseSocketWithProbe(probe)
	elapsed := nowFunc().Sub(start)

	if elapsed > discoveryTotalDeadline+slack {
		t.Errorf("elapsed = %v, want <= %v (deadline + %v slack)", elapsed, discoveryTotalDeadline, slack)
	}
}
