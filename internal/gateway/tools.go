package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"zshell-gateway/internal/device"
)

type toolSpec struct {
	Name        string
	Description string
	InputSchema map[string]any
	ReadOnly    bool
}

func RegisterTools(server *mcp.Server, devices *device.Manager) {
	registerDeviceList(server, devices)
	registerTransferTools(server, devices)
	for _, spec := range toolSpecs() {
		registerDeviceTool(server, devices, spec)
	}
}

func registerTransferTools(server *mcp.Server, devices *device.Manager) {
	server.AddTool(&mcp.Tool{
		Name:        "file_transfer",
		Description: "Transfer one file directly between two connected ShellCore devices. File bytes are streamed through Gateway over WebSocket binary frames and do not pass through the model context. The source and target devices must be different. Each device may participate in at most one active transfer in this protocol version.",
		InputSchema: objectSchema(map[string]any{
			"sourceDevice": stringProperty("Connected ShellCore device that owns the source file."),
			"sourcePath":   pathProperty("Source file path on sourceDevice."),
			"targetDevice": stringProperty("Connected ShellCore device that will receive the file."),
			"targetPath":   pathProperty("Destination file path on targetDevice."),
			"overwrite":    boolProperty("Replace an existing destination file. Defaults to false."),
		}, "sourceDevice", "sourcePath", "targetDevice", "targetPath"),
		OutputSchema: map[string]any{"type": "object", "additionalProperties": true},
		Meta:         mcp.Meta{"securitySchemes": []any{map[string]any{"type": "oauth2", "scopes": []string{executeScope}}}},
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(true),
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var input struct {
			SourceDevice string `json:"sourceDevice"`
			SourcePath   string `json:"sourcePath"`
			TargetDevice string `json:"targetDevice"`
			TargetPath   string `json:"targetPath"`
			Overwrite    bool   `json:"overwrite"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &input); err != nil {
			return transferErrorResult("InvalidRequest", "Invalid file_transfer arguments: "+err.Error()), nil
		}
		snapshot, err := devices.StartTransfer(ctx, input.SourceDevice, input.SourcePath, input.TargetDevice, input.TargetPath, input.Overwrite)
		if err != nil {
			return transferErrorResult(transferErrorCode(err), err.Error()), nil
		}
		return transferSnapshotResult(snapshot, snapshot.Status == device.TransferFailed || snapshot.Status == device.TransferCancelled), nil
	})

	server.AddTool(&mcp.Tool{
		Name:        "file_transfer_status",
		Description: "Return current progress and verification state for a cross-device file transfer.",
		InputSchema: objectSchema(map[string]any{
			"transferId": stringProperty("Transfer ID returned by file_transfer."),
		}, "transferId"),
		OutputSchema: map[string]any{"type": "object", "additionalProperties": true},
		Meta:         mcp.Meta{"securitySchemes": []any{map[string]any{"type": "oauth2", "scopes": []string{executeScope}}}},
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    true,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  true,
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		_ = ctx
		var input struct {
			TransferID string `json:"transferId"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &input); err != nil {
			return transferErrorResult("InvalidRequest", "Invalid file_transfer_status arguments: "+err.Error()), nil
		}
		snapshot, err := devices.TransferStatus(input.TransferID)
		if err != nil {
			return transferErrorResult(transferErrorCode(err), err.Error()), nil
		}
		return transferSnapshotResult(snapshot, snapshot.Status == device.TransferFailed || snapshot.Status == device.TransferCancelled), nil
	})

	server.AddTool(&mcp.Tool{
		Name:        "file_transfer_cancel",
		Description: "Cancel an active cross-device file transfer. The receiving ShellCore removes its temporary .zshell-part file.",
		InputSchema: objectSchema(map[string]any{
			"transferId": stringProperty("Transfer ID returned by file_transfer."),
		}, "transferId"),
		OutputSchema: map[string]any{"type": "object", "additionalProperties": true},
		Meta:         mcp.Meta{"securitySchemes": []any{map[string]any{"type": "oauth2", "scopes": []string{executeScope}}}},
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  true,
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		_ = ctx
		var input struct {
			TransferID string `json:"transferId"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &input); err != nil {
			return transferErrorResult("InvalidRequest", "Invalid file_transfer_cancel arguments: "+err.Error()), nil
		}
		snapshot, err := devices.CancelTransfer(input.TransferID)
		if err != nil {
			return transferErrorResult(transferErrorCode(err), err.Error()), nil
		}
		return transferSnapshotResult(snapshot, false), nil
	})
}

func transferSnapshotResult(snapshot device.TransferSnapshot, isError bool) *mcp.CallToolResult {
	encoded, _ := json.Marshal(snapshot)
	var structured map[string]any
	_ = json.Unmarshal(encoded, &structured)
	text := fmt.Sprintf(
		"transfer: %s\nstatus: %s\nsource: %s:%s\ntarget: %s:%s\nprogress: %.1f%% (%d/%d bytes)\nspeed: %.2f MiB/s",
		snapshot.TransferID,
		snapshot.Status,
		snapshot.SourceDevice,
		snapshot.SourcePath,
		snapshot.TargetDevice,
		snapshot.TargetPath,
		snapshot.Progress,
		snapshot.Transferred,
		snapshot.Size,
		snapshot.BytesPerSecond/(1024*1024),
	)
	if snapshot.SHA256 != "" {
		text += "\nsha256: " + snapshot.SHA256
	}
	if snapshot.Error != "" {
		text += "\nerror: " + snapshot.Error
	}
	result := &mcp.CallToolResult{StructuredContent: structured, IsError: isError}
	result.Content = append(result.Content, &mcp.TextContent{Text: text})
	return result
}

func transferErrorResult(code, message string) *mcp.CallToolResult {
	result := &mcp.CallToolResult{
		StructuredContent: map[string]any{"error": code, "message": message},
		IsError:           true,
	}
	result.Content = append(result.Content, &mcp.TextContent{Text: message})
	return result
}

func transferErrorCode(err error) string {
	switch {
	case errors.Is(err, device.ErrNoDevice):
		return "NoDeviceConnected"
	case errors.Is(err, device.ErrDeviceNotFound):
		return "DeviceNotFound"
	case errors.Is(err, device.ErrTransferNotFound):
		return "TransferNotFound"
	case errors.Is(err, device.ErrTransferDeviceBusy):
		return "TransferDeviceBusy"
	case errors.Is(err, device.ErrTransferSameDevice):
		return "TransferSameDevice"
	case errors.Is(err, device.ErrInvalidTransferRequest):
		return "InvalidTransferRequest"
	default:
		return "TransferError"
	}
}

func registerDeviceTool(server *mcp.Server, devices *device.Manager, spec toolSpec) {
	readOnly := spec.ReadOnly
	destructive := !readOnly
	openWorld := !readOnly

	server.AddTool(&mcp.Tool{
		Name:        spec.Name,
		Description: spec.Description + " When multiple ShellCore devices are connected, pass the device name returned by device_list.",
		InputSchema: withDeviceSchema(spec.InputSchema),
		OutputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": true,
		},
		Meta: mcp.Meta{
			"securitySchemes": []any{
				map[string]any{
					"type":   "oauth2",
					"scopes": []string{executeScope},
				},
			},
		},
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    readOnly,
			DestructiveHint: boolPtr(destructive),
			IdempotentHint:  readOnly,
			OpenWorldHint:   boolPtr(openWorld),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		deviceName, arguments, err := splitDeviceArguments(req.Params.Arguments)
		if err != nil {
			return nil, err
		}

		result, failure, err := devices.Call(ctx, deviceName, spec.Name, arguments)
		if err != nil {
			switch {
			case errors.Is(err, device.ErrNoDevice):
				return routingErrorResult("NoDeviceConnected", "No ShellCore device is connected.", devices.List()), nil
			case errors.Is(err, device.ErrDeviceRequired):
				return routingErrorResult("DeviceRequired", "Multiple ShellCore devices are connected. Call device_list and pass device explicitly.", devices.List()), nil
			case errors.Is(err, device.ErrDeviceNotFound):
				return routingErrorResult("DeviceNotFound", "The requested ShellCore device is not connected. Call device_list for current devices.", devices.List()), nil
			default:
				return nil, err
			}
		}
		if failure != nil {
			return toolFailure(spec.Name, failure), nil
		}

		var structured map[string]any
		if err := json.Unmarshal(result.Structured, &structured); err != nil {
			return nil, fmt.Errorf("decode ShellCore result for %q: %w", spec.Name, err)
		}
		mcpResult := &mcp.CallToolResult{
			StructuredContent: structured,
			IsError:           result.IsError,
		}
		mcpResult.Content = append(mcpResult.Content, &mcp.TextContent{Text: formatToolText(spec.Name, structured)})
		return mcpResult, nil
	})
}

func registerDeviceList(server *mcp.Server, devices *device.Manager) {
	server.AddTool(&mcp.Tool{
		Name:        "device_list",
		Description: "List all currently connected ShellCore devices and their workspaces. Use the returned name as the device selector for other tools.",
		InputSchema: objectSchema(map[string]any{}),
		OutputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": true,
		},
		Meta: mcp.Meta{
			"securitySchemes": []any{
				map[string]any{
					"type":   "oauth2",
					"scopes": []string{executeScope},
				},
			},
		},
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    true,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  true,
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		_ = ctx
		_ = req
		connected := devices.List()
		structured := map[string]any{
			"count":   len(connected),
			"devices": connected,
		}
		result := &mcp.CallToolResult{StructuredContent: structured}
		result.Content = append(result.Content, &mcp.TextContent{Text: formatDeviceListText(connected)})
		return result, nil
	})
}

func withDeviceSchema(schema map[string]any) map[string]any {
	copySchema := make(map[string]any, len(schema))
	for key, value := range schema {
		copySchema[key] = value
	}

	properties := map[string]any{}
	if existing, ok := schema["properties"].(map[string]any); ok {
		for key, value := range existing {
			properties[key] = value
		}
	}
	deviceProperty := map[string]any{
		"type":        "string",
		"description": "ShellCore device name returned by device_list. Optional only when exactly one device is connected.",
		"minLength":   1,
	}
	properties["device"] = deviceProperty
	copySchema["properties"] = properties
	return copySchema
}
func splitDeviceArguments(arguments json.RawMessage) (string, json.RawMessage, error) {
	if len(arguments) == 0 {
		return "", json.RawMessage(`{}`), nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &fields); err != nil {
		return "", nil, fmt.Errorf("decode tool arguments: %w", err)
	}

	var deviceName string
	if raw, ok := fields["device"]; ok {
		if err := json.Unmarshal(raw, &deviceName); err != nil {
			return "", nil, fmt.Errorf("device must be a string")
		}
		delete(fields, "device")
	}

	forwarded, err := json.Marshal(fields)
	if err != nil {
		return "", nil, fmt.Errorf("encode ShellCore arguments: %w", err)
	}
	return strings.TrimSpace(deviceName), forwarded, nil
}

func routingErrorResult(code, message string, devices []device.ConnectedDevice) *mcp.CallToolResult {
	result := &mcp.CallToolResult{
		StructuredContent: map[string]any{
			"error":   code,
			"message": message,
			"count":   len(devices),
			"devices": devices,
		},
		IsError: true,
	}
	result.Content = append(result.Content, &mcp.TextContent{Text: message})
	return result
}

func formatDeviceListText(devices []device.ConnectedDevice) string {
	if len(devices) == 0 {
		return "No ShellCore devices are connected."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d ShellCore device(s) connected:\n", len(devices))
	for _, item := range devices {
		fmt.Fprintf(&b, "- %s | %s/%s", item.Name, item.OS, item.Arch)
		if item.Workspace != "" {
			fmt.Fprintf(&b, " | workspace: %s", item.Workspace)
		}
		if item.Version != "" {
			fmt.Fprintf(&b, " | core: %s", item.Version)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func toolFailure(name string, failure *device.Failure) *mcp.CallToolResult {
	structured := map[string]any{"error": failure.Code}
	for key, value := range failure.Details {
		structured[key] = value
	}

	text := fmt.Sprintf("%s failed: %s", name, failure.Code)
	if failure.Code == "HumanControlActive" {
		text = "Human control is active. This operation was not started."
	}
	if failure.Code == "BrowserFeatureDisabled" {
		text = "Browser functionality is disabled on this ShellCore. Restart that device with --browser to enable browser tools."
	}
	result := &mcp.CallToolResult{
		StructuredContent: structured,
		IsError:           true,
	}
	result.Content = append(result.Content, &mcp.TextContent{Text: text})
	return result
}
func formatToolText(name string, value map[string]any) string {
	switch name {
	case "environment_info":
		return fmt.Sprintf(
			"OS: %s\nArch: %s\nZig: %s\nWorkspace: %s",
			stringField(value, "os"),
			stringField(value, "arch"),
			stringField(value, "zigVersion"),
			stringField(value, "workspace"),
		)
	case "control_status":
		if boolField(value, "canExecute") {
			return "Agent has execution control."
		}
		return "Human control is active; new Agent mutations are blocked."
	case "exec":
		stdout := mapField(value, "stdout")
		stderr := mapField(value, "stderr")
		exitCode := "unavailable"
		if code, ok := numberField(value, "exitCode"); ok {
			exitCode = fmt.Sprintf("%.0f", code)
		}
		return fmt.Sprintf(
			"execution: %.0f\nshell: %s\ntermination: %s\nterminationSource: %s\ntimedOut: %t\nexitCode: %s\n\nstdout (%s):\n%s\n\nstderr (%s):\n%s",
			numberOrZero(value, "executionId"),
			stringField(value, "shell"),
			stringField(value, "termination"),
			stringField(value, "terminationSource"),
			boolField(value, "timedOut"),
			exitCode,
			stringField(stdout, "encoding"),
			stringField(stdout, "data"),
			stringField(stderr, "encoding"),
			stringField(stderr, "data"),
		)
	case "job_start":
		return fmt.Sprintf(
			"job %.0f started\nstatus: %s",
			numberOrZero(value, "jobId"),
			stringField(value, "status"),
		)
	case "job_status", "job_stop":
		var b strings.Builder
		fmt.Fprintf(&b, "job: %.0f\nstatus: %s\ncommand: %s\n",
			numberOrZero(value, "jobId"), stringField(value, "status"), stringField(value, "command"))
		if cwd := stringField(value, "cwd"); cwd != "" {
			fmt.Fprintf(&b, "cwd: %s\n", cwd)
		}
		if code, ok := numberField(value, "exitCode"); ok {
			fmt.Fprintf(&b, "exitCode: %.0f\n", code)
		} else {
			b.WriteString("exitCode: unavailable\n")
		}
		if termination := stringField(value, "termination"); termination != "" {
			fmt.Fprintf(&b, "termination: %s\n", termination)
		}
		if source := stringField(value, "terminationSource"); source != "" {
			fmt.Fprintf(&b, "terminationSource: %s\n", source)
		}
		if worker := stringField(value, "workerError"); worker != "" {
			fmt.Fprintf(&b, "workerError: %s\n", worker)
		}
		fmt.Fprintf(&b, "stdoutBytes: %.0f\nstderrBytes: %.0f\n",
			numberOrZero(value, "stdoutBytes"), numberOrZero(value, "stderrBytes"))
		return b.String()
	case "job_logs":
		stdout := mapField(value, "stdout")
		stderr := mapField(value, "stderr")
		return fmt.Sprintf(
			"job: %.0f\nstdoutNextOffset: %.0f\nstderrNextOffset: %.0f\n\nstdout (%s):\n%s\n\nstderr (%s):\n%s",
			numberOrZero(value, "jobId"),
			numberOrZero(stdout, "nextOffset"),
			numberOrZero(stderr, "nextOffset"),
			stringField(stdout, "encoding"),
			stringField(stdout, "data"),
			stringField(stderr, "encoding"),
			stringField(stderr, "data"),
		)
	case "job_list":
		items, _ := value["jobs"].([]any)
		if len(items) == 0 {
			return "No jobs."
		}
		var b strings.Builder
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			fmt.Fprintf(&b, "[%.0f] %s - %s\n",
				numberOrZero(item, "job_id"), stringField(item, "status"), stringField(item, "command"))
		}
		return b.String()
	case "shell_start":
		text := fmt.Sprintf(
			"shell %.0f started\nstatus: %s\nbackend: %s",
			numberOrZero(value, "shellId"),
			stringField(value, "status"),
			stringField(value, "backend"),
		)
		if cwd := stringField(value, "initialCwd"); cwd != "" {
			text += "\ninitialCwd: " + cwd
		}
		return text
	case "shell_write":
		return fmt.Sprintf(
			"shell: %.0f\nsentBytes: %.0f\nenter: %t",
			numberOrZero(value, "shellId"),
			numberOrZero(value, "sentBytes"),
			boolField(value, "enter"),
		)
	case "shell_read":
		stdout := mapField(value, "stdout")
		stderr := mapField(value, "stderr")
		text := fmt.Sprintf("shell: %.0f\nstatus: %s",
			numberOrZero(value, "shellId"), stringField(value, "status"))
		if source := stringField(value, "terminationSource"); source != "" {
			text += "\nterminationSource: " + source
		}
		return fmt.Sprintf(
			"%s\nstdoutNextOffset: %.0f\nstderrNextOffset: %.0f\n\nstdout (%s):\n%s\n\nstderr (%s):\n%s",
			text,
			numberOrZero(stdout, "nextOffset"),
			numberOrZero(stderr, "nextOffset"),
			stringField(stdout, "encoding"),
			stringField(stdout, "data"),
			stringField(stderr, "encoding"),
			stringField(stderr, "data"),
		)
	case "shell_kill":
		text := fmt.Sprintf(
			"shell: %.0f\nstatus: %s",
			numberOrZero(value, "shellId"), stringField(value, "status"))
		if termination := stringField(value, "termination"); termination != "" {
			text += "\ntermination: " + termination
		}
		if source := stringField(value, "terminationSource"); source != "" {
			text += "\nterminationSource: " + source
		}
		return text
	case "shell_list":
		items, _ := value["shells"].([]any)
		if len(items) == 0 {
			return "No shells."
		}
		var b strings.Builder
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			fmt.Fprintf(&b, "[%.0f] %s", numberOrZero(item, "shellId"), stringField(item, "status"))
			if cwd := stringField(item, "initialCwd"); cwd != "" {
				fmt.Fprintf(&b, " - %s", cwd)
			}
			b.WriteByte('\n')
		}
		return b.String()
	case "file_stat":
		return fmt.Sprintf(
			"path: %s\nkind: %s\nsize: %.0f bytes\nmtimeMs: %.0f",
			stringField(value, "path"),
			stringField(value, "kind"),
			numberOrZero(value, "size"),
			numberOrZero(value, "mtimeMs"),
		)
	case "file_list":
		entries, _ := value["entries"].([]any)
		var b strings.Builder
		fmt.Fprintf(&b, "path: %s\nnextOffset: %.0f\neof: %t\n",
			stringField(value, "path"), numberOrZero(value, "nextOffset"), boolField(value, "eof"))
		for _, raw := range entries {
			entry, _ := raw.(map[string]any)
			fmt.Fprintf(&b, "%s\t%s", stringField(entry, "kind"), stringField(entry, "name"))
			if size, ok := numberField(entry, "size"); ok {
				fmt.Fprintf(&b, "\t%.0f bytes", size)
			}
			b.WriteByte('\n')
		}
		return b.String()
	case "file_read":
		content := mapField(value, "content")
		return fmt.Sprintf(
			"path: %s\nsize: %.0f\noffset: %.0f\nnextOffset: %.0f\neof: %t\nencoding: %s\n\n%s",
			stringField(value, "path"),
			numberOrZero(value, "size"),
			numberOrZero(value, "offset"),
			numberOrZero(value, "nextOffset"),
			boolField(value, "eof"),
			stringField(content, "encoding"),
			stringField(content, "data"),
		)
	case "file_write":
		return fmt.Sprintf(
			"path: %s\nbytesWritten: %.0f\nsize: %.0f\nappended: %t",
			stringField(value, "path"),
			numberOrZero(value, "bytesWritten"),
			numberOrZero(value, "size"),
			boolField(value, "appended"),
		)
	case "file_mkdir":
		return fmt.Sprintf("path: %s\nrecursive: %t", stringField(value, "path"), boolField(value, "recursive"))
	case "browser_status":
		return formatBrowserStatus(value)
	case "browser_snapshot":
		data := mapField(value, "data")
		if snapshot := stringField(data, "snapshot"); snapshot != "" {
			return snapshot
		}
		encoded, _ := json.Marshal(value)
		return string(encoded)
	case "browser_get":
		data := mapField(value, "data")
		if title := stringField(data, "title"); title != "" {
			return title
		}
		if url := stringField(data, "url"); url != "" {
			return url
		}
		encoded, _ := json.Marshal(value)
		return string(encoded)
	case "browser_takeover":
		browser := mapField(value, "browser")
		text := formatBrowserStatus(browser)
		if boolField(value, "snapshotRequired") {
			text += "\nFresh browser_snapshot required before ref-based actions."
		}
		return text
	default:
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
}

func formatBrowserStatus(value map[string]any) string {
	enabled := true
	if value != nil {
		if flag, ok := value["enabled"].(bool); ok {
			enabled = flag
		}
	}
	available := boolField(value, "available")
	active := boolField(value, "active")
	var b strings.Builder
	fmt.Fprintf(&b, "enabled: %t\navailable: %t\nactive: %t", enabled, available, active)
	if !enabled {
		b.WriteString("\nBrowser functionality was not enabled when this ShellCore started.")
	}
	if mode := stringField(value, "mode"); mode != "" {
		fmt.Fprintf(&b, "\nmode: %s", mode)
	}
	fmt.Fprintf(&b, "\nvisible: %t", boolField(value, "visible"))
	if owner := stringField(value, "owner"); owner != "" {
		fmt.Fprintf(&b, "\nowner: %s", owner)
	}
	if profile := stringField(value, "profile"); profile != "" {
		fmt.Fprintf(&b, "\nprofile: %s", profile)
	}
	if executable := stringField(value, "agentBrowserExecutable"); executable != "" {
		fmt.Fprintf(&b, "\nagent-browser: %s", executable)
	}
	if executable := stringField(value, "browserExecutable"); executable != "" {
		fmt.Fprintf(&b, "\nbrowser: %s", executable)
	}
	return b.String()
}

func stringField(value map[string]any, name string) string {
	if value == nil {
		return ""
	}
	text, _ := value[name].(string)
	return text
}

func boolField(value map[string]any, name string) bool {
	if value == nil {
		return false
	}
	flag, _ := value[name].(bool)
	return flag
}

func numberField(value map[string]any, name string) (float64, bool) {
	if value == nil {
		return 0, false
	}
	number, ok := value[name].(float64)
	return number, ok
}

func numberOrZero(value map[string]any, name string) float64 {
	number, _ := numberField(value, name)
	return number
}

func mapField(value map[string]any, name string) map[string]any {
	if value == nil {
		return nil
	}
	nested, _ := value[name].(map[string]any)
	return nested
}

func boolPtr(value bool) *bool { return &value }

func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func commandProperty(description string) map[string]any {
	return map[string]any{
		"type":        "string",
		"description": description,
		"minLength":   1,
	}
}

func idProperty(description string) map[string]any {
	return map[string]any{
		"type":        "integer",
		"description": description,
		"minimum":     1,
		"maximum":     int64(^uint64(0) >> 1),
	}
}

func pathProperty(description string) map[string]any {
	return map[string]any{
		"type":        "string",
		"description": description,
		"minLength":   1,
	}
}

func nonNegativeIntegerProperty(description string, maximum int64) map[string]any {
	return map[string]any{
		"type":        "integer",
		"description": description,
		"minimum":     0,
		"maximum":     maximum,
	}
}

func outputCursorProperty(stream string) map[string]any {
	return map[string]any{
		"type":        "integer",
		"description": fmt.Sprintf("Optional absolute %s byte offset returned as nextOffset by the previous read. When provided, only newer retained output is returned.", stream),
		"minimum":     0,
		"maximum":     int64(^uint64(0) >> 1),
	}
}

func streamReadSchema(idName, idDescription string) map[string]any {
	return objectSchema(map[string]any{
		idName:        idProperty(idDescription),
		"stdoutAfter": outputCursorProperty("stdout"),
		"stderrAfter": outputCursorProperty("stderr"),
	}, idName)
}

func stringProperty(description string) map[string]any {
	return map[string]any{
		"type":        "string",
		"description": description,
	}
}

func enumStringProperty(description string, values ...string) map[string]any {
	return map[string]any{
		"type":        "string",
		"description": description,
		"enum":        values,
	}
}

func boolProperty(description string) map[string]any {
	return map[string]any{
		"type":        "boolean",
		"description": description,
	}
}

func browserRefProperty(description string) map[string]any {
	return map[string]any{
		"type":        "string",
		"description": description + " Use a ref returned by browser_snapshot, such as e1 or @e1.",
		"pattern":     "^@?e[0-9]+$",
	}
}

func toolSpecs() []toolSpec {
	empty := func() map[string]any { return objectSchema(map[string]any{}) }
	jobID := func(description string) map[string]any {
		return objectSchema(map[string]any{"jobId": idProperty(description)}, "jobId")
	}
	shellID := func(description string) map[string]any {
		return objectSchema(map[string]any{"shellId": idProperty(description)}, "shellId")
	}

	return []toolSpec{
		{
			Name:        "environment_info",
			Description: "Return information about the local execution environment.",
			InputSchema: empty(),
			ReadOnly:    true,
		},
		{
			Name:        "control_status",
			Description: "Report whether the agent currently owns zshell execution control.",
			InputSchema: empty(),
			ReadOnly:    true,
		},
		{
			Name:        "exec",
			Description: "Execute a shell command and wait for it to finish. Intended for short-lived commands.",
			InputSchema: objectSchema(map[string]any{
				"command": commandProperty("Shell command to execute."),
				"cwd": map[string]any{
					"type":        "string",
					"description": "Optional working directory. May be any path accessible to the current OS user.",
				},
				"timeoutMs": map[string]any{
					"type":        "integer",
					"description": "Command timeout in milliseconds.",
					"minimum":     1,
					"maximum":     3600000,
				},
			}, "command"),
		},
		{
			Name:        "job_start",
			Description: "Start a long-running shell command in the background and return immediately with a job ID.",
			InputSchema: objectSchema(map[string]any{
				"command": commandProperty("Shell command to start in the background."),
				"cwd": map[string]any{
					"type":        "string",
					"description": "Optional working directory. May be any path accessible to the current OS user.",
				},
			}, "command"),
		},
		{
			Name:        "job_status",
			Description: "Return the current status of a background job.",
			InputSchema: jobID("ID of the background job."),
			ReadOnly:    true,
		},
		{
			Name:        "job_logs",
			Description: "Return captured stdout and stderr from a background job. Optional stdoutAfter/stderrAfter cursors request only output newer than the previous read.",
			InputSchema: streamReadSchema("jobId", "ID of the background job."),
			ReadOnly:    true,
		},
		{
			Name:        "job_stop",
			Description: "Stop a running background job and wait for it to terminate.",
			InputSchema: jobID("ID of the background job to stop."),
		},
		{
			Name:        "job_list",
			Description: "List background jobs managed by zshell.",
			InputSchema: empty(),
			ReadOnly:    true,
		},
		{
			Name:        "shell_start",
			Description: "Start a persistent shell session whose working directory and shell state survive across writes.",
			InputSchema: objectSchema(map[string]any{
				"cwd": map[string]any{
					"type":        "string",
					"description": "Optional initial working directory. May be any path accessible to the current OS user.",
				},
			}),
		},
		{
			Name:        "shell_write",
			Description: "Write input to a persistent shell session. By default a platform newline is appended.",
			InputSchema: objectSchema(map[string]any{
				"shellId": idProperty("ID of the persistent shell session."),
				"input": map[string]any{
					"type":        "string",
					"description": "Text to write to the shell standard input.",
				},
				"enter": map[string]any{
					"type":        "boolean",
					"description": "Whether to append a platform newline after the input. Defaults to true.",
				},
			}, "shellId", "input"),
		},
		{
			Name:        "shell_read",
			Description: "Read stdout and stderr from a persistent shell session. Optional stdoutAfter/stderrAfter cursors request only output newer than the previous read.",
			InputSchema: streamReadSchema("shellId", "ID of the persistent shell session."),
			ReadOnly:    true,
		},
		{
			Name:        "shell_kill",
			Description: "Terminate a persistent shell session and wait for it to stop.",
			InputSchema: shellID("ID of the persistent shell session to terminate."),
		},
		{
			Name:        "shell_list",
			Description: "List persistent shell sessions managed by zshell.",
			InputSchema: empty(),
			ReadOnly:    true,
		},
		{
			Name:        "file_stat",
			Description: "Return metadata for a file-system path without reading its contents.",
			InputSchema: objectSchema(map[string]any{
				"path": pathProperty("File or directory path. Relative paths are resolved from the zshell workspace."),
			}, "path"),
			ReadOnly: true,
		},
		{
			Name:        "file_list",
			Description: "List entries in a directory. Supports bounded pagination by entry offset.",
			InputSchema: objectSchema(map[string]any{
				"path":   pathProperty("Directory path. Defaults to the zshell workspace."),
				"offset": nonNegativeIntegerProperty("Entry offset to start from. Defaults to 0.", int64(^uint64(0)>>1)),
				"maxEntries": map[string]any{
					"type": "integer", "description": "Maximum entries to return. Defaults to 200.", "minimum": 1, "maximum": 1000,
				},
			}),
			ReadOnly: true,
		},
		{
			Name:        "file_read",
			Description: "Read a bounded byte range from a file. UTF-8 is returned directly; arbitrary binary data is returned as base64.",
			InputSchema: objectSchema(map[string]any{
				"path":   pathProperty("File path to read."),
				"offset": nonNegativeIntegerProperty("Absolute byte offset to start reading from. Defaults to 0.", int64(^uint64(0)>>1)),
				"maxBytes": map[string]any{
					"type": "integer", "description": "Maximum bytes to read. Defaults to 262144 and is capped at 1048576.", "minimum": 1, "maximum": 1048576,
				},
			}, "path"),
			ReadOnly: true,
		},
		{
			Name:        "file_write",
			Description: "Write or append data to a file. Data may be UTF-8 text or base64-encoded binary, with a decoded limit of 4 MiB per call.",
			InputSchema: objectSchema(map[string]any{
				"path": pathProperty("File path to create or modify."),
				"data": map[string]any{
					"type": "string", "description": "Text data or base64 data according to encoding.",
				},
				"encoding": map[string]any{
					"type": "string", "description": "Input encoding. Defaults to utf8.", "enum": []string{"utf8", "base64"},
				},
				"append": map[string]any{
					"type": "boolean", "description": "Append instead of replacing the file. Defaults to false.",
				},
			}, "path", "data"),
		},
		{
			Name:        "file_mkdir",
			Description: "Create a directory. Recursive parent creation is enabled by default.",
			InputSchema: objectSchema(map[string]any{
				"path": pathProperty("Directory path to create."),
				"recursive": map[string]any{
					"type": "boolean", "description": "Create missing parent directories. Defaults to true.",
				},
			}, "path"),
		},
		{
			Name:        "browser_status",
			Description: "Return whether browser functionality was enabled for this ShellCore, plus runtime availability, active session state, ownership, profile mode, and discovered executables.",
			InputSchema: empty(),
			ReadOnly:    true,
		},
		{
			Name:        "browser_start",
			Description: "Start the zshell-managed browser session. The selected ShellCore must have been started with --browser. temporary mode is isolated and disposable; persistent mode stores zshell-managed profile state; chrome_profile reuses a named installed Chrome profile. Set visible=true when the user may take over the browser.",
			InputSchema: objectSchema(map[string]any{
				"mode":    enumStringProperty("Browser session mode. Defaults to temporary.", "temporary", "persistent", "chrome_profile"),
				"visible": boolProperty("Show the browser window. Defaults to false. Required for human takeover."),
				"profile": stringProperty("Optional profile name. persistent defaults to default; chrome_profile requires a Chrome profile name."),
			}),
		},
		{
			Name:        "browser_open",
			Description: "Navigate the active zshell browser tab to a URL.",
			InputSchema: objectSchema(map[string]any{
				"url": commandProperty("URL to open."),
			}, "url"),
		},
		{
			Name:        "browser_snapshot",
			Description: "Return the accessibility snapshot and stable element refs for the active page. Use refs from this result with browser_click, browser_fill, browser_select, browser_check, browser_upload, and browser_download.",
			InputSchema: objectSchema(map[string]any{
				"interactiveOnly": boolProperty("Return only interactive elements. Defaults to true."),
			}),
			ReadOnly: true,
		},
		{
			Name:        "browser_click",
			Description: "Click an element ref from browser_snapshot.",
			InputSchema: objectSchema(map[string]any{
				"ref":    browserRefProperty("Element to click."),
				"newTab": boolProperty("Open the click target in a new tab when supported. Defaults to false."),
			}, "ref"),
		},
		{
			Name:        "browser_fill",
			Description: "Clear and fill an input element identified by a browser_snapshot ref.",
			InputSchema: objectSchema(map[string]any{
				"ref":  browserRefProperty("Input element to fill."),
				"text": stringProperty("Text to enter."),
			}, "ref", "text"),
		},
		{
			Name:        "browser_select",
			Description: "Select a value in a select element identified by a browser_snapshot ref.",
			InputSchema: objectSchema(map[string]any{
				"ref":   browserRefProperty("Select element."),
				"value": stringProperty("Option value or label to select."),
			}, "ref", "value"),
		},
		{
			Name:        "browser_check",
			Description: "Check or uncheck a checkbox/radio-like element identified by a browser_snapshot ref.",
			InputSchema: objectSchema(map[string]any{
				"ref":     browserRefProperty("Element to check or uncheck."),
				"checked": boolProperty("Desired checked state. Defaults to true."),
			}, "ref"),
		},
		{
			Name:        "browser_press",
			Description: "Press a keyboard key in the active browser page, such as Enter, Tab, Escape, or Control+a.",
			InputSchema: objectSchema(map[string]any{
				"key": commandProperty("Key or key chord to press."),
			}, "key"),
		},
		{
			Name:        "browser_upload",
			Description: "Upload one or more local device files through a file-input element identified by a browser_snapshot ref.",
			InputSchema: objectSchema(map[string]any{
				"ref": browserRefProperty("File input element."),
				"files": map[string]any{
					"type": "array", "description": "Local file paths on the selected ShellCore device.", "minItems": 1, "maxItems": 16,
					"items": pathProperty("Local file path to upload."),
				},
			}, "ref", "files"),
		},
		{
			Name:        "browser_download",
			Description: "Click a downloadable element ref and save the result to a local path on the selected ShellCore device.",
			InputSchema: objectSchema(map[string]any{
				"ref":  browserRefProperty("Download element."),
				"path": pathProperty("Destination path on the selected ShellCore device."),
			}, "ref", "path"),
		},
		{
			Name:        "browser_tabs",
			Description: "List, create, switch, or close browser tabs. Tab IDs such as t1 are returned by the list action.",
			InputSchema: objectSchema(map[string]any{
				"action": enumStringProperty("Tab action. Defaults to list.", "list", "new", "switch", "close"),
				"tab":    stringProperty("Tab ID, label, or target ID for switch/close."),
				"url":    stringProperty("Optional URL for a new tab."),
				"label":  stringProperty("Optional stable label for a new tab."),
			}),
		},
		{
			Name:        "browser_wait",
			Description: "Wait for the active page to reach a load state.",
			InputSchema: objectSchema(map[string]any{
				"state": enumStringProperty("Page load state to wait for.", "load", "domcontentloaded", "networkidle"),
			}, "state"),
			ReadOnly: true,
		},
		{
			Name:        "browser_get",
			Description: "Return the current browser page URL or title.",
			InputSchema: objectSchema(map[string]any{
				"what": enumStringProperty("Page information to return.", "url", "title"),
			}, "what"),
			ReadOnly: true,
		},
		{
			Name:        "browser_screenshot",
			Description: "Capture the active browser page to an image file on the selected ShellCore device. Use file_read afterwards if the image bytes are needed.",
			InputSchema: objectSchema(map[string]any{
				"path": pathProperty("Optional output path. If omitted, agent-browser chooses a temporary screenshot path."),
				"full": boolProperty("Capture the full page instead of the viewport. Defaults to false."),
			}),
		},
		{
			Name:        "browser_takeover",
			Description: "Transfer browser ownership between agent and human. Human takeover requires a visible browser. After control returns to agent, take a fresh browser_snapshot before reusing refs.",
			InputSchema: objectSchema(map[string]any{
				"owner": enumStringProperty("New browser owner.", "human", "agent"),
			}, "owner"),
		},
		{
			Name:        "browser_close",
			Description: "Close the active zshell-managed browser session.",
			InputSchema: empty(),
		},
	}
}
