package runtimekit

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

type fixedEntropy struct{ reader *bytes.Reader }

func (f fixedEntropy) Read(buffer []byte) (int, error) { return f.reader.Read(buffer) }
func (f fixedEntropy) Uint64n(limit uint64) (uint64, error) {
	if limit == 0 {
		return 0, errors.New("zero")
	}
	return 0, nil
}

func TestUUIDv4Shape(t *testing.T) {
	raw := make([]byte, 16)
	for index := range raw {
		raw[index] = byte(index)
	}
	value, err := UUIDv4(fixedEntropy{reader: bytes.NewReader(raw)})
	if err != nil {
		t.Fatal(err)
	}
	if value != "00010203-0405-4607-8809-0a0b0c0d0e0f" {
		t.Fatalf("unexpected UUID %s", value)
	}
}

func TestHex(t *testing.T) {
	value, err := Hex(fixedEntropy{reader: bytes.NewReader([]byte{0, 1, 254, 255})}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if value != "0001feff" {
		t.Fatalf("unexpected hex %s", value)
	}
}

func TestSystemClockSleepCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := (SystemClock{}).Sleep(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error %v", err)
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("cancelled sleep was not prompt")
	}
}
