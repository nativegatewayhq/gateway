package providercredentials

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func testKeyring(t *testing.T) *MasterKeyring {
	t.Helper()
	ring, err := NewMasterKeyring("current", map[string][]byte{"current": bytes.Repeat([]byte{1}, 32), "previous": bytes.Repeat([]byte{2}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func TestEnvelopeEncryptionUsesRandomKeysAndAuthenticatedScope(t *testing.T) {
	store := &Store{keyring: testKeyring(t), entropy: strings.NewReader(strings.Repeat("0123456789abcdef", 20))}
	secret := []byte("provider-secret")
	first, err := store.encrypt(secret, "pcred_00000000000000000000000000000001", "channel_00000000000000000000000000000001", OpenAI, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.encrypt(secret, "pcred_00000000000000000000000000000002", "channel_00000000000000000000000000000001", OpenAI, 2)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.ciphertext, second.ciphertext) || bytes.Contains(first.ciphertext, secret) || bytes.Contains(first.wrappedKey, secret) {
		t.Fatal("envelope encryption was deterministic or exposed plaintext")
	}
	plaintext, err := store.decrypt(first, "pcred_00000000000000000000000000000001", "channel_00000000000000000000000000000001", OpenAI, 1)
	if err != nil || !bytes.Equal(plaintext, secret) {
		t.Fatalf("decrypt=%q err=%v", plaintext, err)
	}
	zero(plaintext)
	if _, err := store.decrypt(first, "pcred_00000000000000000000000000000001", "channel_00000000000000000000000000000002", OpenAI, 1); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("scope tamper error=%v", err)
	}
	malformed := first
	malformed.nonce = []byte{1}
	if _, err := store.decrypt(malformed, "pcred_00000000000000000000000000000001", "channel_00000000000000000000000000000001", OpenAI, 1); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("malformed envelope error=%v", err)
	}
}

func TestMasterKeyringLoadsCurrentAndPreviousKeysWithoutFormattingSecrets(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, 32)
	values := map[string]string{
		"GATEWAY_PROVIDER_CREDENTIAL_CURRENT_KEY_ID": "next",
		"GATEWAY_PROVIDER_CREDENTIAL_KEY_IDS":        "old,next",
		"GATEWAY_PROVIDER_CREDENTIAL_KEY_0":          base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{6}, 32)),
		"GATEWAY_PROVIDER_CREDENTIAL_KEY_1":          base64.StdEncoding.EncodeToString(secret),
	}
	ring, err := LoadMasterKeyring(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err != nil || ring.current != "next" || len(ring.keys) != 2 {
		t.Fatalf("ring=%v err=%v", ring, err)
	}
	if strings.Contains(fmt.Sprintf("%v", ring), string(secret)) {
		t.Fatal("keyring formatting leaked secret")
	}
}

func TestMasterKeyringRejectsPartialDuplicateAndMalformedConfiguration(t *testing.T) {
	tests := []map[string]string{
		{"GATEWAY_PROVIDER_CREDENTIAL_KEY_IDS": "only"},
		{"GATEWAY_PROVIDER_CREDENTIAL_CURRENT_KEY_ID": "only"},
		{"GATEWAY_PROVIDER_CREDENTIAL_CURRENT_KEY_ID": "only", "GATEWAY_PROVIDER_CREDENTIAL_KEY_IDS": "only,only", "GATEWAY_PROVIDER_CREDENTIAL_KEY_0": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)), "GATEWAY_PROVIDER_CREDENTIAL_KEY_1": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))},
		{"GATEWAY_PROVIDER_CREDENTIAL_CURRENT_KEY_ID": "only", "GATEWAY_PROVIDER_CREDENTIAL_KEY_IDS": "only", "GATEWAY_PROVIDER_CREDENTIAL_KEY_0": "not-a-key"},
	}
	for index, values := range tests {
		if ring, err := LoadMasterKeyring(func(key string) (string, bool) { value, ok := values[key]; return value, ok }); ring != nil || !errors.Is(err, ErrKeyUnavailable) {
			t.Fatalf("case=%d ring=%v err=%v", index, ring, err)
		}
	}
}

func TestOpaqueCredentialFormattingScopeAndDestroy(t *testing.T) {
	secret := []byte("opaque-provider-secret")
	credential := Credential{provider: OpenAI, channelID: "channel_00000000000000000000000000000001", value: append([]byte(nil), secret...)}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", credential, credential, credential), string(secret)) {
		t.Fatal("credential formatting leaked plaintext")
	}
	request, _ := http.NewRequest(http.MethodGet, "https://api.openai.com/v1/models", nil)
	if err := credential.ApplyChannel(request, "channel_00000000000000000000000000000002", OpenAI); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("scope mismatch=%v", err)
	}
	if err := credential.ApplyChannel(request, credential.channelID, OpenAI); err != nil || request.Header.Get("Authorization") != "Bearer "+string(secret) {
		t.Fatalf("apply err=%v header=%q", err, request.Header.Get("Authorization"))
	}
	backing := credential.value
	credential.Destroy()
	if len(credential.value) != 0 || !bytes.Equal(backing, make([]byte, len(backing))) {
		t.Fatal("credential was not destroyed")
	}
}
