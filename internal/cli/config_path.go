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
