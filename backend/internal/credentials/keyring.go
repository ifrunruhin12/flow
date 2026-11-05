package credentials

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
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const keySize = 32

type Keyring struct {
	activeVersion int
	keys          map[int][]byte
}

type Envelope struct {
	KeyVersion int
	Ciphertext []byte
}

type keyringFile struct {
	ActiveVersion int               `json:"activeVersion"`
	Keys          map[string]string `json:"keys"`
}

func Load(path string) (*Keyring, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat credential keyring: %w", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("credential keyring must not be readable or writable by group/other")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open credential keyring: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	var payload keyringFile
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode credential keyring: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	keys := make(map[int][]byte, len(payload.Keys))
	for rawVersion, encoded := range payload.Keys {
		version, err := strconv.Atoi(rawVersion)
		if err != nil || version < 1 {
			return nil, fmt.Errorf("credential keyring has invalid version %q", rawVersion)
		}
		key, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(key) != keySize {
			return nil, fmt.Errorf("credential keyring version %d must be a base64-encoded 32-byte key", version)
		}
		keys[version] = bytes.Clone(key)
	}
	return New(payload.ActiveVersion, keys)
}

func New(activeVersion int, keys map[int][]byte) (*Keyring, error) {
	if activeVersion < 1 {
		return nil, errors.New("credential keyring activeVersion must be positive")
	}
	if len(keys) == 0 {
		return nil, errors.New("credential keyring must contain at least one key")
	}
	copyKeys := make(map[int][]byte, len(keys))
	for version, key := range keys {
		if version < 1 || len(key) != keySize {
			return nil, fmt.Errorf("credential key version %d must contain 32 bytes", version)
		}
		copyKeys[version] = bytes.Clone(key)
	}
	if _, ok := copyKeys[activeVersion]; !ok {
		return nil, fmt.Errorf("credential keyring active version %d is missing", activeVersion)
	}
	return &Keyring{activeVersion: activeVersion, keys: copyKeys}, nil
}

func (k *Keyring) ActiveVersion() int { return k.activeVersion }

func (k *Keyring) HasVersion(version int) bool {
	_, ok := k.keys[version]
	return ok
}

func (k *Keyring) Versions() []int {
	versions := make([]int, 0, len(k.keys))
	for version := range k.keys {
		versions = append(versions, version)
	}
	sort.Ints(versions)
	return versions
}

func (k *Keyring) Encrypt(username, purpose string, plaintext []byte) (Envelope, error) {
	aead, err := k.aead(k.activeVersion)
	if err != nil {
		return Envelope{}, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return Envelope{}, fmt.Errorf("generate credential nonce: %w", err)
	}
	sealed := aead.Seal(nil, nonce, plaintext, associatedData(username, purpose))
	return Envelope{KeyVersion: k.activeVersion, Ciphertext: append(nonce, sealed...)}, nil
}

func (k *Keyring) Decrypt(username, purpose string, envelope Envelope) ([]byte, error) {
	aead, err := k.aead(envelope.KeyVersion)
	if err != nil {
		return nil, err
	}
	if len(envelope.Ciphertext) < aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("credential ciphertext is truncated")
	}
	nonce := envelope.Ciphertext[:aead.NonceSize()]
	sealed := envelope.Ciphertext[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, sealed, associatedData(username, purpose))
	if err != nil {
		return nil, errors.New("credential ciphertext authentication failed")
	}
	return plaintext, nil
}

func (k *Keyring) aead(version int) (cipher.AEAD, error) {
	key, ok := k.keys[version]
	if !ok {
		return nil, fmt.Errorf("credential key version %d is unavailable", version)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialize credential cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize credential AEAD: %w", err)
	}
	return aead, nil
}

func associatedData(username, purpose string) []byte {
	return []byte("flow:v1:" + strings.TrimSpace(purpose) + ":" + strings.ToLower(strings.TrimSpace(username)))
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("credential keyring contains multiple JSON values")
		}
		return fmt.Errorf("decode credential keyring trailer: %w", err)
	}
	return nil
}
