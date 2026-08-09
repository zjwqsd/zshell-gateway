package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var ErrNoDevice = errors.New("no device connected")

type Manager struct {
	mu      sync.RWMutex
	current *Session
	nextID  atomic.Uint64
}

type Session struct {
	manager *Manager
	conn    net.Conn
	info    Info
	callMu  sync.Mutex
	closed  sync.Once
}

func NewManager() *Manager {
	return &Manager{}
}

func (m *Manager) Attach(conn net.Conn, info Info) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil {
		return nil, false
	}
	session := &Session{manager: m, conn: conn, info: info}
	m.current = session
	return session, true
}

func (m *Manager) Connected() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current != nil
}

func (m *Manager) CurrentInfo() (Info, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current == nil {
		return Info{}, false
	}
	return m.current.info, true
}

func (m *Manager) currentSession() (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current == nil {
		return nil, ErrNoDevice
	}
	return m.current, nil
}

func (m *Manager) Call(ctx context.Context, operation string, arguments json.RawMessage) (Result, *Failure, error) {
	session, err := m.currentSession()
	if err != nil {
		return Result{}, nil, err
	}
	result, failure, err := session.call(ctx, m.nextID.Add(1), operation, arguments)
	if err != nil {
		m.Detach(session)
		return Result{}, nil, ErrNoDevice
	}
	return result, failure, nil
}

func (m *Manager) Ping(session *Session, timeout time.Duration) error {
	if !m.isCurrent(session) {
		return ErrNoDevice
	}
	if err := session.ping(m.nextID.Add(1), timeout); err != nil {
		m.Detach(session)
		return err
	}
	return nil
}

func (m *Manager) Monitor(session *Session) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if err := m.Ping(session, 5*time.Second); err != nil {
			return
		}
	}
}

func (m *Manager) Detach(session *Session) {
	m.mu.Lock()
	if m.current == session {
		m.current = nil
	}
	m.mu.Unlock()
	session.close()
}

func (m *Manager) Close() {
	m.mu.Lock()
	session := m.current
	m.current = nil
	m.mu.Unlock()
	if session != nil {
		session.close()
	}
}

func (m *Manager) isCurrent(session *Session) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current == session
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

	if err := writeFrame(s.conn, callMessage{
		Type:      "call",
		ID:        id,
		Operation: operation,
		Arguments: arguments,
	}); err != nil {
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
