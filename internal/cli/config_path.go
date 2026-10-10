package cli

import (
	"fmt"
	"os"
	"path/filepath"
)

// Selection never skips an existing but invalid file; validation belongs to config.Load.
func resolveConfig(explicit, host string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	if host != "" {
		return filepath.Abs(host)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	path := filepath.Join(filepath.Dir(exe), "config.toml")
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("cannot access model configuration")
		}
		return "", fmt.Errorf("config.toml not found next to executable; use --config FILE")
	}
	return path, nil
}

// Missing default settings preserve built-in defaults; explicit files are required.
func resolveSettings(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	path := filepath.Join(filepath.Dir(exe), "settings.json")
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("cannot access settings.json next to executable")
	}
	return path, nil
}
