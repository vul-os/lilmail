package cache

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"lilmail/internal/crypto"
)

var (
	ErrNotFound     = errors.New("item not found in cache")
	ErrExpired      = errors.New("cached item has expired")
	ErrSizeExceeded = errors.New("cache size limit exceeded")
)

type FileCache struct {
	dir     string
	maxSize int64
	ttl     time.Duration
	crypto  *crypto.Manager

	// Cache statistics and management
	currentSize   int64
	sizeMutex     sync.RWMutex
	cleanupTicker *time.Ticker

	// Index for quick lookups
	index      map[string]*indexEntry
	indexMutex sync.RWMutex
}

type indexEntry struct {
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Encrypted bool      `json:"encrypted"`
}

func NewFileCache(dir string, maxSize int64, ttl time.Duration, crypto *crypto.Manager) (*FileCache, error) {
	// Ensure cache directory exists
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	cache := &FileCache{
		dir:           dir,
		maxSize:       maxSize,
		ttl:           ttl,
		crypto:        crypto,
		index:         make(map[string]*indexEntry),
		cleanupTicker: time.NewTicker(time.Hour),
	}

	// Load existing cache index
	if err := cache.loadIndex(); err != nil {
		return nil, err
	}

	// Start cleanup routine
	go cache.cleanupRoutine()

	return cache, nil
}

func (c *FileCache) cleanupRoutine() {
	for range c.cleanupTicker.C {
		c.cleanup()
	}
}

func (c *FileCache) cleanup() {
	c.indexMutex.Lock()
	defer c.indexMutex.Unlock()

	now := time.Now()
	var totalSize int64
	deleted := make(map[string]struct{})

	// First pass: mark expired items
	for key, entry := range c.index {
		if now.After(entry.ExpiresAt) {
			deleted[key] = struct{}{}
			os.Remove(entry.Path) // Remove the file
			continue
		}
		totalSize += entry.Size
	}

	// Second pass: if still over maxSize, remove oldest items
	if totalSize > c.maxSize {
		type ageEntry struct {
			key       string
			createdAt time.Time
			size      int64
		}
		var items []ageEntry
		for key, entry := range c.index {
			if _, isDeleted := deleted[key]; !isDeleted {
				items = append(items, ageEntry{key, entry.CreatedAt, entry.Size})
			}
		}

		// Sort by creation time
		sort.Slice(items, func(i, j int) bool {
			return items[i].createdAt.Before(items[j].createdAt)
		})

		// Remove oldest items until under maxSize
		for _, item := range items {
			if totalSize <= c.maxSize {
				break
			}
			deleted[item.key] = struct{}{}
			totalSize -= item.size
			os.Remove(c.index[item.key].Path)
		}
	}

	// Remove deleted items from index
	for key := range deleted {
		delete(c.index, key)
	}

	// Update current size
	c.sizeMutex.Lock()
	c.currentSize = totalSize
	c.sizeMutex.Unlock()

	// Save updated index
	c.saveIndex()
}

func (c *FileCache) Set(key string, data []byte, encrypted bool) error {
	c.indexMutex.Lock()
	defer c.indexMutex.Unlock()

	// Check if we need to encrypt the data
	if encrypted {
		var err error
		data, err = c.crypto.Encrypt(data)
		if err != nil {
			return fmt.Errorf("failed to encrypt data: %w", err)
		}
	}

	// Create filename from key
	filename := filepath.Join(c.dir, fmt.Sprintf("%x", sha256.Sum256([]byte(key))))

	// Check if we have enough space
	newSize := int64(len(data))
	if c.currentSize+newSize > c.maxSize {
		return ErrSizeExceeded
	}

	// Write data to file
	if err := os.WriteFile(filename, data, 0600); err != nil {
		return fmt.Errorf("failed to write cache file: %w", err)
	}

	// Update index
	c.index[key] = &indexEntry{
		Path:      filename,
		Size:      newSize,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(c.ttl),
		Encrypted: encrypted,
	}

	// Update size
	c.sizeMutex.Lock()
	c.currentSize += newSize
	c.sizeMutex.Unlock()

	return c.saveIndex()
}

func (c *FileCache) Get(key string) ([]byte, error) {
	c.indexMutex.RLock()
	entry, exists := c.index[key]
	c.indexMutex.RUnlock()

	if !exists {
		return nil, ErrNotFound
	}

	if time.Now().After(entry.ExpiresAt) {
		c.Delete(key)
		return nil, ErrExpired
	}

	data, err := os.ReadFile(entry.Path)
	if err != nil {
		c.Delete(key)
		return nil, fmt.Errorf("failed to read cache file: %w", err)
	}

	if entry.Encrypted {
		data, err = c.crypto.Decrypt(data)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt data: %w", err)
		}
	}

	return data, nil
}

func (c *FileCache) Delete(key string) error {
	c.indexMutex.Lock()
	defer c.indexMutex.Unlock()

	entry, exists := c.index[key]
	if !exists {
		return nil
	}

	// Remove file
	if err := os.Remove(entry.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove cache file: %w", err)
	}

	// Update size
	c.sizeMutex.Lock()
	c.currentSize -= entry.Size
	c.sizeMutex.Unlock()

	// Remove from index
	delete(c.index, key)

	return c.saveIndex()
}

func (c *FileCache) Clear() error {
	c.indexMutex.Lock()
	defer c.indexMutex.Unlock()

	// Remove all cached files
	if err := os.RemoveAll(c.dir); err != nil {
		return fmt.Errorf("failed to clear cache directory: %w", err)
	}

	// Recreate cache directory
	if err := os.MkdirAll(c.dir, 0700); err != nil {
		return fmt.Errorf("failed to recreate cache directory: %w", err)
	}

	// Reset index and size
	c.index = make(map[string]*indexEntry)
	c.sizeMutex.Lock()
	c.currentSize = 0
	c.sizeMutex.Unlock()

	return c.saveIndex()
}

func (c *FileCache) loadIndex() error {
	indexPath := filepath.Join(c.dir, "index.json")
	data, err := os.ReadFile(indexPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read index file: %w", err)
	}

	if err := json.Unmarshal(data, &c.index); err != nil {
		return fmt.Errorf("failed to parse index file: %w", err)
	}

	// Verify files and calculate size
	var totalSize int64
	for key, entry := range c.index {
		if _, err := os.Stat(entry.Path); os.IsNotExist(err) {
			delete(c.index, key)
			continue
		}
		totalSize += entry.Size
	}

	c.sizeMutex.Lock()
	c.currentSize = totalSize
	c.sizeMutex.Unlock()

	return nil
}

func (c *FileCache) saveIndex() error {
	data, err := json.Marshal(c.index)
	if err != nil {
		return fmt.Errorf("failed to marshal index: %w", err)
	}

	indexPath := filepath.Join(c.dir, "index.json")
	if err := os.WriteFile(indexPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write index file: %w", err)
	}

	return nil
}
