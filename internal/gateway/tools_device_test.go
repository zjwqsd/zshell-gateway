package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWithDeviceSchemaKeepsDeviceDynamic(t *testing.T) {
	base := objectSchema(map[string]any{
		"command": commandProperty("command"),
	}, "command")

	schema := withDeviceSchema(base)
	properties := schema["properties"].(map[string]any)
	deviceProperty := properties["device"].(map[string]any)
	if got := deviceProperty["type"]; got != "string" {
		t.Fatalf("device type=%v, want string", got)
	}
	if _, exists := deviceProperty["enum"]; exists {
		t.Fatal("device schema must not enumerate live device names")
	}

	originalProperties := base["properties"].(map[string]any)
	if _, exists := originalProperties["device"]; exists {
		t.Fatal("withDeviceSchema mutated the base schema")
	}
}

func TestSplitDeviceArguments(t *testing.T) {
	name, forwarded, err := splitDeviceArguments(json.RawMessage(`{"device":"alpha","command":"pwd"}`))
	if err != nil {
		t.Fatal(err)
	}
	if name != "alpha" {
		t.Fatalf("device=%q, want alpha", name)
	}

	var fields map[string]any
	if err := json.Unmarshal(forwarded, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["device"]; exists {
		t.Fatal("device selector was forwarded to ShellCore")
	}
	if fields["command"] != "pwd" {
		t.Fatalf("command=%v", fields["command"])
	}
}

func TestFormatBrowserStatusDisabled(t *testing.T) {
	text := formatBrowserStatus(map[string]any{
		"enabled":   false,
		"available": false,
		"active":    false,
	})
	if text != "enabled: false\navailable: false\nactive: false\nBrowser functionality was not enabled when this ShellCore started.\nvisible: false" {
		t.Fatalf("unexpected browser status text: %q", text)
	}
}

func TestFormatBrowserStatusLegacyCoreDefaultsEnabled(t *testing.T) {
	text := formatBrowserStatus(map[string]any{
		"available": true,
		"active":    false,
	})
	if !strings.HasPrefix(text, "enabled: true\navailable: true") {
		t.Fatalf("legacy browser status should default to enabled: %q", text)
	}
}

func TestJobStartSchemaIsDirectProcessOnly(t *testing.T) {
	var spec *toolSpec
	for _, candidate := range toolSpecs() {
		if candidate.Name == "job_start" {
			copy := candidate
			spec = &copy
			break
		}
	}
	if spec == nil {
		t.Fatal("job_start tool missing")
	}
	properties := spec.InputSchema["properties"].(map[string]any)
	for _, name := range []string{"program", "args", "cwd"} {
		if _, ok := properties[name]; !ok {
			t.Fatalf("job_start schema missing %q", name)
		}
	}
	if _, ok := properties["command"]; ok {
		t.Fatal("job_start must not expose command shell compatibility")
	}
}

func TestFormatDirectJobInvocationPreservesArguments(t *testing.T) {
	text := formatToolText("job_status", map[string]any{
		"jobId":   7.0,
		"status":  "running",
		"program": "python",
		"args":    []any{"train.py", "--name", "hello world"},
	})
	if !strings.Contains(text, "program: python") || !strings.Contains(text, `"hello world"`) {
		t.Fatalf("unexpected direct job text: %q", text)
	}
}

func TestShellStartSchemaSupportsTerminalSelectionAndSizing(t *testing.T) {
	for _, spec := range toolSpecs() {
		if spec.Name != "shell_start" {
			continue
		}
		properties := spec.InputSchema["properties"].(map[string]any)
		for _, name := range []string{"shell", "args", "cwd", "cols", "rows"} {
			if _, ok := properties[name]; !ok {
				t.Fatalf("shell_start schema missing %q", name)
			}
		}
		return
	}
	t.Fatal("shell_start tool missing")
}

func TestFileEditingAndSearchToolSchemas(t *testing.T) {
	found := map[string]toolSpec{}
	for _, spec := range toolSpecs() {
		if spec.Name == "file_search" || spec.Name == "file_patch" {
			found[spec.Name] = spec
		}
	}

	search, ok := found["file_search"]
	if !ok {
		t.Fatal("file_search tool missing")
	}
	if !search.ReadOnly {
		t.Fatal("file_search must be read-only")
	}
	searchProps := search.InputSchema["properties"].(map[string]any)
	for _, name := range []string{"path", "query", "glob", "maxResults"} {
		if _, ok := searchProps[name]; !ok {
			t.Fatalf("file_search schema missing %q", name)
		}
	}

	patch, ok := found["file_patch"]
	if !ok {
		t.Fatal("file_patch tool missing")
	}
	if patch.ReadOnly {
		t.Fatal("file_patch must be mutating")
	}
	patchProps := patch.InputSchema["properties"].(map[string]any)
	for _, name := range []string{"path", "patch"} {
		if _, ok := patchProps[name]; !ok {
			t.Fatalf("file_patch schema missing %q", name)
		}
	}
}

func TestFormatFileSearchAndPatch(t *testing.T) {
	searchText := formatToolText("file_search", map[string]any{
		"path":      "src",
		"query":     "needle",
		"truncated": false,
		"matches": []any{
			map[string]any{"path": "src/a.zig", "line": 12.0, "column": 4.0, "text": "xxxneedle"},
		},
	})
	if !strings.Contains(searchText, "src/a.zig:12:4: xxxneedle") {
		t.Fatalf("unexpected file_search text: %q", searchText)
	}

	patchText := formatToolText("file_patch", map[string]any{
		"path":         "src/a.zig",
		"hunksApplied": 2.0,
		"size":         123.0,
	})
	if patchText != "path: src/a.zig\nhunksApplied: 2\nsize: 123" {
		t.Fatalf("unexpected file_patch text: %q", patchText)
	}
}
