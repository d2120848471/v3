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
	if _, err := UUIDv4(fixedEntropy{reader: bytes.NewReader(nil)}); err == nil {
		t.Fatal("UUIDv4 accepted exhausted entropy")
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
	if _, err := Hex(fixedEntropy{reader: bytes.NewReader(nil)}, -1); err == nil {
		t.Fatal("Hex accepted negative byte count")
	}
	if _, err := Hex(fixedEntropy{reader: bytes.NewReader(nil)}, 1); err == nil {
		t.Fatal("Hex accepted exhausted entropy")
	}
}

func TestSystemSourcesAndCryptoEntropy(t *testing.T) {
	sources := NewSystemSources()
	if err := sources.Validate(); err != nil {
		t.Fatal(err)
	}
	if sources.Clock.Now().IsZero() {
		t.Fatal("system clock returned zero time")
	}
	if value, err := sources.Entropy.Uint64n(1); err != nil || value != 0 {
		t.Fatalf("system entropy Uint64n(1)=%d, %v", value, err)
	}
	if err := (Sources{}).Validate(); err == nil {
		t.Fatal("empty sources passed validation")
	}
	if err := (Sources{Clock: SystemClock{}}).Validate(); err == nil {
		t.Fatal("sources without entropy passed validation")
	}

	entropy := CryptoEntropy{Reader: bytes.NewReader([]byte{1, 2})}
	buffer := make([]byte, 2)
	if count, err := entropy.Read(buffer); err != nil || count != len(buffer) || !bytes.Equal(buffer, []byte{1, 2}) {
		t.Fatalf("entropy read count=%d buffer=%v err=%v", count, buffer, err)
	}
	if _, err := entropy.Uint64n(0); err == nil {
		t.Fatal("Uint64n accepted zero limit")
	}
	if value, err := (CryptoEntropy{Reader: bytes.NewReader(make([]byte, 8))}).Uint64n(2); err != nil || value != 0 {
		t.Fatalf("deterministic Uint64n(2)=%d, %v", value, err)
	}
	if _, err := (CryptoEntropy{Reader: bytes.NewReader(nil)}).Uint64n(2); err == nil {
		t.Fatal("Uint64n accepted exhausted entropy")
	}
}

func TestSystemClockSleepNonPositive(t *testing.T) {
	clock := SystemClock{}
	if err := clock.Sleep(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := clock.Sleep(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected cancelled zero sleep error %v", err)
	}
	if err := clock.Sleep(context.Background(), time.Nanosecond); err != nil {
		t.Fatal(err)
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
