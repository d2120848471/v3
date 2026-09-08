package vision

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"testing"
)

func TestChamferAlignmentMatchesOriginalAtROIBoundaries(t *testing.T) {
	for _, test := range []struct {
		name                       string
		width, height, shadowWidth int
		box                        BoundingBox
	}{
		{"uncropped", 72, 36, 24, BoundingBox{6, 6, 18, 30}},
		{"left-and-right-clipping", 72, 36, 24, BoundingBox{0, 6, 24, 30}},
		{"top-clipping", 72, 36, 24, BoundingBox{2, 0, 22, 24}},
		{"bottom-clipping", 72, 36, 24, BoundingBox{2, 12, 22, 36}},
		{"all-edges", 72, 36, 24, BoundingBox{0, 0, 24, 36}},
		{"same-size-changing-offset", 26, 36, 24, BoundingBox{0, 0, 24, 36}},
		{"no-horizontal-travel", 24, 36, 24, BoundingBox{0, 0, 24, 36}},
		{"single-pixel", 1, 1, 1, BoundingBox{0, 0, 1, 1}},
		{"one-row", 40, 1, 12, BoundingBox{0, 0, 12, 1}},
		{"one-column-shadow", 40, 20, 1, BoundingBox{0, 0, 1, 20}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &frame{
				width: test.width, height: test.height, shadowWidth: test.shadowWidth,
				maxCanvasLeft: test.width - test.shadowWidth, alphaBox: test.box,
			}
			random := rand.New(rand.NewSource(20260908))
			points := chamferTestPoints(random, test.box)
			magnitude := make([]float64, f.width*f.height)
			for i := range magnitude {
				magnitude[i] = float64(120 + random.Intn(1024))
			}
			t.Run("textured", func(t *testing.T) {
				assertChamferAlignmentMatchesOriginal(t, f, magnitude, points, true)
			})
			t.Run("flat-to-textured", func(t *testing.T) {
				// 没有背景边缘的位置会跳过 alpha 计算，随后必须仍使用正确的几何。
				partial := append([]float64(nil), magnitude...)
				for y := 0; y < f.height; y++ {
					clear(partial[y*f.width : y*f.width+f.width/2])
				}
				assertChamferAlignmentMatchesOriginal(t, f, partial, points, true)
			})
			t.Run("no-background-edge", func(t *testing.T) {
				assertChamferAlignmentMatchesOriginal(t, f, make([]float64, len(magnitude)), points, false)
			})
			t.Run("no-alpha-edge", func(t *testing.T) {
				assertChamferAlignmentMatchesOriginal(t, f, magnitude, nil, false)
			})
		})
	}
}

func TestChamferAlignmentMatchesOriginalRandomized(t *testing.T) {
	random := rand.New(rand.NewSource(20260908))
	for sample := range 40 {
		width, height := 12+random.Intn(53), 1+random.Intn(32)
		shadowWidth := 1 + random.Intn(width)
		left, top := random.Intn(shadowWidth), random.Intn(height)
		box := BoundingBox{
			Left: left, Top: top,
			Right:  left + 1 + random.Intn(shadowWidth-left),
			Bottom: top + 1 + random.Intn(height-top),
		}
		f := &frame{
			width: width, height: height, shadowWidth: shadowWidth,
			maxCanvasLeft: width - shadowWidth, alphaBox: box,
		}
		points := chamferTestPoints(random, box)
		magnitude := make([]float64, width*height)
		for i := range magnitude {
			magnitude[i] = random.Float64()*1000 + 120
		}
		t.Run(fmt.Sprintf("sample-%02d", sample), func(t *testing.T) {
			assertChamferAlignmentMatchesOriginal(t, f, magnitude, points, true)
		})
	}
}

func chamferTestPoints(random *rand.Rand, box BoundingBox) []point {
	var points []point
	for y := box.Top; y < box.Bottom; y++ {
		for x := box.Left; x < box.Right; x++ {
			if (x == box.Left && y == box.Top) || (x == box.Right-1 && y == box.Bottom-1) || random.Intn(4) == 0 {
				points = append(points, point{x: x, y: y})
			}
		}
	}
	return points
}

func assertChamferAlignmentMatchesOriginal(t *testing.T, f *frame, magnitude []float64, points []point, wantFinite bool) {
	t.Helper()
	want, err := originalChamferAlignment(f, context.Background(), magnitude, points)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.chamferAlignment(context.Background(), magnitude, points)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("alignment length=%d, want %d", len(got), len(want))
	}
	finite := 0
	for left := range want {
		if math.Float64bits(got[left]) != math.Float64bits(want[left]) {
			t.Fatalf("alignment[%d]=%v (%016x), want %v (%016x); frame=%dx%d shadowWidth=%d box=%+v", left, got[left], math.Float64bits(got[left]), want[left], math.Float64bits(want[left]), f.width, f.height, f.shadowWidth, f.alphaBox)
		}
		if !math.IsInf(got[left], 0) && !math.IsNaN(got[left]) {
			finite++
		}
	}
	if (finite > 0) != wantFinite {
		t.Fatalf("finite alignments=%d, want any finite=%v", finite, wantFinite)
	}
}

func TestChamferAlignmentCancellationDuringScan(t *testing.T) {
	f := &frame{width: 40, height: 12, shadowWidth: 12, maxCanvasLeft: 28, alphaBox: BoundingBox{0, 0, 12, 12}}
	magnitude := make([]float64, f.width*f.height)
	for i := range magnitude {
		magnitude[i] = 200
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelAfterDoneChecks{Context: base, cancel: cancel, remaining: 2}
	if _, err := f.chamferAlignment(ctx, magnitude, []point{{0, 0}, {11, 11}}); err != context.Canceled {
		t.Fatalf("error=%v, want context.Canceled during scan", err)
	}
}

type cancelAfterDoneChecks struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (ctx *cancelAfterDoneChecks) Done() <-chan struct{} {
	ctx.remaining--
	if ctx.remaining == 0 {
		ctx.cancel()
	}
	return ctx.Context.Done()
}

// originalChamferAlignment 保留优化前每个位置重建 alpha 的计算顺序，作为等价回归参考。
func originalChamferAlignment(f *frame, ctx context.Context, magnitude []float64, alphaPoints []point) ([]float64, error) {
	alignment := make([]float64, f.maxCanvasLeft+1)
	for i := range alignment {
		alignment[i] = math.Inf(1)
	}
	maxROI := (f.alphaBox.width() + 10) * (f.alphaBox.Bottom - f.alphaBox.Top + 10)
	quantileScratch := make([]float64, 0, maxROI)
	backgroundScratch := make([]bool, maxROI)
	alphaScratch := make([]bool, maxROI)
	distanceBackgroundScratch := make([]float64, maxROI)
	distanceAlphaScratch := make([]float64, maxROI)
	for left := 0; left <= f.maxCanvasLeft; left++ {
		if left&7 == 0 {
			if err := checkContext(ctx); err != nil {
				return nil, err
			}
		}
		roiLeft := max(0, left+f.alphaBox.Left-5)
		roiRight := min(f.width, left+f.alphaBox.Right+5)
		roiTop := max(0, f.alphaBox.Top-5)
		roiBottom := min(f.height, f.alphaBox.Bottom+5)
		roiWidth, roiHeight := roiRight-roiLeft, roiBottom-roiTop
		if roiWidth <= 0 || roiHeight <= 0 {
			continue
		}
		quantileScratch = quantileScratch[:0]
		for y := roiTop; y < roiBottom; y++ {
			quantileScratch = append(quantileScratch, magnitude[y*f.width+roiLeft:y*f.width+roiRight]...)
		}
		threshold := max(120, quantile(quantileScratch, 0.75))
		pixelCount := roiWidth * roiHeight
		backgroundBoundary := backgroundScratch[:pixelCount]
		clear(backgroundBoundary)
		backgroundCount := 0
		for y := 0; y < roiHeight; y++ {
			for x := 0; x < roiWidth; x++ {
				if magnitude[(roiTop+y)*f.width+roiLeft+x] >= threshold {
					backgroundBoundary[y*roiWidth+x] = true
					backgroundCount++
				}
			}
		}
		if backgroundCount == 0 {
			continue
		}
		localAlpha := alphaScratch[:pixelCount]
		clear(localAlpha)
		alphaCount := 0
		for _, alpha := range alphaPoints {
			x, y := alpha.x+left-roiLeft, alpha.y-roiTop
			if x >= 0 && x < roiWidth && y >= 0 && y < roiHeight {
				localAlpha[y*roiWidth+x] = true
				alphaCount++
			}
		}
		if alphaCount == 0 {
			continue
		}
		distanceToBackground := distanceBackgroundScratch[:pixelCount]
		distanceToAlpha := distanceAlphaScratch[:pixelCount]
		distanceToFeatureInto(backgroundBoundary, roiWidth, roiHeight, distanceToBackground)
		distanceToFeatureInto(localAlpha, roiWidth, roiHeight, distanceToAlpha)
		var alphaToBackground, backgroundToAlpha float64
		for index, isAlpha := range localAlpha {
			if isAlpha {
				alphaToBackground += distanceToBackground[index]
			}
			if backgroundBoundary[index] {
				backgroundToAlpha += distanceToAlpha[index]
			}
		}
		alignment[left] = (alphaToBackground/float64(alphaCount) + backgroundToAlpha/float64(backgroundCount)) / 2
	}

	return alignment, nil
}
