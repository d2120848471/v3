package protocol

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
)

type pythonOracle struct {
	JSEncoded           string            `json:"jsEncoded"`
	RPCEncoded          string            `json:"rpcEncoded"`
	Canonical           string            `json:"canonical"`
	StringToSign        string            `json:"stringToSign"`
	Signature           string            `json:"signature"`
	AES                 map[string]string `json:"aes"`
	FingerprintCipher   string            `json:"fingerprintCipher"`
	DeviceToken         string            `json:"deviceToken"`
	ParsedGatherCost    string            `json:"parsedGatherCost"`
	Transform256Base64  string            `json:"transform256Base64"`
	PEArg               string            `json:"peArg"`
	Data                string            `json:"data"`
	CompressedBase64    string            `json:"compressedBase64"`
	PayloadJSON         string            `json:"payloadJSON"`
	BusinessParam       string            `json:"businessParam"`
	BusinessQuery       string            `json:"businessQuery"`
	BusinessSignedQuery string            `json:"businessSignedQuery"`
}

func loadOracle(t *testing.T) pythonOracle {
	t.Helper()
	raw, err := os.ReadFile("testdata/python_oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var oracle pythonOracle
	if err := json.Unmarshal(raw, &oracle); err != nil {
		t.Fatal(err)
	}
	return oracle
}

func TestSigningMatchesPythonOracle(t *testing.T) {
	oracle := loadOracle(t)
	value := "空 格!*()~**"
	if got, _ := JSEncodeURIComponent(value); got != oracle.JSEncoded {
		t.Fatalf("JS encode: %q", got)
	}
	if got, _ := RPCPercentEncode(value); got != oracle.RPCEncoded {
		t.Fatalf("RPC encode: %q", got)
	}
	params := map[string]string{"Z-last": value, "Format": "JSON", "Action": "InitCaptchaV3", "AaduaneId": "abc+123", "Signature": "ignored"}
	if got, _ := RPCV1CanonicalQuery(params); got != oracle.Canonical {
		t.Fatalf("canonical: %q", got)
	}
	if got, _ := RPCV1StringToSign(params); got != oracle.StringToSign {
		t.Fatalf("string to sign: %q", got)
	}
	if got, _ := RPCV1Signature(params, "fixture-secret"); got != oracle.Signature {
		t.Fatalf("signature: %q", got)
	}
	if got, _ := JSFormURLEncode([]Field{{"a", "a b"}, {"x", "!*"}}); got != "a=a%20b&x=!*" {
		t.Fatalf("form: %q", got)
	}
}

func TestAESOracleAndInvalidInputs(t *testing.T) {
	oracle := loadOracle(t)
	expectedLengths := map[string]int{"empty": 0, "15": 15, "16": 16, "17": 17}
	for name, encoded := range oracle.AES {
		plaintext, err := AESCBCDecryptBase64(encoded, "0123456789abcdef")
		if err != nil {
			t.Fatalf("decrypt %s: %v", name, err)
		}
		if length, ok := expectedLengths[name]; ok && len(plaintext) != length {
			t.Fatalf("%s length=%d", name, len(plaintext))
		}
		rebuilt, err := AESCBCEncryptBase64(plaintext, "0123456789abcdef")
		if err != nil || rebuilt != encoded {
			t.Fatalf("AES %s round trip: %v", name, err)
		}
	}
	if _, err := AESCBCDecryptBase64("%%", "0123456789abcdef"); err == nil {
		t.Fatal("bad base64 accepted")
	}
	if _, err := AESCBCDecryptBase64(oracle.AES["empty"], "short"); err == nil {
		t.Fatal("bad key accepted")
	}
	ciphertext, _ := base64.StdEncoding.DecodeString(oracle.AES["16"])
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err := AESCBCDecryptBase64(base64.StdEncoding.EncodeToString(ciphertext), "0123456789abcdef"); err == nil {
		t.Fatal("bad padding accepted")
	}
}

func TestDeviceTokenAndConfig(t *testing.T) {
	oracle := loadOracle(t)
	token, err := BuildDeviceToken("offline-session-ABCDEFGH", oracle.FingerprintCipher, "0007", "offline-token-salt")
	if err != nil || token != oracle.DeviceToken {
		t.Fatalf("build token: %v", err)
	}
	parsed, err := ParseDeviceToken(token, "offline-token-salt")
	if err != nil || parsed.GatherCost != oracle.ParsedGatherCost {
		t.Fatalf("parse token: %+v %v", parsed, err)
	}
	if _, err := ParseDeviceToken(token, "wrong"); err == nil {
		t.Fatal("wrong salt accepted")
	}
	encode := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	plain := strings.Join([]string{encode("fedcba9876543210"), encode("1"), "offline-session", "1.5.1", "", "", "", "1700000000000", "203.0.113.8", "1"}, "#")
	ciphertext, err := AESCBCEncryptBase64([]byte(plain), "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	config, err := ParseDeviceConfig(ciphertext, "0123456789abcdef")
	if err != nil || config.Key != "fedcba9876543210" || config.Switch != 1 || len(config.ExtraSegments) != 1 {
		t.Fatalf("config: %+v %v", config, err)
	}
}

func TestTransformAndDataMatchPythonOracle(t *testing.T) {
	oracle := loadOracle(t)
	input := make([]byte, 256)
	for index := range input {
		input[index] = byte(index)
	}
	transformed, err := Transform64(input, PE091DataKey, false)
	if err != nil || base64.StdEncoding.EncodeToString(transformed) != oracle.Transform256Base64 {
		t.Fatalf("transform: %v", err)
	}
	restored, err := Transform64(transformed, PE091DataKey, true)
	if err != nil || !bytes.Equal(restored, input) {
		t.Fatalf("inverse transform: %v", err)
	}
	arg, _ := Transform64([]byte("0123456789abcdef"), "0kd8i0mclivjow32", false)
	if base64.StdEncoding.EncodeToString(arg) != oracle.PEArg {
		t.Fatal("PE arg mismatch")
	}
	decoded, err := UnpackData(oracle.Data)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.JSONText != oracle.PayloadJSON || decoded.CompressedBase64 != oracle.CompressedBase64 {
		t.Fatal("decoded data mismatch")
	}
	currentPayload := decoded.Payload
	currentPayload.TrackList.SI = ""
	currentData, err := PackData(currentPayload, decoded.ChecksumPrefix)
	if err != nil {
		t.Fatal(err)
	}
	currentDecoded, err := UnpackData(currentData)
	if err != nil || currentDecoded.Payload.TrackList.SI != "" || strings.Contains(currentDecoded.JSONText, `"si"`) {
		t.Fatalf("current 10-field TrackList rejected: %v", err)
	}
	repacked, err := PackData(decoded.Payload, decoded.ChecksumPrefix)
	if err != nil {
		t.Fatal(err)
	}
	redecoded, err := UnpackData(repacked)
	if err != nil || redecoded.JSONText != oracle.PayloadJSON || redecoded.ChecksumPrefix != decoded.ChecksumPrefix {
		t.Fatalf("semantic repack mismatch: %v", err)
	}
	if _, err := UnpackData("%%"); err == nil {
		t.Fatal("bad data base64 accepted")
	}
	if _, err := PackData(DataPayload{}, strings.Repeat("0", 32)); err == nil {
		t.Fatal("bad schema accepted")
	}
}

func TestUnpackRejectsSchemaError(t *testing.T) {
	text := strings.Repeat("0", 30) + "01" + `{"TrackList":{}}`
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	_, _ = writer.Write([]byte(text))
	_ = writer.Close()
	inner := []byte(base64.StdEncoding.EncodeToString(compressed.Bytes()))
	transformed, _ := Transform64(inner, PE091DataKey, false)
	if _, err := UnpackData(base64.StdEncoding.EncodeToString(transformed)); err == nil {
		t.Fatal("bad schema accepted")
	}
}

func TestUnpackRejectsInvalidUTF8AndOversize(t *testing.T) {
	encodeRaw := func(raw []byte) string {
		t.Helper()
		var compressed bytes.Buffer
		writer := zlib.NewWriter(&compressed)
		if _, err := writer.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		inner := []byte(base64.StdEncoding.EncodeToString(compressed.Bytes()))
		transformed, err := Transform64(inner, PE091DataKey, false)
		if err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(transformed)
	}
	prefix := []byte(strings.Repeat("0", 30) + "01")
	if _, err := UnpackData(encodeRaw(append(prefix, 0xff))); err == nil {
		t.Fatal("invalid UTF-8 data accepted")
	}
	oversize := make([]byte, maxDataPlaintextSize+1)
	copy(oversize, prefix)
	for index := len(prefix); index < len(oversize); index++ {
		oversize[index] = 'x'
	}
	if _, err := UnpackData(encodeRaw(oversize)); err == nil {
		t.Fatal("oversized data plaintext accepted")
	}
}

func TestParamsMatchPythonOracle(t *testing.T) {
	oracle := loadOracle(t)
	business, err := BuildBusinessCaptchaVerifyParam("certify-fixture", "1ug4aptr", "security-fixture", true)
	if err != nil || business != oracle.BusinessParam {
		t.Fatalf("business param: %v", err)
	}
	parsed, err := ParseBusinessCaptchaVerifyParam(business)
	if err != nil || parsed.CertifyID != "certify-fixture" || parsed.SceneID != "1ug4aptr" || parsed.SecurityToken != "security-fixture" || !parsed.IsSign {
		t.Fatalf("parsed business param=%+v err=%v", parsed, err)
	}
	verify, err := BuildVerifyCaptchaParam("scene", "certify", "token", "data")
	if err != nil || verify != `{"sceneId":"scene","certifyId":"certify","deviceToken":"token","data":"data"}` {
		t.Fatalf("verify param: %q %v", verify, err)
	}
	canonical, signed, err := BuildBusinessSignedQuery("body", "token", "salt", 1700000000123, "00112233445566778899aabbccddeeff")
	if err != nil || canonical != oracle.BusinessQuery || !strings.HasPrefix(signed, canonical+"&lgsign=") {
		t.Fatalf("business query: %v", err)
	}
}

func TestProtocolValidationBranches(t *testing.T) {
	badUTF8 := string([]byte{0xff})
	if _, err := JSEncodeURIComponent(badUTF8); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
	if _, err := JSFormURLEncode([]Field{{badUTF8, "x"}}); err == nil {
		t.Fatal("invalid form key accepted")
	}
	if _, err := JSFormURLEncode([]Field{{"x", badUTF8}}); err == nil {
		t.Fatal("invalid form value accepted")
	}
	if _, err := CompactJSON(math.NaN()); err == nil {
		t.Fatal("NaN JSON accepted")
	}
	if _, err := RPCV1Signature(nil, ""); err == nil {
		t.Fatal("empty RPC secret accepted")
	}
	if _, err := RPCV1StringToSign(map[string]string{badUTF8: "x"}); err == nil {
		t.Fatal("invalid string-to-sign input accepted")
	}
	if _, err := RPCV1Signature(map[string]string{badUTF8: "x"}, "secret"); err == nil {
		t.Fatal("invalid signature input accepted")
	}
	if _, err := RPCV1CanonicalQuery(map[string]string{badUTF8: "x"}); err == nil {
		t.Fatal("invalid RPC key accepted")
	}
	if _, err := RPCV1CanonicalQuery(map[string]string{"x": badUTF8}); err == nil {
		t.Fatal("invalid RPC value accepted")
	}
	if _, err := AESCBCEncryptBase64WithIV(nil, []byte("0123456789abcdef"), []byte("short")); err == nil {
		t.Fatal("short IV accepted")
	}
	if _, err := AESCBCDecryptBase64WithIV("AA==", []byte("0123456789abcdef"), []byte("short")); err == nil {
		t.Fatal("short decrypt IV accepted")
	}
	if _, err := Transform64(nil, "", false); err == nil {
		t.Fatal("empty transform key accepted")
	}
	if _, err := Transform64(nil, "密钥", false); err == nil {
		t.Fatal("non-ASCII transform key accepted")
	}
	valid := DataPayload{TrackList: TrackList{StartTime: 1}, TrackStartTime: 1, VerifyTime: 2}
	packed, err := PackData(valid, "")
	if err != nil {
		t.Fatalf("random-prefix pack: %v", err)
	}
	if _, err := UnpackData(packed); err != nil {
		t.Fatalf("random-prefix unpack: %v", err)
	}
	if _, err := PackData(valid, "xyz"); err == nil {
		t.Fatal("bad prefix accepted")
	}
	if _, err := PackData(DataPayload{TrackList: TrackList{StartTime: math.Inf(1)}, TrackStartTime: 1, VerifyTime: 2}, strings.Repeat("0", 32)); err == nil {
		t.Fatal("non-finite time accepted")
	}
	if _, err := BuildVerifyCaptchaParam("", "c", "t", "d"); err == nil {
		t.Fatal("empty verify field accepted")
	}
	if _, err := BuildBusinessCaptchaVerifyParam("", "s", "t", true); err == nil {
		t.Fatal("empty business field accepted")
	}
	for _, value := range []string{
		"%%",
		base64.StdEncoding.EncodeToString([]byte(`{"certifyId":"c","sceneId":"s","isSign":true}`)),
		base64.StdEncoding.EncodeToString([]byte(`{"certifyId":"c","sceneId":"s","isSign":true,"securityToken":"t","extra":1}`)),
		base64.StdEncoding.EncodeToString([]byte(`{"certifyId":"c","sceneId":"s","isSign":true,"securityToken":"t"}{}`)),
	} {
		if _, err := ParseBusinessCaptchaVerifyParam(value); err == nil {
			t.Fatalf("invalid business parameter accepted: %q", value)
		}
	}
	if _, _, err := BuildBusinessSignedQuery("", "", "", -1, strings.Repeat("0", 32)); err == nil {
		t.Fatal("negative lgtime accepted")
	}
	if _, _, err := BuildBusinessSignedQuery("", "", "", 1, strings.Repeat("A", 32)); err == nil {
		t.Fatal("uppercase nonce accepted")
	}
	if _, err := BuildDeviceToken("bad#session", "cipher", "1", "salt"); err == nil {
		t.Fatal("bad session accepted")
	}
	if _, err := BuildDeviceToken("session", "bad#cipher", "1", "salt"); err == nil {
		t.Fatal("bad cipher accepted")
	}
	if _, err := BuildDeviceToken("session", "cipher", "-1", "salt"); err == nil {
		t.Fatal("bad cost accepted")
	}
	if _, err := BuildDeviceToken("session", "cipher", "1", ""); err == nil {
		t.Fatal("empty salt accepted")
	}
	if _, err := ParseDeviceToken("%%", "salt"); err == nil {
		t.Fatal("bad token base64 accepted")
	}
	nonUTF8 := base64.StdEncoding.EncodeToString([]byte{0xff})
	if _, err := ParseDeviceToken(nonUTF8, "salt"); err == nil {
		t.Fatal("non-UTF8 token accepted")
	}
	wrongPlatform := base64.StdEncoding.EncodeToString([]byte("MOB#s#c#1#00000000000000000000000000000000"))
	if _, err := ParseDeviceToken(wrongPlatform, "salt"); err == nil {
		t.Fatal("wrong platform accepted")
	}
	validToken, _ := BuildDeviceToken("session", "cipher", "1", "salt")
	if _, err := ParseDeviceToken(validToken, ""); err == nil {
		t.Fatal("empty parse salt accepted")
	}
	badCost := base64.StdEncoding.EncodeToString([]byte("WEB#s#c#x#00000000000000000000000000000000"))
	if _, err := ParseDeviceToken(badCost, "salt"); err == nil {
		t.Fatal("bad parsed cost accepted")
	}
	badChecksum := base64.StdEncoding.EncodeToString([]byte("WEB#s#c#1#ABC"))
	if _, err := ParseDeviceToken(badChecksum, "salt"); err == nil {
		t.Fatal("bad checksum accepted")
	}
	if _, err := ParseDeviceConfig("%%", "0123456789abcdef"); err == nil {
		t.Fatal("bad config accepted")
	}
	shortConfig, _ := AESCBCEncryptBase64([]byte("a#b"), "0123456789abcdef")
	if _, err := ParseDeviceConfig(shortConfig, "0123456789abcdef"); err == nil {
		t.Fatal("short config accepted")
	}
	badInnerConfig, _ := AESCBCEncryptBase64([]byte("%%#MQ==#session#v####1#ip"), "0123456789abcdef")
	if _, err := ParseDeviceConfig(badInnerConfig, "0123456789abcdef"); err == nil {
		t.Fatal("bad inner config base64 accepted")
	}
	badSwitchConfig, _ := AESCBCEncryptBase64([]byte(base64.StdEncoding.EncodeToString([]byte("fedcba9876543210"))+"#eA==#session#v####1#ip"), "0123456789abcdef")
	if _, err := ParseDeviceConfig(badSwitchConfig, "0123456789abcdef"); err == nil {
		t.Fatal("bad config switch accepted")
	}
	badPluginConfig, _ := AESCBCEncryptBase64([]byte(base64.StdEncoding.EncodeToString([]byte("fedcba9876543210"))+"#MQ==#session#v#%%###1#ip"), "0123456789abcdef")
	if _, err := ParseDeviceConfig(badPluginConfig, "0123456789abcdef"); err == nil {
		t.Fatal("bad plugin config accepted")
	}
	innerGarbage, _ := Transform64([]byte("%%"), PE091DataKey, false)
	if _, err := UnpackData(base64.StdEncoding.EncodeToString(innerGarbage)); err == nil {
		t.Fatal("bad inner data base64 accepted")
	}
	badZlibInner := []byte(base64.StdEncoding.EncodeToString([]byte("not-zlib")))
	badZlib, _ := Transform64(badZlibInner, PE091DataKey, false)
	if _, err := UnpackData(base64.StdEncoding.EncodeToString(badZlib)); err == nil {
		t.Fatal("bad zlib data accepted")
	}
	var shortCompressed bytes.Buffer
	shortWriter := zlib.NewWriter(&shortCompressed)
	_, _ = shortWriter.Write([]byte("short"))
	_ = shortWriter.Close()
	shortInner := []byte(base64.StdEncoding.EncodeToString(shortCompressed.Bytes()))
	shortTransformed, _ := Transform64(shortInner, PE091DataKey, false)
	if _, err := UnpackData(base64.StdEncoding.EncodeToString(shortTransformed)); err == nil {
		t.Fatal("short data plaintext accepted")
	}
}
