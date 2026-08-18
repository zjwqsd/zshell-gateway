package device

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	httpPollTimeout         = 20 * time.Second
	httpMaxControlBodyBytes = 8 << 20
	httpMaxChunkBytes       = 4 << 20
	httpOutboundQueue       = 32
)

var errHTTPTransportClosed = errors.New("http device transport closed")

type httpTransferChunk struct {
	transferID string
	sequence   uint64
	payload    []byte
	ack        chan error
}

type httpTransport struct {
	id       string
	outbound chan []byte
	inbound  chan transportFrame
	done     chan struct{}
	closed   sync.Once
	onClose  func()

	chunkMu sync.Mutex
	chunk   *httpTransferChunk
}

func (t *httpTransport) Name() string { return "http" }

func (t *httpTransport) Send(value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	select {
	case t.outbound <- payload:
		return nil
	case <-t.done:
		return errHTTPTransportClosed
	}
}

func (t *httpTransport) SendTransferChunk(transferID string, sequence uint64, payload []byte) error {
	chunk := &httpTransferChunk{
		transferID: transferID,
		sequence:   sequence,
		payload:    payload,
		ack:        make(chan error, 1),
	}
	t.chunkMu.Lock()
	if t.chunk != nil {
		t.chunkMu.Unlock()
		return errors.New("http transfer chunk already pending")
	}
	t.chunk = chunk
	t.chunkMu.Unlock()
	defer func() {
		t.chunkMu.Lock()
		if t.chunk == chunk {
			t.chunk = nil
		}
		t.chunkMu.Unlock()
	}()

	if err := t.Send(struct {
		Type       string `json:"type"`
		TransferID string `json:"transferId"`
		Sequence   uint64 `json:"sequence"`
	}{Type: "transport_chunk", TransferID: transferID, Sequence: sequence}); err != nil {
		return err
	}

	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case err := <-chunk.ack:
		return err
	case <-timer.C:
		return errors.New("http transfer chunk acknowledgement timed out")
	case <-t.done:
		return errHTTPTransportClosed
	}
}

func (t *httpTransport) ReceiveFrame() (transportFrame, error) {
	select {
	case frame := <-t.inbound:
		return frame, nil
	case <-t.done:
		return transportFrame{}, errHTTPTransportClosed
	}
}

func (t *httpTransport) Close() error {
	t.closed.Do(func() {
		close(t.done)
		if t.onClose != nil {
			t.onClose()
		}
	})
	return nil
}

type httpDeviceSession struct {
	transport *httpTransport
	session   *Session
}

type httpHandler struct {
	token    string
	manager  *Manager
	mu       sync.RWMutex
	sessions map[string]*httpDeviceSession
}

func NewHTTPHandler(token string, manager *Manager) http.Handler {
	return &httpHandler{
		token:    token,
		manager:  manager,
		sessions: make(map[string]*httpDeviceSession),
	}
}

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	presented, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok || subtle.ConstantTimeCompare([]byte(presented), []byte(h.token)) != 1 {
		http.Error(w, "device authentication failed", http.StatusUnauthorized)
		return
	}

	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/device/http"), "/")
	if path == "" {
		h.handleHello(w, r)
		return
	}
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	entry := h.lookup(parts[0])
	if entry == nil {
		http.Error(w, "device session not found", http.StatusGone)
		return
	}

	switch {
	case len(parts) == 2 && parts[1] == "poll":
		h.handlePoll(entry, w, r)
	case len(parts) == 2 && parts[1] == "message":
		h.handleMessage(entry, w, r)
	case len(parts) == 5 && parts[1] == "transfer" && parts[3] == "chunk":
		h.handleTransferChunk(entry, parts[2], parts[4], w, r)
	case len(parts) == 6 && parts[1] == "transfer" && parts[3] == "chunk" && parts[5] == "ack":
		h.handleTransferChunkAck(entry, parts[2], parts[4], w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *httpHandler) handleHello(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, httpMaxControlBodyBytes+1))
	if err != nil || len(body) > httpMaxControlBodyBytes {
		http.Error(w, "invalid hello body", http.StatusBadRequest)
		return
	}
	var hello helloMessage
	if err := json.Unmarshal(body, &hello); err != nil {
		http.Error(w, "invalid hello JSON", http.StatusBadRequest)
		return
	}
	if hello.Type != "hello" || hello.Protocol != ProtocolVersion {
		writeJSON(w, http.StatusOK, helloAck{Type: "hello_ack", Accepted: false, Message: "unsupported protocol"})
		return
	}

	id, err := newHTTPSessionID()
	if err != nil {
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}
	transport := &httpTransport{
		id:       id,
		outbound: make(chan []byte, httpOutboundQueue),
		inbound:  make(chan transportFrame, httpOutboundQueue),
		done:     make(chan struct{}),
	}
	session, attached := h.manager.Attach(transport, hello.Device)
	if !attached {
		writeJSON(w, http.StatusOK, helloAck{Type: "hello_ack", Accepted: false, Message: "invalid or duplicate device name"})
		return
	}
	entry := &httpDeviceSession{transport: transport, session: session}
	transport.onClose = func() { h.remove(id, entry) }
	h.mu.Lock()
	h.sessions[id] = entry
	h.mu.Unlock()

	go h.manager.Monitor(session)
	go func() {
		err := h.manager.Serve(session)
		if err != nil {
			slog.Warn("ShellCore transport ended", "device", session.info.Name, "transport", "http", "error", err)
		}
		slog.Info("ShellCore disconnected", "device", session.info.Name, "transport", "http")
	}()

	slog.Info("ShellCore connected",
		"device", session.info.Name,
		"workspace", session.info.Workspace,
		"os", session.info.OS,
		"arch", session.info.Arch,
		"transport", "http",
		"remote", r.RemoteAddr,
	)
	w.Header().Set("X-Zshell-Session-ID", id)
	writeJSON(w, http.StatusOK, helloAck{Type: "hello_ack", Accepted: true, Message: "ok"})
}

func (h *httpHandler) handlePoll(entry *httpDeviceSession, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	timer := time.NewTimer(httpPollTimeout)
	defer timer.Stop()
	select {
	case payload := <-entry.transport.outbound:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	case <-timer.C:
		w.WriteHeader(http.StatusNoContent)
	case <-entry.transport.done:
		http.Error(w, "device session closed", http.StatusGone)
	case <-r.Context().Done():
	}
}

func (h *httpHandler) handleMessage(entry *httpDeviceSession, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, httpMaxControlBodyBytes+1))
	if err != nil || len(body) > httpMaxControlBodyBytes || !json.Valid(body) {
		http.Error(w, "invalid device message", http.StatusBadRequest)
		return
	}
	select {
	case entry.transport.inbound <- transportFrame{Data: body}:
		w.WriteHeader(http.StatusNoContent)
	case <-entry.transport.done:
		http.Error(w, "device session closed", http.StatusGone)
	case <-r.Context().Done():
	}
}

func (h *httpHandler) handleTransferChunk(entry *httpDeviceSession, transferID, sequenceText string, w http.ResponseWriter, r *http.Request) {
	sequence, err := strconv.ParseUint(sequenceText, 10, 64)
	if err != nil || !validTransferID(transferID) {
		http.Error(w, "invalid transfer chunk path", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPost:
		if contentType := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(contentType), "application/octet-stream") {
			http.Error(w, "transfer chunk requires application/octet-stream", http.StatusUnsupportedMediaType)
			return
		}
		defer r.Body.Close()
		payload, err := io.ReadAll(io.LimitReader(r.Body, httpMaxChunkBytes+1))
		if err != nil || len(payload) > httpMaxChunkBytes {
			http.Error(w, "invalid transfer chunk", http.StatusRequestEntityTooLarge)
			return
		}
		if err := h.manager.handleTransferChunk(entry.session, transferID, sequence, payload); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case http.MethodGet:
		entry.transport.chunkMu.Lock()
		chunk := entry.transport.chunk
		entry.transport.chunkMu.Unlock()
		if chunk == nil {
			http.Error(w, "transfer chunk not ready", http.StatusConflict)
			return
		}
		if chunk.transferID != transferID || chunk.sequence != sequence {
			http.Error(w, "unexpected transfer chunk", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Zshell-Transfer-ID", chunk.transferID)
		w.Header().Set("X-Zshell-Sequence", strconv.FormatUint(chunk.sequence, 10))
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(chunk.payload); err != nil {
			select {
			case chunk.ack <- err:
			default:
			}
		}
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *httpHandler) handleTransferChunkAck(entry *httpDeviceSession, transferID, sequenceText string, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sequence, err := strconv.ParseUint(sequenceText, 10, 64)
	if err != nil || !validTransferID(transferID) {
		http.Error(w, "invalid transfer chunk acknowledgement", http.StatusBadRequest)
		return
	}
	entry.transport.chunkMu.Lock()
	chunk := entry.transport.chunk
	entry.transport.chunkMu.Unlock()
	if chunk == nil || chunk.transferID != transferID || chunk.sequence != sequence {
		http.Error(w, "transfer chunk is no longer pending", http.StatusConflict)
		return
	}
	var ackErr error
	if r.URL.Query().Get("ok") == "0" {
		message := strings.TrimSpace(r.Header.Get("X-Zshell-Error"))
		if message == "" {
			message = "target rejected transfer chunk"
		}
		ackErr = errors.New(message)
	}
	select {
	case chunk.ack <- ackErr:
		w.WriteHeader(http.StatusNoContent)
	case <-entry.transport.done:
		http.Error(w, "device session closed", http.StatusGone)
	case <-r.Context().Done():
	}
}

func (h *httpHandler) lookup(id string) *httpDeviceSession {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.sessions[id]
}

func (h *httpHandler) remove(id string, expected *httpDeviceSession) {
	h.mu.Lock()
	if h.sessions[id] == expected {
		delete(h.sessions, id)
	}
	h.mu.Unlock()
}

func newHTTPSessionID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func validTransferID(id string) bool {
	if len(id) != transferIDBytes*2 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
