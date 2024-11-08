package storage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileStorage implements fiber's Storage interface for file-based session storage
type FileStorage struct {
	dir string
	mu  sync.RWMutex
}

type sessionData struct {
	Value     []byte    `json:"value"`
	ExpiresAt time.Time `json:"expires_at"`
}

// NewFileStorage creates a new file storage instance
func NewFileStorage(directory string) (*FileStorage, error) {
	// Create directory if it doesn't exist
	if err := os.MkdirAll(directory, 0755); err != nil {
		return nil, err
	}

	return &FileStorage{
		dir: directory,
	}, nil
}

// Get retrieves session data from file
func (s *FileStorage) Get(key string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := s.readFile(key)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // Return nil for non-existent keys
		}
		return nil, err
	}

	// Check if session has expired
	if time.Now().After(data.ExpiresAt) {
		s.Delete(key) // Clean up expired session
		return nil, nil
	}

	return data.Value, nil
}

// Set stores session data to file
func (s *FileStorage) Set(key string, val []byte, exp time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data := sessionData{
		Value:     val,
		ExpiresAt: time.Now().Add(exp),
	}

	return s.writeFile(key, data)
}

// Delete removes session file
func (s *FileStorage) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.getPath(key)
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Reset removes all session files
func (s *FileStorage) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}

	for _, d := range dir {
		if filepath.Ext(d.Name()) == ".session" {
			os.Remove(filepath.Join(s.dir, d.Name()))
		}
	}

	return nil
}

// Close implements Storage.Close
func (s *FileStorage) Close() error {
	return nil
}

// Helper methods

func (s *FileStorage) getPath(key string) string {
	return filepath.Join(s.dir, key+".session")
}

func (s *FileStorage) readFile(key string) (*sessionData, error) {
	path := s.getPath(key)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var session sessionData
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, err
	}

	return &session, nil
}

func (s *FileStorage) writeFile(key string, data sessionData) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}

	return os.WriteFile(s.getPath(key), jsonData, 0644)
}
