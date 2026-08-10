package device

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func TestWebSocketRejectsMissingToken(t *testing.T) {
	manager := NewManager()
	server := httptest.NewServer(NewWebSocketHandler("012345678901234567890123", manager))
	defer server.Close()

	wsURL := "ws" + server.URL[len("http"):] + "/"
	origin, _ := url.Parse("http://localhost/")
	config, err := websocket.NewConfig(wsURL, origin.String())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := websocket.DialConfig(config); err == nil {
		t.Fatal("unauthenticated websocket connection was accepted")
	}
}

func TestWebSocketHelloRegistersDevice(t *testing.T) {
	const token = "012345678901234567890123"
	manager := NewManager()
	server := httptest.NewServer(NewWebSocketHandler(token, manager))
	defer server.Close()

	wsURL := "ws" + server.URL[len("http"):] + "/"
	config, err := websocket.NewConfig(wsURL, "http://localhost/")
	if err != nil {
		t.Fatal(err)
	}

	config.Header.Set("Authorization", "Bearer "+token)
	conn, err := websocket.DialConfig(config)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()

	if err := websocket.JSON.Send(conn, helloMessage{
		Type:     "hello",
		Protocol: ProtocolVersion,
		Device:   Info{Name: "ws-test", Workspace: "/tmp/ws", OS: "linux", Arch: "x86_64", Version: "test"},
	}); err != nil {
		t.Fatal(err)
	}
	var ack helloAck
	if err := websocket.JSON.Receive(conn, &ack); err != nil {
		t.Fatal(err)
	}
	if !ack.Accepted {
		t.Fatalf("hello rejected: %+v", ack)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		devices := manager.List()
		if len(devices) == 1 && devices[0].Name == "ws-test" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("device was not registered: %+v", manager.List())
}

func TestBearerToken(t *testing.T) {
	request, _ := http.NewRequest(http.MethodGet, "http://example.test", nil)
	request.Header.Set("Authorization", "Bearer abc")
	if token, ok := bearerToken(request.Header.Get("Authorization")); !ok || token != "abc" {
		t.Fatalf("token=%q ok=%v", token, ok)
	}
}
