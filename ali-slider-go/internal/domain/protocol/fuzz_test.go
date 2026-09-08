package protocol

import (
	"encoding/json"
	"os"
	"testing"
	"unicode/utf8"
)

const maxFuzzInputBytes = 64 << 10

type protocolFuzzOracle struct {
	Data        string            `json:"data"`
	PayloadJSON string            `json:"payloadJSON"`
	DeviceToken string            `json:"deviceToken"`
	AES         map[string]string `json:"aes"`
}

func loadProtocolFuzzOracle(f *testing.F) protocolFuzzOracle {
	f.Helper()
	raw, err := os.ReadFile("testdata/python_oracle.json")
	if err != nil {
		f.Fatal(err)
	}
	var oracle protocolFuzzOracle
	if err := json.Unmarshal(raw, &oracle); err != nil {
		f.Fatal(err)
	}
	if oracle.Data == "" || oracle.PayloadJSON == "" || oracle.DeviceToken == "" || oracle.AES["16"] == "" {
		f.Fatal("python oracle is incomplete")
	}
	return oracle
}

func boundedFuzzString(value []byte) string {
	// ponytail: fuzz worker 输入限制在 64 KiB；生产解压路径另有 4 MiB 上限。
	if len(value) > maxFuzzInputBytes {
		value = value[:maxFuzzInputBytes]
	}
	return string(value)
}

func FuzzUnpackData(f *testing.F) {
	oracle := loadProtocolFuzzOracle(f)
	for _, seed := range [][]byte{
		{},
		{0xff},
		[]byte("%%"),
		[]byte("AA=="),
		[]byte("A===\n"),
		[]byte(oracle.Data[:len(oracle.Data)-1]),
		[]byte(oracle.Data),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		decoded, err := UnpackData(boundedFuzzString(raw))
		if err == nil && len(decoded.JSONText) > maxDataPlaintextSize {
			t.Fatalf("decoded plaintext=%d exceeds limit", len(decoded.JSONText))
		}
	})
}

func FuzzProtocolDecoders(f *testing.F) {
	oracle := loadProtocolFuzzOracle(f)
	for _, seed := range [][]byte{
		{},
		{0xff},
		[]byte("%%"),
		[]byte("AA=="),
		[]byte("AAAA\n"),
		[]byte(oracle.PayloadJSON),
		[]byte(oracle.DeviceToken),
		[]byte(oracle.AES["16"]),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		value := boundedFuzzString(raw)
		decoded, err := decodeBase64Strict(value)
		if err == nil && len(decoded) > len(value) {
			t.Fatalf("base64 output=%d exceeds input=%d", len(decoded), len(value))
		}
		_, _ = AESCBCDecryptBase64(value, "0123456789abcdef")
		_, _ = ParseDeviceToken(value, "offline-token-salt")
		_, _ = ParseDeviceConfig(value, "0123456789abcdef")
		_, _ = decodePayload(value)
	})
}

func FuzzPercentAndFormEncoding(f *testing.F) {
	for _, seed := range []struct{ key, value []byte }{
		{[]byte{}, []byte{}},
		{[]byte("a b"), []byte("!*()~**")},
		{[]byte("键"), []byte("空 格")},
		{[]byte{0xff}, []byte("value")},
		{[]byte("key"), []byte{0xff}},
	} {
		f.Add(seed.key, seed.value)
	}

	f.Fuzz(func(t *testing.T, rawKey, rawValue []byte) {
		key, value := boundedFuzzString(rawKey), boundedFuzzString(rawValue)
		for _, input := range []string{key, value} {
			encoded, err := JSEncodeURIComponent(input)
			if err == nil && (!utf8.ValidString(input) || len(encoded) > 3*len(input)) {
				t.Fatal("encodeURIComponent violated UTF-8/size bound")
			}
			encoded, err = RPCPercentEncode(input)
			if err == nil && (!utf8.ValidString(input) || len(encoded) > 3*len(input)) {
				t.Fatal("RPC percent encoding violated UTF-8/size bound")
			}
		}
		encoded, err := JSFormURLEncode([]Field{{Key: key, Value: value}})
		if err == nil && len(encoded) > 3*(len(key)+len(value))+1 {
			t.Fatal("form encoding exceeded linear size bound")
		}
	})
}
