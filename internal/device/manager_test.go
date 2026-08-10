package device

import (
	"errors"
	"testing"
	"time"
)

type fakeTransport struct{}

func (*fakeTransport) Send(any) error              { return nil }
func (*fakeTransport) Receive(any) error           { return nil }
func (*fakeTransport) SetDeadline(time.Time) error { return nil }
func (*fakeTransport) Close() error                { return nil }

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
