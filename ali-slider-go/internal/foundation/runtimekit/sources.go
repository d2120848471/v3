// Package runtimekit 提供可替换的时钟、熵和 UUID 来源。
//
// 生产实现只使用 Go 标准库的密码学安全随机源；测试可注入确定性实现，避免
// 时间和随机状态在并发测试中互相污染。
package runtimekit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"time"
)

// Clock 同时提供墙钟和可取消等待。time.Time 自带的 monotonic 部分只用于耗时。
type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

// Entropy 提供并发安全的密码学随机字节和无偏整数。
type Entropy interface {
	Read([]byte) (int, error)
	Uint64n(uint64) (uint64, error)
}

// Sources 是一轮挑战使用的外部非确定性来源。
type Sources struct {
	Clock   Clock
	Entropy Entropy
}

// NewSystemSources 返回生产环境来源。每个方法均可被多个 goroutine 安全调用。
func NewSystemSources() Sources {
	return Sources{Clock: SystemClock{}, Entropy: CryptoEntropy{Reader: rand.Reader}}
}

// Validate 拒绝部分注入，避免测试替身意外进入生产路径。
func (s Sources) Validate() error {
	if s.Clock == nil || s.Entropy == nil {
		return errors.New("runtime sources must include clock and entropy")
	}
	return nil
}

// SystemClock 使用系统墙钟，并通过 timer 响应 context 取消。
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

func (SystemClock) Sleep(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// CryptoEntropy 封装 crypto/rand.Reader。Reader 字段只用于确定性测试。
type CryptoEntropy struct {
	Reader io.Reader
}

func (e CryptoEntropy) reader() io.Reader {
	if e.Reader == nil {
		return rand.Reader
	}
	return e.Reader
}

func (e CryptoEntropy) Read(buffer []byte) (int, error) {
	return io.ReadFull(e.reader(), buffer)
}

func (e CryptoEntropy) Uint64n(limit uint64) (uint64, error) {
	if limit == 0 {
		return 0, errors.New("random limit must be positive")
	}
	value, err := rand.Int(e.reader(), new(big.Int).SetUint64(limit))
	if err != nil {
		return 0, err
	}
	return value.Uint64(), nil
}

// UUIDv4 返回 RFC 4122 形状的小写 UUIDv4。
func UUIDv4(entropy Entropy) (string, error) {
	var raw [16]byte
	if _, err := entropy.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	var dst [36]byte
	hex.Encode(dst[0:8], raw[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], raw[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], raw[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], raw[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], raw[10:16])
	return string(dst[:]), nil
}

// Hex 返回 count 个随机字节的小写十六进制文本。
func Hex(entropy Entropy, count int) (string, error) {
	if count < 0 {
		return "", errors.New("random byte count must be non-negative")
	}
	raw := make([]byte, count)
	if _, err := entropy.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
