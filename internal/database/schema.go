package database

import (
	"fmt"
	"os"
	"path/filepath"
)

// loadSchema searches the working directory and its parents for the schema.
func loadSchema() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("locate database schema: %w", err)
	}
	for {
		path := filepath.Join(dir, "scripts", "schema.sql")
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("read database schema: %w", err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("database schema scripts/schema.sql not found")
		}
		dir = parent
	}
}
