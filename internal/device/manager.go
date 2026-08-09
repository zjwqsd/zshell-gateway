package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrNoDevice       = errors.New("no device connected")
	ErrDeviceRequired = errors.New("multiple devices connected; device is required")
	ErrDeviceNotFound = errors.New("device not found")
)

type Manager struct {
	mu         sync.RWMutex
	sessions   map[string]*Session
	nextCallID atomic.Uint64
}

type Session struct {
	manager *Manager
	conn    net.Conn
	info    Info
	callMu  sync.Mutex
	closed  sync.Once
}

type ConnectedDevice struct {
	Name      string `json:"name"`
	Workspace string `json:"workspace,omitempty"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Version   string `json:"version"`
}

func NewManager() *Manager {
	return &Manager{sessions: make(map[string]*Session)}
}

func (m *Manager) Attach(conn net.Conn, info Info) (*Session, bool) {
	name := strings.TrimSpace(info.Name)
	if name == "" {
		return nil, false
	}
	info.Name = name

	m.mu.Lock()
	if _, exists := m.sessions[name]; exists {
		m.mu.Unlock()
		return nil, false
	}
	session := &Session{manager: m, conn: conn, info: info}
	m.sessions[name] = session
	m.mu.Unlock()

	return session, true
}

func (m *Manager) List() []ConnectedDevice {
	m.mu.RLock()
	devices := make([]ConnectedDevice, 0, len(m.sessions))
	for _, session := range m.sessions {
		devices = append(devices, session.device())
	}
	m.mu.RUnlock()

	sort.Slice(devices, func(i, j int) bool {
		return devices[i].Name < devices[j].Name
	})
	return devices
}

func (m *Manager) resolve(name string) (*Session, error) {
	name = strings.TrimSpace(name)

	m.mu.RLock()
	defer m.mu.RUnlock()

	if name != "" {
		session := m.sessions[name]
		if session == nil {
			return nil, ErrDeviceNotFound
		}
		return session, nil
	}

	switch len(m.sessions) {
	case 0:
		return nil, ErrNoDevice
	case 1:
		for _, session := range m.sessions {
			return session, nil
		}
		panic("unreachable")
	default:
		return nil, ErrDeviceRequired
	}
}

func (m *Manager) Call(ctx context.Context, name, operation string, arguments json.RawMessage) (Result, *Failure, error) {
	session, err := m.resolve(name)
	if err != nil {
		return Result{}, nil, err
	}

	result, failure, err := session.call(ctx, m.nextCallID.Add(1), operation, arguments)
	if err != nil {
		m.Detach(session)
		if strings.TrimSpace(name) != "" {
			return Result{}, nil, ErrDeviceNotFound
		}
		return Result{}, nil, ErrNoDevice
	}
	return result, failure, nil
}

func (m *Manager) Ping(session *Session, timeout time.Duration) error {
	if !m.isAttached(session) {
		return ErrNoDevice
	}
	if err := session.ping(m.nextCallID.Add(1), timeout); err != nil {
		m.Detach(session)
		return err
	}
	return nil
}

func (m *Manager) Monitor(session *Session) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if err := m.Ping(session, 3*time.Second); err != nil {
			return
		}
	}
}

func (m *Manager) Detach(session *Session) {
	m.mu.Lock()
	if current := m.sessions[session.info.Name]; current == session {
		delete(m.sessions, session.info.Name)
	}
	m.mu.Unlock()
	session.close()
}

func (m *Manager) Close() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for name, session := range m.sessions {
		delete(m.sessions, name)
		sessions = append(sessions, session)
	}
	m.mu.Unlock()

	for _, session := range sessions {
		session.close()
	}
}

func (m *Manager) isAttached(session *Session) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[session.info.Name] == session
}

func (s *Session) device() ConnectedDevice {
	return ConnectedDevice{
		Name:      s.info.Name,
		Workspace: s.info.Workspace,
		OS:        s.info.OS,
		Arch:      s.info.Arch,
		Version:   s.info.Version,
	}
}

func (s *Session) close() {
	s.closed.Do(func() {
		_ = s.conn.Close()
	})
}

func (s *Session) call(ctx context.Context, id uint64, operation string, arguments json.RawMessage) (Result, *Failure, error) {
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	if !json.Valid(arguments) {
		return Result{}, nil, fmt.Errorf("arguments for %q are not valid JSON", operation)
	}

	s.callMu.Lock()
	defer s.callMu.Unlock()
	if err := applyDeadline(s.conn, ctx); err != nil {
		return Result{}, nil, err
	}
	defer clearDeadline(s.conn)

	if err := writeFrame(s.conn, callMessage{Type: "call", ID: id, Operation: operation, Arguments: arguments}); err != nil {
		return Result{}, nil, err
	}

	var response wireResponse
	if err := readFrame(s.conn, &response); err != nil {
		return Result{}, nil, err
	}
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
}

func (s *Session) ping(id uint64, timeout time.Duration) error {
	s.callMu.Lock()
	defer s.callMu.Unlock()
	if err := s.conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	defer clearDeadline(s.conn)

	if err := writeFrame(s.conn, pingMessage{Type: "ping", ID: id}); err != nil {
		return err
	}
	var response wireResponse
	if err := readFrame(s.conn, &response); err != nil {
		return err
	}
	if response.Type != "pong" || response.ID != id {
		return fmt.Errorf("invalid device pong for %d", id)
	}
	return nil
}

func applyDeadline(conn net.Conn, ctx context.Context) error {
	if deadline, ok := ctx.Deadline(); ok {
		return conn.SetDeadline(deadline)
	}
	return conn.SetDeadline(time.Time{})
}

func clearDeadline(conn net.Conn) {
	_ = conn.SetDeadline(time.Time{})
}
