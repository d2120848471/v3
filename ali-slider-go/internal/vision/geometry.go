package vision

import (
	"errors"
	"math"
)

// PuzzleXFromSlidePos 复刻前端由手柄位移计算拼图横坐标的二次式。
func PuzzleXFromSlidePos(slidePos float64) (float64, error) {
	if math.IsNaN(slidePos) || math.IsInf(slidePos, 0) || slidePos < 0 {
		return 0, errors.New("slide position must be a finite non-negative number")
	}
	return slidePos * (3*slidePos + 65) / 845, nil
}

// JSMathRound 复刻非负数上的 JavaScript Math.round，即 floor(x+0.5)。
func JSMathRound(value float64) (int, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, errors.New("value must be a finite non-negative number")
	}
	maxInt := int(^uint(0) >> 1)
	if value > float64(maxInt)-0.5 {
		return 0, errors.New("rounded value overflows int")
	}
	return int(math.Floor(value + 0.5)), nil
}

// SlidePosFromPuzzleX 反解手柄位移，先 clamp 到 DOM 可移动范围，再按 JavaScript 语义取整。
func SlidePosFromPuzzleX(puzzleX, renderedWidth, handleWidth float64) (int, error) {
	if math.IsNaN(puzzleX) || math.IsInf(puzzleX, 0) || puzzleX < 0 {
		return 0, errors.New("puzzle x must be a finite non-negative number")
	}
	if math.IsNaN(renderedWidth) || math.IsInf(renderedWidth, 0) || renderedWidth <= 0 {
		return 0, errors.New("rendered width must be finite and positive")
	}
	if math.IsNaN(handleWidth) || math.IsInf(handleWidth, 0) || handleWidth < 0 || handleWidth > renderedWidth {
		return 0, errors.New("handle width must be within 0..rendered width")
	}
	raw := (-65 + math.Sqrt(65*65+12*845*puzzleX)) / 6
	raw = min(max(raw, 0), renderedWidth-handleWidth)
	return JSMathRound(raw)
}
