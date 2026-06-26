package utils

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SaveCache atomically saves data to the specified cache file.
// It writes to a temporary file in the same directory and then renames it so
// that readers never see a partial write. The file is created with mode 0600.
func SaveCache(filePath string, data interface{}) error {
	dir := filepath.Dir(filePath)

	// Write to a temp file in the same directory so os.Rename is atomic.
	tmp, err := os.CreateTemp(dir, ".cache-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp cache file: %v", err)
	}
	tmpName := tmp.Name()

	// Ensure the temp file is cleaned up on any error path.
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpName)
		}
	}()

	// Restrict permissions to owner-read/write only (0600).
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to set cache file permissions: %v", err)
	}

	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(data); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to encode data to cache file: %v", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close temp cache file: %v", err)
	}

	if err := os.Rename(tmpName, filePath); err != nil {
		return fmt.Errorf("failed to commit cache file: %v", err)
	}

	success = true
	return nil
}

// LoadCache loads data from the specified cache file.
func LoadCache(filePath string, data interface{}) error {
	// Read the file
	fileContent, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read cache file: %v", err)
	}

	// Decode the data from the JSON file
	err = json.Unmarshal(fileContent, data)
	if err != nil {
		return fmt.Errorf("failed to decode data from cache file: %v", err)
	}

	return nil
}
