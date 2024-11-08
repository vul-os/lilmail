package utils

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
)

// SaveCache saves data to the specified cache file.
func SaveCache(filePath string, data interface{}) error {
	// Open or create the cache file
	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create cache file: %v", err)
	}
	defer file.Close()

	// Encode the data into JSON and write it to the file
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ") // Pretty print for easier inspection
	err = encoder.Encode(data)
	if err != nil {
		return fmt.Errorf("failed to encode data to cache file: %v", err)
	}

	return nil
}

// LoadCache loads data from the specified cache file.
func LoadCache(filePath string, data interface{}) error {
	// Read the file
	fileContent, err := ioutil.ReadFile(filePath)
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

// CheckCacheFile checks if the cache file exists. Returns true if the file exists, false otherwise.
func CheckCacheFile(filePath string) bool {
	_, err := os.Stat(filePath)
	return !os.IsNotExist(err)
}

// ClearCache deletes the cache file to force a reload.
func ClearCache(filePath string) error {
	err := os.Remove(filePath)
	if err != nil {
		return fmt.Errorf("failed to remove cache file: %v", err)
	}
	return nil
}
