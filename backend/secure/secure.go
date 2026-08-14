package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
)

const (
	keyFile      = "secret.key"
	tokenFile    = "alor_token.enc"
	portfolioFile = "alor_portfolio.enc"
)

// Store encrypts sensitive credentials (Alor refresh token) on disk.
type Store struct {
	dataDir string
	key     []byte
}

// NewStore loads (or generates) an AES key and returns a Store bound to dataDir.
func NewStore(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}

	keyPath := filepath.Join(dataDir, keyFile)
	key, err := os.ReadFile(keyPath)
	if err != nil || len(key) != 32 {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(keyPath, key, 0600); err != nil {
			return nil, err
		}
	}

	return &Store{dataDir: dataDir, key: key}, nil
}

// NewStoreWithKey allows injecting a key from env for reproducible decryption.
func NewStoreWithKey(dataDir string, envKey string) (*Store, error) {
	if envKey != "" {
		sum := sha256.Sum256([]byte(envKey))
		return &Store{dataDir: dataDir, key: sum[:]}, nil
	}
	return NewStore(dataDir)
}

func (s *Store) encrypt(plain string) (string, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func (s *Store) decrypt(enc string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	nonce, ciphertext := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (s *Store) SaveToken(token string) error {
	if token == "" {
		return errors.New("empty token")
	}
	enc, err := s.encrypt(token)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dataDir, tokenFile), []byte(enc), 0600)
}

func (s *Store) LoadToken() (string, error) {
	data, err := os.ReadFile(filepath.Join(s.dataDir, tokenFile))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return s.decrypt(string(data))
}

func (s *Store) HasToken() bool {
	t, err := s.LoadToken()
	return err == nil && t != ""
}

func (s *Store) SavePortfolio(portfolio string) error {
	if portfolio == "" {
		return errors.New("empty portfolio")
	}
	enc, err := s.encrypt(portfolio)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dataDir, portfolioFile), []byte(enc), 0600)
}

func (s *Store) LoadPortfolio() (string, error) {
	data, err := os.ReadFile(filepath.Join(s.dataDir, portfolioFile))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return s.decrypt(string(data))
}