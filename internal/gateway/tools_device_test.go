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
