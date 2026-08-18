package device

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/websocket"
)

const (
	webSocketHandshakeTimeout = 10 * time.Second
	maxWebSocketPayloadBytes  = 8 << 20
)

type webSocketTransport struct {
	conn *websocket.Conn
}

func (t *webSocketTransport) Name() string { return "websocket" }

func (t *webSocketTransport) Send(value any) error {
	return websocket.JSON.Send(t.conn, value)
}

func (t *webSocketTransport) SendTransferChunk(transferID string, sequence uint64, payload []byte) error {
	frame, err := encodeTransferFrame(transferID, sequence, payload)
	if err != nil {
		return err
	}
	return websocket.Message.Send(t.conn, frame)
}

var receiveFrameCodec = websocket.Codec{
	Unmarshal: func(data []byte, payloadType byte, target any) error {
		frame, ok := target.(*transportFrame)
		if !ok {
			return fmt.Errorf("unexpected WebSocket frame target %T", target)
		}
		frame.Binary = payloadType == websocket.BinaryFrame
		frame.Data = data
		return nil
	},
}

func (t *webSocketTransport) ReceiveFrame() (transportFrame, error) {
	var frame transportFrame
	if err := receiveFrameCodec.Receive(t.conn, &frame); err != nil {
		return transportFrame{}, err
	}
	return frame, nil
}

func (t *webSocketTransport) SetDeadline(deadline time.Time) error {
	return t.conn.SetDeadline(deadline)
}

func (t *webSocketTransport) Close() error {
	return t.conn.Close()
}

func NewWebSocketHandler(token string, manager *Manager) http.Handler {
	handler := &webSocketHandler{token: token, manager: manager}
	return websocket.Server{
		Handshake: handler.handshake,
		Handler:   handler.handle,
	}
}

type webSocketHandler struct {
	token   string
	manager *Manager
}

func (h *webSocketHandler) handshake(_ *websocket.Config, r *http.Request) error {
	if r.Method != http.MethodGet {
		return fmt.Errorf("websocket upgrade requires GET")
	}
	presented, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok || subtle.ConstantTimeCompare([]byte(presented), []byte(h.token)) != 1 {
		return fmt.Errorf("device authentication failed")
	}
	return nil
}

func (h *webSocketHandler) handle(conn *websocket.Conn) {
	conn.MaxPayloadBytes = maxWebSocketPayloadBytes
	_ = conn.SetDeadline(time.Now().Add(webSocketHandshakeTimeout))

	var hello helloMessage
	if err := websocket.JSON.Receive(conn, &hello); err != nil {
		slog.Warn("ShellCore WebSocket hello failed", "remote", conn.Request().RemoteAddr, "error", err)
		return
	}
	if hello.Type != "hello" || hello.Protocol != ProtocolVersion {
		_ = websocket.JSON.Send(conn, helloAck{Type: "hello_ack", Accepted: false, Message: "unsupported protocol"})
		return
	}

	transport := &webSocketTransport{conn: conn}
	session, ok := h.manager.Attach(transport, hello.Device)
	if !ok {
		_ = websocket.JSON.Send(conn, helloAck{Type: "hello_ack", Accepted: false, Message: "invalid or duplicate device name"})
		return
	}
	if err := websocket.JSON.Send(conn, helloAck{Type: "hello_ack", Accepted: true, Message: "ok"}); err != nil {
		h.manager.Detach(session)
		return
	}
	_ = conn.SetDeadline(time.Time{})

	slog.Info("ShellCore connected",
		"device", session.info.Name,
		"workspace", session.info.Workspace,
		"os", session.info.OS,
		"arch", session.info.Arch,
		"transport", "websocket",
		"remote", conn.Request().RemoteAddr,
	)

	go h.manager.Monitor(session)
	err := h.manager.Serve(session)
	if err != nil {
		slog.Warn("ShellCore transport ended",
			"device", session.info.Name,
			"transport", "websocket",
			"error", err,
		)
	}
	slog.Info("ShellCore disconnected", "device", session.info.Name, "transport", "websocket")
}

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}
