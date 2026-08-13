package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrNoDevice        = errors.New("no device connected")
	ErrDeviceRequired  = errors.New("multiple devices connected; device is required")
	ErrDeviceNotFound  = errors.New("device not found")
	ErrTransportClosed = errors.New("device transport closed")
)

const (
	heartbeatInterval  = 10 * time.Second
	heartbeatTimeout   = 10 * time.Second
	heartbeatFailLimit = 3
)

type transportFrame struct {
	Binary bool
	Data   []byte
}

type messageTransport interface {
	Send(any) error
	SendBinary([]byte) error
	ReceiveFrame() (transportFrame, error)
	Close() error
}

type Manager struct {
	mu         sync.RWMutex
	devices    map[string]*Device
	nextCallID atomic.Uint64

	transferMu     sync.RWMutex
	transfers      map[string]*Transfer
	activeTransfer map[string]string
}

// Device is the durable identity known to the Gateway. Its current WebSocket
// session is replaceable: losing a transport marks the device offline without
// conflating the device identity with that particular socket.
type Device struct {
	info     Info
	session  *Session
	lastSeen time.Time
}

type pendingKind uint8

const (
	pendingCall pendingKind = iota
	pendingPing
)

type pendingResponse struct {
	kind pendingKind
	ch   chan wireResponse
}

// Session owns one live transport. Exactly one goroutine reads from transport;
// concurrent callers only register response waiters and serialize writes.
type Session struct {
	manager   *Manager
	transport messageTransport
	info      Info

	sendMu      sync.Mutex
	pendingMu   sync.Mutex
	pending     map[uint64]*pendingResponse
	activeCalls atomic.Int64

	done   chan struct{}
	closed sync.Once
}

type ConnectedDevice struct {
	Name      string `json:"name"`
	Workspace string `json:"workspace,omitempty"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Version   string `json:"version"`
	Transport string `json:"transport"`
}

func NewManager() *Manager {
	return &Manager{
		devices:        make(map[string]*Device),
		transfers:      make(map[string]*Transfer),
		activeTransfer: make(map[string]string),
	}
}

func normalizeInfo(info Info) (Info, bool) {
	info.Name = strings.TrimSpace(info.Name)
	info.Workspace = strings.TrimSpace(info.Workspace)
	info.OS = strings.TrimSpace(info.OS)
	info.Arch = strings.TrimSpace(info.Arch)
	info.Version = strings.TrimSpace(info.Version)
	if info.Name == "" || len(info.Name) > 128 {
		return Info{}, false
	}
	if len(info.Workspace) > 4096 || len(info.OS) > 128 || len(info.Arch) > 128 || len(info.Version) > 128 {
		return Info{}, false
	}
	return info, true
}

func (m *Manager) Attach(transport messageTransport, info Info) (*Session, bool) {
	info, ok := normalizeInfo(info)
	if !ok || transport == nil {
		return nil, false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	device := m.devices[info.Name]
	if device == nil {
		device = &Device{}
		m.devices[info.Name] = device
	} else if device.session != nil {
		// A healthy same-name session must be explicitly detached before the
		// identity can bind a replacement transport.
		return nil, false
	}

	session := &Session{
		manager:   m,
		transport: transport,
		info:      info,
		pending:   make(map[uint64]*pendingResponse),
		done:      make(chan struct{}),
	}
	device.info = info
	device.session = session
	device.lastSeen = time.Now()
	return session, true
}

func (m *Manager) List() []ConnectedDevice {
	m.mu.RLock()
	devices := make([]ConnectedDevice, 0, len(m.devices))
	for _, device := range m.devices {
		if device.session == nil {
			continue
		}
		devices = append(devices, connectedDevice(device.info))
	}
	m.mu.RUnlock()

	sort.Slice(devices, func(i, j int) bool {
		return devices[i].Name < devices[j].Name
	})
	return devices
}

func connectedDevice(info Info) ConnectedDevice {
	return ConnectedDevice{
		Name:      info.Name,
		Workspace: info.Workspace,
		OS:        info.OS,
		Arch:      info.Arch,
		Version:   info.Version,
		Transport: "websocket",
	}
}

func (m *Manager) resolve(name string) (*Session, error) {
	name = strings.TrimSpace(name)

	m.mu.RLock()
	defer m.mu.RUnlock()
	if name != "" {
		device := m.devices[name]
		if device == nil || device.session == nil {
			return nil, ErrDeviceNotFound
		}
		return device.session, nil
	}

	var only *Session
	count := 0
	for _, device := range m.devices {
		if device.session == nil {
			continue
		}
		count++
		only = device.session
		if count > 1 {
			return nil, ErrDeviceRequired
		}
	}
	if count == 0 {
		return nil, ErrNoDevice
	}
	return only, nil
}

func (m *Manager) Call(ctx context.Context, name, operation string, arguments json.RawMessage) (Result, *Failure, error) {
	session, err := m.resolve(name)
	if err != nil {
		return Result{}, nil, err
	}

	result, failure, err := session.call(ctx, m.nextCallID.Add(1), operation, arguments)
	if err == nil {
		return result, failure, nil
	}

	// Cancellation/deadline of one MCP request is not evidence that the device
	// transport is dead. Only transport failures detach the current session.
	if errors.Is(err, ErrTransportClosed) {
		m.Detach(session)
		if strings.TrimSpace(name) != "" {
			return Result{}, nil, ErrDeviceNotFound
		}
		return Result{}, nil, ErrNoDevice
	}
	return Result{}, nil, err
}

// Serve is the sole reader for a live Session. All results and pongs are
// demultiplexed by request id to their waiting caller.
func (m *Manager) Serve(session *Session) error {
	err := session.readLoop()
	m.Detach(session)
	return err
}

func (m *Manager) Ping(session *Session, timeout time.Duration) error {
	if !m.isAttached(session) {
		return ErrTransportClosed
	}
	return session.ping(m.nextCallID.Add(1), timeout)
}

func (m *Manager) Monitor(session *Session) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	failures := 0
	for {
		select {
		case <-session.done:
			return
		case <-ticker.C:
			// ShellCore handles calls synchronously. Do not inject a heartbeat
			// behind an active command and then mistake the delayed pong for death.
			if session.activeCalls.Load() > 0 {
				failures = 0
				continue
			}

			err := m.Ping(session, heartbeatTimeout)
			if err == nil {
				failures = 0
				continue
			}
			if errors.Is(err, ErrTransportClosed) {
				slog.Warn("ShellCore heartbeat transport failed",
					"device", session.info.Name,
					"error", err,
				)
				m.Detach(session)
				return
			}

			failures++
			slog.Warn("ShellCore heartbeat missed",
				"device", session.info.Name,
				"failure", failures,
				"limit", heartbeatFailLimit,
				"error", err,
			)
			if failures >= heartbeatFailLimit {
				m.Detach(session)
				return
			}
		}
	}
}

func (m *Manager) Detach(session *Session) {
	m.mu.Lock()
	if device := m.devices[session.info.Name]; device != nil && device.session == session {
		device.session = nil
		device.lastSeen = time.Now()
	}
	m.mu.Unlock()
	session.close()
	m.failTransfersForDevice(session.info.Name, "device disconnected")
}

func (m *Manager) Close() {
	m.cancelAllTransfers("gateway shutting down")
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.devices))
	for _, device := range m.devices {
		if device.session != nil {
			sessions = append(sessions, device.session)
			device.session = nil
			device.lastSeen = time.Now()
		}
	}
	m.mu.Unlock()

	for _, session := range sessions {
		session.close()
	}
}

func (m *Manager) isAttached(session *Session) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	device := m.devices[session.info.Name]
	return device != nil && device.session == session
}

func (m *Manager) touch(session *Session) {
	m.mu.Lock()
	if device := m.devices[session.info.Name]; device != nil && device.session == session {
		device.lastSeen = time.Now()
	}
	m.mu.Unlock()
}

func (s *Session) close() {
	s.closed.Do(func() {
		close(s.done)
		_ = s.transport.Close()

		s.pendingMu.Lock()
		s.pending = make(map[uint64]*pendingResponse)
		s.activeCalls.Store(0)
		s.pendingMu.Unlock()
	})
}

func (s *Session) readLoop() error {
	for {
		frame, err := s.transport.ReceiveFrame()
		if err != nil {
			return fmt.Errorf("%w: receive: %v", ErrTransportClosed, err)
		}
		s.manager.touch(s)

		if frame.Binary {
			if err := s.manager.handleTransferBinary(s, frame.Data); err != nil {
				slog.Warn("transfer binary frame rejected", "device", s.info.Name, "error", err)
			}
			continue
		}

		var response wireResponse
		if err := json.Unmarshal(frame.Data, &response); err != nil {
			return fmt.Errorf("%w: decode response: %v", ErrTransportClosed, err)
		}

		switch response.Type {
		case "result", "pong":
			pending := s.takePending(response.ID)
			if pending == nil {
				// Expected for a late response whose upstream MCP request was already
				// cancelled or whose heartbeat timed out.
				continue
			}
			pending.ch <- response
		default:
			if strings.HasPrefix(response.Type, "transfer_") {
				s.manager.handleTransferMessage(s, response)
				continue
			}
			return fmt.Errorf("%w: unsupported response type %q", ErrTransportClosed, response.Type)
		}
	}
}

func (s *Session) call(ctx context.Context, id uint64, operation string, arguments json.RawMessage) (Result, *Failure, error) {
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	if !json.Valid(arguments) {
		return Result{}, nil, fmt.Errorf("arguments for %q are not valid JSON", operation)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, nil, err
	}

	pending, err := s.registerPending(id, pendingCall)
	if err != nil {
		return Result{}, nil, err
	}
	s.activeCalls.Add(1)

	if err := s.send(callMessage{Type: "call", ID: id, Operation: operation, Arguments: arguments}); err != nil {
		s.unregisterPending(id)
		return Result{}, nil, err
	}

	select {
	case response := <-pending.ch:
		if response.Type != "result" || response.ID != id || len(response.Payload) == 0 {
			return Result{}, nil, fmt.Errorf("invalid response for device call %d", id)
		}

		var envelope callEnvelope
		if err := json.Unmarshal(response.Payload, &envelope); err != nil {
			return Result{}, nil, fmt.Errorf("decode device call envelope: %w", err)
		}
		if !envelope.OK {
			if envelope.Error == nil {
				return Result{}, nil, fmt.Errorf("device returned failed response without an error")
			}
			return Result{}, envelope.Error, nil
		}
		if len(envelope.Result) == 0 {
			return Result{}, nil, fmt.Errorf("device returned success without a result")
		}
		return Result{Structured: envelope.Result, IsError: envelope.IsError}, nil, nil
	case <-ctx.Done():
		// Keep the pending slot until the Core eventually responds. The sole
		// reader must still consume that response so the shared transport stays
		// synchronized for subsequent calls.
		return Result{}, nil, ctx.Err()
	case <-s.done:
		return Result{}, nil, ErrTransportClosed
	}
}

func (s *Session) ping(id uint64, timeout time.Duration) error {
	pending, err := s.registerPending(id, pendingPing)
	if err != nil {
		return err
	}
	if err := s.send(pingMessage{Type: "ping", ID: id}); err != nil {
		s.unregisterPending(id)
		return err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case response := <-pending.ch:
		if response.Type != "pong" || response.ID != id {
			return fmt.Errorf("invalid device pong for %d", id)
		}
		return nil
	case <-timer.C:
		s.unregisterPending(id)
		return fmt.Errorf("heartbeat timeout after %s", timeout)
	case <-s.done:
		s.unregisterPending(id)
		return ErrTransportClosed
	}
}

func (s *Session) registerPending(id uint64, kind pendingKind) (*pendingResponse, error) {
	select {
	case <-s.done:
		return nil, ErrTransportClosed
	default:
	}

	pending := &pendingResponse{kind: kind, ch: make(chan wireResponse, 1)}
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if _, exists := s.pending[id]; exists {
		return nil, fmt.Errorf("duplicate pending request id %d", id)
	}
	s.pending[id] = pending
	return pending, nil
}

func (s *Session) takePending(id uint64) *pendingResponse {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	pending := s.pending[id]
	if pending == nil {
		return nil
	}
	delete(s.pending, id)
	if pending.kind == pendingCall {
		s.activeCalls.Add(-1)
	}
	return pending
}

func (s *Session) unregisterPending(id uint64) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	pending := s.pending[id]
	if pending == nil {
		return
	}
	delete(s.pending, id)
	if pending.kind == pendingCall {
		s.activeCalls.Add(-1)
	}
}

func (s *Session) send(value any) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	select {
	case <-s.done:
		return ErrTransportClosed
	default:
	}
	if err := s.transport.Send(value); err != nil {
		return fmt.Errorf("%w: send: %v", ErrTransportClosed, err)
	}
	return nil
}

func (s *Session) sendBinary(payload []byte) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	select {
	case <-s.done:
		return ErrTransportClosed
	default:
	}
	if err := s.transport.SendBinary(payload); err != nil {
		return fmt.Errorf("%w: send binary: %v", ErrTransportClosed, err)
	}
	return nil
}
