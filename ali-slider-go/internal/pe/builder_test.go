package pe

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/runtimekit"
	"github.com/d2120848471/v3/ali-slider-go/internal/track"
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

type oracleDocument struct {
	PEArg       string `json:"peArg"`
	Data        string `json:"data"`
	PayloadJSON string `json:"payloadJSON"`
}

func loadOracle(t *testing.T) oracleDocument {
	t.Helper()
	raw, err := os.ReadFile("../protocol/testdata/python_oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var oracle oracleDocument
	if err := json.Unmarshal(raw, &oracle); err != nil {
		t.Fatal(err)
	}
	return oracle
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

func builderWithEntropy(entropy runtimekit.Entropy, nowMS int64) Builder {
	return Builder{
		Profile: oracleProfile(),
		Sources: runtimekit.Sources{Clock: fixedClock{now: time.UnixMilli(nowMS)}, Entropy: entropy},
	}
}

func TestBuildMatchesPythonOracleSemantics(t *testing.T) {
	oracle := loadOracle(t)
	oracleDecoded, err := protocol.UnpackData(oracle.Data)
	if err != nil {
		t.Fatal(err)
	}
	prefixBytes, err := hex.DecodeString(oracleDecoded.ChecksumPrefix[:30])
	if err != nil {
		t.Fatal(err)
	}
	expectedX := 29
	result, err := builderWithEntropy(&byteEntropy{data: prefixBytes}, 2_000_000_000_000).Build(context.Background(), Input{
		SceneID: "scene-id", CertifyID: "0123456789abcdef",
		Dimensions: Dimensions{RenderedWidth: 300, HandleWidth: 40}, Track: oracleTrack(),
		StaticPath: "3.29.0/pe.091.00665af58b020d81.js", ExpectedXPos: &expectedX,
		InitBeginTimeMS: 1_999_999_994_250, FirstTouchAgeMS: 700,
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := protocol.UnpackData(result.Data)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.JSONText != oracle.PayloadJSON || decoded.Payload.Arg != oracle.PEArg {
		t.Fatalf("payload differs from Python oracle:\n%s", decoded.JSONText)
	}
	if decoded.ChecksumPrefix != oracleDecoded.ChecksumPrefix {
		t.Fatalf("prefix=%s", decoded.ChecksumPrefix)
	}
	if result.XPos != 29 || result.SlidePos != 80 || result.TrackStartTimeMS != 1_999_999_994_250 || result.VerifyTimeMS != 1_999_999_995_000 {
		t.Fatalf("coordinates/times: %+v", result)
	}
	if result.TrackEventCount != 4 || result.DataMousemoveEventCount != 5 || result.PostGetterDataEventCount != 1 {
		t.Fatalf("event counts: %+v", result)
	}
	if !reflect.DeepEqual(result.PayloadKeys, []string{"TrackList", "TrackStartTime", "VerifyTime", "xPos", "slidePos", "arg"}) || len(result.TrackKeys) != 11 {
		t.Fatal("field ordering metadata differs")
	}
	getter := result.DeviceGetterPlans[0]
	if getter.Owner != "python-compatible.getToken" || getter.DerivedAtMS != 750 || getter.Arguments[0].Value != "0123456789abcdef" || getter.Arguments[0].EqualsSceneID {
		t.Fatalf("getter: %+v", getter)
	}
	if len(result.InteractionEvents) != 4 || result.InteractionEvents[0] != (InteractionEvent{"mousemove", 94, 548, 700, true}) {
		t.Fatalf("interaction events: %+v", result.InteractionEvents)
	}
}

func TestBuildUnknownArgUsesInjectedEntropyAndDefaults(t *testing.T) {
	argumentBytes := []byte{1, 2, 3, 4}
	prefixBytes := make([]byte, 15)
	for index := range prefixBytes {
		prefixBytes[index] = byte(index)
	}
	entropy := &byteEntropy{data: append(append([]byte(nil), argumentBytes...), prefixBytes...)}
	result, err := builderWithEntropy(entropy, 10_000).Build(context.Background(), Input{
		SceneID: "same", CertifyID: "same", Track: []track.Event{
			{X: 0, DT: 0}, {X: 3, DT: 1}, {X: 3, DT: 1},
		}, StaticPath: "unknown", InitBeginTimeMS: 5_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := protocol.UnpackData(result.Data)
	if err != nil {
		t.Fatal(err)
	}
	// CertifyID 是四字节，未知 arg 先消费同长度 CSPRNG。
	wantArg := base64.StdEncoding.EncodeToString(argumentBytes)
	if decoded.Payload.Arg != wantArg {
		t.Fatalf("unknown arg=%q", decoded.Payload.Arg)
	}
	if result.TargetFirstTouchAgeMS != defaultFirstTouchAgeMS || !result.DeviceGetterPlans[0].Arguments[0].EqualsSceneID {
		t.Fatalf("defaults/getter: %+v", result)
	}
	if result.DeviceGetterPlans[0].Arguments[0].Length != 4 {
		t.Fatal("getter length mismatch")
	}
}

func TestKnownArgumentTable(t *testing.T) {
	if len(argKeys) != 21 {
		t.Fatalf("arg key count=%d", len(argKeys))
	}
	failing := &byteEntropy{err: errors.New("entropy must not be used")}
	for path := range argKeys {
		argument, err := buildArgument("certify", path+".js", failing)
		if err != nil || argument == "" {
			t.Fatalf("known path %s: %q %v", path, argument, err)
		}
	}
}

func TestTrackAndRoundingValidation(t *testing.T) {
	if value, err := jsRound(0.5); err != nil || value != 1 {
		t.Fatalf("half-up: %d %v", value, err)
	}
	for _, value := range []float64{-1, math.NaN(), math.Inf(1), float64(int(^uint(0) >> 1))} {
		if _, err := jsRound(value); err == nil {
			t.Fatalf("jsRound accepted %v", value)
		}
	}
	if dimensions, err := normalizeDimensions(Dimensions{}); err != nil || dimensions != (Dimensions{300, 40}) {
		t.Fatalf("default dimensions: %+v %v", dimensions, err)
	}
	for _, dimensions := range []Dimensions{{1, 2}, {-1, 1}, {1, 0}} {
		if _, err := normalizeDimensions(dimensions); err == nil {
			t.Fatalf("dimensions accepted: %+v", dimensions)
		}
	}
	valid := oracleTrack()
	if normalized, duration, err := normalizeTrack(valid); err != nil || len(normalized) != 4 || duration != 50 {
		t.Fatalf("valid track: %v", err)
	}
	invalid := [][]track.Event{
		valid[:2],
		append([]track.Event{{Type: "touchstart", DT: 1}}, valid[1:]...),
		{{Type: "touchstart"}, {Type: "bad"}, {Type: "touchend"}},
		{{Type: "touchmove"}, {Type: "touchmove"}, {Type: "touchend"}},
		{{Type: "touchstart"}, {Type: "touchend"}, {Type: "touchend"}},
		{{Type: "touchstart"}, {Type: "touchmove", DT: -1}, {Type: "touchend"}},
		{{Type: "touchstart"}, {Type: "touchmove", DT: 60_001}, {Type: "touchend"}},
	}
	for index, events := range invalid {
		if _, _, err := normalizeTrack(events); err == nil {
			t.Fatalf("invalid track %d accepted", index)
		}
	}
	tooLong := make([]track.Event, maximumTrackEvents+1)
	if _, _, err := normalizeTrack(tooLong); err == nil {
		t.Fatal("513-event track accepted")
	}
	if _, err := buildTrackList([]eventRecord{{kind: "touchstart", text: "x"}, {kind: "touchmove", text: "x"}, {kind: "touchend", text: "x", x: -1, y: 1}}, device.ScreenProfile{}, 1, 1); err == nil {
		t.Fatal("negative RAF x accepted")
	}
	if _, err := buildTrackList([]eventRecord{{kind: "touchstart", text: "x"}, {kind: "touchmove", text: "x"}, {kind: "touchend", text: "x", x: 1, y: -1}}, device.ScreenProfile{}, 1, 1); err == nil {
		t.Fatal("negative RAF y accepted")
	}
}

func TestBuilderErrors(t *testing.T) {
	validInput := Input{SceneID: "s", CertifyID: "c", Track: oracleTrack(), InitBeginTimeMS: 5_000}
	validBuilder := builderWithEntropy(&byteEntropy{data: make([]byte, 100)}, 10_000)
	//lint:ignore SA1012 此处故意传 nil，验证公共边界拒绝空 context。
	if _, err := validBuilder.Build(nil, validInput); err == nil {
		t.Fatal("nil context accepted")
	}
	if _, err := (Builder{}).Build(context.Background(), validInput); err == nil {
		t.Fatal("missing sources accepted")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := validBuilder.Build(canceled, validInput); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context: %v", err)
	}
	badInputs := []Input{
		{CertifyID: "c", Track: oracleTrack()},
		{SceneID: "s", Track: oracleTrack()},
		{SceneID: "s", CertifyID: "c", Dimensions: Dimensions{1, 2}, Track: oracleTrack()},
		{SceneID: "s", CertifyID: "c", Track: oracleTrack()[:2]},
		{SceneID: "s", CertifyID: "c", Track: oracleTrack(), FirstTouchAgeMS: -1},
	}
	for index, input := range badInputs {
		if _, err := validBuilder.Build(context.Background(), input); err == nil {
			t.Fatalf("bad input %d accepted", index)
		}
	}
	oldClockBuilder := builderWithEntropy(&byteEntropy{data: make([]byte, 100)}, 1)
	if _, err := oldClockBuilder.Build(context.Background(), Input{SceneID: "s", CertifyID: "c", Track: oracleTrack()}); err == nil {
		t.Fatal("invalid logical clock accepted")
	}
	mismatch := 100
	input := validInput
	input.ExpectedXPos = &mismatch
	if _, err := validBuilder.Build(context.Background(), input); err == nil {
		t.Fatal("xPos mismatch accepted")
	}
	input = validInput
	input.Track = oracleTrack()
	input.Track[0].X = -originX - 1
	if _, err := validBuilder.Build(context.Background(), input); err == nil {
		t.Fatal("negative absolute x accepted")
	}
	input = validInput
	input.Track = oracleTrack()
	input.Track[len(input.Track)-1].Y = -originY - 1
	if _, err := validBuilder.Build(context.Background(), input); err == nil {
		t.Fatal("negative RAF y accepted")
	}
	failingBuilder := builderWithEntropy(&byteEntropy{err: errors.New("entropy failure")}, 10_000)
	if _, err := failingBuilder.Build(context.Background(), validInput); err == nil {
		t.Fatal("unknown arg entropy failure ignored")
	}
	known := validInput
	known.StaticPath = "3.29.0/pe.091.00665af58b020d81"
	if _, err := failingBuilder.Build(context.Background(), known); err == nil {
		t.Fatal("prefix entropy failure ignored")
	}
}

func TestHelpersRejectEntropyFailureAndUnicodeLength(t *testing.T) {
	failure := &byteEntropy{err: errors.New("boom")}
	if _, err := buildArgument("c", "unknown", failure); err == nil {
		t.Fatal("argument entropy failure ignored")
	}
	if _, err := dataPrefix(failure); err == nil {
		t.Fatal("prefix entropy failure ignored")
	}
	bytes, _ := hex.DecodeString(strings.Repeat("00", 15))
	result, err := builderWithEntropy(&byteEntropy{data: bytes}, 10_000).Build(context.Background(), Input{
		SceneID: "s", CertifyID: "测试", Track: oracleTrack(), StaticPath: "3.29.0/pe.091.00665af58b020d81", InitBeginTimeMS: 5_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.DeviceGetterPlans[0].Arguments[0].Length != 2 {
		t.Fatal("getter length must count Unicode code points")
	}
}
