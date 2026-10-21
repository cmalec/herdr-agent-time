package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const sidebarSnippet = `[ui.sidebar.agents]
rows = [
  ["state_icon", "machine", "workspace", "tab"],
  ["agent", "$turn_time"],
]
`

type configureResult int

const (
	configureAlready configureResult = iota
	configureAppended
	configureNeedsManual
)

func herdrConfigPath() (string, error) {
	if path := os.Getenv("HERDR_CONFIG_PATH"); path != "" {
		return path, nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "herdr", "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "herdr", "config.toml"), nil
}

// agentTable returns the body of [ui.sidebar.agents] and whether the table
// exists. Lines are returned without their trailing newline.
func agentTable(text string) (body []string, found bool) {
	lines := strings.Split(text, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "[ui.sidebar.agents]" {
			start = i + 1
			break
		}
	}
	if start == -1 {
		return nil, false
	}
	for i := start; i < len(lines); i++ {
		if isTableHeader(lines[i]) {
			return lines[start:i], true
		}
	}
	return lines[start:], true
}

func isTableHeader(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "[") {
		return false
	}
	if comment := strings.Index(trimmed, "#"); comment >= 0 {
		trimmed = strings.TrimSpace(trimmed[:comment])
	}
	if !strings.HasSuffix(trimmed, "]") {
		return false
	}
	// Array rows contain quotes and commas; table headers do not.
	return !strings.ContainsAny(trimmed, "\",")
}

func configureFile(path string) (configureResult, error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	text := string(data)

	if body, found := agentTable(text); found {
		for _, line := range body {
			if strings.Contains(line, "$turn_time") {
				return configureAlready, nil
			}
		}
		return configureNeedsManual, nil
	}

	if len(text) > 0 && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if len(text) > 0 && !strings.HasSuffix(text, "\n\n") {
		text += "\n"
	}
	text += "# Added by cmalec.agent-time: current turn runtime in the agent sidebar.\n" + sidebarSnippet

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
		backup := fmt.Sprintf("%s.bak-agent-time-%d", path, time.Now().Unix())
		_ = os.WriteFile(backup, data, mode)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		return 0, err
	}
	return configureAppended, nil
}

func configureSidebar() error {
	path, err := herdrConfigPath()
	if err != nil {
		return err
	}
	result, err := configureFile(path)
	if err != nil {
		return err
	}
	switch result {
	case configureAlready:
		fmt.Printf("%s already uses $turn_time in [ui.sidebar.agents]\n", path)
		return nil
	case configureNeedsManual:
		fmt.Printf("%s already has [ui.sidebar.agents] without $turn_time; add it next to \"agent\" in that table:\n\n%s", path, sidebarSnippet)
		return nil
	}

	fmt.Printf("added the agent timer row to %s\n", path)
	bin := os.Getenv("HERDR_BIN_PATH")
	if bin == "" {
		bin = "herdr"
	}
	cmd := exec.Command(bin, "server", "reload-config")
	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("config written; reload manually with: herdr server reload-config (%v)\n%s", err, output)
		return nil
	}
	fmt.Printf("reloaded Herdr config: %s\n", strings.TrimSpace(string(output)))
	return nil
}
