package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2/sse"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/worker"
)

// outputStreamPollInterval controls how often the stream checks for new output.
const outputStreamPollInterval = 2 * time.Second

// handleAgentOutputStream streams agent output as SSE events.
// New turns are sent as they appear; keepalives are sent every 15s.
//
// SSE event format:
//
//	event: turn
//	data: {"turns": [...]}
func (s *Server) handleAgentOutputStream(w http.ResponseWriter, r *http.Request, name string) {
	cfg := s.state.Config()
	agentCfg, ok := findAgent(cfg, name)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent "+name+" not found")
		return
	}

	// Try session log streaming first, fall back to peek polling. The
	// provider the reader dispatches on comes from resolveAgentTranscript,
	// which resolves the builtin family rather than the config name.
	workDir := s.resolveAgentWorkDir(agentCfg, name)
	var provider, logPath string
	resolveLogPath := func() string { return "" }
	if workDir != "" {
		transcriptState, err := s.resolveAgentTranscript(name, agentCfg)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		provider = transcriptState.provider
		logPath = transcriptState.path
		if logPath != "" {
			logPath, _ = s.resolveSafeStreamTranscriptPath(provider, logPath)
		}
		resolveLogPath = func() string {
			resolved, err := s.resolveAgentTranscript(name, agentCfg)
			if err != nil {
				return ""
			}
			return resolved.path
		}
	}

	handle := s.agentWorkerHandle(name, cfg)
	running, _ := workerHandleRunning(r.Context(), handle)

	// If no session log and agent isn't running, return 404 before committing SSE headers.
	if logPath == "" && !running {
		writeError(w, http.StatusNotFound, "not_found", "agent "+name+" not running")
		return
	}

	// Commit SSE headers. Include agent status so clients can distinguish
	// live streaming from historical replay.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if !running {
		w.Header().Set("GC-Agent-Status", "stopped")
	}
	w.WriteHeader(http.StatusOK)
	if err := http.NewResponseController(w).Flush(); err != nil {
		_ = err
	}

	ctx := r.Context()
	workerOps := s.watchAgentWorkerOperationSignals(ctx, name, cfg)
	if logPath != "" {
		s.streamSessionLog(ctx, w, name, provider, logPath, resolveLogPath, workerOps)
	} else {
		s.streamPeekOutput(ctx, w, name, handle, workerOps)
	}
}

func (s *Server) resolveSafeStreamTranscriptPath(provider, path string) (string, error) {
	return sessionlog.ResolveTranscriptPath(provider, s.sessionLogPaths(), path)
}

func (s *Server) safeStreamTranscriptSize(provider, path string) (int64, error) {
	transcript, err := sessionlog.OpenTranscript(provider, s.sessionLogPaths(), path)
	if err != nil {
		return 0, err
	}
	defer transcript.Close() //nolint:errcheck // read-only transcript
	info, err := transcript.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// streamSessionLog polls a session log file and emits new turns as SSE events.
// Uses file size tracking to skip re-reads when the file hasn't grown, and
// UUID-based cursor to correctly identify new turns after DAG resolution.
func (s *Server) streamSessionLog(
	ctx context.Context,
	w http.ResponseWriter,
	name, provider, logPath string,
	resolvePath func() string,
	wake <-chan struct{},
) {
	currentPath, _ := s.resolveSafeStreamTranscriptPath(provider, strings.TrimSpace(logPath))
	if currentPath == "" {
		return
	}
	lw := newLogFileWatcher(currentPath)
	defer lw.Close()

	var lastSize int64
	var lastSentUUID string
	var seq uint64
	sentUUIDs := make(map[string]struct{})
	lw.onReset = func() {
		lastSize = 0
		lastSentUUID = ""
		sentUUIDs = make(map[string]struct{})
	}

	readAndEmit := func() bool {
		if resolvePath != nil {
			if resolvedPath := strings.TrimSpace(resolvePath()); resolvedPath != "" {
				if safePath, err := s.resolveSafeStreamTranscriptPath(provider, resolvedPath); err == nil && safePath != currentPath {
					currentPath = safePath
					lw.UpdatePath(currentPath)
				}
			}
		}
		if currentPath == "" {
			return false
		}

		safePath, err := s.resolveSafeStreamTranscriptPath(provider, currentPath)
		if err != nil {
			return false
		}
		if safePath != currentPath {
			currentPath = safePath
			lw.UpdatePath(currentPath)
		}
		currentSize, err := s.safeStreamTranscriptSize(provider, safePath)
		if err != nil {
			return false
		}
		if currentSize == lastSize {
			return false
		}

		// Use tail=1 (last compaction segment) to limit parsing scope.
		factory, err := s.workerFactory(s.state.SessionsBeadStore().Store)
		if err != nil {
			return false
		}
		transcript, err := factory.ReadTranscript(worker.TranscriptRequest{
			Provider:        provider,
			TranscriptPath:  safePath,
			TailCompactions: 1,
		})
		if err != nil {
			return false
		}
		sess := transcript.Session
		lastSize = currentSize

		turns := make([]outputTurn, 0, len(sess.Messages))
		uuids := make([]string, 0, len(sess.Messages))
		for _, e := range sess.Messages {
			turn := entryToTurn(e)
			if turn.Text == "" {
				continue
			}
			turns = append(turns, turn)
			uuids = append(uuids, e.UUID)
		}
		if len(turns) == 0 {
			return false
		}

		var toSend []outputTurn

		if lastSentUUID == "" {
			// First emission: send everything.
			toSend = turns
		} else {
			found := false
			for i, uuid := range uuids {
				if uuid == lastSentUUID {
					toSend = turns[i+1:]
					found = true
					break
				}
			}
			if !found {
				// Cursor lost (DAG rewrite, compaction). Instead of
				// re-syncing from the beginning (which causes duplicate/
				// out-of-order messages on the client), emit only turns
				// we haven't previously sent.
				log.Printf("agent stream: cursor %s lost, emitting only new turns", lastSentUUID)
				for i, uuid := range uuids {
					if _, seen := sentUUIDs[uuid]; !seen {
						toSend = append(toSend, turns[i])
					}
				}
			}
		}

		// Track all current UUIDs so cursor-lost can filter correctly.
		lastSentUUID = uuids[len(uuids)-1]
		for _, uuid := range uuids {
			sentUUIDs[uuid] = struct{}{}
		}

		if len(toSend) == 0 {
			return false
		}
		seq++

		data, err := json.Marshal(agentOutputResponse{
			Agent:  name,
			Format: "conversation",
			Turns:  toSend,
		})
		if err != nil {
			return false
		}
		fmt.Fprintf(w, "event: turn\nid: %d\ndata: %s\n\n", seq, data) //nolint:errcheck
		if err := http.NewResponseController(w).Flush(); err != nil {
			_ = err
		}
		return true
	}

	lw.Run(ctx, readAndEmit, func() { writeSSEComment(w) }, RunOpts{Wake: wake})
}

// streamPeekOutput polls Peek() through the worker boundary and emits changes
// as SSE events.
func (s *Server) streamPeekOutput(ctx context.Context, w http.ResponseWriter, name string, handle agentPeekHandle, wake <-chan struct{}) {
	poll := time.NewTicker(outputStreamPollInterval)
	defer poll.Stop()
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	var lastOutput string
	var seq uint64

	emitPeek := func() {
		running, err := workerHandleRunning(ctx, handle)
		if err != nil || !running {
			return
		}
		output, err := handle.Peek(ctx, 100)
		if err != nil || output == lastOutput {
			return
		}
		lastOutput = output
		seq++

		turns := []outputTurn{}
		if output != "" {
			turns = append(turns, outputTurn{Role: "output", Text: output})
		}
		data, err := json.Marshal(agentOutputResponse{
			Agent:  name,
			Format: "text",
			Turns:  turns,
		})
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: turn\nid: %d\ndata: %s\n\n", seq, data) //nolint:errcheck
		if err := http.NewResponseController(w).Flush(); err != nil {
			_ = err
		}
	}

	// Emit initial state immediately.
	emitPeek()

	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			emitPeek()
		case _, ok := <-wake:
			if !ok {
				wake = nil
				continue
			}
			emitPeek()
		case <-keepalive.C:
			writeSSEComment(w)
		}
	}
}

func (s *Server) streamSessionLogHuma(
	ctx context.Context,
	send sse.Sender,
	name, provider, logPath string,
	resolvePath func() string,
	wake <-chan struct{},
) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	send = cancelOnSendError(send, cancel)

	currentPath, _ := s.resolveSafeStreamTranscriptPath(provider, strings.TrimSpace(logPath))
	if currentPath == "" {
		return
	}
	lw := newLogFileWatcher(currentPath)
	defer lw.Close()

	var lastSize int64
	var lastSentUUID string
	var seq int
	sentUUIDs := make(map[string]struct{})
	lw.onReset = func() {
		lastSize = 0
		lastSentUUID = ""
		sentUUIDs = make(map[string]struct{})
	}

	readAndEmit := func() bool {
		if resolvePath != nil {
			if resolvedPath := strings.TrimSpace(resolvePath()); resolvedPath != "" {
				if safePath, err := s.resolveSafeStreamTranscriptPath(provider, resolvedPath); err == nil && safePath != currentPath {
					currentPath = safePath
					lw.UpdatePath(currentPath)
				}
			}
		}
		if currentPath == "" {
			return false
		}

		safePath, err := s.resolveSafeStreamTranscriptPath(provider, currentPath)
		if err != nil {
			return false
		}
		if safePath != currentPath {
			currentPath = safePath
			lw.UpdatePath(currentPath)
		}
		currentSize, err := s.safeStreamTranscriptSize(provider, safePath)
		if err != nil {
			return false
		}
		if currentSize == lastSize {
			return false
		}

		factory, err := s.workerFactory(s.state.SessionsBeadStore().Store)
		if err != nil {
			return false
		}
		transcript, err := factory.ReadTranscript(worker.TranscriptRequest{
			Provider:        provider,
			TranscriptPath:  safePath,
			TailCompactions: 1,
		})
		if err != nil {
			return false
		}
		sess := transcript.Session
		lastSize = currentSize

		turns := make([]outputTurn, 0, len(sess.Messages))
		uuids := make([]string, 0, len(sess.Messages))
		for _, entry := range sess.Messages {
			turn := entryToTurn(entry)
			if turn.Text == "" {
				continue
			}
			turns = append(turns, turn)
			uuids = append(uuids, entry.UUID)
		}
		if len(turns) == 0 {
			return false
		}

		var toSend []outputTurn
		if lastSentUUID == "" {
			toSend = turns
		} else {
			found := false
			for i, uuid := range uuids {
				if uuid == lastSentUUID {
					toSend = turns[i+1:]
					found = true
					break
				}
			}
			if !found {
				log.Printf("agent stream: cursor %s lost, emitting only new turns", lastSentUUID)
				for i, uuid := range uuids {
					if _, seen := sentUUIDs[uuid]; !seen {
						toSend = append(toSend, turns[i])
					}
				}
			}
		}

		lastSentUUID = uuids[len(uuids)-1]
		for _, uuid := range uuids {
			sentUUIDs[uuid] = struct{}{}
		}

		if len(toSend) == 0 {
			return false
		}
		seq++
		_ = send(sse.Message{ID: seq, Data: agentOutputResponse{
			Agent:  name,
			Format: "conversation",
			Turns:  toSend,
		}})
		return true
	}

	lw.Run(ctx, readAndEmit, func() {
		_ = send.Data(HeartbeatEvent{Timestamp: time.Now().UTC().Format(time.RFC3339)})
	}, RunOpts{Wake: wake})
}

func (s *Server) streamPeekOutputHuma(ctx context.Context, send sse.Sender, name string, handle agentPeekHandle, wake <-chan struct{}) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	send = cancelOnSendError(send, cancel)

	poll := time.NewTicker(outputStreamPollInterval)
	defer poll.Stop()
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	var lastOutput string
	var seq int

	emitPeek := func() {
		running, err := workerHandleRunning(ctx, handle)
		if err != nil || !running {
			return
		}
		output, err := handle.Peek(ctx, 100)
		if err != nil || output == lastOutput {
			return
		}
		lastOutput = output
		seq++

		turns := []outputTurn{}
		if output != "" {
			turns = append(turns, outputTurn{Role: "output", Text: output})
		}
		_ = send(sse.Message{ID: seq, Data: agentOutputResponse{
			Agent:  name,
			Format: "text",
			Turns:  turns,
		}})
	}

	emitPeek()

	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			emitPeek()
		case _, ok := <-wake:
			if !ok {
				wake = nil
				continue
			}
			emitPeek()
		case <-keepalive.C:
			_ = send.Data(HeartbeatEvent{Timestamp: time.Now().UTC().Format(time.RFC3339)})
		}
	}
}
