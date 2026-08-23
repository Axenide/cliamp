package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bjarneo/cliamp/applog"
	"github.com/bjarneo/cliamp/internal/playback"
)

// Dispatcher is how the server sends commands to the TUI.
// In main.go, this is wired to prog.Send().
type Dispatcher interface {
	Send(msg any)
}

const ipcRequestReadTimeout = 60 * time.Second

// Server listens on a Unix socket and dispatches IPC commands.
type Server struct {
	listener    net.Listener
	sockPath    string
	disp        Dispatcher
	plugins     PluginDispatcher
	broker      *Broker
	brokerOwned bool

	v2Mu       sync.RWMutex
	v2         V2Dispatcher
	operations *OperationRegistry
	jobs       *JobStore
	context    context.Context
	cancel     context.CancelFunc

	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error

	connMu sync.Mutex
	conns  map[net.Conn]struct{} // live connections, closed on shutdown
}

// addConn registers a live connection. It returns false if the server is
// already shutting down, in which case the caller must close the connection
// and return. The done check shares connMu with closeConns so a connection
// accepted during shutdown is always closed by exactly one of them.
func (s *Server) addConn(c net.Conn) bool {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	select {
	case <-s.done:
		return false
	default:
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Server) removeConn(c net.Conn) {
	s.connMu.Lock()
	delete(s.conns, c)
	s.connMu.Unlock()
}

func (s *Server) closeConns() {
	s.connMu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.connMu.Unlock()
}

// SetPluginDispatcher wires in the Lua plugin manager after the server starts.
// Plugin dispatch is optional — without it, plugin subcommands return an error.
func (s *Server) SetPluginDispatcher(p PluginDispatcher) {
	s.plugins = p
}

// SetV2Dispatcher wires the runtime owner for version 2 requests. V2 remains
// available for capability discovery and subscriptions when it is nil.
func (s *Server) SetV2Dispatcher(dispatcher V2Dispatcher) {
	s.v2Mu.Lock()
	s.v2 = dispatcher
	s.v2Mu.Unlock()
}

// SetOperationRegistry replaces the advertised V2 capability set. It should
// be called during runtime setup before accepting client traffic.
func (s *Server) SetOperationRegistry(registry *OperationRegistry) {
	s.v2Mu.Lock()
	s.operations = registry
	s.v2Mu.Unlock()
}

// JobStore returns the server's in-memory V2 job store.
func (s *Server) JobStore() *JobStore {
	return s.jobs
}

// Broker returns the server event broker. Callers may publish runtime events
// but must not close a broker they do not own.
func (s *Server) Broker() *Broker {
	return s.broker
}

// Done closes when the server begins shutdown.
func (s *Server) Done() <-chan struct{} {
	return s.done
}

// NewServer creates and starts the IPC server with a new event broker.
func NewServer(sockPath string, disp Dispatcher) (*Server, error) {
	return newServer(sockPath, disp, NewBroker(), true)
}

// NewServerWithBroker creates and starts the IPC server using broker. The
// caller retains ownership of a supplied broker and it is not closed by Server.
func NewServerWithBroker(sockPath string, disp Dispatcher, broker *Broker) (*Server, error) {
	owned := false
	if broker == nil {
		broker = NewBroker()
		owned = true
	}
	return newServer(sockPath, disp, broker, owned)
}

func newServer(sockPath string, disp Dispatcher, broker *Broker, brokerOwned bool) (*Server, error) {
	if err := cleanStaleSocket(sockPath); err != nil {
		return nil, err
	}

	// Ensure the parent directory exists.
	if err := os.MkdirAll(filepath.Dir(sockPath), 0700); err != nil {
		return nil, fmt.Errorf("ipc: mkdir: %w", err)
	}

	ln, err := listenSocket(sockPath)
	if err != nil {
		return nil, fmt.Errorf("ipc: listen: %w", err)
	}

	// Restrict socket permissions to owner only.
	if err := os.Chmod(sockPath, 0600); err != nil {
		ln.Close()
		os.Remove(sockPath)
		return nil, fmt.Errorf("ipc: chmod: %w", err)
	}

	// Write PID file.
	pidPath := sockPath + ".pid"
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		ln.Close()
		os.Remove(sockPath)
		return nil, fmt.Errorf("ipc: write pid: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		listener:    ln,
		sockPath:    sockPath,
		disp:        disp,
		broker:      broker,
		brokerOwned: brokerOwned,
		operations:  DefaultOperationRegistry(),
		jobs:        NewJobStore(),
		context:     ctx,
		cancel:      cancel,
		done:        make(chan struct{}),
		conns:       make(map[net.Conn]struct{}),
	}

	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// Close shuts down the server, removes socket and PID file.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.jobs != nil {
			s.jobs.CancelAll()
		}
		if s.done != nil {
			close(s.done)
		}
		if s.listener != nil {
			s.closeErr = s.listener.Close()
		}
		// Close in-flight connections so their handleConn read loops unblock
		// immediately rather than waiting out the per-request read deadline.
		s.closeConns()
		s.wg.Wait()
		if s.brokerOwned && s.broker != nil {
			s.broker.Close()
		}
		if s.sockPath != "" {
			_ = os.Remove(s.sockPath)
			_ = os.Remove(s.sockPath + ".pid")
		}
	})
	return s.closeErr
}

// acceptLoop accepts incoming connections until the server is closed.
func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
			}
			// A closed listener is permanent — stop instead of spinning.
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Other errors may be transient (e.g. EMFILE); log and back off
			// rather than silently retrying.
			applog.Warn("ipc: accept: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// handleConn reads newline-delimited JSON requests from a single connection,
// dispatches them, and writes JSON responses.
func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	if !s.addConn(conn) {
		return // server shutting down
	}
	defer s.removeConn(conn)

	scanner := newFrameScanner(conn)

	for {
		// Per-request deadline so long-lived streaming clients (e.g. vis bands
		// polling) aren't killed at a fixed wall clock, but idle clients still
		// time out.
		conn.SetReadDeadline(time.Now().Add(ipcRequestReadTimeout))
		if !scanner.Scan() {
			return
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		version, id, versioned, err := parseProtocolVersion(line)
		if err != nil {
			writeResponse(conn, Response{OK: false, Error: "invalid JSON: " + err.Error()})
			continue
		}
		if !versioned {
			var req Request
			if err := json.Unmarshal(line, &req); err != nil {
				writeResponse(conn, Response{OK: false, Error: "invalid JSON: " + err.Error()})
				continue
			}

			if strings.EqualFold(req.Cmd, "subscribe") {
				s.streamSubscription(conn, req.Topics)
				return
			}

			resp := s.dispatch(req)
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			writeResponse(conn, resp)
			continue
		}

		if version != protocolVersion2 {
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_ = writeJSONLine(conn, V2Response{
				ID:    id,
				OK:    false,
				Error: v2Error(V2ErrorCodeInvalidVersion, V2MessageInvalidVersion),
			})
			continue
		}

		var req V2Request
		if err := json.Unmarshal(line, &req); err != nil {
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_ = writeJSONLine(conn, V2Response{ID: id, OK: false, Error: invalidV2Request()})
			continue
		}
		if isV2Subscribe(req) {
			s.streamV2Subscription(conn, req)
			return
		}

		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = writeJSONLine(conn, s.dispatchV2(req))
	}
}

func (s *Server) streamSubscription(conn net.Conn, topics []string) {
	s.streamSubscriptionWithAck(conn, topics, Response{OK: true})
}

func (s *Server) streamV2Subscription(conn net.Conn, req V2Request) {
	s.streamSubscriptionWithAck(conn, req.Topics, V2Response{ID: req.ID, OK: true})
}

func (s *Server) streamSubscriptionWithAck(conn net.Conn, topics []string, acknowledgement any) {
	// handleConn sets a per-request read deadline. Subscriptions are idle,
	// server-to-client streams after the initial request, so they must not
	// inherit that deadline or they will be closed every request timeout.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		s.writeSubscriptionError(conn, acknowledgement, err)
		return
	}

	if s.broker == nil {
		s.writeSubscriptionError(conn, acknowledgement, errors.New("event broker is unavailable"))
		return
	}
	sub, err := s.broker.Subscribe(topics)
	if err != nil {
		s.writeSubscriptionError(conn, acknowledgement, err)
		return
	}
	defer sub.Close()
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if !writeJSONLine(conn, acknowledgement) {
		return
	}

	// A subscription is server-to-client after its acknowledgment. Keep a read
	// pending solely to detect client disconnects even when no events publish.
	peerClosed := make(chan struct{})
	go func() {
		var one [1]byte
		_, _ = conn.Read(one[:])
		close(peerClosed)
	}()

	for {
		select {
		case <-s.done:
			return
		case <-peerClosed:
			return
		case event, ok := <-sub.Events():
			if !ok {
				return
			}
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if !writeJSONLine(conn, event) {
				return
			}
		}
	}
}

func (s *Server) writeSubscriptionError(conn net.Conn, acknowledgement any, err error) {
	switch response := acknowledgement.(type) {
	case V2Response:
		response.OK = false
		response.Error = invalidV2Params()
		_ = writeJSONLine(conn, response)
	default:
		_ = writeJSONLine(conn, Response{OK: false, Error: err.Error()})
	}
}

// parseProtocolVersion distinguishes exactly unversioned V1 requests from
// versioned envelopes. Any present version field, including null, is V2-shaped
// and therefore receives a structured V2 invalid_version response when invalid.
func parseProtocolVersion(line []byte) (version int, id json.RawMessage, versioned bool, err error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(line, &envelope); err != nil {
		return 0, nil, false, err
	}
	versionRaw, ok := envelope["version"]
	if !ok {
		return 0, nil, false, nil
	}
	returnVersioned := true
	id = cloneRawMessage(envelope["id"])
	if err := json.Unmarshal(versionRaw, &version); err != nil {
		return 0, id, returnVersioned, nil
	}
	return version, id, returnVersioned, nil
}

func isV2Subscribe(req V2Request) bool {
	return strings.EqualFold(req.Method, "subscribe") || strings.EqualFold(req.Operation, "subscribe")
}

func (s *Server) dispatchV2(req V2Request) V2Response {
	response := V2Response{ID: cloneRawMessage(req.ID)}
	method := strings.ToLower(strings.TrimSpace(req.Method))
	s.v2Mu.RLock()
	operations := s.operations
	s.v2Mu.RUnlock()
	operation := strings.TrimSpace(req.Operation)
	if operation == "" && operations != nil {
		if _, ok := operations.Lookup(req.Method); ok {
			operation = req.Method
		}
	}

	switch method {
	case "capabilities":
		if operation != "" {
			response.Error = invalidV2Request()
			return response
		}
		return s.v2Capabilities(response)
	case "job.get":
		return s.v2GetJob(response, req.JobID)
	case "job.cancel":
		return s.v2CancelJob(response, req.JobID)
	case "state.get", "spectrum.get":
		if operation != "" {
			response.Error = invalidV2Request()
			return response
		}
		return s.dispatchV2ToOwner(response, req)
	}
	if operation == "capabilities" {
		return s.v2Capabilities(response)
	}
	if operation == "" {
		response.Error = invalidV2Request()
		return response
	}
	if operations == nil {
		response.Error = v2Error(V2ErrorCodeUnavailable, V2MessageUnavailable)
		return response
	}
	if err := operations.Validate(operation, req.Params); err != nil {
		response.Error = err
		return response
	}
	// A method alias is normalized at the server boundary so runtime owners only
	// need to dispatch the canonical operation name.
	req.Operation = operation

	return s.dispatchV2ToOwner(response, req)
}

func (s *Server) dispatchV2ToOwner(response V2Response, req V2Request) V2Response {
	s.v2Mu.RLock()
	dispatcher := s.v2
	s.v2Mu.RUnlock()
	if dispatcher == nil {
		response.Error = v2Error(V2ErrorCodeUnavailable, V2MessageUnavailable)
		return response
	}
	ctx := s.context
	if ctx == nil {
		ctx = context.Background()
	}
	result, err := dispatcher.DispatchV2(ctx, req)
	if err != nil {
		response.Error = cloneV2Error(err)
		if response.Error.Code == "" || response.Error.Message == "" {
			response.Error = v2Error(V2ErrorCodeInternal, V2MessageInternal)
		}
		return response
	}
	if err := validV2Result(result.Result); err != nil {
		response.Error = v2ErrorFromError(err)
		return response
	}
	response.OK = true
	response.Result = cloneRawMessage(result.Result)
	response.Snapshot = cloneSnapshot(result.Snapshot)
	if result.Job != nil {
		job := cloneJob(*result.Job)
		response.Job = &job
	}
	return response
}

func (s *Server) v2Capabilities(response V2Response) V2Response {
	s.v2Mu.RLock()
	operations := s.operations
	s.v2Mu.RUnlock()
	if operations == nil {
		response.Error = v2Error(V2ErrorCodeUnavailable, V2MessageUnavailable)
		return response
	}
	result, err := json.Marshal(operations.Operations())
	if err != nil {
		response.Error = v2Error(V2ErrorCodeInternal, V2MessageInternal)
		return response
	}
	response.OK = true
	response.Result = result
	return response
}

func (s *Server) v2GetJob(response V2Response, jobID string) V2Response {
	if jobID == "" {
		response.Error = invalidV2Params()
		return response
	}
	if s.jobs == nil {
		response.Error = v2Error(V2ErrorCodeUnavailable, V2MessageUnavailable)
		return response
	}
	job, ok := s.jobs.Get(jobID)
	if !ok {
		response.Error = v2Error(V2ErrorCodeNotFound, V2MessageNotFound)
		return response
	}
	response.OK = true
	response.Job = &job
	return response
}

func (s *Server) v2CancelJob(response V2Response, jobID string) V2Response {
	if jobID == "" {
		response.Error = invalidV2Params()
		return response
	}
	if s.jobs == nil {
		response.Error = v2Error(V2ErrorCodeUnavailable, V2MessageUnavailable)
		return response
	}
	if err := s.jobs.Cancel(jobID); err != nil {
		switch {
		case errors.Is(err, ErrJobNotFound):
			response.Error = v2Error(V2ErrorCodeNotFound, V2MessageNotFound)
		case errors.Is(err, ErrInvalidJobState):
			response.Error = v2Error(V2ErrorCodeConflict, V2MessageConflict)
		default:
			response.Error = v2Error(V2ErrorCodeInternal, V2MessageInternal)
		}
		return response
	}
	job, _ := s.jobs.Get(jobID)
	response.OK = true
	response.Job = &job
	return response
}

// dispatch handles a single parsed request.
func (s *Server) dispatch(req Request) Response {
	switch strings.ToLower(req.Cmd) {
	case "play":
		s.disp.Send(PlayMsg{})
		return Response{OK: true}

	case "pause":
		s.disp.Send(PauseMsg{})
		return Response{OK: true}

	case "toggle":
		s.disp.Send(playback.PlayPauseMsg{})
		return Response{OK: true}

	case "stop":
		s.disp.Send(playback.StopMsg{})
		return Response{OK: true}

	case "next":
		s.disp.Send(playback.NextMsg{})
		return Response{OK: true}

	case "prev":
		s.disp.Send(playback.PrevMsg{})
		return Response{OK: true}

	case "volume":
		s.disp.Send(VolumeMsg{DB: req.Value})
		return Response{OK: true}

	case "seek":
		s.disp.Send(SeekMsg{Offset: time.Duration(req.Value * float64(time.Second))})
		return Response{OK: true}

	case "load":
		if req.Playlist == "" {
			return Response{OK: false, Error: "load requires a playlist name"}
		}
		reply := make(chan Response, 1)
		s.disp.Send(LoadMsg{Playlist: req.Playlist, Reply: reply})
		return waitReply(reply, s.done, "load", 3*time.Second)

	case "queue":
		if req.Path == "" {
			return Response{OK: false, Error: "queue requires a path"}
		}
		s.disp.Send(QueueMsg{Path: req.Path})
		return Response{OK: true}

	case "url.load":
		if strings.TrimSpace(req.Path) == "" {
			return Response{OK: false, Error: "url.load requires a URL"}
		}
		reply := make(chan Response, 1)
		s.disp.Send(URLRequestMsg{URL: req.Path, Reply: reply})
		return waitReply(reply, s.done, "url.load", 60*time.Second)

	case "save":
		reply := make(chan Response, 1)
		s.disp.Send(SaveRequestMsg{Reply: reply})
		return waitReply(reply, s.done, "save", 5*time.Minute)

	case "theme":
		if req.Name == "" {
			return Response{OK: false, Error: "theme requires a name"}
		}
		reply := make(chan Response, 1)
		s.disp.Send(ThemeMsg{Name: req.Name, Reply: reply})
		return waitReply(reply, s.done, "theme", 3*time.Second)

	case "vis":
		if req.Name == "" {
			return Response{OK: false, Error: "vis requires a mode name"}
		}
		reply := make(chan Response, 1)
		s.disp.Send(VisMsg{Name: req.Name, Reply: reply})
		return waitReply(reply, s.done, "vis", 3*time.Second)

	case "shuffle":
		reply := make(chan Response, 1)
		s.disp.Send(ShuffleMsg{Name: req.Name, Reply: reply})
		return waitReply(reply, s.done, "shuffle", 3*time.Second)

	case "repeat":
		reply := make(chan Response, 1)
		s.disp.Send(RepeatMsg{Name: req.Name, Reply: reply})
		return waitReply(reply, s.done, "repeat", 3*time.Second)

	case "mono":
		reply := make(chan Response, 1)
		s.disp.Send(MonoMsg{Name: req.Name, Reply: reply})
		return waitReply(reply, s.done, "mono", 3*time.Second)

	case "speed":
		if req.Value <= 0 {
			return Response{OK: false, Error: "speed must be positive"}
		}
		reply := make(chan Response, 1)
		s.disp.Send(SpeedMsg{Speed: req.Value, Reply: reply})
		return waitReply(reply, s.done, "speed", 3*time.Second)

	case "eq":
		reply := make(chan Response, 1)
		s.disp.Send(EQMsg{Name: req.Name, Band: req.Band, Value: req.Value, Reply: reply})
		return waitReply(reply, s.done, "eq", 3*time.Second)

	case "device":
		if req.Name == "" {
			return Response{OK: false, Error: "device requires a name (or 'list')"}
		}
		reply := make(chan Response, 1)
		s.disp.Send(DeviceMsg{Name: req.Name, Reply: reply})
		return waitReply(reply, s.done, "device", 3*time.Second)

	case "status":
		return s.handleStatus()

	case "bands":
		reply := make(chan Response, 1)
		s.disp.Send(BandsRequestMsg{Reply: reply})
		return waitReply(reply, s.done, "bands", 1*time.Second)

	case "queue.list", "queue.play", "queue.enqueue", "queue.remove", "queue.move", "queue.clear", "track.play", "track.queue":
		reply := make(chan Response, 1)
		s.disp.Send(QueueRequestMsg{Op: strings.ToLower(req.Cmd), Index: req.Index, To: req.To, Track: req.Track, Reply: reply})
		return waitReply(reply, s.done, req.Cmd, 10*time.Second)

	case "provider.list", "provider.playlists", "provider.tracks", "provider.load", "provider.search",
		"provider.artists", "provider.artist_albums", "provider.albums", "provider.album_tracks", "provider.load_album",
		"provider.favorite", "provider.catalog",
		"playlist.create", "playlist.rename", "playlist.delete", "playlist.add", "playlist.add_many", "playlist.replace", "playlist.remove", "playlist.bookmark":
		reply := make(chan Response, 1)
		s.disp.Send(LibraryRequestMsg{
			Op: strings.ToLower(req.Cmd), Provider: req.Provider, Playlist: req.Playlist,
			Query: req.Query, Artist: req.Artist, Album: req.Album, Sort: req.Sort, Offset: req.Offset,
			Limit: req.Limit, Index: req.Index, NewName: req.NewName, Track: req.Track, Tracks: req.Tracks, Reply: reply,
		})
		return waitReply(reply, s.done, req.Cmd, 30*time.Second)

	case "lyrics":
		reply := make(chan Response, 1)
		s.disp.Send(LyricsRequestMsg{Reply: reply})
		return waitReply(reply, s.done, "lyrics", 15*time.Second)

	case "history", "history.clear":
		reply := make(chan Response, 1)
		s.disp.Send(HistoryRequestMsg{Op: strings.ToLower(req.Cmd), Limit: req.Limit, Reply: reply})
		return waitReply(reply, s.done, "history", 5*time.Second)

	case "plugin.call":
		if s.plugins == nil {
			return Response{OK: false, Error: "plugins not enabled"}
		}
		if req.Name == "" || req.Sub == "" {
			return Response{OK: false, Error: "plugin.call requires plugin name and subcommand"}
		}
		out, err := s.plugins.EmitCommand(req.Name, req.Sub, req.Args)
		if err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, Output: out}

	case "plugin.commands":
		if s.plugins == nil {
			return Response{OK: false, Error: "plugins not enabled"}
		}
		return Response{OK: true, Items: s.plugins.CommandList()}

	default:
		return Response{OK: false, Error: "unknown command: " + req.Cmd}
	}
}

// waitReply waits up to timeout for a response on the reply channel, returning
// a "<label> timeout" error if it elapses or a shutdown error if the server
// closes first.
func waitReply(reply chan Response, done chan struct{}, label string, timeout time.Duration) Response {
	select {
	case resp := <-reply:
		return resp
	case <-time.After(timeout):
		return Response{OK: false, Error: label + " timeout"}
	case <-done:
		return Response{OK: false, Error: "server shutting down"}
	}
}

// handleStatus sends a StatusRequestMsg to the TUI and waits for a response
// with a timeout.
func (s *Server) handleStatus() Response {
	reply := make(chan Response, 1)
	s.disp.Send(StatusRequestMsg{Reply: reply})
	return waitReply(reply, s.done, "status", 3*time.Second)
}

// writeResponse marshals a Response as JSON and writes it followed by a newline.
func writeResponse(conn net.Conn, resp Response) {
	_ = writeJSONLine(conn, resp)
}

func writeJSONLine(conn net.Conn, value any) bool {
	data, err := json.Marshal(value)
	if err != nil {
		return false
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		applog.Warn("ipc: write response: %v", err)
		return false
	}
	return true
}

// cleanStaleSocket removes a leftover socket and PID file from a dead process.
// A connect probe always runs before deleting either path, so a live server is
// never displaced because its PID file is missing, stale, or malformed.
func cleanStaleSocket(sockPath string) error {
	conn, err := dialSocket(sockPath, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("ipc: cliamp is already running")
	}
	if !isSocketUnavailable(err) {
		return fmt.Errorf("ipc: probe socket %s: %w", sockPath, err)
	}

	pidPath := sockPath + ".pid"
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		// No PID file — remove socket if it exists (orphan from crash).
		os.Remove(sockPath)
		return nil
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		// Corrupt PID file — clean up.
		os.Remove(pidPath)
		os.Remove(sockPath)
		return nil
	}

	alive, err := processAlive(pid)
	if err != nil {
		return fmt.Errorf("checking process liveness for socket %s: %w", sockPath, err)
	}
	if !alive {
		// Process is dead — clean up stale files.
		os.Remove(pidPath)
		os.Remove(sockPath)
		return nil
	}

	return fmt.Errorf("ipc: cliamp is already running (pid %d)", pid)
}
