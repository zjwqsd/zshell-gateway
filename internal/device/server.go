package device

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"
)

type Server struct {
	listener net.Listener
	token    string
	manager  *Manager
}

func NewServer(listener net.Listener, token string, manager *Manager) *Server {
	return &Server{listener: listener, token: token, manager: manager}
}

func (s *Server) Serve() error {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept ShellCore connection: %w", err)
		}
		go s.handle(conn)
	}
}

func (s *Server) Close() error {
	return s.listener.Close()
}

func (s *Server) handle(conn net.Conn) {
	accepted := false
	defer func() {
		if !accepted {
			_ = conn.Close()
		}
	}()

	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout * time.Second))
	var hello helloMessage
	if err := readFrame(conn, &hello); err != nil {
		slog.Warn("ShellCore handshake failed", "remote", conn.RemoteAddr(), "error", err)
		return
	}

	if hello.Type != "hello" || hello.Protocol != ProtocolVersion {
		_ = writeFrame(conn, helloAck{Type: "hello_ack", Accepted: false, Message: "unsupported protocol"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(hello.Token), []byte(s.token)) != 1 {
		_ = writeFrame(conn, helloAck{Type: "hello_ack", Accepted: false, Message: "authentication failed"})
		return
	}
	hello.Device.Name = strings.TrimSpace(hello.Device.Name)
	if hello.Device.Name == "" {
		hello.Device.Name = "shellcore"
	}

	session, ok := s.manager.Attach(conn, hello.Device)
	if !ok {
		_ = writeFrame(conn, helloAck{Type: "hello_ack", Accepted: false, Message: "another device is already connected"})
		return
	}
	if err := writeFrame(conn, helloAck{Type: "hello_ack", Accepted: true, Message: "ok"}); err != nil {
		s.manager.Detach(session)
		return
	}
	_ = conn.SetDeadline(time.Time{})
	accepted = true

	slog.Info("ShellCore connected",
		"name", hello.Device.Name,
		"os", hello.Device.OS,
		"arch", hello.Device.Arch,
		"remote", conn.RemoteAddr(),
	)
	s.manager.Monitor(session)
	slog.Info("ShellCore disconnected", "name", hello.Device.Name)
}
