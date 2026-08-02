package autostart

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// runKey is the per-user autostart key. HKCU needs no elevation, unlike the
// machine-wide equivalent.
const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

const supported = true

// command is the Run value: the quoted executable path, so a path containing
// spaces still launches.
func command() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("autostart: locate the executable: %w", err)
	}
	return `"` + exe + `"`, nil
}

func enabled() (bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("autostart: open the Run key: %w", err)
	}
	defer key.Close()

	value, _, err := key.GetStringValue(EntryName)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("autostart: read the Run value: %w", err)
	}

	want, err := command()
	if err != nil {
		return false, err
	}
	// An entry left behind by a copy that has since moved is stale, not
	// enabled: reporting it as enabled would leave the user unable to fix it
	// from the menu.
	return strings.EqualFold(strings.TrimSpace(value), want), nil
}

func enable() error {
	value, err := command()
	if err != nil {
		return err
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("autostart: open the Run key: %w", err)
	}
	defer key.Close()

	if err := key.SetStringValue(EntryName, value); err != nil {
		return fmt.Errorf("autostart: write the Run value: %w", err)
	}
	return nil
}

func disable() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("autostart: open the Run key: %w", err)
	}
	defer key.Close()

	if err := key.DeleteValue(EntryName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("autostart: delete the Run value: %w", err)
	}
	return nil
}
