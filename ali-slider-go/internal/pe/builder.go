// Package pe 实现 Aliyun V3 Puzzle PE 的纯 Go 数据构造。
package pe

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/d2120848471/v3/ali-slider-go/internal/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/runtimekit"
	"github.com/d2120848471/v3/ali-slider-go/internal/track"
)

const (
	originX                = 94
	originY                = 548
	freshnessMarginMS      = int64(750)
	maximumTrackEvents     = 512
	maximumTrackDurationMS = int64(60_000)
	defaultRenderedWidth   = 300
	defaultHandleWidth     = 40
	defaultFirstTouchAgeMS = 650
)

var argKeys = map[string]string{
	"3.29.0/pe.075.f195b27e016eecc3": "0kgx07m96ptqvk0c",
	"3.29.0/pe.068.4270dea37d68dd55": "om8wsc61jxel4dza",
	"3.29.0/pe.096.af431a090d6756cf": "2szbggnqfxa3rr37",
	"3.29.0/pe.094.f10b48a2d9910e6c": "8srw808jd9nvcnpf",
	"3.29.0/pe.061.66e362095c40df65": "ckg2kd9zn3zpct19",
	"3.29.0/pe.078.1ec68fc4b6bb14b7": "jogfs7z1zyf1e9go",
	"3.29.0/pe.088.ba6d7f87b920b698": "8iadi5j9z4kjf2cq",
	"3.29.0/pe.067.4c6dfbace0e490ad": "h3kdq43rgqryzcik",
	"3.29.0/pe.087.0603cddfaf6ed853": "m1i4acijv5u4km6r",
	"3.29.0/pe.072.64e9154e3053635f": "wmt6ga3rxqi4m8ei",
	"3.29.0/pe.086.5cd1be4bee85aff9": "34chpeu1rpq0gu7o",
	"3.29.0/pe.054.0622378da6a707ca": "nasmu37k6ajq78et",
	"3.29.0/pe.071.dc130f88dee78b04": "wwp31xzwdszbk3vw",
	"3.29.0/pe.062.40231f5d7f8351a3": "0lncwrzc9lwba9fq",
	"3.29.0/pe.077.b1b6f0c91d158f85": "jltdjjn2hw5ccco8",
	"3.29.0/pe.089.0db63cec7a44dbe2": "zl3j41v4shnf0iqd",
	"3.29.0/pe.080.eaa50e5b7a73b0c6": "tkasxtbj5agi48hs",
	"3.29.0/pe.066.d9f02d3dd6d9f5f6": "n2j4v2qqb6rz3djl",
	"3.29.0/pe.069.bcf01ca00f5bd260": "l0wo7rcn2ebvqg9e",
	"3.29.0/pe.091.00665af58b020d81": "0kd8i0mclivjow32",
	"3.29.0/pe.098.f37c3b7710aa77a9": "xgarfd2mxn3ne5qu",
}

// Builder 持有一轮 PE 所见的设备画像和非确定性来源。
type Builder struct {
	Profile device.Profile
	Sources runtimekit.Sources
}

type Dimensions struct {
	RenderedWidth int
	HandleWidth   int
}

type Input struct {
	SceneID           string
	CertifyID         string
	Dimensions        Dimensions
	Track             []track.Event
	StaticPath        string
	ArgumentKey       string
	IncludeScreenInfo bool
	ExpectedXPos      *int
	InitBeginTimeMS   int64
	FirstTouchAgeMS   int
}

type GetterArgument struct {
	ValueType       string
	Length          int
	Value           string
	EqualsCertifyID bool
	EqualsSceneID   bool
}

type GetterPlan struct {
	Owner         string
	DerivedAtMS   float64
	ArgumentCount int
	Arguments     []GetterArgument
}

type InteractionEvent struct {
	Type      string
	X         float64
	Y         float64
	TimeStamp float64
	IsTrusted bool
}

type Result struct {
	Data                     string
	TrackEventCount          int
	PayloadKeys              []string
	TrackKeys                []string
	XPos                     int
	SlidePos                 int
	TrackStartTimeMS         int64
	VerifyTimeMS             int64
	TargetFirstTouchAgeMS    int
	FirstTouchAgeMS          float64
	TouchDurationMS          float64
	LastTouchToVerifyMS      float64
	PostInteractionDelayMS   float64
	DispatchLatenessMS       float64
	DeviceGetterPlans        []GetterPlan
	DataMousemoveEventCount  int
	PostGetterDataEventCount int
	InteractionEvents        []InteractionEvent
}

type normalizedEvent struct {
	x, y int
	dt   int
	kind string
}

// Build 生成 Verify data，并立即解包检查字节合同。
func (builder Builder) Build(ctx context.Context, input Input) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("PE context is required")
	}
	if err := builder.Sources.Validate(); err != nil {
		return Result{}, fmt.Errorf("PE sources: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if input.SceneID == "" || input.CertifyID == "" {
		return Result{}, errors.New("scene ID and certify ID are required")
	}
	dimensions, err := normalizeDimensions(input.Dimensions)
	if err != nil {
		return Result{}, err
	}
	events, duration, err := normalizeTrack(input.Track)
	if err != nil {
		return Result{}, err
	}
	firstAge := input.FirstTouchAgeMS
	if firstAge == 0 {
		firstAge = defaultFirstTouchAgeMS
	}
	if firstAge < 1 {
		return Result{}, errors.New("first touch age must be positive")
	}

	nowMS := builder.Sources.Clock.Now().UnixMilli()
	requestedStart := input.InitBeginTimeMS
	if requestedStart == 0 {
		requestedStart = nowMS
	}
	latestStart := nowMS - int64(firstAge) - duration - freshnessMarginMS
	trackStart := min(requestedStart, latestStart)
	if trackStart < 1 {
		return Result{}, errors.New("cannot establish a valid PE logical clock")
	}

	elapsed := int64(firstAge)
	records := make([]eventRecord, 0, len(events))
	interactions := make([]InteractionEvent, 0, len(events))
	lastMoveX := 0
	for _, event := range events {
		elapsed += int64(event.dt)
		x, err := jsRound(float64(originX + event.x))
		if err != nil {
			return Result{}, fmt.Errorf("track x: %w", err)
		}
		y := int(math.Floor(float64(originY+event.y) + 0.5))
		records = append(records, eventRecord{kind: event.kind, text: formatRecord(x, y, elapsed), x: x, y: y})
		interactions = append(interactions, InteractionEvent{Type: "mousemove", X: float64(x), Y: float64(y), TimeStamp: float64(elapsed), IsTrusted: true})
		if event.kind == "touchmove" {
			lastMoveX = event.x
		}
	}

	maximumSlide := dimensions.RenderedWidth - dimensions.HandleWidth
	slidePos, err := jsRound(float64(min(max(lastMoveX, 0), maximumSlide)))
	if err != nil {
		return Result{}, err
	}
	xValue := float64(slidePos) * (3*float64(slidePos) + 65) / 845
	xPos, err := jsRound(xValue)
	if err != nil {
		return Result{}, err
	}
	if input.ExpectedXPos != nil && abs(xPos-*input.ExpectedXPos) > 1 {
		return Result{}, fmt.Errorf("PE xPos does not match expected value: %d != %d", xPos, *input.ExpectedXPos)
	}

	trackList, err := buildTrackList(records, builder.Profile.Screen, trackStart, elapsed, input.IncludeScreenInfo)
	if err != nil {
		return Result{}, err
	}
	argument, err := buildArgument(input.CertifyID, input.StaticPath, input.ArgumentKey)
	if err != nil {
		return Result{}, err
	}
	verifyTime := trackStart + elapsed
	payload := protocol.DataPayload{
		TrackList: trackList, TrackStartTime: trackStart, VerifyTime: verifyTime,
		XPos: strconv.Itoa(xPos), SlidePos: strconv.Itoa(slidePos), Arg: argument,
	}
	prefix, err := dataPrefix(builder.Sources.Entropy)
	if err != nil {
		return Result{}, err
	}
	data, err := protocol.PackData(payload, prefix)
	if err != nil {
		return Result{}, fmt.Errorf("pack PE data: %w", err)
	}
	decoded, err := protocol.UnpackData(data)
	if err != nil {
		return Result{}, fmt.Errorf("self-check PE data: %w", err)
	}
	expectedJSON, err := protocol.CompactJSON(payload)
	if err != nil || decoded.JSONText != expectedJSON || decoded.ChecksumPrefix != prefix {
		return Result{}, errors.New("PE data self-check mismatch")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	getterArgument := GetterArgument{
		ValueType: "string", Length: utf8.RuneCountInString(input.CertifyID), Value: input.CertifyID,
		EqualsCertifyID: true, EqualsSceneID: input.CertifyID == input.SceneID,
	}
	trackKeys := []string{"mc", "tc", "mu", "te", "mp", "tmv", "mm", "ks", "fi", "startTime"}
	if input.IncludeScreenInfo {
		trackKeys = append(trackKeys, "si")
	}
	return Result{
		Data: data, TrackEventCount: len(events),
		PayloadKeys: []string{"TrackList", "TrackStartTime", "VerifyTime", "xPos", "slidePos", "arg"},
		TrackKeys:   trackKeys,
		XPos:        xPos, SlidePos: slidePos, TrackStartTimeMS: trackStart, VerifyTimeMS: verifyTime,
		TargetFirstTouchAgeMS: firstAge, FirstTouchAgeMS: float64(firstAge), TouchDurationMS: float64(duration),
		DeviceGetterPlans:       []GetterPlan{{Owner: "python-compatible.getToken", DerivedAtMS: float64(elapsed), ArgumentCount: 1, Arguments: []GetterArgument{getterArgument}}},
		DataMousemoveEventCount: len(records) + 1, PostGetterDataEventCount: 1,
		InteractionEvents: interactions,
	}, nil
}

func normalizeDimensions(value Dimensions) (Dimensions, error) {
	if value == (Dimensions{}) {
		return Dimensions{RenderedWidth: defaultRenderedWidth, HandleWidth: defaultHandleWidth}, nil
	}
	if value.RenderedWidth <= 0 || value.HandleWidth <= 0 || value.HandleWidth > value.RenderedWidth {
		return Dimensions{}, errors.New("slider dimensions are invalid")
	}
	return value, nil
}

func normalizeTrack(events []track.Event) ([]normalizedEvent, int64, error) {
	if len(events) < 3 || len(events) > maximumTrackEvents {
		return nil, 0, fmt.Errorf("track must contain 3..%d events", maximumTrackEvents)
	}
	output := make([]normalizedEvent, len(events))
	var duration int64
	for index, event := range events {
		if event.DT < 0 || index == 0 && event.DT != 0 {
			return nil, 0, fmt.Errorf("track event %d has invalid dt", index)
		}
		duration += int64(event.DT)
		if duration > maximumTrackDurationMS {
			return nil, 0, errors.New("track duration exceeds 60000ms")
		}
		kind := event.Type
		if kind == "" {
			switch index {
			case 0:
				kind = "touchstart"
			case len(events) - 1:
				kind = "touchend"
			default:
				kind = "touchmove"
			}
		}
		if kind != "touchstart" && kind != "touchmove" && kind != "touchend" {
			return nil, 0, fmt.Errorf("track event %d has invalid type", index)
		}
		output[index] = normalizedEvent{x: event.X, y: event.Y, dt: event.DT, kind: kind}
	}
	if output[0].kind != "touchstart" || output[len(output)-1].kind != "touchend" {
		return nil, 0, errors.New("track must start with touchstart and end with touchend")
	}
	for _, event := range output[1 : len(output)-1] {
		if event.kind != "touchmove" {
			return nil, 0, errors.New("track middle events must be touchmove")
		}
	}
	return output, duration, nil
}

func jsRound(value float64) (int, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value >= float64(int(^uint(0)>>1)) {
		return 0, errors.New("value cannot be rounded with JavaScript semantics")
	}
	return int(math.Floor(value + 0.5)), nil
}

type eventRecord struct {
	kind, text string
	x, y       int
}

func formatRecord(x, y int, timestamp int64) string {
	return strconv.Itoa(x) + "," + strconv.Itoa(y) + "," + strconv.FormatInt(timestamp, 10) + ",1"
}

func buildTrackList(records []eventRecord, screen device.ScreenProfile, trackStart, elapsed int64, includeScreenInfo bool) (protocol.TrackList, error) {
	starts, moves, ends, all := make([]string, 0, 1), make([]string, 0, len(records)-2), make([]string, 0, 1), make([]string, 0, len(records)+1)
	for _, record := range records {
		all = append(all, record.text)
		switch record.kind {
		case "touchstart":
			starts = append(starts, record.text)
		case "touchmove":
			moves = append(moves, record.text)
		case "touchend":
			ends = append(ends, record.text)
		}
	}
	last := records[len(records)-1]
	rafX, err := jsRound(float64(last.x) * 1.15)
	if err != nil {
		return protocol.TrackList{}, fmt.Errorf("RAF x: %w", err)
	}
	rafY, err := jsRound(float64(last.y) * 1.15)
	if err != nil {
		return protocol.TrackList{}, fmt.Errorf("RAF y: %w", err)
	}
	all = append(all, formatRecord(rafX, rafY, elapsed+16))
	trackList := protocol.TrackList{
		MC: "", TC: strings.Join(starts, "|"), MU: "", TE: strings.Join(ends, "|"), MP: "",
		TMV: strings.Join(moves, "|"), MM: strings.Join(all, "|"), KS: "", FI: "", StartTime: trackStart,
	}
	if includeScreenInfo {
		si := []string{
			strconv.Itoa(screen.InnerWidth), strconv.Itoa(screen.Width), strconv.Itoa(screen.InnerHeight),
			strconv.Itoa(screen.InnerWidth), strconv.Itoa(screen.InnerHeight), strconv.Itoa(screen.OuterHeight),
			strconv.Itoa(screen.Height), "120.00000000000082", strconv.Itoa(screen.OuterWidth),
		}
		trackList.SI = strings.Join(si, ",")
	}
	return trackList, nil
}

func buildArgument(certifyID, staticPath, resolvedKey string) (string, error) {
	path := strings.TrimSuffix(staticPath, ".js")
	key := resolvedKey
	if key == "" {
		key = argKeys[path]
	}
	if key == "" {
		return "", fmt.Errorf("%w: argument key is unavailable", ErrUnsupportedPE)
	}
	transformed, err := protocol.Transform64([]byte(certifyID), key, false)
	if err != nil {
		return "", fmt.Errorf("transform PE arg: %w", err)
	}
	return base64.StdEncoding.EncodeToString(transformed), nil
}

func dataPrefix(entropy runtimekit.Entropy) (string, error) {
	raw := make([]byte, 15)
	if _, err := io.ReadFull(entropy, raw); err != nil {
		return "", fmt.Errorf("generate PE data prefix: %w", err)
	}
	return hex.EncodeToString(raw) + "01", nil
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
