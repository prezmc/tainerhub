package regcred

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Registry passwords are stored in the database encrypted with AES-256-GCM.
// The key lives in a file in the hub's data folder, not in the database, so
// a database dump or backup alone doesn't give the passwords away.

// KeyFile is the key's file name in the hub's data folder.
const KeyFile = "registry.key"

const keySize = 32

// sealVersion prefixes every sealed value, so the format can change later.
const sealVersion = 1

// LoadOrCreateKey reads the key at path, creating a random one (0600) the
// first time. Creating is atomic and never replaces a key that's already
// there: secrets sealed with it would be lost.
func LoadOrCreateKey(path string) ([]byte, error) {
	key, err := readKey(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return key, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	key = make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return nil, err
	}
	if _, err := tmp.WriteString(base64.StdEncoding.EncodeToString(key) + "\n"); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	// A hard link fails if the name exists, unlike rename: if another start
	// won the race, use its key.
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return readKey(path)
		}
		return nil, err
	}
	return key, nil
}

func readKey(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		// Someone loosened it (or a restore did): tighten it back.
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("%s is readable by others and can't be fixed: %w", path, err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) != keySize {
		return nil, fmt.Errorf("%s is not a valid registry key", path)
	}
	return key, nil
}

// Sealer encrypts and decrypts registry secrets with one key.
type Sealer struct{ aead cipher.AEAD }

func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != keySize {
		return nil, errors.New("registry key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plain for the registry named by context. The context is
// authenticated too, so a secret copied onto another registry's row in the
// database doesn't decrypt.
func (s *Sealer) Seal(plain []byte, context string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte{sealVersion}, nonce...)
	return s.aead.Seal(out, nonce, plain, []byte(context)), nil
}

// ErrUnseal means a secret can't be decrypted: a different key (the data
// folder was replaced) or a damaged value.
var ErrUnseal = errors.New("can't decrypt the saved registry password (was the hub's data folder replaced?)")

func (s *Sealer) Open(sealed []byte, context string) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < 1+n+s.aead.Overhead() || sealed[0] != sealVersion {
		return nil, ErrUnseal
	}
	plain, err := s.aead.Open(nil, sealed[1:1+n], sealed[1+n:], []byte(context))
	if err != nil {
		return nil, ErrUnseal
	}
	return plain, nil
}
