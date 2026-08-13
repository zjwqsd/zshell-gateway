package device

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	transferBinaryMagic      = "ZTF1"
	transferIDBytes          = 16
	transferBinaryHeaderSize = len(transferBinaryMagic) + transferIDBytes + 8
	maxTransferHistory       = 256
	transferIdleTimeout      = 2 * time.Minute
)

var (
	ErrTransferNotFound       = errors.New("transfer not found")
	ErrTransferDeviceBusy     = errors.New("device is already participating in a transfer")
	ErrTransferSameDevice     = errors.New("source and target devices must be different")
	ErrInvalidTransferRequest = errors.New("invalid transfer request")
)

type TransferState string

const (
	TransferPreparing TransferState = "preparing"
	TransferRunning   TransferState = "transferring"
	TransferVerifying TransferState = "verifying"
	TransferCompleted TransferState = "completed"
	TransferFailed    TransferState = "failed"
	TransferCancelled TransferState = "cancelled"
)

type Transfer struct {
	ID           string
	SourceDevice string
	SourcePath   string
	TargetDevice string
	TargetPath   string
	Overwrite    bool

	source *Session
	target *Session

	State       TransferState
	Size        uint64
	Transferred uint64
	SHA256      string
	Error       string

	sourceReady bool
	targetReady bool
	nextSeq     uint64
	relayMu     sync.Mutex
	createdAt   time.Time
	startedAt   time.Time
	updatedAt   time.Time
	ready       chan struct{}
	readyOnce   sync.Once
	timer       *time.Timer
}

type TransferSnapshot struct {
	TransferID     string        `json:"transferId"`
	Status         TransferState `json:"status"`
	SourceDevice   string        `json:"sourceDevice"`
	SourcePath     string        `json:"sourcePath"`
	TargetDevice   string        `json:"targetDevice"`
	TargetPath     string        `json:"targetPath"`
	Overwrite      bool          `json:"overwrite"`
	Size           uint64        `json:"size"`
	Transferred    uint64        `json:"transferred"`
	Progress       float64       `json:"progress"`
	BytesPerSecond float64       `json:"bytesPerSecond"`
	SHA256         string        `json:"sha256,omitempty"`
	Error          string        `json:"error,omitempty"`
	CreatedAt      time.Time     `json:"createdAt"`
	UpdatedAt      time.Time     `json:"updatedAt"`
}

type transferSourceStartMessage struct {
	Type       string `json:"type"`
	TransferID string `json:"transferId"`
	Path       string `json:"path"`
}

type transferTargetStartMessage struct {
	Type       string `json:"type"`
	TransferID string `json:"transferId"`
	Path       string `json:"path"`
	Overwrite  bool   `json:"overwrite"`
}

type transferIDMessage struct {
	Type       string `json:"type"`
	TransferID string `json:"transferId"`
}

type transferCommitMessage struct {
	Type       string `json:"type"`
	TransferID string `json:"transferId"`
	Size       uint64 `json:"size"`
	SHA256     string `json:"sha256"`
}

func (m *Manager) StartTransfer(ctx context.Context, sourceDevice, sourcePath, targetDevice, targetPath string, overwrite bool) (TransferSnapshot, error) {
	sourceDevice = strings.TrimSpace(sourceDevice)
	targetDevice = strings.TrimSpace(targetDevice)
	sourcePath = strings.TrimSpace(sourcePath)
	targetPath = strings.TrimSpace(targetPath)
	if sourceDevice == "" || targetDevice == "" || sourcePath == "" || targetPath == "" {
		return TransferSnapshot{}, ErrInvalidTransferRequest
	}
	if sourceDevice == targetDevice {
		return TransferSnapshot{}, ErrTransferSameDevice
	}
	if err := ctx.Err(); err != nil {
		return TransferSnapshot{}, err
	}

	source, err := m.resolve(sourceDevice)
	if err != nil {
		return TransferSnapshot{}, err
	}
	target, err := m.resolve(targetDevice)
	if err != nil {
		return TransferSnapshot{}, err
	}

	id, err := newTransferID()
	if err != nil {
		return TransferSnapshot{}, err
	}
	now := time.Now().UTC()
	t := &Transfer{
		ID:           id,
		SourceDevice: sourceDevice,
		SourcePath:   sourcePath,
		TargetDevice: targetDevice,
		TargetPath:   targetPath,
		Overwrite:    overwrite,
		source:       source,
		target:       target,
		State:        TransferPreparing,
		createdAt:    now,
		updatedAt:    now,
		ready:        make(chan struct{}),
	}

	m.transferMu.Lock()
	if _, busy := m.activeTransfer[sourceDevice]; busy {
		m.transferMu.Unlock()
		return TransferSnapshot{}, fmt.Errorf("%w: %s", ErrTransferDeviceBusy, sourceDevice)
	}
	if _, busy := m.activeTransfer[targetDevice]; busy {
		m.transferMu.Unlock()
		return TransferSnapshot{}, fmt.Errorf("%w: %s", ErrTransferDeviceBusy, targetDevice)
	}
	m.pruneTransferHistoryLocked()
	m.transfers[id] = t
	m.activeTransfer[sourceDevice] = id
	m.activeTransfer[targetDevice] = id
	t.timer = time.AfterFunc(transferIdleTimeout, func() {
		m.expireTransfer(id)
	})
	m.transferMu.Unlock()

	t.relayMu.Lock()

	if err := target.send(transferTargetStartMessage{
		Type:       "transfer_target_start",
		TransferID: id,
		Path:       targetPath,
		Overwrite:  overwrite,
	}); err != nil {
		m.failTransferRelayLocked(t, id, "failed to prepare target: "+err.Error())
	} else if err := source.send(transferSourceStartMessage{
		Type:       "transfer_source_start",
		TransferID: id,
		Path:       sourcePath,
	}); err != nil {
		m.failTransferRelayLocked(t, id, "failed to prepare source: "+err.Error())
	}
	t.relayMu.Unlock()

	select {
	case <-t.ready:
		return m.TransferStatus(id)
	case <-ctx.Done():
		_, _ = m.CancelTransfer(id)
		return TransferSnapshot{}, ctx.Err()
	}
}

func (m *Manager) TransferStatus(id string) (TransferSnapshot, error) {
	id = strings.TrimSpace(id)
	m.transferMu.RLock()
	t := m.transfers[id]
	if t == nil {
		m.transferMu.RUnlock()
		return TransferSnapshot{}, ErrTransferNotFound
	}
	snapshot := snapshotTransfer(t)
	m.transferMu.RUnlock()
	return snapshot, nil
}

func (m *Manager) CancelTransfer(id string) (TransferSnapshot, error) {
	id = strings.TrimSpace(id)
	m.transferMu.RLock()
	t := m.transfers[id]
	if t == nil {
		m.transferMu.RUnlock()
		return TransferSnapshot{}, ErrTransferNotFound
	}
	m.transferMu.RUnlock()

	// Relay and cancellation share this lock so a target can only observe
	// either chunk-before-cancel or cancel-without-a-late-chunk.
	t.relayMu.Lock()
	defer t.relayMu.Unlock()

	m.transferMu.Lock()
	if m.transfers[id] != t {
		m.transferMu.Unlock()
		return TransferSnapshot{}, ErrTransferNotFound
	}
	if isFinalTransferState(t.State) {
		snapshot := snapshotTransfer(t)
		m.transferMu.Unlock()
		return snapshot, nil
	}
	t.State = TransferCancelled
	t.Error = "cancelled"
	t.updatedAt = time.Now().UTC()
	m.releaseTransferDevicesLocked(t)
	t.readyOnce.Do(func() { close(t.ready) })
	source, target := t.source, t.target
	t.source = nil
	t.target = nil
	snapshot := snapshotTransfer(t)
	m.transferMu.Unlock()

	message := transferIDMessage{Type: "transfer_cancel", TransferID: id}
	if source != nil {
		_ = source.send(message)
	}
	if target != nil {
		_ = target.send(message)
	}
	return snapshot, nil
}

func (m *Manager) handleTransferMessage(session *Session, response wireResponse) {
	id := strings.TrimSpace(response.TransferID)
	if id == "" {
		return
	}

	switch response.Type {
	case "transfer_source_ready":
		m.transferSourceReady(session, id, response.Size)
	case "transfer_target_ready":
		m.transferTargetReady(session, id)
	case "transfer_source_finish":
		m.transferSourceFinish(session, id, response.Size, response.SHA256)
	case "transfer_target_finish":
		m.transferTargetFinish(session, id, response.Size, response.SHA256)
	case "transfer_failed":
		m.transferPeerFailed(session, id, response.Role, response.Error)
	case "transfer_source_cancelled":
		m.transferSourceCancelled(session, id)
	}
}

func (m *Manager) transferSourceReady(session *Session, id string, size uint64) {
	var start *Session
	m.transferMu.RLock()
	t := m.transfers[id]
	m.transferMu.RUnlock()
	if t == nil {
		return
	}
	t.relayMu.Lock()
	defer t.relayMu.Unlock()

	m.transferMu.Lock()
	if m.transfers[id] != t || isFinalTransferState(t.State) || session != t.source || t.State != TransferPreparing {
		m.transferMu.Unlock()
		return
	}
	t.sourceReady = true
	t.Size = size
	t.updatedAt = time.Now().UTC()
	if t.targetReady {
		now := time.Now().UTC()
		t.State = TransferRunning
		t.startedAt = now
		t.updatedAt = now
		start = t.source
		t.readyOnce.Do(func() { close(t.ready) })
	}
	m.transferMu.Unlock()

	if start != nil {
		if err := start.send(transferIDMessage{Type: "transfer_send", TransferID: id}); err != nil {
			m.failTransferRelayLocked(t, id, "failed to start source stream: "+err.Error())
		}
	}
}

func (m *Manager) transferTargetReady(session *Session, id string) {
	var start *Session
	m.transferMu.RLock()
	t := m.transfers[id]
	m.transferMu.RUnlock()
	if t == nil {
		return
	}
	t.relayMu.Lock()
	defer t.relayMu.Unlock()

	m.transferMu.Lock()
	if m.transfers[id] != t || isFinalTransferState(t.State) || session != t.target || t.State != TransferPreparing {
		m.transferMu.Unlock()
		return
	}
	t.targetReady = true
	t.updatedAt = time.Now().UTC()
	if t.sourceReady {
		now := time.Now().UTC()
		t.State = TransferRunning
		t.startedAt = now
		t.updatedAt = now
		start = t.source
		t.readyOnce.Do(func() { close(t.ready) })
	}
	m.transferMu.Unlock()

	if start != nil {
		if err := start.send(transferIDMessage{Type: "transfer_send", TransferID: id}); err != nil {
			m.failTransferRelayLocked(t, id, "failed to start source stream: "+err.Error())
		}
	}
}

func (m *Manager) handleTransferBinary(session *Session, frame []byte) error {
	if len(frame) < transferBinaryHeaderSize || string(frame[:len(transferBinaryMagic)]) != transferBinaryMagic {
		return errors.New("invalid transfer binary frame")
	}
	id := hex.EncodeToString(frame[len(transferBinaryMagic) : len(transferBinaryMagic)+transferIDBytes])
	sequenceOffset := len(transferBinaryMagic) + transferIDBytes
	sequence := binary.BigEndian.Uint64(frame[sequenceOffset : sequenceOffset+8])
	payloadBytes := uint64(len(frame) - transferBinaryHeaderSize)

	m.transferMu.RLock()
	t := m.transfers[id]
	if t == nil {
		m.transferMu.RUnlock()
		return ErrTransferNotFound
	}
	m.transferMu.RUnlock()

	t.relayMu.Lock()
	defer t.relayMu.Unlock()

	m.transferMu.Lock()
	if m.transfers[id] != t {
		m.transferMu.Unlock()
		return nil
	}
	if isFinalTransferState(t.State) {
		m.transferMu.Unlock()
		return nil
	}
	if t.State != TransferRunning || session != t.source {
		m.transferMu.Unlock()
		return fmt.Errorf("transfer %s is not accepting source chunks", id)
	}
	if sequence != t.nextSeq {
		want := t.nextSeq
		m.transferMu.Unlock()
		m.failTransferRelayLocked(t, id, fmt.Sprintf("chunk sequence mismatch: got %d want %d", sequence, want))
		return errors.New("transfer chunk sequence mismatch")
	}
	if t.Transferred+payloadBytes > t.Size {
		m.transferMu.Unlock()
		m.failTransferRelayLocked(t, id, "source sent more bytes than declared")
		return errors.New("transfer size overflow")
	}
	target := t.target
	m.transferMu.Unlock()

	// This synchronous write is intentional: the target WebSocket and disk path
	// apply backpressure all the way to the source without buffering the file in
	// Gateway memory.
	if err := target.sendBinary(frame); err != nil {
		m.failTransferRelayLocked(t, id, "target transport failed: "+err.Error())
		return err
	}

	m.transferMu.Lock()
	if m.transfers[id] == t && t.State == TransferRunning && t.nextSeq == sequence {
		t.nextSeq++
		t.Transferred += payloadBytes
		t.updatedAt = time.Now().UTC()
	}
	m.transferMu.Unlock()
	return nil
}

func (m *Manager) transferSourceFinish(session *Session, id string, size uint64, sha256 string) {
	sha256 = strings.ToLower(strings.TrimSpace(sha256))
	if !validSHA256(sha256) {
		m.failTransfer(id, "source returned an invalid SHA-256")
		return
	}

	m.transferMu.RLock()
	t := m.transfers[id]
	m.transferMu.RUnlock()
	if t == nil {
		return
	}
	t.relayMu.Lock()
	defer t.relayMu.Unlock()

	var target *Session
	m.transferMu.Lock()
	if m.transfers[id] != t || isFinalTransferState(t.State) || session != t.source || t.State != TransferRunning {
		m.transferMu.Unlock()
		return
	}
	if size != t.Size || t.Transferred != t.Size {
		message := fmt.Sprintf("source size mismatch: declared=%d relayed=%d expected=%d", size, t.Transferred, t.Size)
		m.transferMu.Unlock()
		m.failTransferRelayLocked(t, id, message)
		return
	}
	t.State = TransferVerifying
	t.SHA256 = sha256
	t.updatedAt = time.Now().UTC()
	target = t.target
	m.transferMu.Unlock()

	if err := target.send(transferCommitMessage{
		Type:       "transfer_commit",
		TransferID: id,
		Size:       size,
		SHA256:     sha256,
	}); err != nil {
		m.failTransferRelayLocked(t, id, "failed to commit target: "+err.Error())
	}
}

func (m *Manager) transferTargetFinish(session *Session, id string, size uint64, sha256 string) {
	sha256 = strings.ToLower(strings.TrimSpace(sha256))
	m.transferMu.Lock()
	t := m.transfers[id]
	if t == nil || isFinalTransferState(t.State) || session != t.target || t.State != TransferVerifying {
		m.transferMu.Unlock()
		return
	}
	if size != t.Size || sha256 != t.SHA256 {
		m.transferMu.Unlock()
		m.failTransfer(id, "target verification did not match source")
		return
	}
	t.State = TransferCompleted
	t.Transferred = t.Size
	t.updatedAt = time.Now().UTC()
	m.releaseTransferDevicesLocked(t)
	t.source = nil
	t.target = nil
	m.transferMu.Unlock()
}

func (m *Manager) transferPeerFailed(session *Session, id, role, message string) {
	m.transferMu.RLock()
	t := m.transfers[id]
	validPeer := t != nil && (session == t.source || session == t.target)
	m.transferMu.RUnlock()
	if !validPeer {
		return
	}
	role = strings.TrimSpace(role)
	message = strings.TrimSpace(message)
	if message == "" {
		message = "peer reported transfer failure"
	}
	if role != "" {
		message = role + ": " + message
	}
	m.failTransfer(id, message)
}

func (m *Manager) transferSourceCancelled(session *Session, id string) {
	m.transferMu.RLock()
	t := m.transfers[id]
	if t == nil || session != t.source {
		m.transferMu.RUnlock()
		return
	}
	state := t.State
	m.transferMu.RUnlock()
	if state != TransferCancelled {
		m.failTransfer(id, "source stream cancelled unexpectedly")
	}
}

func (m *Manager) failTransfer(id, message string) {
	m.transferMu.RLock()
	t := m.transfers[id]
	m.transferMu.RUnlock()
	if t == nil {
		return
	}
	t.relayMu.Lock()
	defer t.relayMu.Unlock()
	m.failTransferRelayLocked(t, id, message)
}

// failTransferRelayLocked finalizes a failure while the caller holds relayMu.
// Keeping peer cancellation under the same lock prevents data frames from
// being delivered after the target has processed transfer_cancel.
func (m *Manager) failTransferRelayLocked(t *Transfer, id, message string) {
	m.transferMu.Lock()
	if m.transfers[id] != t || isFinalTransferState(t.State) {
		m.transferMu.Unlock()
		return
	}
	t.State = TransferFailed
	t.Error = message
	t.updatedAt = time.Now().UTC()
	m.releaseTransferDevicesLocked(t)
	t.readyOnce.Do(func() { close(t.ready) })
	source, target := t.source, t.target
	t.source = nil
	t.target = nil
	m.transferMu.Unlock()

	cancel := transferIDMessage{Type: "transfer_cancel", TransferID: id}
	if source != nil {
		_ = source.send(cancel)
	}
	if target != nil {
		_ = target.send(cancel)
	}
}

func (m *Manager) expireTransfer(id string) {
	m.transferMu.RLock()
	t := m.transfers[id]
	m.transferMu.RUnlock()
	if t == nil {
		return
	}

	t.relayMu.Lock()
	defer t.relayMu.Unlock()

	m.transferMu.Lock()
	if m.transfers[id] != t || isFinalTransferState(t.State) {
		m.transferMu.Unlock()
		return
	}
	idle := time.Since(t.updatedAt)
	if idle < transferIdleTimeout {
		remaining := transferIdleTimeout - idle
		t.timer = time.AfterFunc(remaining, func() {
			m.expireTransfer(id)
		})
		m.transferMu.Unlock()
		return
	}
	m.transferMu.Unlock()

	m.failTransferRelayLocked(t, id, "transfer timed out after 2 minutes without progress")
}

func (m *Manager) failTransfersForDevice(deviceName, reason string) {
	m.transferMu.RLock()
	ids := make([]string, 0, 1)
	for id, t := range m.transfers {
		if isFinalTransferState(t.State) {
			continue
		}
		if t.SourceDevice == deviceName || t.TargetDevice == deviceName {
			ids = append(ids, id)
		}
	}
	m.transferMu.RUnlock()
	for _, id := range ids {
		m.failTransfer(id, reason+": "+deviceName)
	}
}

func (m *Manager) cancelAllTransfers(reason string) {
	m.transferMu.RLock()
	ids := make([]string, 0, len(m.transfers))
	for id, t := range m.transfers {
		if !isFinalTransferState(t.State) {
			ids = append(ids, id)
		}
	}
	m.transferMu.RUnlock()
	for _, id := range ids {
		m.failTransfer(id, reason)
	}
}

func (m *Manager) pruneTransferHistoryLocked() {
	for len(m.transfers) >= maxTransferHistory {
		oldestID := ""
		var oldest time.Time
		for id, t := range m.transfers {
			if !isFinalTransferState(t.State) {
				continue
			}
			if oldestID == "" || t.updatedAt.Before(oldest) {
				oldestID = id
				oldest = t.updatedAt
			}
		}
		if oldestID == "" {
			return
		}
		delete(m.transfers, oldestID)
	}
}

func (m *Manager) releaseTransferDevicesLocked(t *Transfer) {
	if t.timer != nil {
		t.timer.Stop()
	}
	if m.activeTransfer[t.SourceDevice] == t.ID {
		delete(m.activeTransfer, t.SourceDevice)
	}
	if m.activeTransfer[t.TargetDevice] == t.ID {
		delete(m.activeTransfer, t.TargetDevice)
	}
}

func snapshotTransfer(t *Transfer) TransferSnapshot {
	progress := 0.0
	if t.Size > 0 {
		progress = float64(t.Transferred) / float64(t.Size) * 100
	} else if t.State == TransferCompleted || t.State == TransferVerifying {
		progress = 100
	}

	bytesPerSecond := 0.0
	if !t.startedAt.IsZero() && t.Transferred > 0 {
		end := time.Now().UTC()
		if isFinalTransferState(t.State) {
			end = t.updatedAt
		}
		if elapsed := end.Sub(t.startedAt).Seconds(); elapsed > 0 {
			bytesPerSecond = float64(t.Transferred) / elapsed
		}
	}

	return TransferSnapshot{
		TransferID:     t.ID,
		Status:         t.State,
		SourceDevice:   t.SourceDevice,
		SourcePath:     t.SourcePath,
		TargetDevice:   t.TargetDevice,
		TargetPath:     t.TargetPath,
		Overwrite:      t.Overwrite,
		Size:           t.Size,
		Transferred:    t.Transferred,
		Progress:       progress,
		BytesPerSecond: bytesPerSecond,
		SHA256:         t.SHA256,
		Error:          t.Error,
		CreatedAt:      t.createdAt,
		UpdatedAt:      t.updatedAt,
	}
}

func isFinalTransferState(state TransferState) bool {
	return state == TransferCompleted || state == TransferFailed || state == TransferCancelled
}

func newTransferID() (string, error) {
	var id [transferIDBytes]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
