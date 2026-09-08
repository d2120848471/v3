package vision

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// FuzzPNGPairSeeds 用有界资源限制覆盖 PNG 头、解码与视觉入口。
// 普通 go test 只执行下列种子；持续 fuzz 必须由开发者显式启用。
func FuzzPNGPairSeeds(f *testing.F) {
	background := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	shadow := image.NewNRGBA(image.Rect(0, 0, 8, 16))
	for y := range 16 {
		for x := range 16 {
			background.SetNRGBA(x, y, color.NRGBA{R: uint8(80 + x), G: uint8(90 + y), B: 100, A: 255})
		}
	}
	for y := 4; y < 12; y++ {
		for x := 2; x < 6; x++ {
			shadow.SetNRGBA(x, y, color.NRGBA{A: 255})
		}
	}
	f.Add(encodeFuzzPNG(background), encodeFuzzPNG(shadow))
	f.Add([]byte{}, []byte{})
	f.Add(append([]byte(nil), pngSignature...), []byte("not-png"))

	limits := Limits{MaxBytes: 64 << 10, MaxDimension: 64, MaxPixels: 64 * 64}
	f.Fuzz(func(t *testing.T, backgroundPNG, shadowPNG []byte) {
		if len(backgroundPNG) > limits.MaxBytes || len(shadowPNG) > limits.MaxBytes {
			return
		}
		pair, err := decodePair(backgroundPNG, shadowPNG, limits)
		if err != nil {
			return
		}
		if pair.width < 1 || pair.height < 1 || pair.shadowWidth < 1 || pair.shadowWidth > pair.width {
			t.Fatalf("decoded pair violates geometry: %+v", pair)
		}
		if len(pair.bgr) != pair.width*pair.height*3 || len(pair.alpha) != pair.shadowWidth*pair.height {
			t.Fatal("decoded pair violates buffer invariants")
		}
		// 同一有界输入继续经过公开求解入口，允许正常返回低置信或合同错误。
		_, _ = Solve(context.Background(), backgroundPNG, shadowPNG, limits)
	})
}

func encodeFuzzPNG(source image.Image) []byte {
	var output bytes.Buffer
	if err := png.Encode(&output, source); err != nil {
		panic(err)
	}
	return output.Bytes()
}
