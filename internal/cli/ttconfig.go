package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/aisidev/tt/internal/auth"
)

type cliConfig struct {
	address string
	key     *string
}

func configPath() (string, error) {
	home, ok := os.LookupEnv("HOME")
	if !ok || home == "" || !utf8.ValidString(home) {
		return "", fmt.Errorf("HOME must be set to store CLI credentials")
	}
	return home + "/.ttconfig", nil
}
func loadConfig(path string) *cliConfig {
	data, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(data) {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	address := strings.TrimSpace(lines[0])
	if address == "" {
		return nil
	}
	result := &cliConfig{address: address}
	if len(lines) > 1 {
		key := strings.TrimSpace(lines[1])
		if key != "" {
			result.key = &key
		}
	}
	return result
}
func saveConfig(path, address string, key *string) error {
	if strings.ContainsAny(address, "\n\r") || strings.TrimSpace(address) == "" {
		return fmt.Errorf("invalid controller address")
	}
	if key == nil {
		if saved := loadConfig(path); saved != nil && saved.address == address {
			key = saved.key
		}
	}
	if key != nil {
		if _, err := auth.ParseAPIKey(*key); err != nil {
			return err
		}
	}
	content := address + "\n"
	if key != nil {
		content += *key + "\n"
	}
	return saveConfigFile(path, content)
}
func saveConfigFile(path, content string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".ttconfig-*")
	if err != nil {
		return fmt.Errorf("config staging: %w", err)
	}
	defer file.Close()
	defer os.Remove(file.Name())
	if _, err := file.WriteString(content); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync config: %w", err)
	}
	return os.Rename(file.Name(), path)
}
func resolveConfig(server, key *string, saved *cliConfig) (string, *string, error) {
	if server != nil {
		selected := key
		if selected == nil && saved != nil && saved.address == *server {
			selected = saved.key
		}
		return *server, selected, nil
	}
	if saved != nil {
		selected := key
		if selected == nil {
			selected = saved.key
		}
		return saved.address, selected, nil
	}
	//lint:ignore ST1005 Preserve the pinned compatibility diagnostic.
	return "", nil, fmt.Errorf("No controller address. Run: tt config <addr>")
}
