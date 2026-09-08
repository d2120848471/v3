package engine

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/track"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
)

type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time                       { return clock.now }
func (fixedClock) Sleep(context.Context, time.Duration) error { return nil }

type byteEntropy struct {
	data []byte
	err  error
}

func (entropy *byteEntropy) Read(buffer []byte) (int, error) {
	if entropy.err != nil {
		return 0, entropy.err
	}
	if len(entropy.data) < len(buffer) {
		copied := copy(buffer, entropy.data)
		entropy.data = nil
		return copied, io.EOF
	}
	copy(buffer, entropy.data[:len(buffer)])
	entropy.data = entropy.data[len(buffer):]
	return len(buffer), nil
}

func (*byteEntropy) Uint64n(limit uint64) (uint64, error) {
	if limit == 0 {
		return 0, errors.New("zero limit")
	}
	return 0, nil
}

func oracleProfile() device.Profile {
	return device.Profile{Screen: device.ScreenProfile{
		Width: 412, Height: 915, InnerWidth: 412, InnerHeight: 803,
		OuterWidth: 412, OuterHeight: 915,
	}}
}

func oracleTrack() []track.Event {
	return []track.Event{
		{Type: "touchstart", X: 0, Y: 0, DT: 0},
		{Type: "touchmove", X: 30, Y: 0, DT: 20},
		{Type: "touchmove", X: 80, Y: 0, DT: 20},
		{Type: "touchend", X: 80, Y: 0, DT: 10},
	}
}

func builderWithEntropy(entropy runtimekit.Entropy, nowMS int64) pe.Builder {
	return pe.Builder{
		Profile: oracleProfile(),
		Sources: runtimekit.Sources{Clock: fixedClock{now: time.UnixMilli(nowMS)}, Entropy: entropy},
	}
}
