package vision

import (
	"math"
	"math/rand"
	"testing"
)

func TestLinearSRGBTableMatchesOriginalBits(t *testing.T) {
	for channel, got := range linearSRGB8 {
		want := originalLinearSRGB(float64(channel))
		if math.Float64bits(got) != math.Float64bits(want) {
			t.Fatalf("linearSRGB8[%d]=%016x, want %016x", channel, math.Float64bits(got), math.Float64bits(want))
		}
	}
}

func TestOpenCVCompatibleLabMatchesOriginalBits(t *testing.T) {
	check := func(red, green, blue uint8) {
		t.Helper()
		light, a, b := opencvCompatibleLab(red, green, blue)
		wantLight, wantA, wantB := originalOpenCVCompatibleLab(float64(red), float64(green), float64(blue))
		got, want := [3]float64{light, a, b}, [3]float64{wantLight, wantA, wantB}
		for channel := range got {
			if math.Float64bits(got[channel]) != math.Float64bits(want[channel]) {
				t.Fatalf("Lab(%d,%d,%d)=%v, want %v", red, green, blue, got, want)
			}
		}
	}
	for value := range 256 {
		channel := uint8(value)
		check(channel, channel, channel)
		for _, other := range []uint8{0, 10, 11, 127, 255} {
			check(channel, other, other)
			check(other, channel, other)
			check(other, other, channel)
		}
	}
	random := rand.New(rand.NewSource(20260908))
	for range 4096 {
		check(uint8(random.Intn(256)), uint8(random.Intn(256)), uint8(random.Intn(256)))
	}
}

// 以下参考保留优化前的 sRGB 和 Lab 公式，不读取生产查找表。
func originalLinearSRGB(value float64) float64 {
	value /= 255
	if value <= 0.04045 {
		return value / 12.92
	}
	return math.Pow((value+0.055)/1.055, 2.4)
}

func originalOpenCVCompatibleLab(red, green, blue float64) (lightness, a, b float64) {
	r, g, bl := originalLinearSRGB(red), originalLinearSRGB(green), originalLinearSRGB(blue)
	x := (0.4124564*r + 0.3575761*g + 0.1804375*bl) / 0.95047
	y := 0.2126729*r + 0.7151522*g + 0.0721750*bl
	z := (0.0193339*r + 0.1191920*g + 0.9503041*bl) / 1.08883
	f := func(value float64) float64 {
		if value > 216.0/24389.0 {
			return math.Cbrt(value)
		}
		return (24389.0/27.0*value + 16) / 116
	}
	fx, fy, fz := f(x), f(y), f(z)
	lStar := 116*fy - 16
	aStar := 500 * (fx - fy)
	bStar := 200 * (fy - fz)
	return clamp255(math.Round(lStar * 255 / 100)), clamp255(math.Round(aStar + 128)), clamp255(math.Round(bStar + 128))
}
