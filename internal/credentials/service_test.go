package credentials

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestEncryptDecryptWithAAD(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	service, err := NewService(nil, encoded)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := service.encrypt("secret-value", "binance:api-secret")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), "secret-value") {
		t.Fatal("ciphertext contains plaintext")
	}
	plain, err := service.decrypt(ciphertext, "binance:api-secret")
	if err != nil {
		t.Fatal(err)
	}
	if plain != "secret-value" {
		t.Fatalf("unexpected plaintext %q", plain)
	}
	if _, err := service.decrypt(ciphertext, "mgbx:api-secret"); err == nil {
		t.Fatal("expected authentication failure for wrong AAD")
	}
}

func TestMasterKeyValidation(t *testing.T) {
	for _, value := range []string{"", "not-base64", base64.StdEncoding.EncodeToString(make([]byte, 31))} {
		if _, err := NewService(nil, value); err == nil {
			t.Fatalf("expected invalid master key %q", value)
		}
	}
}

func TestMetadataHelpers(t *testing.T) {
	if got := last4("abcdef"); got != "cdef" {
		t.Fatalf("unexpected last4 %q", got)
	}
	if fingerprint("key") == "" || fingerprint("key") == fingerprint("other") {
		t.Fatal("fingerprint should be stable and distinct")
	}
}
