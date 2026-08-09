package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"zshell-gateway/internal/device"
)

type toolSpec struct {
	Name        string
	Description string
	InputSchema map[string]any
	ReadOnly    bool
}

func RegisterTools(server *mcp.Server, client *device.Manager) {
	for _, spec := range toolSpecs() {
		spec := spec
		readOnly := spec.ReadOnly
		destructive := !readOnly
		openWorld := !readOnly

		server.AddTool(&mcp.Tool{
			Name:        spec.Name,
			Description: spec.Description,
			InputSchema: spec.InputSchema,
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
			arguments := req.Params.Arguments
			if len(arguments) == 0 {
				arguments = json.RawMessage(`{}`)
			}

			result, failure, err := client.Call(ctx, spec.Name, arguments)
			if err != nil {
				if errors.Is(err, device.ErrNoDevice) {
					return noDeviceResult(), nil
				}
				return nil, err
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
}

func noDeviceResult() *mcp.CallToolResult {
	result := &mcp.CallToolResult{
		StructuredContent: map[string]any{
			"error":   "NoDeviceConnected",
			"message": "无设备连接",
		},
		IsError: true,
	}
	result.Content = append(result.Content, &mcp.TextContent{Text: "无设备连接"})
	return result
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
	default:
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
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
	}
}
