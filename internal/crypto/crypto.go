// internal/crypto/crypto.go
package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

var (
	ErrKeyNotFound   = errors.New("encryption key not found")
	ErrInvalidKey    = errors.New("invalid encryption key")
	ErrDecryptFailed = errors.New("failed to decrypt data")
	ErrInvalidNonce  = errors.New("invalid nonce")
	ErrKeyGeneration = errors.New("failed to generate key")
)

const (
	keySize    = 32 // AES-256
	saltSize   = 32
	nonceSize  = 12
	iterations = 100000
	memory     = 64 * 1024
	threads    = 4
)

type Manager struct {
	keyDir    string
	masterKey []byte
	keys      map[string]*Key
	keysMutex sync.RWMutex
	gcm       cipher.AEAD
}

type Key struct {
	ID        string    `json:"id"`
	Salt      []byte    `json:"salt"`
	Nonce     []byte    `json:"nonce"`
	Key       []byte    `json:"key"`
	CreatedAt time.Time `json:"created_at"`
	Active    bool      `json:"active"`
}

func NewManager(keyDir string, password string) (*Manager, error) {
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create key directory: %w", err)
	}

	m := &Manager{
		keyDir: keyDir,
		keys:   make(map[string]*Key),
	}

	if err := m.initializeMasterKey(password); err != nil {
		return nil, err
	}

	if err := m.loadKeys(); err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(m.masterKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	m.gcm = gcm

	return m, nil
}

func (m *Manager) initializeMasterKey(password string) error {
	masterKeyPath := filepath.Join(m.keyDir, "master.key")

	if _, err := os.Stat(masterKeyPath); os.IsNotExist(err) {
		salt := make([]byte, saltSize)
		if _, err := io.ReadFull(rand.Reader, salt); err != nil {
			return fmt.Errorf("failed to generate salt: %w", err)
		}

		m.masterKey = argon2.IDKey(
			[]byte(password),
			salt,
			1,
			memory,
			threads,
			keySize,
		)

		encryptedKey := make([]byte, len(m.masterKey)+len(salt))
		copy(encryptedKey, salt)
		copy(encryptedKey[len(salt):], m.masterKey)

		if err := os.WriteFile(masterKeyPath, encryptedKey, 0600); err != nil {
			return fmt.Errorf("failed to save master key: %w", err)
		}
	} else {
		data, err := os.ReadFile(masterKeyPath)
		if err != nil {
			return fmt.Errorf("failed to read master key: %w", err)
		}

		salt := data[:saltSize]
		m.masterKey = argon2.IDKey(
			[]byte(password),
			salt,
			1,
			memory,
			threads,
			keySize,
		)
	}

	return nil
}

func (m *Manager) loadKeys() error {
	files, err := os.ReadDir(m.keyDir)
	if err != nil {
		return fmt.Errorf("failed to read key directory: %w", err)
	}

	for _, file := range files {
		if file.Name() == "master.key" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(m.keyDir, file.Name()))
		if err != nil {
			continue
		}

		decrypted, err := m.decryptWithMaster(data)
		if err != nil {
			continue
		}

		var key Key
		if err := json.Unmarshal(decrypted, &key); err != nil {
			continue
		}

		m.keys[key.ID] = &key
	}

	return nil
}

func (m *Manager) GenerateKey() (*Key, error) {
	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, ErrKeyGeneration
	}

	salt := make([]byte, saltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, ErrKeyGeneration
	}

	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, ErrKeyGeneration
	}

	if err := m.verifyKey(string(salt[:8]), key); err != nil {
		return nil, fmt.Errorf("key verification failed: %w", err)
	}

	newKey := &Key{
		ID:        base64.RawURLEncoding.EncodeToString(salt[:8]),
		Salt:      salt,
		Nonce:     nonce,
		Key:       key,
		CreatedAt: time.Now(),
		Active:    true,
	}

	if err := m.saveKey(newKey); err != nil {
		return nil, fmt.Errorf("failed to save new key: %w", err)
	}

	return newKey, nil
}

func (m *Manager) Encrypt(data []byte) ([]byte, error) {
	m.keysMutex.RLock()
	var activeKey *Key
	for _, key := range m.keys {
		if key.Active {
			activeKey = key
			break
		}
	}
	m.keysMutex.RUnlock()

	if activeKey == nil {
		// No active key, generate one
		var err error
		activeKey, err = m.GenerateKey()
		if err != nil {
			return nil, err
		}
	}

	nonce := make([]byte, m.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	encrypted := m.gcm.Seal(nonce, nonce, data, nil)

	result := make([]byte, 0, len(activeKey.ID)+1+len(encrypted))
	result = append(result, byte(len(activeKey.ID)))
	result = append(result, []byte(activeKey.ID)...)
	result = append(result, encrypted...)

	return result, nil
}

func (m *Manager) Decrypt(data []byte) ([]byte, error) {
	if len(data) < 1 {
		return nil, ErrDecryptFailed
	}

	idLen := int(data[0])
	if len(data) < 1+idLen {
		return nil, ErrDecryptFailed
	}
	keyID := string(data[1 : 1+idLen])
	encryptedData := data[1+idLen:]

	m.keysMutex.RLock()
	_, exists := m.keys[keyID]
	m.keysMutex.RUnlock()

	if !exists {
		return nil, ErrKeyNotFound
	}

	if len(encryptedData) < m.gcm.NonceSize() {
		return nil, ErrInvalidNonce
	}
	nonce := encryptedData[:m.gcm.NonceSize()]
	ciphertext := encryptedData[m.gcm.NonceSize():]

	plaintext, err := m.gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, ErrDecryptFailed
	}

	return plaintext, nil
}

func (m *Manager) RotateKey(keyID string) error {
	m.keysMutex.Lock()
	defer m.keysMutex.Unlock()

	oldKey, exists := m.keys[keyID]
	if !exists {
		return ErrKeyNotFound
	}

	newKey, err := m.GenerateKey()
	if err != nil {
		return fmt.Errorf("failed to generate new key: %w", err)
	}

	oldData, err := m.loadEncryptedData(keyID)
	if err != nil {
		return fmt.Errorf("failed to load old encrypted data: %w", err)
	}

	if err := m.reencryptData(oldData, newKey.ID); err != nil {
		delete(m.keys, newKey.ID)
		return fmt.Errorf("failed to re-encrypt data: %w", err)
	}

	oldKey.Active = false
	if err := m.saveKey(oldKey); err != nil {
		return fmt.Errorf("failed to update old key: %w", err)
	}

	return nil
}

func (m *Manager) saveKey(key *Key) error {
	data, err := json.Marshal(key)
	if err != nil {
		return fmt.Errorf("failed to marshal key: %w", err)
	}

	encrypted, err := m.encryptWithMaster(data)
	if err != nil {
		return fmt.Errorf("failed to encrypt key: %w", err)
	}

	path := filepath.Join(m.keyDir, key.ID+".key")
	if err := os.WriteFile(path, encrypted, 0600); err != nil {
		return fmt.Errorf("failed to save key file: %w", err)
	}

	m.keys[key.ID] = key
	return nil
}

func (m *Manager) loadEncryptedData(keyID string) (map[string][]byte, error) {
	files, err := os.ReadDir(m.keyDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read key directory: %w", err)
	}

	data := make(map[string][]byte)
	for _, file := range files {
		if file.IsDir() || filepath.Ext(file.Name()) != ".enc" {
			continue
		}

		path := filepath.Join(m.keyDir, file.Name())
		encrypted, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read encrypted file %s: %w", file.Name(), err)
		}

		if strings.HasPrefix(string(encrypted), keyID+":") {
			data[file.Name()] = encrypted
		}
	}

	return data, nil
}

func (m *Manager) reencryptData(oldData map[string][]byte, newKeyID string) error {
	for filename, encrypted := range oldData {
		decrypted, err := m.Decrypt(encrypted)
		if err != nil {
			return fmt.Errorf("failed to decrypt %s: %w", filename, err)
		}

		reencrypted, err := m.Encrypt(decrypted)
		if err != nil {
			return fmt.Errorf("failed to re-encrypt %s: %w", filename, err)
		}

		path := filepath.Join(m.keyDir, filename)
		if err := os.WriteFile(path, reencrypted, 0600); err != nil {
			return fmt.Errorf("failed to save re-encrypted file %s: %w", filename, err)
		}
	}

	return nil
}

func (m *Manager) verifyKey(keyID string, key []byte) error {
	testData := []byte("verification_test")

	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("invalid key: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("failed to create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return fmt.Errorf("failed to generate nonce: %w", err)
	}

	encrypted := gcm.Seal(nonce, nonce, testData, nil)
	decrypted, err := gcm.Open(nil, encrypted[:gcm.NonceSize()], encrypted[gcm.NonceSize():], nil)
	if err != nil {
		return fmt.Errorf("key verification failed: %w", err)
	}

	if !bytes.Equal(decrypted, testData) {
		return errors.New("key verification failed: data mismatch")
	}

	return nil
}

func (m *Manager) encryptWithMaster(data []byte) ([]byte, error) {
	nonce := make([]byte, m.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	return m.gcm.Seal(nonce, nonce, data, nil), nil
}

func (m *Manager) decryptWithMaster(data []byte) ([]byte, error) {
	if len(data) < m.gcm.NonceSize() {
		return nil, ErrInvalidNonce
	}

	nonce := data[:m.gcm.NonceSize()]
	ciphertext := data[m.gcm.NonceSize():]

	return m.gcm.Open(nil, nonce, ciphertext, nil)
}

func (m *Manager) ListKeys() []Key {
	m.keysMutex.RLock()
	defer m.keysMutex.RUnlock()

	keys := make([]Key, 0, len(m.keys))
	for _, key := range m.keys {
		// Create a copy without the actual key material
		keys = append(keys, Key{
			ID:        key.ID,
			CreatedAt: key.CreatedAt,
			Active:    key.Active,
		})
	}

	return keys
}

func (m *Manager) Close() error {
	// Securely zero out sensitive data
	if m.masterKey != nil {
		for i := range m.masterKey {
			m.masterKey[i] = 0
		}
	}

	m.keysMutex.Lock()
	defer m.keysMutex.Unlock()

	for _, key := range m.keys {
		for i := range key.Key {
			key.Key[i] = 0
		}
	}

	return nil
}
