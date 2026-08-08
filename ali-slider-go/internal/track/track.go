// Package track 将脱敏人工轨迹缩放到本轮位移，并施加有界随机扰动。
package track

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/d2120848471/v3/ali-slider-go/internal/runtimekit"
)

const (
	minimumEvents  = 3
	sampleJitterMS = 2.0
)

var (
	//go:embed default_touch_track.json
	defaultFixture  []byte
	fixtureOnce     sync.Once
	fixtureSamples  []sourceSample
	fixtureDistance float64
	fixtureErr      error
)

// Event 是 PE 所需的单个触摸采样。
type Event struct {
	X       int     `json:"x"`
	Y       int     `json:"y"`
	DT      int     `json:"dt"`
	Type    string  `json:"type"`
	Force   float64 `json:"force"`
	RadiusX float64 `json:"radiusX"`
	RadiusY float64 `json:"radiusY"`
}

type sourceSample struct {
	Type string
	T    float64
	X    float64
	Y    float64
}

type scaledSample struct {
	Type string
	X    float64
	Y    float64
	DT   float64
}

type fixtureDocument struct {
	Columns []string        `json:"columns"`
	Image   fixtureImage    `json:"image"`
	Track   [][]interface{} `json:"track"`
}

type fixtureImage struct {
	FinalSliderLeft float64 `json:"finalSliderLeft"`
}

// LoadDefault 返回一条新的轨迹。解析后的只读资产在进程内缓存，输出不共享切片。
func LoadDefault(targetDistance float64, entropy runtimekit.Entropy) ([]Event, error) {
	if entropy == nil {
		return nil, errors.New("track entropy is required")
	}
	if math.IsNaN(targetDistance) || math.IsInf(targetDistance, 0) || targetDistance < 0 {
		return nil, errors.New("target distance must be finite and non-negative")
	}
	samples, sourceDistance, err := parsedFixture()
	if err != nil {
		return nil, err
	}
	scaled, err := rescale(samples, targetDistance, sourceDistance)
	if err != nil {
		return nil, err
	}
	random := randomSource{entropy: entropy}
	thinned, err := thin(scaled, &random)
	if err != nil {
		return nil, err
	}
	return humanize(thinned, targetDistance, &random)
}

func parsedFixture() ([]sourceSample, float64, error) {
	fixtureOnce.Do(func() {
		var document fixtureDocument
		if err := json.Unmarshal(defaultFixture, &document); err != nil {
			fixtureErr = fmt.Errorf("decode embedded track: %w", err)
			return
		}
		indexes := make(map[string]int, len(document.Columns))
		for index, name := range document.Columns {
			indexes[name] = index
		}
		for _, name := range []string{"type", "t", "x", "y"} {
			if _, ok := indexes[name]; !ok {
				fixtureErr = fmt.Errorf("embedded track missing %s column", name)
				return
			}
		}
		for rowIndex, row := range document.Track {
			if len(row) < len(document.Columns) {
				fixtureErr = fmt.Errorf("embedded track row %d is short", rowIndex)
				return
			}
			typeValue, ok := row[indexes["type"]].(string)
			if !ok {
				fixtureErr = fmt.Errorf("embedded track row %d has invalid type", rowIndex)
				return
			}
			t, okT := row[indexes["t"]].(float64)
			x, okX := row[indexes["x"]].(float64)
			y, okY := row[indexes["y"]].(float64)
			if !okT || !okX || !okY {
				fixtureErr = fmt.Errorf("embedded track row %d has invalid coordinates", rowIndex)
				return
			}
			fixtureSamples = append(fixtureSamples, sourceSample{Type: typeValue, T: t, X: x, Y: y})
		}
		if len(fixtureSamples) < minimumEvents {
			fixtureErr = errors.New("embedded track requires at least three events")
			return
		}
		fixtureDistance = document.Image.FinalSliderLeft
		if fixtureDistance <= 0 {
			fixtureDistance = fixtureSamples[len(fixtureSamples)-1].X
		}
	})
	if fixtureErr != nil {
		return nil, 0, fixtureErr
	}
	return fixtureSamples, fixtureDistance, nil
}

func rescale(samples []sourceSample, targetDistance, sourceDistance float64) ([]scaledSample, error) {
	if sourceDistance <= 0 || math.IsNaN(sourceDistance) || math.IsInf(sourceDistance, 0) {
		return nil, errors.New("source distance must be finite and positive")
	}
	output := make([]scaledSample, 0, len(samples))
	var previous float64
	for index, sample := range samples {
		if math.IsNaN(sample.T) || math.IsNaN(sample.X) || math.IsNaN(sample.Y) ||
			math.IsInf(sample.T, 0) || math.IsInf(sample.X, 0) || math.IsInf(sample.Y, 0) {
			return nil, fmt.Errorf("track sample %d contains a non-finite value", index)
		}
		delta := 0.0
		if index > 0 {
			delta = sample.T - previous
			if delta < 0 {
				return nil, errors.New("track timestamps must be monotonic")
			}
		}
		output = append(output, scaledSample{Type: sample.Type, X: sample.X / sourceDistance * targetDistance, Y: sample.Y, DT: delta})
		previous = sample.T
	}
	output[0].X, output[0].DT = 0, 0
	output[len(output)-1].X = targetDistance
	return output, nil
}

func thin(samples []scaledSample, random *randomSource) ([]scaledSample, error) {
	ratio, err := random.uniform(0, 0.10)
	if err != nil {
		return nil, err
	}
	dropCount := min(len(samples)-minimumEvents, int(float64(len(samples))*ratio))
	if dropCount <= 0 {
		return samples, nil
	}
	indexes := make([]int, len(samples)-2)
	for index := range indexes {
		indexes[index] = index + 1
	}
	for index := range indexes {
		chosen, chooseErr := random.intRange(index, len(indexes)-1)
		if chooseErr != nil {
			return nil, chooseErr
		}
		indexes[index], indexes[chosen] = indexes[chosen], indexes[index]
	}
	dropped := make(map[int]struct{}, dropCount)
	for _, index := range indexes[:dropCount] {
		dropped[index] = struct{}{}
	}
	kept := make([]scaledSample, 0, len(samples)-dropCount)
	carried := 0.0
	for index, sample := range samples {
		if _, ok := dropped[index]; ok {
			carried += sample.DT
			continue
		}
		sample.DT += carried
		carried = 0
		kept = append(kept, sample)
	}
	return kept, nil
}

func humanize(samples []scaledSample, targetDistance float64, random *randomSource) ([]Event, error) {
	speed, err := random.uniform(0.80, 1.28)
	if err != nil {
		return nil, err
	}
	yOffset, err := random.intRange(-4, 5)
	if err != nil {
		return nil, err
	}
	forceBase, err := random.uniform(0.35, 0.85)
	if err != nil {
		return nil, err
	}
	radiusBase, err := random.uniform(8, 26)
	if err != nil {
		return nil, err
	}
	radiusRatio, err := random.uniform(0.85, 1.15)
	if err != nil {
		return nil, err
	}
	targetX := int(math.RoundToEven(targetDistance))
	last := len(samples) - 1
	output := make([]Event, 0, len(samples))
	for index, sample := range samples {
		delta := 0
		if index > 0 {
			jitter, jitterErr := random.uniform(-sampleJitterMS, sampleJitterMS)
			if jitterErr != nil {
				return nil, jitterErr
			}
			delta = max(0, int(math.RoundToEven(sample.DT*speed+jitter)))
		}
		x := 0
		switch {
		case index == 0:
		case index >= last-1 || int(math.RoundToEven(sample.X)) >= targetX:
			x = targetX
		default:
			noise, noiseErr := random.intRange(-1, 1)
			if noiseErr != nil {
				return nil, noiseErr
			}
			x = min(targetX, int(math.RoundToEven(sample.X))+noise)
		}
		yNoise, noiseErr := random.intRange(-1, 1)
		if noiseErr != nil {
			return nil, noiseErr
		}
		forceNoise, noiseErr := random.uniform(-0.06, 0.06)
		if noiseErr != nil {
			return nil, noiseErr
		}
		radiusXNoise, noiseErr := random.uniform(-2, 2)
		if noiseErr != nil {
			return nil, noiseErr
		}
		radiusYNoise, noiseErr := random.uniform(-2, 2)
		if noiseErr != nil {
			return nil, noiseErr
		}
		output = append(output, Event{
			X:       x,
			Y:       int(math.RoundToEven(sample.Y)) + yOffset + yNoise,
			DT:      delta,
			Type:    sample.Type,
			Force:   roundEven(clamp(forceBase+forceNoise, 0, 1), 4),
			RadiusX: roundEven(radiusBase+radiusXNoise, 3),
			RadiusY: roundEven((radiusBase+radiusYNoise)*radiusRatio, 3),
		})
	}
	return output, nil
}

type randomSource struct{ entropy runtimekit.Entropy }

func (r *randomSource) uniform(minimum, maximum float64) (float64, error) {
	value, err := r.entropy.Uint64n(1 << 53)
	if err != nil {
		return 0, err
	}
	unit := float64(value) / float64(uint64(1)<<53)
	return minimum + (maximum-minimum)*unit, nil
}

func (r *randomSource) intRange(minimum, maximum int) (int, error) {
	if maximum < minimum {
		return 0, errors.New("invalid random integer range")
	}
	value, err := r.entropy.Uint64n(uint64(maximum - minimum + 1))
	if err != nil {
		return 0, err
	}
	return minimum + int(value), nil
}

func clamp(value, minimum, maximum float64) float64 {
	return math.Min(maximum, math.Max(minimum, value))
}

func roundEven(value float64, digits int) float64 {
	scale := math.Pow10(digits)
	return math.RoundToEven(value*scale) / scale
}
