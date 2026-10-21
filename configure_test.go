package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigureAppendsAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[ui]\nstatus_indicators = \"symbols\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := configureFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if result != configureAppended {
		t.Fatalf("first configure = %v, want appended", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[ui.sidebar.agents]") || !strings.Contains(string(data), "$turn_time") {
		t.Fatalf("config missing sidebar row:\n%s", data)
	}
	if !strings.Contains(string(data), "[ui]\nstatus_indicators") {
		t.Fatal("existing config was not preserved")
	}

	result, err = configureFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if result != configureAlready {
		t.Fatalf("second configure = %v, want already", result)
	}
}

func TestConfigureCreatesMissingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr", "config.toml")
	result, err := configureFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if result != configureAppended {
		t.Fatalf("configure = %v, want appended", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[\"agent\", \"$turn_time\"]") {
		t.Fatalf("unexpected config:\n%s", data)
	}
}

func TestConfigureLeavesCustomRowsAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	custom := `[ui.sidebar.agents]
rows = [["state_icon", "agent", "state_text"], ["workspace"]]
`
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := configureFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if result != configureNeedsManual {
		t.Fatalf("configure = %v, want needs-manual", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != custom {
		t.Fatalf("custom config changed:\n%s", data)
	}
}

func TestConfigureDetectsExistingToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	custom := `[ui.sidebar.agents]
rows = [["state_icon", "agent", "$turn_time"]]

[ui.sidebar.agents.rows_by_agent]
claude = [["state_icon", "agent", "$turn_time"]]
`
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := configureFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if result != configureAlready {
		t.Fatalf("configure = %v, want already", result)
	}
}
