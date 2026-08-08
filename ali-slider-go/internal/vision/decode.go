package vision

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
)

var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

type pngHeader struct {
	width, height int
	colorType     byte
	hasAlpha      bool
}

type decodedPair struct {
	width, height, shadowWidth int
	bgr                        []float64
	alpha                      []uint8
}

func normalizedLimits(limits Limits) (Limits, error) {
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	if limits.MaxBytes <= 0 || limits.MaxDimension <= 0 || limits.MaxPixels <= 0 {
		return Limits{}, errors.New("vision limits must be positive")
	}
	return limits, nil
}

func inspectPNG(data []byte, limits Limits, requireAlpha bool) (pngHeader, error) {
	if len(data) > limits.MaxBytes {
		return pngHeader{}, fmt.Errorf("PNG exceeds %d byte limit", limits.MaxBytes)
	}
	if len(data) < 33 || !bytes.Equal(data[:8], pngSignature) || string(data[12:16]) != "IHDR" || binary.BigEndian.Uint32(data[8:12]) != 13 {
		return pngHeader{}, errors.New("invalid PNG header")
	}
	w64 := uint64(binary.BigEndian.Uint32(data[16:20]))
	h64 := uint64(binary.BigEndian.Uint32(data[20:24]))
	if w64 == 0 || h64 == 0 || w64 > uint64(limits.MaxDimension) || h64 > uint64(limits.MaxDimension) || w64*h64 > uint64(limits.MaxPixels) {
		return pngHeader{}, errors.New("PNG dimensions exceed limits")
	}
	colorType := data[25]
	hasAlpha := colorType == 4 || colorType == 6
	if colorType == 3 {
		// 调色板 PNG 只在 IDAT 前存在 tRNS 块时具有 alpha。
		for offset := 8; offset+12 <= len(data); {
			length := int(binary.BigEndian.Uint32(data[offset : offset+4]))
			if length < 0 || offset+12+length > len(data) {
				break
			}
			kind := string(data[offset+4 : offset+8])
			if kind == "tRNS" {
				hasAlpha = true
				break
			}
			if kind == "IDAT" || kind == "IEND" {
				break
			}
			offset += 12 + length
		}
	}
	if requireAlpha && !hasAlpha {
		return pngHeader{}, errors.New("shadow PNG must contain an alpha channel")
	}
	return pngHeader{width: int(w64), height: int(h64), colorType: colorType, hasAlpha: hasAlpha}, nil
}

func decodePair(backgroundPNG, shadowPNG []byte, limits Limits) (decodedPair, error) {
	limits, err := normalizedLimits(limits)
	if err != nil {
		return decodedPair{}, err
	}
	backgroundHeader, err := inspectPNG(backgroundPNG, limits, false)
	if err != nil {
		return decodedPair{}, fmt.Errorf("background: %w", err)
	}
	shadowHeader, err := inspectPNG(shadowPNG, limits, true)
	if err != nil {
		return decodedPair{}, fmt.Errorf("shadow: %w", err)
	}
	if backgroundHeader.height != shadowHeader.height {
		return decodedPair{}, errors.New("shadow and background heights differ")
	}
	if shadowHeader.width > backgroundHeader.width {
		return decodedPair{}, errors.New("shadow width exceeds background width")
	}
	background, err := png.Decode(bytes.NewReader(backgroundPNG))
	if err != nil {
		return decodedPair{}, fmt.Errorf("decode background PNG: %w", err)
	}
	shadow, err := png.Decode(bytes.NewReader(shadowPNG))
	if err != nil {
		return decodedPair{}, fmt.Errorf("decode shadow PNG: %w", err)
	}
	if background.Bounds().Dx() != backgroundHeader.width || background.Bounds().Dy() != backgroundHeader.height || shadow.Bounds().Dx() != shadowHeader.width || shadow.Bounds().Dy() != shadowHeader.height {
		return decodedPair{}, errors.New("decoded PNG dimensions differ from IHDR")
	}

	pair := decodedPair{
		width:       backgroundHeader.width,
		height:      backgroundHeader.height,
		shadowWidth: shadowHeader.width,
		bgr:         make([]float64, backgroundHeader.width*backgroundHeader.height*3),
		alpha:       make([]uint8, shadowHeader.width*shadowHeader.height),
	}
	backgroundBounds := background.Bounds()
	for y := 0; y < pair.height; y++ {
		for x := 0; x < pair.width; x++ {
			// NRGBA 取得非预乘 RGB，与 OpenCV IMREAD_COLOR 忽略 alpha 的语义一致。
			pixel := nonPremultipliedRGBA(background, backgroundBounds.Min.X+x, backgroundBounds.Min.Y+y)
			index := (y*pair.width + x) * 3
			pair.bgr[index], pair.bgr[index+1], pair.bgr[index+2] = float64(pixel.B), float64(pixel.G), float64(pixel.R)
		}
	}
	shadowBounds := shadow.Bounds()
	alphaFound := false
	for y := 0; y < pair.height; y++ {
		for x := 0; x < pair.shadowWidth; x++ {
			alpha := alphaAt(shadow, shadowBounds.Min.X+x, shadowBounds.Min.Y+y)
			pair.alpha[y*pair.shadowWidth+x] = alpha
			alphaFound = alphaFound || alpha > alphaThreshold
		}
	}
	if !alphaFound {
		return decodedPair{}, errors.New("shadow contains no non-transparent puzzle shape")
	}
	return pair, nil
}

func alphaAt(source image.Image, x, y int) uint8 {
	switch typed := source.(type) {
	case *image.NRGBA:
		return typed.NRGBAAt(x, y).A
	case *image.NRGBA64:
		return uint8(typed.NRGBA64At(x, y).A >> 8)
	case *image.RGBA:
		return typed.RGBAAt(x, y).A
	case *image.RGBA64:
		return uint8(typed.RGBA64At(x, y).A >> 8)
	case *image.Alpha:
		return typed.AlphaAt(x, y).A
	case *image.Alpha16:
		return uint8(typed.Alpha16At(x, y).A >> 8)
	case *image.Paletted:
		return colorToNRGBA(typed.Palette[typed.ColorIndexAt(x, y)]).A
	default:
		_, _, _, alpha := source.At(x, y).RGBA()
		return uint8(alpha >> 8)
	}
}

func nonPremultipliedRGBA(source image.Image, x, y int) color.NRGBA {
	switch typed := source.(type) {
	case *image.NRGBA:
		return typed.NRGBAAt(x, y)
	case *image.NRGBA64:
		pixel := typed.NRGBA64At(x, y)
		return color.NRGBA{R: uint8(pixel.R >> 8), G: uint8(pixel.G >> 8), B: uint8(pixel.B >> 8), A: uint8(pixel.A >> 8)}
	case *image.RGBA:
		pixel := typed.RGBAAt(x, y)
		return unpremultiply8(pixel.R, pixel.G, pixel.B, pixel.A)
	case *image.RGBA64:
		pixel := typed.RGBA64At(x, y)
		return unpremultiply16(pixel.R, pixel.G, pixel.B, pixel.A)
	case *image.Gray:
		value := typed.GrayAt(x, y).Y
		return color.NRGBA{R: value, G: value, B: value, A: 255}
	case *image.Gray16:
		value := uint8(typed.Gray16At(x, y).Y >> 8)
		return color.NRGBA{R: value, G: value, B: value, A: 255}
	case *image.Paletted:
		return colorToNRGBA(typed.At(x, y))
	default:
		return colorToNRGBA(source.At(x, y))
	}
}

func unpremultiply16(red, green, blue, alpha uint16) color.NRGBA {
	if alpha == 0 {
		return color.NRGBA{}
	}
	return color.NRGBA{
		R: uint8(min(uint32(red)*0xffff/uint32(alpha), 0xffff) >> 8),
		G: uint8(min(uint32(green)*0xffff/uint32(alpha), 0xffff) >> 8),
		B: uint8(min(uint32(blue)*0xffff/uint32(alpha), 0xffff) >> 8),
		A: uint8(alpha >> 8),
	}
}

func colorToNRGBA(value color.Color) color.NRGBA {
	switch typed := value.(type) {
	case color.NRGBA:
		return typed
	case color.NRGBA64:
		return color.NRGBA{R: uint8(typed.R >> 8), G: uint8(typed.G >> 8), B: uint8(typed.B >> 8), A: uint8(typed.A >> 8)}
	case color.RGBA:
		return unpremultiply8(typed.R, typed.G, typed.B, typed.A)
	case color.Gray:
		return color.NRGBA{R: typed.Y, G: typed.Y, B: typed.Y, A: 255}
	case color.Gray16:
		component := uint8(typed.Y >> 8)
		return color.NRGBA{R: component, G: component, B: component, A: 255}
	}
	red, green, blue, alpha := value.RGBA()
	if alpha == 0 {
		return color.NRGBA{}
	}
	return color.NRGBA{
		R: uint8(min(red*0xffff/alpha, 0xffff) >> 8),
		G: uint8(min(green*0xffff/alpha, 0xffff) >> 8),
		B: uint8(min(blue*0xffff/alpha, 0xffff) >> 8),
		A: uint8(alpha >> 8),
	}
}

func unpremultiply8(red, green, blue, alpha uint8) color.NRGBA {
	if alpha == 0 {
		return color.NRGBA{}
	}
	return color.NRGBA{
		R: uint8(min(uint16(red)*255/uint16(alpha), 255)),
		G: uint8(min(uint16(green)*255/uint16(alpha), 255)),
		B: uint8(min(uint16(blue)*255/uint16(alpha), 255)),
		A: alpha,
	}
}
