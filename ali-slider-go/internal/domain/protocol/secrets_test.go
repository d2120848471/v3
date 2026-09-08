package protocol

import (
	"fmt"
	"strings"
	"testing"
)

func TestResolveFrontendSecretsAndRedaction(t *testing.T) {
	left, err := ResolveFrontendSecrets()
	if err != nil {
		t.Fatal(err)
	}
	right, err := ResolveFrontendSecrets()
	if err != nil {
		t.Fatal(err)
	}
	values := []string{
		left.MainRPCKeyID(), left.MainRPCKeySecret(), left.DeviceTokenSalt(), left.DeviceRPCKeyID(),
		left.DeviceRPCKeySecret(), left.DeviceRequestKey(), left.DeviceResponseKey(), left.DeviceFlagKey(),
		left.DeviceUploadKey(), left.DevicePreIDKey(),
	}
	for index, value := range values {
		if value == "" {
			t.Fatalf("secret %d is empty", index)
		}
	}
	if left.MainRPCKeyID() != right.MainRPCKeyID() {
		t.Fatal("cached secrets changed")
	}
	for _, rendered := range []string{fmt.Sprint(left), fmt.Sprintf("%+v", left), fmt.Sprintf("%#v", left)} {
		if !strings.Contains(rendered, "redacted") {
			t.Fatalf("secret container was not redacted: %q", rendered)
		}
		for _, value := range values {
			if strings.Contains(rendered, value) {
				t.Fatal("secret container formatting leaked a value")
			}
		}
	}
}

func TestDecodeFrontendSecretsRejectsInvalidMaterial(t *testing.T) {
	if _, err := decodeFrontendSecrets(nil); err == nil {
		t.Fatal("invalid secret count accepted")
	}
	invalid := append([]string(nil), frontendCiphertexts[:]...)
	invalid[0] = "%%"
	if _, err := decodeFrontendSecrets(invalid); err == nil {
		t.Fatal("invalid ciphertext accepted")
	}
	nonUTF8, err := AESCBCEncryptBase64([]byte{0xff}, frontendAccessKey)
	if err != nil {
		t.Fatal(err)
	}
	invalid = append([]string(nil), frontendCiphertexts[:]...)
	invalid[0] = nonUTF8
	if _, err := decodeFrontendSecrets(invalid); err == nil {
		t.Fatal("non-UTF8 plaintext accepted")
	}
	empty, err := AESCBCEncryptBase64(nil, frontendAccessKey)
	if err != nil {
		t.Fatal(err)
	}
	invalid = append([]string(nil), frontendCiphertexts[:]...)
	invalid[0] = empty
	if _, err := decodeFrontendSecrets(invalid); err == nil {
		t.Fatal("empty secret accepted")
	}
}
