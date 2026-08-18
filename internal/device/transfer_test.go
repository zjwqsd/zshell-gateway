package device

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"
	"time"
)

func TestCrossDeviceTransferLifecycleAndBinaryRelay(t *testing.T) {
	manager := NewManager()
	sourceTransport := newChannelTransport()
	targetTransport := newChannelTransport()
	source, ok := manager.Attach(sourceTransport, Info{Name: "source", OS: "linux", Arch: "x86_64"})
	if !ok {
		t.Fatal("attach source failed")
	}
	target, ok := manager.Attach(targetTransport, Info{Name: "target", OS: "windows", Arch: "x86_64"})
	if !ok {
		t.Fatal("attach target failed")
	}
	sourceServe := make(chan error, 1)
	targetServe := make(chan error, 1)
	go func() { sourceServe <- manager.Serve(source) }()
	go func() { targetServe <- manager.Serve(target) }()
	defer func() {
		manager.Detach(source)
		manager.Detach(target)
		<-sourceServe
		<-targetServe
	}()

	type startResult struct {
		snapshot TransferSnapshot
		err      error
	}
	started := make(chan startResult, 1)
	go func() {
		snapshot, err := manager.StartTransfer(context.Background(), "source", "/tmp/source.bin", "target", `D:\\target.bin`, false)
		started <- startResult{snapshot: snapshot, err: err}
	}()

	var targetStart transferTargetStartMessage
	select {
	case raw := <-targetTransport.sent:
		var ok bool
		targetStart, ok = raw.(transferTargetStartMessage)
		if !ok {
			t.Fatalf("target first message=%T, want transferTargetStartMessage", raw)
		}
	case <-time.After(time.Second):
		t.Fatal("target prepare was not sent")
	}
	if targetStart.TransferID == "" {
		t.Fatal("transfer id is empty")
	}

	select {
	case raw := <-sourceTransport.sent:
		start, ok := raw.(transferSourceStartMessage)
		if !ok {
			t.Fatalf("source first message=%T, want transferSourceStartMessage", raw)
		}
		if start.TransferID != targetStart.TransferID {
			t.Fatalf("source id=%s target id=%s", start.TransferID, targetStart.TransferID)
		}
	case <-time.After(time.Second):
		t.Fatal("source prepare was not sent")
	}

	targetTransport.recv <- wireResponse{Type: "transfer_target_ready", TransferID: targetStart.TransferID}
	sourceTransport.recv <- wireResponse{Type: "transfer_source_ready", TransferID: targetStart.TransferID, Size: 5}

	select {
	case raw := <-sourceTransport.sent:
		message, ok := raw.(transferIDMessage)
		if !ok || message.Type != "transfer_send" || message.TransferID != targetStart.TransferID {
			t.Fatalf("source start-stream message=%#v", raw)
		}
	case <-time.After(time.Second):
		t.Fatal("transfer_send was not sent")
	}

	var snapshot TransferSnapshot
	select {
	case result := <-started:
		if result.err != nil {
			t.Fatal(result.err)
		}
		snapshot = result.snapshot
	case <-time.After(time.Second):
		t.Fatal("StartTransfer did not return after both peers were ready")
	}
	if snapshot.Status != TransferRunning || snapshot.Size != 5 {
		t.Fatalf("start snapshot=%+v", snapshot)
	}

	frame := makeTransferTestFrame(t, targetStart.TransferID, 0, []byte("hello"))
	if err := manager.handleTransferBinary(source, frame); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-targetTransport.sent:
		binaryFrame, ok := raw.([]byte)
		if !ok {
			t.Fatalf("target chunk type=%T, want []byte", raw)
		}
		if string(binaryFrame) != string(frame) {
			t.Fatal("binary frame changed while relaying")
		}
	case <-time.After(time.Second):
		t.Fatal("binary chunk was not relayed")
	}

	digest := sha256.Sum256([]byte("hello"))
	sha := hex.EncodeToString(digest[:])
	sourceTransport.recv <- wireResponse{
		Type:       "transfer_source_finish",
		TransferID: targetStart.TransferID,
		Size:       5,
		SHA256:     sha,
	}
	select {
	case raw := <-targetTransport.sent:
		commit, ok := raw.(transferCommitMessage)
		if !ok || commit.TransferID != targetStart.TransferID || commit.Size != 5 || commit.SHA256 != sha {
			t.Fatalf("target commit=%#v", raw)
		}
	case <-time.After(time.Second):
		t.Fatal("target commit was not sent")
	}

	targetTransport.recv <- wireResponse{
		Type:       "transfer_target_finish",
		TransferID: targetStart.TransferID,
		Size:       5,
		SHA256:     sha,
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status, err := manager.TransferStatus(targetStart.TransferID)
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == TransferCompleted {
			if status.Transferred != 5 || status.SHA256 != sha || status.Progress != 100 {
				t.Fatalf("completed status=%+v", status)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("transfer did not reach completed state")
}

func TestTransferRejectsBusyDevice(t *testing.T) {
	manager := NewManager()
	a := &fakeTransport{}
	b := &fakeTransport{}
	c := &fakeTransport{}
	if _, ok := manager.Attach(a, Info{Name: "a"}); !ok {
		t.Fatal("attach a")
	}
	if _, ok := manager.Attach(b, Info{Name: "b"}); !ok {
		t.Fatal("attach b")
	}
	if _, ok := manager.Attach(c, Info{Name: "c"}); !ok {
		t.Fatal("attach c")
	}

	// Reserve a and b directly; StartTransfer waits for peer readiness, so this
	// isolates the reservation rule without needing a live read loop.
	manager.transferMu.Lock()
	manager.activeTransfer["a"] = "existing"
	manager.activeTransfer["b"] = "existing"
	manager.transferMu.Unlock()

	if _, err := manager.StartTransfer(context.Background(), "a", "x", "c", "y", false); err == nil {
		t.Fatal("busy source device was accepted")
	}
}

func makeTransferTestFrame(t *testing.T, id string, sequence uint64, payload []byte) []byte {
	t.Helper()
	rawID, err := hex.DecodeString(id)
	if err != nil || len(rawID) != transferIDBytes {
		t.Fatalf("bad test transfer id %q", id)
	}
	frame := make([]byte, transferBinaryHeaderSize+len(payload))
	copy(frame, transferBinaryMagic)
	copy(frame[len(transferBinaryMagic):], rawID)
	sequenceOffset := len(transferBinaryMagic) + transferIDBytes
	binary.BigEndian.PutUint64(frame[sequenceOffset:sequenceOffset+8], sequence)
	copy(frame[transferBinaryHeaderSize:], payload)
	return frame
}

func TestCancelTransferNotifiesBothPeersAndReleasesDevices(t *testing.T) {
	manager := NewManager()
	sourceTransport := newChannelTransport()
	targetTransport := newChannelTransport()
	source, ok := manager.Attach(sourceTransport, Info{Name: "cancel-source"})
	if !ok {
		t.Fatal("attach source failed")
	}
	target, ok := manager.Attach(targetTransport, Info{Name: "cancel-target"})
	if !ok {
		t.Fatal("attach target failed")
	}
	sourceServe := make(chan error, 1)
	targetServe := make(chan error, 1)
	go func() { sourceServe <- manager.Serve(source) }()
	go func() { targetServe <- manager.Serve(target) }()
	defer func() {
		manager.Detach(source)
		manager.Detach(target)
		<-sourceServe
		<-targetServe
	}()

	started := make(chan TransferSnapshot, 1)
	go func() {
		snapshot, _ := manager.StartTransfer(context.Background(), "cancel-source", "a.bin", "cancel-target", "b.bin", false)
		started <- snapshot
	}()

	targetRaw := <-targetTransport.sent
	targetStart := targetRaw.(transferTargetStartMessage)
	<-sourceTransport.sent // transfer_source_start
	targetTransport.recv <- wireResponse{Type: "transfer_target_ready", TransferID: targetStart.TransferID}
	sourceTransport.recv <- wireResponse{Type: "transfer_source_ready", TransferID: targetStart.TransferID, Size: 1024}
	<-sourceTransport.sent // transfer_send
	startSnapshot := <-started
	if startSnapshot.Status != TransferRunning {
		t.Fatalf("start status=%s", startSnapshot.Status)
	}

	cancelled, err := manager.CancelTransfer(targetStart.TransferID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != TransferCancelled {
		t.Fatalf("cancel status=%s", cancelled.Status)
	}

	for name, ch := range map[string]chan any{
		"source": sourceTransport.sent,
		"target": targetTransport.sent,
	} {
		select {
		case raw := <-ch:
			message, ok := raw.(transferIDMessage)
			if !ok || message.Type != "transfer_cancel" || message.TransferID != targetStart.TransferID {
				t.Fatalf("%s cancel message=%#v", name, raw)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s did not receive cancel", name)
		}
	}

	manager.transferMu.RLock()
	_, sourceBusy := manager.activeTransfer["cancel-source"]
	_, targetBusy := manager.activeTransfer["cancel-target"]
	manager.transferMu.RUnlock()
	if sourceBusy || targetBusy {
		t.Fatal("cancelled transfer did not release device reservations")
	}
}

type blockingBinaryTransport struct {
	*channelTransport
	started chan struct{}
	release chan struct{}
}

func newBlockingBinaryTransport() *blockingBinaryTransport {
	return &blockingBinaryTransport{
		channelTransport: newChannelTransport(),
		started:          make(chan struct{}, 1),
		release:          make(chan struct{}),
	}
}

func (t *blockingBinaryTransport) SendTransferChunk(transferID string, sequence uint64, payload []byte) error {
	t.started <- struct{}{}
	<-t.release
	return t.channelTransport.SendTransferChunk(transferID, sequence, payload)
}

func TestCancelWaitsForInFlightChunkBeforeNotifyingTarget(t *testing.T) {
	manager := NewManager()
	sourceTransport := newChannelTransport()
	targetTransport := newBlockingBinaryTransport()
	source, ok := manager.Attach(sourceTransport, Info{Name: "race-source"})
	if !ok {
		t.Fatal("attach source failed")
	}
	target, ok := manager.Attach(targetTransport, Info{Name: "race-target"})
	if !ok {
		t.Fatal("attach target failed")
	}
	sourceServe := make(chan error, 1)
	targetServe := make(chan error, 1)
	go func() { sourceServe <- manager.Serve(source) }()
	go func() { targetServe <- manager.Serve(target) }()
	defer func() {
		manager.Detach(source)
		manager.Detach(target)
		<-sourceServe
		<-targetServe
	}()

	started := make(chan TransferSnapshot, 1)
	go func() {
		snapshot, _ := manager.StartTransfer(context.Background(), "race-source", "a.bin", "race-target", "b.bin", false)
		started <- snapshot
	}()

	targetStart := (<-targetTransport.sent).(transferTargetStartMessage)
	<-sourceTransport.sent
	targetTransport.recv <- wireResponse{Type: "transfer_target_ready", TransferID: targetStart.TransferID}
	sourceTransport.recv <- wireResponse{Type: "transfer_source_ready", TransferID: targetStart.TransferID, Size: 5}
	<-sourceTransport.sent
	<-started

	frameDone := make(chan error, 1)
	go func() {
		frameDone <- manager.handleTransferBinary(source, makeTransferTestFrame(t, targetStart.TransferID, 0, []byte("hello")))
	}()
	select {
	case <-targetTransport.started:
	case <-time.After(time.Second):
		t.Fatal("binary relay did not enter target transport")
	}

	cancelDone := make(chan TransferSnapshot, 1)
	go func() {
		snapshot, _ := manager.CancelTransfer(targetStart.TransferID)
		cancelDone <- snapshot
	}()
	select {
	case <-cancelDone:
		t.Fatal("cancel returned while a target chunk was still in flight")
	case <-time.After(25 * time.Millisecond):
	}

	close(targetTransport.release)
	if err := <-frameDone; err != nil {
		t.Fatal(err)
	}
	cancelled := <-cancelDone
	if cancelled.Status != TransferCancelled {
		t.Fatalf("cancel status=%s", cancelled.Status)
	}

	first := <-targetTransport.sent
	if _, ok := first.([]byte); !ok {
		t.Fatalf("target first post-start message=%T, want binary frame", first)
	}
	second := <-targetTransport.sent
	message, ok := second.(transferIDMessage)
	if !ok || message.Type != "transfer_cancel" {
		t.Fatalf("target second post-start message=%#v, want transfer_cancel", second)
	}
}

func TestExpireTransferFailsIdleTransferAndReleasesDevices(t *testing.T) {
	manager := NewManager()
	source, ok := manager.Attach(&fakeTransport{}, Info{Name: "idle-source"})
	if !ok {
		t.Fatal("attach source failed")
	}
	target, ok := manager.Attach(&fakeTransport{}, Info{Name: "idle-target"})
	if !ok {
		t.Fatal("attach target failed")
	}

	id := "00112233445566778899aabbccddeeff"
	stale := time.Now().UTC().Add(-transferIdleTimeout - time.Second)
	transfer := &Transfer{
		ID:           id,
		SourceDevice: "idle-source",
		TargetDevice: "idle-target",
		source:       source,
		target:       target,
		State:        TransferRunning,
		createdAt:    stale,
		startedAt:    stale,
		updatedAt:    stale,
		ready:        make(chan struct{}),
	}
	manager.transferMu.Lock()
	manager.transfers[id] = transfer
	manager.activeTransfer[transfer.SourceDevice] = id
	manager.activeTransfer[transfer.TargetDevice] = id
	manager.transferMu.Unlock()

	manager.expireTransfer(id)
	snapshot, err := manager.TransferStatus(id)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != TransferFailed {
		t.Fatalf("expired status=%s", snapshot.Status)
	}
	if snapshot.Error != "transfer timed out after 2 minutes without progress" {
		t.Fatalf("expired error=%q", snapshot.Error)
	}
	manager.transferMu.RLock()
	_, sourceBusy := manager.activeTransfer[transfer.SourceDevice]
	_, targetBusy := manager.activeTransfer[transfer.TargetDevice]
	manager.transferMu.RUnlock()
	if sourceBusy || targetBusy {
		t.Fatal("expired transfer did not release device reservations")
	}
}

func TestLateSourceChunkAfterCancelIsIgnored(t *testing.T) {
	manager := NewManager()
	source, ok := manager.Attach(&fakeTransport{}, Info{Name: "late-source"})
	if !ok {
		t.Fatal("attach source failed")
	}
	_, ok = manager.Attach(&fakeTransport{}, Info{Name: "late-target"})
	if !ok {
		t.Fatal("attach target failed")
	}

	id := "00112233445566778899aabbccddeeff"
	transfer := &Transfer{
		ID:           id,
		SourceDevice: "late-source",
		TargetDevice: "late-target",
		State:        TransferCancelled,
		Size:         5,
		createdAt:    time.Now().UTC(),
		updatedAt:    time.Now().UTC(),
		ready:        make(chan struct{}),
	}
	manager.transferMu.Lock()
	manager.transfers[id] = transfer
	manager.transferMu.Unlock()

	frame := makeTransferTestFrame(t, id, 0, []byte("hello"))
	if err := manager.handleTransferBinary(source, frame); err != nil {
		t.Fatalf("late cancelled frame returned error: %v", err)
	}
}
