package token

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultTokenPath returns the default path for the login token file.
func DefaultTokenPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kiro-bridge", "token.json")
}

// LoadLoginToken reads a LoginToken from a JSON file.
func LoadLoginToken(path string) (*LoginToken, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lt LoginToken
	if err := json.Unmarshal(data, &lt); err != nil {
		return nil, fmt.Errorf("parse token file: %w", err)
	}
	return &lt, nil
}

// SaveLoginToken atomically writes a LoginToken to a JSON file with 0600 permissions.
func SaveLoginToken(path string, lt *LoginToken) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create token dir: %w", err)
	}

	data, err := json.MarshalIndent(lt, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal token: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename token file: %w", err)
	}
	return nil
}
