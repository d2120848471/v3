package protocol

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"unicode/utf8"
)

const (
	PE091DataKey         = "3e627e1b4c63f913"
	maxDataPlaintextSize = 4 << 20
)

var initial64State = [64]byte{
	32, 50, 10, 51, 6, 44, 37, 16, 46, 11, 62, 19, 43, 25, 23, 30,
	60, 33, 53, 34, 7, 26, 12, 48, 5, 2, 20, 4, 61, 13, 47, 49,
	18, 29, 27, 22, 1, 17, 39, 56, 41, 38, 55, 31, 15, 58, 52, 40,
	8, 57, 45, 35, 59, 36, 42, 54, 63, 3, 24, 28, 14, 9, 0, 21,
}

type TrackList struct {
	MC        string `json:"mc"`
	TC        string `json:"tc"`
	MU        string `json:"mu"`
	TE        string `json:"te"`
	MP        string `json:"mp"`
	TMV       string `json:"tmv"`
	MM        string `json:"mm"`
	KS        string `json:"ks"`
	FI        string `json:"fi"`
	StartTime any    `json:"startTime"`
	// SI 在旧版 SDK 中存在；当前 SDK 已删除。omitempty 让调用方按运行时
	// 采集到的 Track schema 精确选择 10 字段或历史 11 字段格式。
	SI string `json:"si,omitempty"`
}

type DataPayload struct {
	TrackList      TrackList `json:"TrackList"`
	TrackStartTime any       `json:"TrackStartTime"`
	VerifyTime     any       `json:"VerifyTime"`
	XPos           string    `json:"xPos"`
	SlidePos       string    `json:"slidePos"`
	Arg            string    `json:"arg"`
}

type DecodedData struct {
	ChecksumPrefix   string
	JSONText         string
	Payload          DataPayload
	CompressedBase64 string
}

func keyedState64(key string) ([64]byte, error) {
	if key == "" {
		return [64]byte{}, fmt.Errorf("64-state key must not be empty")
	}
	for _, character := range []byte(key) {
		if character > 0x7f {
			return [64]byte{}, fmt.Errorf("64-state key must contain ASCII bytes only")
		}
	}
	state := initial64State
	swap := 0
	for index := 0; index < 64; index++ {
		swap = (((index + swap + int(state[index]) + int(state[swap])) >> 1) + int(key[index%len(key)])) & 63
		state[index], state[swap] = state[swap], state[index]
	}
	return state, nil
}

// Transform64 执行 PE 的 64-state 可逆变换；它不是 RC4。
func Transform64(data []byte, key string, decrypt bool) ([]byte, error) {
	state, err := keyedState64(key)
	if err != nil {
		return nil, err
	}
	output := make([]byte, len(data))
	stateIndex, swap := 0, 0
	for offset, source := range data {
		swap = ((stateIndex ^ swap) + (int(state[stateIndex]) ^ int(state[swap]))) & 63
		state[stateIndex], state[swap] = state[swap], state[stateIndex]
		mixA := int(state[stateIndex]) + int(state[swap])
		mixB := int(state[mixA&63])
		var value int
		if decrypt {
			value = (int(source) ^ mixB ^ mixA) - stateIndex - int(state[stateIndex]) + swap + int(state[swap])
		} else {
			value = (int(source) + stateIndex + int(state[stateIndex]) - swap - int(state[swap])) ^ mixA ^ mixB
		}
		output[offset] = byte(value & 0xff)
		stateIndex = (stateIndex + 1) & 63
	}
	return output, nil
}

func validatePrefix(value string) (string, error) {
	if len(value) != 32 {
		return "", fmt.Errorf("data prefix must contain 32 hex characters")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", fmt.Errorf("data prefix must contain 32 hex characters")
	}
	return strings.ToLower(value), nil
}

func numberOK(value any) bool {
	if value == nil {
		return false
	}
	if number, ok := value.(json.Number); ok {
		_, err := number.Float64()
		return err == nil
	}
	item := reflect.ValueOf(value)
	switch item.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	case reflect.Float32, reflect.Float64:
		return !math.IsNaN(item.Float()) && !math.IsInf(item.Float(), 0)
	default:
		return false
	}
}

func validatePayload(payload DataPayload) error {
	if !numberOK(payload.TrackList.StartTime) || !numberOK(payload.TrackStartTime) || !numberOK(payload.VerifyTime) {
		return fmt.Errorf("data time fields must be finite numbers")
	}
	return nil
}

func randomPrefix() (string, error) {
	raw := make([]byte, 15)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", fmt.Errorf("generate data prefix: %w", err)
	}
	return hex.EncodeToString(raw) + "01", nil
}

// PackData 以固定字段顺序封装 data。prefix 为空时生成 15 随机字节 + 01。
func PackData(payload DataPayload, prefix string) (string, error) {
	return packDataWithKey(payload, prefix, PE091DataKey)
}

func packDataWithKey(payload DataPayload, prefix, key string) (string, error) {
	if err := validatePayload(payload); err != nil {
		return "", err
	}
	var err error
	if prefix == "" {
		prefix, err = randomPrefix()
		if err != nil {
			return "", err
		}
	}
	prefix, err = validatePrefix(prefix)
	if err != nil {
		return "", err
	}
	text, err := CompactJSON(payload)
	if err != nil {
		return "", err
	}
	var compressed bytes.Buffer
	writer, err := zlib.NewWriterLevel(&compressed, 6)
	if err != nil {
		return "", err
	}
	if _, err = writer.Write([]byte(prefix + text)); err == nil {
		err = writer.Close()
	} else {
		_ = writer.Close()
	}
	if err != nil {
		return "", fmt.Errorf("compress data: %w", err)
	}
	inner := []byte(base64.StdEncoding.EncodeToString(compressed.Bytes()))
	transformed, err := Transform64(inner, key, false)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(transformed), nil
}

func exactKeys(raw map[string]json.RawMessage, expected ...string) bool {
	if len(raw) != len(expected) {
		return false
	}
	for _, key := range expected {
		if _, ok := raw[key]; !ok {
			return false
		}
	}
	return true
}

func decodePayload(text string) (DataPayload, error) {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil || !exactKeys(raw, "TrackList", "TrackStartTime", "VerifyTime", "xPos", "slidePos", "arg") {
		return DataPayload{}, fmt.Errorf("data payload schema is invalid")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return DataPayload{}, fmt.Errorf("data JSON has trailing content")
	}
	var trackRaw map[string]json.RawMessage
	if err := json.Unmarshal(raw["TrackList"], &trackRaw); err != nil ||
		(!exactKeys(trackRaw, "mc", "tc", "mu", "te", "mp", "tmv", "mm", "ks", "fi", "startTime") &&
			!exactKeys(trackRaw, "mc", "tc", "mu", "te", "mp", "tmv", "mm", "ks", "fi", "startTime", "si")) {
		return DataPayload{}, fmt.Errorf("TrackList schema is invalid")
	}
	decoder = json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var payload DataPayload
	if err := decoder.Decode(&payload); err != nil {
		return DataPayload{}, fmt.Errorf("decode data payload: %w", err)
	}
	if err := validatePayload(payload); err != nil {
		return DataPayload{}, err
	}
	return payload, nil
}

// UnpackData 严格校验 Base64、64-state、zlib、UTF-8 prefix 和 schema。
func UnpackData(data string) (DecodedData, error) {
	ciphertext, err := decodeBase64Strict(data)
	if err != nil {
		return DecodedData{}, fmt.Errorf("decode outer data: %w", err)
	}
	inner, err := Transform64(ciphertext, PE091DataKey, true)
	if err != nil {
		return DecodedData{}, err
	}
	compressed, err := decodeBase64Strict(string(inner))
	if err != nil {
		return DecodedData{}, fmt.Errorf("decode inner data: %w", err)
	}
	reader, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return DecodedData{}, fmt.Errorf("open zlib data: %w", err)
	}
	serialized, readErr := io.ReadAll(io.LimitReader(reader, maxDataPlaintextSize+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		return DecodedData{}, fmt.Errorf("decompress data")
	}
	if len(serialized) > maxDataPlaintextSize {
		return DecodedData{}, fmt.Errorf("data plaintext exceeds size limit")
	}
	if !utf8.Valid(serialized) {
		return DecodedData{}, fmt.Errorf("data plaintext is not valid UTF-8")
	}
	if len(serialized) < 32 {
		return DecodedData{}, fmt.Errorf("data plaintext is too short")
	}
	prefix, err := validatePrefix(string(serialized[:32]))
	if err != nil {
		return DecodedData{}, err
	}
	text := string(serialized[32:])
	payload, err := decodePayload(text)
	if err != nil {
		return DecodedData{}, err
	}
	return DecodedData{prefix, text, payload, string(inner)}, nil
}
