package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// hookEvents mirrors scripts/install-hooks.sh: the events worth capturing for
// session observability. Deliberately excludes high-frequency (MessageDisplay)
// and sensitive (Elicitation*) events — see the shell script for the rationale.
var hookEvents = []string{
	"SessionStart", "SessionEnd",
	"UserPromptSubmit", "UserPromptExpansion", "Stop", "StopFailure",
	"PreToolUse", "PostToolUse", "PostToolUseFailure", "PostToolBatch",
	"SubagentStart", "SubagentStop",
	"PreCompact", "PostCompact",
	"Notification",
	"TaskCreated", "TaskCompleted",
	"ConfigChange", "CwdChanged", "InstructionsLoaded",
	"PermissionRequest", "PermissionDenied",
	"FileChanged", "WorktreeCreate", "WorktreeRemove",
}

// settingsTarget resolves the settings.json to edit and a human label for it.
func settingsTarget(project bool, dir string) (path, scope string, err error) {
	if project {
		if dir == "" {
			if dir, err = os.Getwd(); err != nil {
				return "", "", err
			}
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", "", err
		}
		return filepath.Join(abs, ".claude", "settings.json"), "this project", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), "all projects", nil
}

// Install registers the hook command into a Claude Code settings.json. It
// installs a stable copy of the running binary (so the hook path survives an
// npx/cache eviction or a `brew upgrade` symlink swap), then appends a hook
// group for each event — but only where ours isn't already present, so other
// tools' hooks are never touched. settings.json is backed up first.
func Install(settingsPath string) error {
	bin, err := installStableBinary()
	if err != nil {
		return fmt.Errorf("install hook binary: %w", err)
	}
	command := hookCommand(bin)

	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return err
	}

	settings := map[string]any{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		if len(strings.TrimSpace(string(data))) > 0 {
			if err := json.Unmarshal(data, &settings); err != nil {
				return fmt.Errorf("%s is not valid JSON: %w", settingsPath, err)
			}
		}
		backup := settingsPath + ".bak-" + time.Now().Format("20060102T150405")
		if err := os.WriteFile(backup, data, 0o644); err != nil {
			return err
		}
		fmt.Println("Backed up existing settings to", backup)
	} else if !os.IsNotExist(err) {
		return err
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}

	added, skipped := 0, 0
	for _, ev := range hookEvents {
		groups, _ := hooks[ev].([]any)
		if hookGroupPresent(groups, command) {
			skipped++
			continue
		}
		hooks[ev] = append(groups, map[string]any{
			"matcher": "",
			"hooks":   []any{map[string]any{"type": "command", "command": command}},
		})
		added++
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
		return err
	}

	fmt.Printf("Done: %d event(s) newly hooked, %d already present.\n", added, skipped)
	fmt.Println("Settings file:", settingsPath)
	fmt.Println("Hook command: ", command)
	return nil
}

// hookGroupPresent reports whether any hook group already runs command, so
// re-running init is idempotent and never duplicates our entry.
func hookGroupPresent(groups []any, command string) bool {
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		hs, _ := gm["hooks"].([]any)
		for _, h := range hs {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if c, _ := hm["command"].(string); c == command {
				return true
			}
		}
	}
	return false
}

// hookCommand builds the settings.json command string for the stable binary,
// quoting the path if it contains spaces (e.g. C:\Users\Jane Doe\…).
func hookCommand(bin string) string {
	if strings.ContainsAny(bin, " \t") {
		return `"` + bin + `" hook`
	}
	return bin + " hook"
}

// installStableBinary copies the running executable to a fixed location so the
// hook command in settings.json keeps working regardless of how the binary was
// originally launched (npx cache, Homebrew Cellar symlink, a temp dir, …). If
// we're already running from that location it's a no-op.
func installStableBinary() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".observability-code", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := "observe-claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(dir, name)

	if a, err := filepath.Abs(self); err == nil && sameNormalized(a, dst) {
		return dst, nil
	}
	if err := copyFile(self, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func sameNormalized(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// Write to a temp file then rename, so a running hook is never truncated.
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
