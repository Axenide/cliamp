package main

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/bjarneo/cliamp/internal/playback"
	"github.com/bjarneo/cliamp/ipc"
	"github.com/bjarneo/cliamp/playlist"
)

const (
	daemonRuntimeStateTopic    = "runtime.state"
	daemonRuntimePlaybackTopic = "runtime.playback"
	daemonRuntimePlaylistTopic = "runtime.playlist"
	daemonRuntimeSettingsTopic = "runtime.settings"
)

type daemonV2ReadRequest struct {
	request ipc.V2Request
	reply   chan daemonV2ReadResult
}

type daemonV2ReadResult struct {
	result ipc.V2Result
	err    *ipc.V2Error
}

type daemonV2JobRequest struct {
	request ipc.V2Request
	jobs    *ipc.JobStore
	jobID   string
}

type daemonRuntimeFingerprint struct {
	playlistRevision uint64
	state            string
	trackPath        string
	logicalPath      string
	detached         bool
	index            int
	total            int
	playNextTotal    int
	volume           float64
	playlist         string
	shuffle          bool
	repeat           string
	mono             bool
	speed            float64
	eq               [10]float64
	eqPreset         string
	device           string
	visualizer       string
	streamTitle      string
	streamError      string
}

func newDaemonV2Dispatcher(d *daemon, jobs *ipc.JobStore) ipc.V2Dispatcher {
	return ipc.V2DispatcherFunc(func(ctx context.Context, request ipc.V2Request) (ipc.V2Result, *ipc.V2Error) {
		if d == nil || jobs == nil {
			return ipc.V2Result{}, daemonV2UnavailableError()
		}
		switch strings.ToLower(strings.TrimSpace(request.Method)) {
		case "state.get", "spectrum.get":
			return d.dispatchV2Read(ctx, request)
		}
		if err := ctx.Err(); err != nil {
			return ipc.V2Result{}, daemonV2CanceledError()
		}

		operation := daemonV2Operation(request)
		if operation == "" {
			return ipc.V2Result{}, daemonV2InvalidParamsError()
		}
		job, err := jobs.CreateWithContext(ctx, operation)
		if err != nil {
			return ipc.V2Result{}, &ipc.V2Error{Code: ipc.V2ErrorCodeConflict, Message: ipc.V2MessageConflict}
		}
		if !d.enqueueV2(ctx, daemonV2JobRequest{request: request, jobs: jobs, jobID: job.ID}) {
			_ = jobs.Cancel(job.ID)
			return ipc.V2Result{}, daemonV2UnavailableError()
		}
		return ipc.V2Result{Job: &job}, nil
	})
}

func (d *daemon) dispatchV2Read(ctx context.Context, request ipc.V2Request) (ipc.V2Result, *ipc.V2Error) {
	reply := make(chan daemonV2ReadResult, 1)
	if !d.enqueueV2(ctx, daemonV2ReadRequest{request: request, reply: reply}) {
		return ipc.V2Result{}, daemonV2UnavailableError()
	}
	select {
	case result := <-reply:
		return result.result, result.err
	case <-ctx.Done():
		return ipc.V2Result{}, daemonV2CanceledError()
	}
}

func (d *daemon) enqueueV2(ctx context.Context, message any) bool {
	if d.control == nil {
		d.handleMessage(message)
		return true
	}
	select {
	case d.control <- message:
		return true
	case <-ctx.Done():
		return false
	default:
		return false
	}
}

func (d *daemon) handleV2ReadRequest(msg daemonV2ReadRequest) {
	method := strings.ToLower(strings.TrimSpace(msg.request.Method))
	switch method {
	case "state.get":
		snapshot := d.runtimeSnapshot()
		msg.reply <- daemonV2ReadResult{result: ipc.V2Result{Snapshot: &snapshot}}
	case "spectrum.get":
		reply := make(chan ipc.Response, 1)
		d.mu.Lock()
		d.handleBands(ipc.BandsRequestMsg{Reply: reply})
		d.mu.Unlock()
		response := <-reply
		if !response.OK {
			msg.reply <- daemonV2ReadResult{err: daemonV2UnavailableError()}
			return
		}
		result, err := json.Marshal(response)
		if err != nil {
			msg.reply <- daemonV2ReadResult{err: daemonV2InternalError()}
			return
		}
		msg.reply <- daemonV2ReadResult{result: ipc.V2Result{Result: result}}
	default:
		msg.reply <- daemonV2ReadResult{err: daemonV2InvalidParamsError()}
	}
}

func (d *daemon) handleV2JobRequest(msg daemonV2JobRequest) {
	if msg.jobs == nil || msg.jobID == "" {
		return
	}
	ctx, err := msg.jobs.Start(msg.jobID)
	if err != nil {
		return
	}
	if ctx.Err() != nil {
		_ = msg.jobs.Cancel(msg.jobID)
		return
	}

	response, protocolErr := d.executeV2Operation(ctx, msg.request)
	if ctx.Err() != nil {
		_ = msg.jobs.Cancel(msg.jobID)
		return
	}
	if protocolErr != nil {
		_ = msg.jobs.Fail(msg.jobID, *protocolErr)
		return
	}
	if !response.OK {
		_ = msg.jobs.Fail(msg.jobID, *daemonV2InternalError())
		return
	}
	result, err := json.Marshal(response)
	if err != nil {
		_ = msg.jobs.Fail(msg.jobID, *daemonV2InternalError())
		return
	}

	// Publish before capturing the snapshot so a terminal job and retained state
	// always describe the same committed daemon mutation.
	d.publishRuntimeState()
	snapshot := d.runtimeSnapshot()
	if ctx.Err() != nil {
		_ = msg.jobs.Cancel(msg.jobID)
		return
	}
	_ = msg.jobs.SucceedWithSnapshot(msg.jobID, result, snapshot)
}

func (d *daemon) executeV2Operation(ctx context.Context, request ipc.V2Request) (ipc.Response, *ipc.V2Error) {
	legacy, protocolErr := daemonV2LegacyRequest(request)
	if protocolErr != nil {
		return ipc.Response{}, protocolErr
	}
	if ctx.Err() != nil {
		return ipc.Response{}, daemonV2CanceledError()
	}
	if legacy.Revision != 0 && d.playlist != nil && legacy.Revision != d.playlist.Revision() && daemonV2MutatesLivePlaylist(legacy.Cmd) {
		return ipc.Response{}, &ipc.V2Error{Code: ipc.V2ErrorCodeConflict, Message: ipc.V2MessageConflict}
	}

	switch legacy.Cmd {
	case "status":
		d.mu.Lock()
		response := d.statusResponse()
		d.mu.Unlock()
		return response, nil
	case "play":
		d.handleMessage(ipc.PlayMsg{})
	case "pause":
		d.handleMessage(ipc.PauseMsg{})
	case "toggle":
		d.handleMessage(playback.PlayPauseMsg{})
	case "stop":
		d.handleMessage(playback.StopMsg{})
	case "next":
		d.handleMessage(playback.NextMsg{})
	case "prev":
		d.handleMessage(playback.PrevMsg{})
	case "volume":
		d.handleMessage(playback.SetVolumeMsg{VolumeDB: legacy.Value})
		return ipc.Response{OK: true, Volume: d.player.Volume()}, nil
	case "volume.adjust":
		d.handleMessage(playback.SetVolumeMsg{VolumeDB: d.player.Volume() + legacy.Value})
		return ipc.Response{OK: true, Volume: d.player.Volume()}, nil
	case "seek":
		d.handleMessage(ipc.SeekMsg{Offset: secondsToDuration(legacy.Value)})
	case "seek.absolute":
		d.handleMessage(playback.SetPositionMsg{Position: secondsToDuration(legacy.Value)})
	case "speed":
		if !validDaemonV2Speed(legacy.Value) {
			return ipc.Response{}, daemonV2InvalidParamsError()
		}
		return d.v2Reply(ipc.SpeedMsg{Speed: legacy.Value})
	case "speed.adjust":
		return d.v2Reply(ipc.SpeedMsg{Speed: d.player.Speed() + legacy.Value})
	case "shuffle":
		return d.v2Reply(ipc.ShuffleMsg{Name: legacy.Name})
	case "repeat":
		return d.v2Reply(ipc.RepeatMsg{Name: legacy.Name})
	case "mono":
		return d.v2Reply(ipc.MonoMsg{Name: legacy.Name})
	case "eq":
		return d.v2Reply(ipc.EQMsg{Name: legacy.Name, Band: legacy.Band, Value: legacy.Value})
	case "device":
		if legacy.Name == "" {
			return ipc.Response{}, daemonV2InvalidParamsError()
		}
		return d.v2Reply(ipc.DeviceMsg{Name: legacy.Name})
	case "theme":
		return ipc.Response{}, daemonV2UnavailableError()
	case "vis":
		return ipc.Response{}, daemonV2UnavailableError()
	case "load":
		if legacy.Playlist == "" {
			return ipc.Response{}, daemonV2InvalidParamsError()
		}
		return d.v2Reply(ipc.LoadMsg{Playlist: legacy.Playlist})
	case "queue":
		if legacy.Path == "" {
			return ipc.Response{}, daemonV2InvalidParamsError()
		}
		d.handleMessage(ipc.QueueMsg{Path: legacy.Path})
		return d.queueResponse(), nil
	case "queue.list", "queue.play", "queue.enqueue", "queue.remove", "queue.move", "queue.clear", "track.play", "track.queue":
		if legacy.Cmd == "queue.list" {
			return d.queueResponsePage(legacy.Offset, legacy.Limit), nil
		}
		return d.v2Reply(ipc.QueueRequestMsg{Op: legacy.Cmd, Index: legacy.Index, To: legacy.To, Track: legacy.Track})
	case "playnext.list", "playnext.remove", "playnext.move", "playnext.clear":
		return d.handleV2PlayNext(legacy)
	case "url.load":
		if legacy.Path == "" {
			return ipc.Response{}, daemonV2InvalidParamsError()
		}
		return d.v2Reply(ipc.URLRequestMsg{URL: legacy.Path})
	case "save":
		return d.v2Reply(ipc.SaveRequestMsg{})
	case "lyrics":
		return d.v2Reply(ipc.LyricsRequestMsg{})
	case "history", "history.clear":
		return d.v2Reply(ipc.HistoryRequestMsg{Op: legacy.Cmd, Limit: legacy.Limit})
	case "provider.list", "provider.playlists", "provider.tracks", "provider.load", "provider.search",
		"provider.artists", "provider.artist_albums", "provider.albums", "provider.album_tracks", "provider.load_album",
		"provider.favorite", "provider.catalog",
		"playlist.create", "playlist.rename", "playlist.delete", "playlist.add", "playlist.add_many", "playlist.replace", "playlist.remove", "playlist.bookmark":
		return d.v2Reply(ipc.LibraryRequestMsg{
			Op: legacy.Cmd, Provider: legacy.Provider, Playlist: legacy.Playlist, Query: legacy.Query,
			Artist: legacy.Artist, Album: legacy.Album, Sort: legacy.Sort, Offset: legacy.Offset,
			Limit: legacy.Limit, Index: legacy.Index, NewName: legacy.NewName, Track: legacy.Track, Tracks: legacy.Tracks,
		})
	default:
		return ipc.Response{}, daemonV2UnavailableError()
	}
	return ipc.Response{OK: true}, nil
}

func (d *daemon) v2Reply(message any) (ipc.Response, *ipc.V2Error) {
	reply := make(chan ipc.Response, 1)
	switch m := message.(type) {
	case ipc.SpeedMsg:
		m.Reply = reply
		message = m
	case ipc.ShuffleMsg:
		m.Reply = reply
		message = m
	case ipc.RepeatMsg:
		m.Reply = reply
		message = m
	case ipc.MonoMsg:
		m.Reply = reply
		message = m
	case ipc.EQMsg:
		m.Reply = reply
		message = m
	case ipc.DeviceMsg:
		m.Reply = reply
		message = m
	case ipc.ThemeMsg:
		m.Reply = reply
		message = m
	case ipc.VisMsg:
		m.Reply = reply
		message = m
	case ipc.LoadMsg:
		m.Reply = reply
		message = m
	case ipc.QueueRequestMsg:
		m.Reply = reply
		message = m
	case ipc.URLRequestMsg:
		m.Reply = reply
		message = m
	case ipc.SaveRequestMsg:
		m.Reply = reply
		message = m
	case ipc.LyricsRequestMsg:
		m.Reply = reply
		message = m
	case ipc.HistoryRequestMsg:
		m.Reply = reply
		message = m
	case ipc.LibraryRequestMsg:
		m.Reply = reply
		message = m
	default:
		return ipc.Response{}, daemonV2InternalError()
	}
	d.handleMessage(message)
	return <-reply, nil
}

func (d *daemon) handleV2PlayNext(request ipc.Request) (ipc.Response, *ipc.V2Error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.playlist == nil {
		return ipc.Response{}, daemonV2UnavailableError()
	}
	switch request.Cmd {
	case "playnext.list":
		return d.playNextResponsePage(request.Offset, request.Limit), nil
	case "playnext.remove":
		if request.Index < 0 || request.Index >= d.playlist.QueueLen() {
			return ipc.Response{}, daemonV2InvalidParamsError()
		}
		d.playlist.RemoveQueueAt(request.Index)
	case "playnext.move":
		if !d.playlist.MoveQueue(request.Index, request.To) {
			return ipc.Response{}, daemonV2InvalidParamsError()
		}
	case "playnext.clear":
		d.playlist.ClearQueue()
	}
	return d.playNextResponse(), nil
}

func (d *daemon) playNextResponse() ipc.Response {
	return d.playNextResponsePage(0, 0)
}

func (d *daemon) playNextResponsePage(offset, limit int) ipc.Response {
	entries := d.playlist.QueueEntries()
	total := len(entries)
	if offset < 0 || offset >= total {
		return ipc.Response{OK: true, Total: total}
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	end := min(total, offset+limit)
	items := make([]ipc.TrackInfo, end-offset)
	for i, entry := range entries[offset:end] {
		items[i] = trackInfo(entry.Track, entry.TrackIndex, offset+i+1)
	}
	return ipc.Response{OK: true, Tracks: items, Total: total}
}

func (d *daemon) queueResponsePage(offset, limit int) ipc.Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	tracks := d.playlist.Tracks()
	total := len(tracks)
	if offset < 0 || offset >= total {
		return ipc.Response{OK: true, Index: d.playlist.Index(), Total: total}
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	end := min(total, offset+limit)
	items := make([]ipc.TrackInfo, end-offset)
	for i, track := range tracks[offset:end] {
		index := offset + i
		items[i] = trackInfo(track, index, d.playlist.QueuePosition(index))
	}
	return ipc.Response{OK: true, Tracks: items, Index: d.playlist.Index(), Total: total}
}

func daemonV2LegacyRequest(request ipc.V2Request) (ipc.Request, *ipc.V2Error) {
	var legacy ipc.Request
	if len(request.Params) > 0 {
		if err := json.Unmarshal(request.Params, &legacy); err != nil {
			return ipc.Request{}, daemonV2InvalidParamsError()
		}
	}
	legacy.Cmd = daemonV2Operation(request)
	if legacy.Cmd == "" {
		return ipc.Request{}, daemonV2InvalidParamsError()
	}
	return legacy, nil
}

func daemonV2Operation(request ipc.V2Request) string {
	operation := strings.ToLower(strings.TrimSpace(request.Operation))
	if operation == "" {
		operation = strings.ToLower(strings.TrimSpace(request.Method))
	}
	switch operation {
	case "player.play", "runtime.play":
		return "play"
	case "player.pause", "runtime.pause":
		return "pause"
	case "player.toggle", "runtime.toggle":
		return "toggle"
	case "player.stop", "runtime.stop":
		return "stop"
	case "player.next", "runtime.next":
		return "next"
	case "player.prev", "player.previous", "runtime.prev":
		return "prev"
	case "player.volume.set", "runtime.volume":
		return "volume"
	case "player.volume.adjust":
		return "volume.adjust"
	case "player.seek.relative", "runtime.seek":
		return "seek"
	case "player.seek.absolute":
		return "seek.absolute"
	case "player.speed.set", "runtime.speed":
		return "speed"
	case "player.speed.adjust":
		return "speed.adjust"
	case "runtime.snapshot", "runtime.status":
		return "status"
	case "runtime.queue.list", "runtime.playlist.get":
		return "queue.list"
	case "runtime.queue.play", "runtime.playlist.play":
		return "queue.play"
	case "runtime.queue.enqueue":
		return "queue.enqueue"
	case "runtime.queue.remove", "runtime.playlist.remove":
		return "queue.remove"
	case "runtime.queue.move", "runtime.playlist.move":
		return "queue.move"
	case "runtime.queue.clear", "runtime.playlist.clear":
		return "queue.clear"
	case "runtime.library.search":
		return "provider.search"
	default:
		return operation
	}
}

func daemonV2MutatesLivePlaylist(operation string) bool {
	switch operation {
	case "queue", "queue.play", "queue.enqueue", "queue.remove", "queue.move", "queue.clear", "track.play", "track.queue", "playnext.remove", "playnext.move", "playnext.clear":
		return true
	default:
		return false
	}
}

func validDaemonV2Speed(speed float64) bool {
	return speed > 0 && !math.IsNaN(speed) && !math.IsInf(speed, 0)
}

func secondsToDuration(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

func daemonV2InvalidParamsError() *ipc.V2Error {
	return &ipc.V2Error{Code: ipc.V2ErrorCodeInvalidParams, Message: ipc.V2MessageInvalidParams}
}

func daemonV2UnavailableError() *ipc.V2Error {
	return &ipc.V2Error{Code: ipc.V2ErrorCodeUnavailable, Message: ipc.V2MessageUnavailable}
}

func daemonV2CanceledError() *ipc.V2Error {
	return &ipc.V2Error{Code: ipc.V2ErrorCodeCanceled, Message: ipc.V2MessageCanceled}
}

func daemonV2InternalError() *ipc.V2Error {
	return &ipc.V2Error{Code: ipc.V2ErrorCodeInternal, Message: ipc.V2MessageInternal}
}

func (d *daemon) runtimeSnapshot() ipc.RuntimeSnapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.runtimeSnapshotLocked()
}

func (d *daemon) runtimeSnapshotLocked() ipc.RuntimeSnapshot {
	snapshot := ipc.RuntimeSnapshot{Revision: d.runtimeRevision, Playlist: d.loadedPlaylist, Device: d.device}
	if d.playlist != nil {
		snapshot.PlaylistRevision = d.playlist.Revision()
		snapshot.Index = d.playlist.Index()
		snapshot.Total = d.playlist.Len()
		snapshot.PlayNextTotal = d.playlist.QueueLen()
		shuffled := d.playlist.Shuffled()
		snapshot.Shuffle = &shuffled
		snapshot.Repeat = d.playlist.Repeat().String()
		if track, index := d.playlist.Current(); index >= 0 {
			logical := trackInfo(track, index, d.playlist.QueuePosition(index))
			snapshot.LogicalTrack = &logical
		}
	}
	if d.player == nil {
		return snapshot
	}
	switch {
	case d.player.IsPlaying() && !d.player.IsPaused():
		snapshot.State = "playing"
	case d.player.IsPaused():
		snapshot.State = "paused"
	default:
		snapshot.State = "stopped"
	}

	if track, index, ok := d.currentPlaybackTrackLocked(); ok {
		actual := trackInfo(track, index, 0)
		if d.playlist != nil && index >= 0 {
			actual.QueuePosition = d.playlist.QueuePosition(index)
		}
		if track.Stream {
			applyStreamTitle(&actual, track, d.player.StreamTitle())
		}
		snapshot.Track = &actual
		if snapshot.LogicalTrack != nil {
			snapshot.PlaybackDetached = actual.Path != snapshot.LogicalTrack.Path
		}
	}
	position, duration := d.player.PositionAndDuration()
	snapshot.Position = position.Seconds()
	snapshot.Duration = duration.Seconds()
	snapshot.Seekable = d.player.Seekable()
	snapshot.Volume = d.player.Volume()
	mono := d.player.Mono()
	snapshot.Mono = &mono
	snapshot.Speed = d.player.Speed()
	snapshot.EQPreset = d.eqPreset
	bands := d.player.EQBands()
	snapshot.EQBands = append([]float64(nil), bands[:]...)
	if d.vis != nil {
		snapshot.Visualizer = d.vis.ModeName()
	}
	if err := d.player.StreamErr(); err != nil {
		snapshot.StreamError = err.Error()
	}
	return snapshot
}

func (d *daemon) currentPlaybackTrackLocked() (track playlist.Track, index int, ok bool) {
	if d.hasPlaybackTrack {
		index = -1
		if d.playlist != nil {
			if logical, logicalIndex := d.playlist.Current(); logical.Path == d.playbackTrack.Path {
				index = logicalIndex
			}
		}
		return d.playbackTrack, index, true
	}
	if d.playlist == nil || (!d.player.IsPlaying() && !d.player.IsPaused()) {
		return playlist.Track{}, -1, false
	}
	track, index = d.playlist.Current()
	return track, index, index >= 0
}

func (d *daemon) publishRuntimeState() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.publishRuntimeStateLocked()
}

func (d *daemon) publishRuntimeStateLocked() {
	if d.broker == nil || d.player == nil || d.playlist == nil {
		return
	}
	fingerprint := d.runtimeFingerprintLocked()
	if d.runtimeReady && fingerprint == d.runtimeLast {
		return
	}
	d.runtimeReady = true
	d.runtimeLast = fingerprint
	d.runtimeRevision++
	snapshot := d.runtimeSnapshotLocked()
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	for _, topic := range []string{daemonRuntimeStateTopic, daemonRuntimePlaybackTopic, daemonRuntimePlaylistTopic, daemonRuntimeSettingsTopic} {
		_ = d.broker.Publish(topic, payload, true)
	}
}

func (d *daemon) runtimeFingerprintLocked() daemonRuntimeFingerprint {
	fingerprint := daemonRuntimeFingerprint{
		device:   d.device,
		eqPreset: d.eqPreset,
		playlist: d.loadedPlaylist,
	}
	fingerprint.playlistRevision = d.playlist.Revision()
	fingerprint.index = d.playlist.Index()
	fingerprint.total = d.playlist.Len()
	fingerprint.playNextTotal = d.playlist.QueueLen()
	fingerprint.shuffle = d.playlist.Shuffled()
	fingerprint.repeat = d.playlist.Repeat().String()
	if logical, _ := d.playlist.Current(); logical.Path != "" {
		fingerprint.logicalPath = logical.Path
	}
	if d.player.IsPlaying() && !d.player.IsPaused() {
		fingerprint.state = "playing"
	} else if d.player.IsPaused() {
		fingerprint.state = "paused"
	} else {
		fingerprint.state = "stopped"
	}
	if track, _, ok := d.currentPlaybackTrackLocked(); ok {
		fingerprint.trackPath = track.Path
		fingerprint.detached = track.Path != fingerprint.logicalPath
		if track.Stream {
			fingerprint.streamTitle = d.player.StreamTitle()
		}
	}
	fingerprint.volume = d.player.Volume()
	fingerprint.mono = d.player.Mono()
	fingerprint.speed = d.player.Speed()
	fingerprint.eq = d.player.EQBands()
	if d.vis != nil {
		fingerprint.visualizer = d.vis.ModeName()
	}
	if err := d.player.StreamErr(); err != nil {
		fingerprint.streamError = err.Error()
	}
	return fingerprint
}

func publishDaemonV2JobEvents(done <-chan struct{}, jobs *ipc.JobStore, broker *ipc.Broker) {
	if jobs == nil || broker == nil {
		return
	}
	for {
		select {
		case <-done:
			return
		case event, ok := <-jobs.Events():
			if !ok {
				return
			}
			payload, err := json.Marshal(event)
			if err == nil {
				_ = broker.Publish("runtime.job", payload, false)
			}
		}
	}
}
