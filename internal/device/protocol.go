package device

import "encoding/json"

const ProtocolVersion = 3

type Info struct {
	Name      string `json:"name"`
	Workspace string `json:"workspace,omitempty"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Version   string `json:"version"`
}

type helloMessage struct {
	Type     string `json:"type"`
	Protocol int    `json:"protocol"`
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
	Type       string          `json:"type"`
	ID         uint64          `json:"id,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	TransferID string          `json:"transferId,omitempty"`
	Size       uint64          `json:"size,omitempty"`
	SHA256     string          `json:"sha256,omitempty"`
	Role       string          `json:"role,omitempty"`
	Error      string          `json:"error,omitempty"`
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
