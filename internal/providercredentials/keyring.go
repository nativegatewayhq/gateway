package providercredentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

var (
	ErrKeyUnavailable = errors.New("provider credential master key unavailable")
	ErrDecrypt        = errors.New("provider credential decrypt failed")
)

type MasterKeyring struct {
	current string
	keys    map[string][32]byte
}

type MasterKeyProvider interface {
	CurrentID() (string, error)
	WrapDataKey(string, []byte, []byte) ([]byte, []byte, error)
	UnwrapDataKey(string, []byte, []byte, []byte) ([]byte, error)
	OperationTags([]byte, ...string) ([][]byte, error)
}

func (*MasterKeyring) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "<provider-credential-keyring>")
}

func LoadMasterKeyring(lookup LookupEnv) (*MasterKeyring, error) {
	current, currentConfigured := lookup("GATEWAY_PROVIDER_CREDENTIAL_CURRENT_KEY_ID")
	idsRaw, idsConfigured := lookup("GATEWAY_PROVIDER_CREDENTIAL_KEY_IDS")
	if !currentConfigured && !idsConfigured {
		return nil, nil
	}
	if !currentConfigured || !idsConfigured || current == "" || idsRaw == "" || !validKeyID(current) {
		return nil, ErrKeyUnavailable
	}
	ids := strings.Split(idsRaw, ",")
	if len(ids) == 0 || len(ids) > 16 {
		return nil, ErrKeyUnavailable
	}
	keys := make(map[string][32]byte, len(ids))
	for index, id := range ids {
		if !validKeyID(id) {
			return nil, ErrKeyUnavailable
		}
		if _, duplicate := keys[id]; duplicate {
			return nil, ErrKeyUnavailable
		}
		raw, exists := lookup(fmt.Sprintf("GATEWAY_PROVIDER_CREDENTIAL_KEY_%d", index))
		if !exists {
			return nil, ErrKeyUnavailable
		}
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(decoded) != 32 {
			return nil, ErrKeyUnavailable
		}
		var key [32]byte
		copy(key[:], decoded)
		zero(decoded)
		keys[id] = key
	}
	if _, ok := keys[current]; !ok {
		return nil, ErrKeyUnavailable
	}
	return &MasterKeyring{current: current, keys: keys}, nil
}

func NewMasterKeyring(current string, keys map[string][]byte) (*MasterKeyring, error) {
	if !validKeyID(current) {
		return nil, ErrKeyUnavailable
	}
	copyKeys := make(map[string][32]byte, len(keys))
	for id, raw := range keys {
		if !validKeyID(id) || len(raw) != 32 {
			return nil, ErrKeyUnavailable
		}
		var key [32]byte
		copy(key[:], raw)
		copyKeys[id] = key
	}
	if _, ok := copyKeys[current]; !ok {
		return nil, ErrKeyUnavailable
	}
	return &MasterKeyring{current: current, keys: copyKeys}, nil
}

func (ring *MasterKeyring) CurrentID() (string, error) {
	if ring == nil || !validKeyID(ring.current) {
		return "", ErrKeyUnavailable
	}
	return ring.current, nil
}

func (ring *MasterKeyring) aead(id string) (cipher.AEAD, error) {
	if ring == nil {
		return nil, ErrKeyUnavailable
	}
	key, ok := ring.keys[id]
	if !ok {
		return nil, ErrKeyUnavailable
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, ErrKeyUnavailable
	}
	return cipher.NewGCM(block)
}

func (ring *MasterKeyring) WrapDataKey(id string, plaintext, aad []byte) ([]byte, []byte, error) {
	aead, err := ring.aead(id)
	if err != nil {
		return nil, nil, err
	}
	if len(plaintext) != 32 {
		return nil, nil, ErrKeyUnavailable
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, ErrKeyUnavailable
	}
	return aead.Seal(nil, nonce, plaintext, aad), nonce, nil
}

func (ring *MasterKeyring) UnwrapDataKey(id string, ciphertext, nonce, aad []byte) ([]byte, error) {
	aead, err := ring.aead(id)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() || len(ciphertext) != 32+aead.Overhead() {
		return nil, ErrDecrypt
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

func (ring *MasterKeyring) OperationTags(plaintext []byte, fields ...string) ([][]byte, error) {
	return ring.operationTags(plaintext, fields...)
}

func (ring *MasterKeyring) operationTag(plaintext []byte, fields ...string) ([]byte, error) {
	return ring.operationTagWithKey(ring.current, plaintext, fields...)
}

func (ring *MasterKeyring) operationTags(plaintext []byte, fields ...string) ([][]byte, error) {
	if ring == nil {
		return nil, ErrKeyUnavailable
	}
	ids := make([]string, 0, len(ring.keys))
	for id := range ring.keys {
		if id != ring.current {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	ids = append([]string{ring.current}, ids...)
	tags := make([][]byte, 0, len(ids))
	for _, id := range ids {
		tag, err := ring.operationTagWithKey(id, plaintext, fields...)
		if err != nil {
			for _, existing := range tags {
				zero(existing)
			}
			return nil, err
		}
		tags = append(tags, tag)
	}
	return tags, nil
}

func (ring *MasterKeyring) operationTagWithKey(id string, plaintext []byte, fields ...string) ([]byte, error) {
	if ring == nil {
		return nil, ErrKeyUnavailable
	}
	key, ok := ring.keys[id]
	if !ok {
		return nil, ErrKeyUnavailable
	}
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte("nativegateway/provider-credential-operation/v1\x00"))
	_, _ = mac.Write([]byte(strings.Join(fields, "\x00")))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(plaintext)
	return mac.Sum(nil), nil
}

func validKeyID(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || (index > 0 && (character == '_' || character == '-')) {
			continue
		}
		return false
	}
	return true
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
