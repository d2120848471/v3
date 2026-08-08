package vision

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPythonOracleEdgeDecoyFixtures(t *testing.T) {
	for _, test := range []struct {
		name       string
		hasGap     bool
		wantX      int
		wantMethod string
	}{{name: "gap", hasGap: true, wantX: 241, wantMethod: "global-chamfer"}, {name: "no-gap", hasGap: false, wantX: 171}} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join("testdata", "python-edge-decoy", test.name)
			background, err := os.ReadFile(filepath.Join(root, "back.png"))
			if err != nil {
				t.Fatal(err)
			}
			shadow, err := os.ReadFile(filepath.Join(root, "shadow.png"))
			if err != nil {
				t.Fatal(err)
			}
			estimate, err := Solve(context.Background(), background, shadow, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			if test.hasGap {
				if absInt(estimate.XPos-test.wantX) > 1 || estimate.Confidence < chamferTrigger || !hasMethod(estimate.Candidates, test.wantMethod) {
					t.Fatalf("Go estimate=%+v; Python oracle wants x=%d±1 confidence>=%.2f method=%s", estimate, test.wantX, chamferTrigger, test.wantMethod)
				}
			} else if estimate.Confidence >= chamferTrigger || hasMethod(estimate.Candidates, "global-chamfer") {
				t.Fatalf("Go negative estimate=%+v; Python oracle rejects the gap", estimate)
			}
		})
	}
}

func TestDifficultContourFallback(t *testing.T) {
	background, shadow := syntheticAssets(t, true)
	estimate, err := Solve(context.Background(), background, shadow, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if absInt(estimate.XPos-241) > 1 {
		t.Fatalf("xPos=%d, want 241±1; confidence=%f candidates=%+v", estimate.XPos, estimate.Confidence, estimate.Candidates)
	}
	if estimate.Confidence < chamferTrigger {
		t.Fatalf("confidence=%f, want >= %f", estimate.Confidence, chamferTrigger)
	}
	if !hasMethod(estimate.Candidates, "global-chamfer") {
		t.Fatalf("candidates=%+v, want global-chamfer", estimate.Candidates)
	}
}

func TestContourFallbackRejectsSceneWithoutGap(t *testing.T) {
	background, shadow := syntheticAssets(t, false)
	estimate, err := Solve(context.Background(), background, shadow, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if estimate.Confidence >= chamferTrigger {
		t.Fatalf("confidence=%f, want < %f; xPos=%d candidates=%+v", estimate.Confidence, chamferTrigger, estimate.XPos, estimate.Candidates)
	}
	if hasMethod(estimate.Candidates, "global-chamfer") {
		t.Fatalf("candidates=%+v, unexpected global-chamfer", estimate.Candidates)
	}
}

func TestInputBoundaries(t *testing.T) {
	background, shadow := syntheticAssets(t, true)
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Solve(ctx, background, shadow, DefaultLimits()); err != context.Canceled {
			t.Fatalf("error=%v, want context.Canceled", err)
		}
	})
	t.Run("invalid PNG", func(t *testing.T) {
		if _, err := Solve(context.Background(), []byte("not png"), shadow, DefaultLimits()); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("byte limit", func(t *testing.T) {
		limits := DefaultLimits()
		limits.MaxBytes = len(background) - 1
		if _, err := Solve(context.Background(), background, shadow, limits); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("dimension limit", func(t *testing.T) {
		limits := DefaultLimits()
		limits.MaxDimension = 100
		if _, err := Solve(context.Background(), background, shadow, limits); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("shadow without alpha", func(t *testing.T) {
		gray := image.NewGray(image.Rect(0, 0, 52, 200))
		if _, err := Solve(context.Background(), background, encodePNG(t, gray), DefaultLimits()); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("height mismatch", func(t *testing.T) {
		other := image.NewNRGBA(image.Rect(0, 0, 52, 199))
		other.SetNRGBA(1, 1, color.NRGBA{A: 255})
		if _, err := Solve(context.Background(), background, encodePNG(t, other), DefaultLimits()); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("shadow wider than background", func(t *testing.T) {
		other := image.NewNRGBA(image.Rect(0, 0, 297, 200))
		other.SetNRGBA(1, 1, color.NRGBA{A: 255})
		if _, err := Solve(context.Background(), background, encodePNG(t, other), DefaultLimits()); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("empty alpha", func(t *testing.T) {
		other := image.NewNRGBA(image.Rect(0, 0, 52, 200))
		if _, err := Solve(context.Background(), background, encodePNG(t, other), DefaultLimits()); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("shape too small", func(t *testing.T) {
		other := image.NewNRGBA(image.Rect(0, 0, 52, 200))
		other.SetNRGBA(20, 20, color.NRGBA{A: 255})
		if _, err := Solve(context.Background(), background, encodePNG(t, other), DefaultLimits()); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestConcurrentSolveIsDeterministic(t *testing.T) {
	background, shadow := syntheticAssets(t, true)
	want, err := Solve(context.Background(), background, shadow, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	const workers = 16
	results := make(chan Estimate, workers)
	errors := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			got, solveErr := Solve(context.Background(), background, shadow, DefaultLimits())
			if solveErr != nil {
				errors <- solveErr
				return
			}
			results <- got
		}()
	}
	wait.Wait()
	close(results)
	close(errors)
	for solveErr := range errors {
		t.Error(solveErr)
	}
	for got := range results {
		if got.XPos != want.XPos || got.SlidePos != want.SlidePos || got.Confidence != want.Confidence || len(got.Candidates) != len(want.Candidates) {
			t.Fatalf("non-deterministic result: got=%+v want=%+v", got, want)
		}
		for index := range got.Candidates {
			if got.Candidates[index] != want.Candidates[index] {
				t.Fatalf("candidate[%d]=%+v, want %+v", index, got.Candidates[index], want.Candidates[index])
			}
		}
	}
}

func TestGeometry(t *testing.T) {
	for _, test := range []struct {
		value float64
		want  int
	}{{0, 0}, {0.49, 0}, {0.5, 1}, {1.5, 2}} {
		got, err := JSMathRound(test.value)
		if err != nil || got != test.want {
			t.Fatalf("JSMathRound(%v)=(%d,%v), want %d", test.value, got, err, test.want)
		}
	}
	for _, invalid := range []float64{-1, math.NaN(), math.Inf(1)} {
		if _, err := JSMathRound(invalid); err == nil {
			t.Fatalf("JSMathRound(%v) accepted", invalid)
		}
	}
	x, err := PuzzleXFromSlidePos(120)
	if err != nil {
		t.Fatal(err)
	}
	slide, err := SlidePosFromPuzzleX(x, 300, 40)
	if err != nil || slide != 120 {
		t.Fatalf("round trip slide=(%d,%v), want 120", slide, err)
	}
	clamped, err := SlidePosFromPuzzleX(1e9, 300, 40)
	if err != nil || clamped != 260 {
		t.Fatalf("clamped slide=(%d,%v), want 260", clamped, err)
	}
	if _, err := PuzzleXFromSlidePos(-1); err == nil {
		t.Fatal("negative slide position accepted")
	}
	if _, err := JSMathRound(math.MaxFloat64); err == nil {
		t.Fatal("overflowing round accepted")
	}
	for _, dimensions := range [][2]float64{{0, 0}, {300, -1}, {300, 301}, {math.Inf(1), 1}} {
		if _, err := SlidePosFromPuzzleX(1, dimensions[0], dimensions[1]); err == nil {
			t.Fatalf("invalid dimensions accepted: %v", dimensions)
		}
	}
}

func TestPixelModelConversions(t *testing.T) {
	images := []image.Image{
		image.NewNRGBA(image.Rect(0, 0, 1, 1)),
		image.NewNRGBA64(image.Rect(0, 0, 1, 1)),
		image.NewRGBA(image.Rect(0, 0, 1, 1)),
		image.NewRGBA64(image.Rect(0, 0, 1, 1)),
		image.NewGray(image.Rect(0, 0, 1, 1)),
		image.NewGray16(image.Rect(0, 0, 1, 1)),
	}
	for _, source := range images {
		_ = nonPremultipliedRGBA(source, 0, 0)
	}
	alphaImages := []image.Image{
		images[0], images[1], images[2], images[3],
		image.NewAlpha(image.Rect(0, 0, 1, 1)),
		image.NewAlpha16(image.Rect(0, 0, 1, 1)),
	}
	for _, source := range alphaImages {
		_ = alphaAt(source, 0, 0)
	}
	palette := color.Palette{color.NRGBA{R: 10, G: 20, B: 30, A: 40}}
	paletted := image.NewPaletted(image.Rect(0, 0, 1, 1), palette)
	if got := alphaAt(paletted, 0, 0); got != 40 {
		t.Fatalf("palette alpha=%d, want 40", got)
	}
	_ = nonPremultipliedRGBA(paletted, 0, 0)
	for _, value := range []color.Color{
		color.NRGBA{R: 1, A: 2}, color.NRGBA64{R: 0xffff, A: 0xffff},
		color.RGBA{R: 1, A: 2}, color.Gray{Y: 3}, color.Gray16{Y: 0xffff},
	} {
		_ = colorToNRGBA(value)
	}
	_ = unpremultiply16(1, 2, 3, 0)
	_ = unpremultiply16(1, 2, 3, 4)
	_ = unpremultiply8(1, 2, 3, 0)
}

func BenchmarkSolve(b *testing.B) {
	background, shadow := syntheticAssets(b, true)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := Solve(context.Background(), background, shadow, DefaultLimits()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSolveParallel(b *testing.B) {
	background, shadow := syntheticAssets(b, true)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(parallel *testing.PB) {
		for parallel.Next() {
			if _, err := Solve(context.Background(), background, shadow, DefaultLimits()); err != nil {
				b.Error(err)
			}
		}
	})
}

func hasMethod(candidates []Candidate, method string) bool {
	for _, candidate := range candidates {
		if candidate.Method == method {
			return true
		}
	}
	return false
}

func syntheticAssets(testingT interface {
	Helper()
	Fatalf(string, ...any)
}, includeGap bool) ([]byte, []byte) {
	testingT.Helper()
	const height, width, shadowWidth = 200, 296, 52
	random := rand.New(rand.NewSource(16))
	background := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			b, g, r := 165.0, 185.0, 200.0
			if y >= 95 && y < 195 && x >= 70 && x < 220 {
				b, g, r = 210, 220, 230
			}
			if x >= 240 {
				b = min(max(20+random.NormFloat64()*45, 0), 180)
				g = min(max(45+random.NormFloat64()*45, 0), 180)
				r = min(max(70+random.NormFloat64()*45, 0), 180)
			}
			background.SetNRGBA(x, y, color.NRGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 255})
		}
	}
	alpha := make([]uint8, height*shadowWidth)
	for y := 0; y < height; y++ {
		for x := 0; x < shadowWidth; x++ {
			inside := x >= 4 && x <= 47 && y >= 126 && y <= 179
			inside = inside || circleContains(x, y, 26, 126, 11)
			inside = inside || circleContains(x, y, 47, 151, 10)
			if circleContains(x, y, 4, 156, 9) {
				inside = false
			}
			if inside {
				alpha[y*shadowWidth+x] = 255
			}
		}
	}
	alpha = gaussian3x3(alpha, shadowWidth, height)
	if includeGap {
		solid := make([]bool, len(alpha))
		for index, value := range alpha {
			solid[index] = value > shapeThreshold
		}
		fill := erodeEllipse5(solid, shadowWidth, height)
		for relative, use := range fill {
			if !use {
				continue
			}
			y, x := relative/shadowWidth, relative%shadowWidth
			pixel := background.NRGBAAt(241+x, y)
			pixel.R = uint8(0.5*float64(pixel.R) + 124)
			pixel.G = uint8(0.5*float64(pixel.G) + 124)
			pixel.B = uint8(0.5*float64(pixel.B) + 124)
			background.SetNRGBA(241+x, y, pixel)
		}
	}
	shadow := image.NewNRGBA(image.Rect(0, 0, shadowWidth, height))
	for y := 0; y < height; y++ {
		for x := 0; x < shadowWidth; x++ {
			shadow.SetNRGBA(x, y, color.NRGBA{A: alpha[y*shadowWidth+x]})
		}
	}
	return encodePNG(testingT, background), encodePNG(testingT, shadow)
}

func encodePNG(testingT interface {
	Helper()
	Fatalf(string, ...any)
}, value image.Image) []byte {
	testingT.Helper()
	var output bytes.Buffer
	if err := png.Encode(&output, value); err != nil {
		testingT.Fatalf("encode PNG: %v", err)
	}
	return output.Bytes()
}

func circleContains(x, y, centerX, centerY, radius int) bool {
	dx, dy := x-centerX, y-centerY
	return dx*dx+dy*dy <= radius*radius
}

func gaussian3x3(source []uint8, width, height int) []uint8 {
	result := make([]uint8, len(source))
	weights := [3]int{1, 2, 1}
	reflect101 := func(value, size int) int {
		if value < 0 {
			return -value
		}
		if value >= size {
			return 2*size - value - 2
		}
		return value
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			sum := 0
			for ky := -1; ky <= 1; ky++ {
				for kx := -1; kx <= 1; kx++ {
					sum += int(source[reflect101(y+ky, height)*width+reflect101(x+kx, width)]) * weights[ky+1] * weights[kx+1]
				}
			}
			result[y*width+x] = uint8((sum + 8) / 16)
		}
	}
	return result
}

func erodeEllipse5(source []bool, width, height int) []bool {
	result := make([]bool, len(source))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			inside := true
			for dy := -2; dy <= 2 && inside; dy++ {
				span := 2
				if dy == -2 || dy == 2 {
					span = 0
				}
				for dx := -span; dx <= span; dx++ {
					nx, ny := x+dx, y+dy
					if nx >= 0 && nx < width && ny >= 0 && ny < height && !source[ny*width+nx] {
						inside = false
						break
					}
				}
			}
			result[y*width+x] = inside && source[y*width+x]
		}
	}
	return result
}
