package device

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
)

const (
	ProtocolVersion  = 1
	maxFrameSize     = 16 << 20
	handshakeTimeout = 10
)

type Info struct {
	Name    string `json:"name"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Version string `json:"version"`
}

type helloMessage struct {
	Type     string `json:"type"`
	Protocol int    `json:"protocol"`
	Token    string `json:"token"`
	Device   Info   `json:"device"`
}

type helloAck struct {
	Type     string `json:"type"`
	Accepted bool   `json:"accepted"`
	Message  string `json:"message,omitempty"`
}

type callMessage struct {
	Type      string          `json:"type"`
	ID        uint64          `json:"id"`
	Operation string          `json:"operation"`
	Arguments json.RawMessage `json:"arguments"`
}

type pingMessage struct {
	Type string `json:"type"`
	ID   uint64 `json:"id"`
}

type wireResponse struct {
	Type    string          `json:"type"`
	ID      uint64          `json:"id"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type callEnvelope struct {
	OK             bool            `json:"ok"`
	InvalidRequest bool            `json:"invalidRequest,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
	IsError        bool            `json:"isError,omitempty"`
	Error          *Failure        `json:"error,omitempty"`
}

type Result struct {
	Structured json.RawMessage
	IsError    bool
}

type Failure struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func writeFrame(conn net.Conn, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode device frame: %w", err)
	}
	if len(payload) > maxFrameSize {
		return fmt.Errorf("device frame exceeds %d bytes", maxFrameSize)
	}

	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if err := writeAll(conn, header[:]); err != nil {
		return fmt.Errorf("write device frame header: %w", err)
	}
	if err := writeAll(conn, payload); err != nil {
		return fmt.Errorf("write device frame payload: %w", err)
	}
	return nil
}

func readFrame(conn net.Conn, value any) error {
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return fmt.Errorf("read device frame header: %w", err)
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 || length > maxFrameSize {
		return fmt.Errorf("invalid device frame length %d", length)
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(conn, payload); err != nil {
		return fmt.Errorf("read device frame payload: %w", err)
	}
	if err := json.Unmarshal(payload, value); err != nil {
		return fmt.Errorf("decode device frame: %w", err)
	}
	return nil
}

func writeAll(conn net.Conn, data []byte) error {
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
