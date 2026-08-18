package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeTransport struct{}

func (*fakeTransport) Name() string                                   { return "websocket" }
func (*fakeTransport) Send(any) error                                 { return nil }
func (*fakeTransport) SendTransferChunk(string, uint64, []byte) error { return nil }
func (*fakeTransport) ReceiveFrame() (transportFrame, error)          { return transportFrame{}, nil }
func (*fakeTransport) SetDeadline(time.Time) error                    { return nil }
func (*fakeTransport) Close() error                                   { return nil }

func TestManagerMultipleDevicesAndDuplicateName(t *testing.T) {
	manager := NewManager()
	alpha, ok := manager.Attach(&fakeTransport{}, Info{
		Name:      "alpha",
		Workspace: "C:/work/alpha",
		OS:        "windows",
		Arch:      "x86_64",
		Version:   "test",
	})
	if !ok {
		t.Fatal("first alpha connection was rejected")
	}

	if _, ok := manager.Attach(&fakeTransport{}, Info{Name: "alpha"}); ok {
		t.Fatal("duplicate alpha connection was accepted")
	}

	beta, ok := manager.Attach(&fakeTransport{}, Info{
		Name:      "beta",
		Workspace: "/srv/beta",
		OS:        "linux",
		Arch:      "x86_64",
		Version:   "test",
	})
	if !ok {
		t.Fatal("beta connection was rejected")
	}

	devices := manager.List()
	if len(devices) != 2 {
		t.Fatalf("got %d devices, want 2", len(devices))
	}
	if devices[0].Name != "alpha" || devices[1].Name != "beta" {
		t.Fatalf("devices are not sorted by name: %+v", devices)
	}
	if devices[0].Workspace != "C:/work/alpha" || devices[1].Workspace != "/srv/beta" {
		t.Fatalf("workspace metadata was not preserved: %+v", devices)
	}
	if devices[0].Transport != "websocket" || devices[1].Transport != "websocket" {
		t.Fatalf("transport metadata is wrong: %+v", devices)
	}

	if _, err := manager.resolve(""); !errors.Is(err, ErrDeviceRequired) {
		t.Fatalf("resolve without name: got %v, want ErrDeviceRequired", err)
	}
	if resolved, err := manager.resolve("alpha"); err != nil || resolved != alpha {
		t.Fatalf("resolve alpha: session=%p err=%v", resolved, err)
	}
	if _, err := manager.resolve("missing"); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("resolve missing: got %v, want ErrDeviceNotFound", err)
	}

	manager.Detach(alpha)
	devices = manager.List()
	if len(devices) != 1 || devices[0].Name != "beta" {
		t.Fatalf("after alpha detach: %+v", devices)
	}
	if resolved, err := manager.resolve(""); err != nil || resolved != beta {
		t.Fatalf("single-device implicit resolve: session=%p err=%v", resolved, err)
	}

	manager.Detach(beta)
	if _, err := manager.resolve(""); !errors.Is(err, ErrNoDevice) {
		t.Fatalf("resolve after all detached: got %v, want ErrNoDevice", err)
	}
}

type channelTransport struct {
	sent      chan any
	recv      chan wireResponse
	closed    chan struct{}
	closeOnce sync.Once
}

func newChannelTransport() *channelTransport {
	return &channelTransport{
		sent:   make(chan any, 16),
		recv:   make(chan wireResponse, 16),
		closed: make(chan struct{}),
	}
}

func (t *channelTransport) Name() string { return "websocket" }

func (t *channelTransport) Send(value any) error {
	select {
	case <-t.closed:
		return errors.New("transport closed")
	case t.sent <- value:
		return nil
	}
}

func (t *channelTransport) SendTransferChunk(transferID string, sequence uint64, payload []byte) error {
	frame, err := encodeTransferFrame(transferID, sequence, payload)
	if err != nil {
		return err
	}
	copyPayload := append([]byte(nil), frame...)
	select {
	case <-t.closed:
		return errors.New("transport closed")
	case t.sent <- copyPayload:
		return nil
	}
}

func (t *channelTransport) ReceiveFrame() (transportFrame, error) {
	select {
	case <-t.closed:
		return transportFrame{}, errors.New("transport closed")
	case response := <-t.recv:
		data, err := json.Marshal(response)
		if err != nil {
			return transportFrame{}, err
		}
		return transportFrame{Data: data}, nil
	}
}

func (t *channelTransport) Close() error {
	t.closeOnce.Do(func() { close(t.closed) })
	return nil
}

func waitForDeviceCount(t *testing.T, manager *Manager, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(manager.List()) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("device count=%d, want %d", len(manager.List()), want)
}

func TestCallCancellationDoesNotDetachDevice(t *testing.T) {
	manager := NewManager()
	transport := newChannelTransport()
	session, ok := manager.Attach(transport, Info{Name: "alpha", OS: "windows", Arch: "x86_64"})
	if !ok {
		t.Fatal("attach failed")
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- manager.Serve(session) }()
	defer func() {
		manager.Detach(session)
		<-serveDone
	}()

	ctx, cancel := context.WithCancel(context.Background())
	callDone := make(chan error, 1)
	go func() {
		_, _, err := manager.Call(ctx, "alpha", "environment_info", json.RawMessage(`{}`))
		callDone <- err
	}()

	var call callMessage
	select {
	case sent := <-transport.sent:
		var ok bool
		call, ok = sent.(callMessage)
		if !ok {
			t.Fatalf("sent message type=%T, want callMessage", sent)
		}
	case <-time.After(time.Second):
		t.Fatal("call was not sent")
	}

	cancel()
	select {
	case err := <-callDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("call error=%v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled call did not return")
	}

	if devices := manager.List(); len(devices) != 1 || devices[0].Name != "alpha" {
		t.Fatalf("cancel detached device: %+v", devices)
	}

	// A late result must still be consumed by the sole reader so it cannot
	// desynchronize the next call on this shared WebSocket.
	transport.recv <- wireResponse{
		Type:    "result",
		ID:      call.ID,
		Payload: json.RawMessage(`{"ok":true,"result":{"late":true}}`),
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if session.activeCalls.Load() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("late result did not clear active call; active=%d", session.activeCalls.Load())
}

func TestTransportLossDetachesButIdentityCanReconnect(t *testing.T) {
	manager := NewManager()
	firstTransport := newChannelTransport()
	first, ok := manager.Attach(firstTransport, Info{Name: "alpha", Workspace: "old", OS: "windows", Arch: "x86_64"})
	if !ok {
		t.Fatal("first attach failed")
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- manager.Serve(first) }()

	if err := firstTransport.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("Serve did not exit after transport loss")
	}
	waitForDeviceCount(t, manager, 0)

	secondTransport := newChannelTransport()
	second, ok := manager.Attach(secondTransport, Info{Name: "alpha", Workspace: "new", OS: "windows", Arch: "x86_64"})
	if !ok {
		t.Fatal("same identity could not bind a replacement transport")
	}
	defer manager.Detach(second)

	devices := manager.List()
	if len(devices) != 1 || devices[0].Name != "alpha" || devices[0].Workspace != "new" {
		t.Fatalf("reconnected device metadata is wrong: %+v", devices)
	}
}

func TestConcurrentCallsAreDemultiplexedByID(t *testing.T) {
	manager := NewManager()
	transport := newChannelTransport()
	session, ok := manager.Attach(transport, Info{Name: "alpha", OS: "windows", Arch: "x86_64"})
	if !ok {
		t.Fatal("attach failed")
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- manager.Serve(session) }()
	defer func() {
		manager.Detach(session)
		<-serveDone
	}()

	type outcome struct {
		result Result
		err    error
	}
	outcomes := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func() {
			result, _, err := manager.Call(context.Background(), "alpha", "environment_info", json.RawMessage(`{}`))
			outcomes <- outcome{result: result, err: err}
		}()
	}

	calls := make([]callMessage, 0, 2)
	for len(calls) < 2 {
		select {
		case sent := <-transport.sent:
			call, ok := sent.(callMessage)
			if !ok {
				t.Fatalf("sent message type=%T, want callMessage", sent)
			}
			calls = append(calls, call)
		case <-time.After(time.Second):
			t.Fatal("concurrent calls were not sent")
		}
	}

	// Deliberately respond in reverse order. The Gateway reader must route each
	// result to the correct waiter by id rather than by receive order.
	for i := len(calls) - 1; i >= 0; i-- {
		transport.recv <- wireResponse{
			Type: "result",
			ID:   calls[i].ID,
			Payload: json.RawMessage(fmt.Sprintf(
				`{"ok":true,"result":{"id":%d}}`, calls[i].ID,
			)),
		}
	}

	for i := 0; i < 2; i++ {
		select {
		case got := <-outcomes:
			if got.err != nil {
				t.Fatalf("concurrent call failed: %v", got.err)
			}
			if len(got.result.Structured) == 0 {
				t.Fatal("concurrent call returned empty result")
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent call did not complete")
		}
	}
}
