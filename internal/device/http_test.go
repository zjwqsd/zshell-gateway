package device

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

const testHTTPDeviceToken = "01234567890123456789012345678901"

type httpTestCore struct {
	t      *testing.T
	server *httptest.Server
	id     string
}

func connectHTTPTestCore(t *testing.T, manager *Manager, name string) *httpTestCore {
	t.Helper()
	server := httptest.NewServer(NewHTTPHandler(testHTTPDeviceToken, manager))
	hello := helloMessage{
		Type:     "hello",
		Protocol: ProtocolVersion,
		Device:   Info{Name: name, Workspace: "/tmp/http", OS: "linux", Arch: "x86_64", Version: "test"},
	}
	body, err := json.Marshal(hello)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/device/http", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testHTTPDeviceToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("hello status=%d", resp.StatusCode)
	}
	var ack helloAck
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		t.Fatal(err)
	}
	if !ack.Accepted {
		t.Fatalf("hello rejected: %s", ack.Message)
	}
	id := resp.Header.Get("X-Zshell-Session-ID")
	if id == "" {
		t.Fatal("missing HTTP session id")
	}
	return &httpTestCore{t: t, server: server, id: id}
}

func (c *httpTestCore) close() { c.server.Close() }

func (c *httpTestCore) request(method, suffix, contentType string, body []byte) *http.Response {
	c.t.Helper()
	req, err := http.NewRequest(method, c.server.URL+"/device/http/"+c.id+"/"+suffix, bytes.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testHTTPDeviceToken)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp
}

func (c *httpTestCore) pollJSON(target any) {
	c.t.Helper()
	resp := c.request(http.MethodGet, "poll", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("poll status=%d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		c.t.Fatal(err)
	}
}

func (c *httpTestCore) postMessage(value any) {
	c.t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		c.t.Fatal(err)
	}
	resp := c.request(http.MethodPost, "message", "application/json", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		payload, _ := io.ReadAll(resp.Body)
		c.t.Fatalf("message status=%d body=%s", resp.StatusCode, payload)
	}
}

func TestHTTPTransportCallAndPing(t *testing.T) {
	manager := NewManager()
	core := connectHTTPTestCore(t, manager, "http-core")
	defer core.close()
	defer manager.Close()

	devices := manager.List()
	if len(devices) != 1 || devices[0].Name != "http-core" || devices[0].Transport != "http" {
		t.Fatalf("unexpected device list: %+v", devices)
	}

	callDone := make(chan error, 1)
	go func() {
		result, failure, err := manager.Call(context.Background(), "http-core", "environment_info", json.RawMessage(`{}`))
		if err == nil && failure != nil {
			err = &testError{failure.Code + ": " + failure.Message}
		}
		if err == nil && string(result.Structured) != `{"answer":42}` {
			err = &testError{"unexpected structured result: " + string(result.Structured)}
		}
		callDone <- err
	}()

	var call callMessage
	core.pollJSON(&call)
	if call.Type != "call" || call.Operation != "environment_info" {
		t.Fatalf("unexpected call: %+v", call)
	}
	core.postMessage(map[string]any{
		"type": "result",
		"id":   call.ID,
		"payload": map[string]any{
			"ok":     true,
			"result": map[string]any{"answer": 42},
		},
	})
	select {
	case err := <-callDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP call did not complete")
	}

	pingDone := make(chan error, 1)
	go func() { pingDone <- manager.Ping(manager.devices["http-core"].session, time.Second) }()
	var ping pingMessage
	core.pollJSON(&ping)
	if ping.Type != "ping" {
		t.Fatalf("unexpected ping: %+v", ping)
	}
	core.postMessage(map[string]any{"type": "pong", "id": ping.ID})
	if err := <-pingDone; err != nil {
		t.Fatal(err)
	}
}

type testError struct{ message string }

func (e *testError) Error() string { return e.message }

func TestHTTPTransferChunkDownloadAndAck(t *testing.T) {
	manager := NewManager()
	core := connectHTTPTestCore(t, manager, "http-target")
	defer core.close()
	defer manager.Close()

	session, err := manager.resolve("http-target")
	if err != nil {
		t.Fatal(err)
	}
	transferID := "00112233445566778899aabbccddeeff"
	payload := []byte("raw-http-transfer-data")
	done := make(chan error, 1)
	go func() { done <- session.sendTransferChunk(transferID, 7, payload) }()

	var notice struct {
		Type       string `json:"type"`
		TransferID string `json:"transferId"`
		Sequence   uint64 `json:"sequence"`
	}
	core.pollJSON(&notice)
	if notice.Type != "transport_chunk" || notice.TransferID != transferID || notice.Sequence != 7 {
		t.Fatalf("unexpected chunk notice: %+v", notice)
	}

	chunkPath := "transfer/" + transferID + "/chunk/" + strconv.FormatUint(notice.Sequence, 10)
	resp := core.request(http.MethodGet, chunkPath, "", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, payload) {
		t.Fatalf("chunk download status=%d body=%q", resp.StatusCode, body)
	}
	if resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("unexpected content type: %q", resp.Header.Get("Content-Type"))
	}

	ack := core.request(http.MethodPost, chunkPath+"/ack?ok=1", "application/json", nil)
	ack.Body.Close()
	if ack.StatusCode != http.StatusNoContent {
		t.Fatalf("ack status=%d", ack.StatusCode)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP transfer chunk did not observe acknowledgement")
	}
}

func TestHTTPSourceChunkCanRelayToWebSocketTransport(t *testing.T) {
	manager := NewManager()
	httpCore := connectHTTPTestCore(t, manager, "http-source")
	defer httpCore.close()
	defer manager.Close()

	wsTransport := newChannelTransport()
	wsTarget, ok := manager.Attach(wsTransport, Info{Name: "ws-target", OS: "linux", Arch: "x86_64"})
	if !ok {
		t.Fatal("attach WebSocket target failed")
	}
	defer manager.Detach(wsTarget)

	httpSession, err := manager.resolve("http-source")
	if err != nil {
		t.Fatal(err)
	}
	transferID, err := newTransferID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	manager.transfers[transferID] = &Transfer{
		ID:           transferID,
		SourceDevice: "http-source",
		TargetDevice: "ws-target",
		source:       httpSession,
		target:       wsTarget,
		State:        TransferRunning,
		Size:         5,
		createdAt:    now,
		startedAt:    now,
		updatedAt:    now,
		ready:        make(chan struct{}),
	}
	manager.activeTransfer["http-source"] = transferID
	manager.activeTransfer["ws-target"] = transferID

	path := "transfer/" + transferID + "/chunk/0"
	resp := httpCore.request(http.MethodPost, path, "application/octet-stream", []byte("hello"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("source upload status=%d", resp.StatusCode)
	}

	select {
	case raw := <-wsTransport.sent:
		frame, ok := raw.([]byte)
		if !ok {
			t.Fatalf("target received %T, want binary frame", raw)
		}
		if len(frame) != transferBinaryHeaderSize+5 || string(frame[transferBinaryHeaderSize:]) != "hello" {
			t.Fatalf("unexpected relayed frame: %x", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("WebSocket target did not receive HTTP source chunk")
	}
}

func TestWebSocketSourceChunkCanRelayToHTTPTransport(t *testing.T) {
	manager := NewManager()
	httpCore := connectHTTPTestCore(t, manager, "http-target-mixed")
	defer httpCore.close()
	defer manager.Close()

	wsTransport := newChannelTransport()
	wsSource, ok := manager.Attach(wsTransport, Info{Name: "ws-source", OS: "linux", Arch: "x86_64"})
	if !ok {
		t.Fatal("attach WebSocket source failed")
	}
	defer manager.Detach(wsSource)

	httpTarget, err := manager.resolve("http-target-mixed")
	if err != nil {
		t.Fatal(err)
	}
	transferID, err := newTransferID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	manager.transfers[transferID] = &Transfer{
		ID:           transferID,
		SourceDevice: "ws-source",
		TargetDevice: "http-target-mixed",
		source:       wsSource,
		target:       httpTarget,
		State:        TransferRunning,
		Size:         5,
		createdAt:    now,
		startedAt:    now,
		updatedAt:    now,
		ready:        make(chan struct{}),
	}
	manager.activeTransfer["ws-source"] = transferID
	manager.activeTransfer["http-target-mixed"] = transferID

	frame, err := encodeTransferFrame(transferID, 0, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- manager.handleTransferBinary(wsSource, frame) }()

	var notice struct {
		Type       string `json:"type"`
		TransferID string `json:"transferId"`
		Sequence   uint64 `json:"sequence"`
	}
	httpCore.pollJSON(&notice)
	if notice.Type != "transport_chunk" || notice.TransferID != transferID || notice.Sequence != 0 {
		t.Fatalf("unexpected mixed chunk notice: %+v", notice)
	}
	chunkPath := "transfer/" + transferID + "/chunk/0"
	resp := httpCore.request(http.MethodGet, chunkPath, "", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "hello" {
		t.Fatalf("mixed chunk status=%d body=%q", resp.StatusCode, body)
	}
	ack := httpCore.request(http.MethodPost, chunkPath+"/ack?ok=1", "application/json", nil)
	ack.Body.Close()
	if ack.StatusCode != http.StatusNoContent {
		t.Fatalf("mixed ack status=%d", ack.StatusCode)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("WebSocket to HTTP relay did not complete")
	}

	snapshot, err := manager.TransferStatus(transferID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Transferred != 5 {
		t.Fatalf("transferred=%d want 5", snapshot.Transferred)
	}
}

func TestHTTPSourceChunkCanRelayToHTTPTransport(t *testing.T) {
	manager := NewManager()
	sourceCore := connectHTTPTestCore(t, manager, "http-source-direct")
	defer sourceCore.close()
	targetCore := connectHTTPTestCore(t, manager, "http-target-direct")
	defer targetCore.close()
	defer manager.Close()

	source, err := manager.resolve("http-source-direct")
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.resolve("http-target-direct")
	if err != nil {
		t.Fatal(err)
	}
	transferID, err := newTransferID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	manager.transfers[transferID] = &Transfer{
		ID:           transferID,
		SourceDevice: "http-source-direct",
		TargetDevice: "http-target-direct",
		source:       source,
		target:       target,
		State:        TransferRunning,
		Size:         5,
		createdAt:    now,
		startedAt:    now,
		updatedAt:    now,
		ready:        make(chan struct{}),
	}
	manager.activeTransfer["http-source-direct"] = transferID
	manager.activeTransfer["http-target-direct"] = transferID

	uploadDone := make(chan int, 1)
	go func() {
		resp := sourceCore.request(http.MethodPost, "transfer/"+transferID+"/chunk/0", "application/octet-stream", []byte("hello"))
		resp.Body.Close()
		uploadDone <- resp.StatusCode
	}()

	var notice struct {
		Type       string `json:"type"`
		TransferID string `json:"transferId"`
		Sequence   uint64 `json:"sequence"`
	}
	targetCore.pollJSON(&notice)
	if notice.Type != "transport_chunk" || notice.TransferID != transferID || notice.Sequence != 0 {
		t.Fatalf("unexpected HTTP target chunk notice: %+v", notice)
	}

	chunkPath := "transfer/" + transferID + "/chunk/0"
	resp := targetCore.request(http.MethodGet, chunkPath, "", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "hello" {
		t.Fatalf("HTTP -> HTTP chunk status=%d body=%q", resp.StatusCode, body)
	}
	if resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("unexpected HTTP -> HTTP content type: %q", resp.Header.Get("Content-Type"))
	}

	ack := targetCore.request(http.MethodPost, chunkPath+"/ack?ok=1", "application/json", nil)
	ack.Body.Close()
	if ack.StatusCode != http.StatusNoContent {
		t.Fatalf("HTTP -> HTTP ack status=%d", ack.StatusCode)
	}

	select {
	case status := <-uploadDone:
		if status != http.StatusNoContent {
			t.Fatalf("HTTP source upload status=%d", status)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP -> HTTP source upload did not finish after target ack")
	}

	snapshot, err := manager.TransferStatus(transferID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Transferred != 5 {
		t.Fatalf("HTTP -> HTTP transferred=%d want 5", snapshot.Transferred)
	}
}
