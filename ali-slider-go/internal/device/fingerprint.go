package device

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/d2120848471/v3/ali-slider-go/internal/runtimekit"
)

const maxInteractionEvents = 512

func sessionProbe(sessionID string) (string, error) {
	if len(sessionID) < 8 {
		return "", errors.New("device session id is too short")
	}
	suffix := sessionID[len(sessionID)-8:]
	for _, value := range []byte(suffix) {
		if value < 32 || value > 126 {
			return "", errors.New("device session suffix is not printable ASCII")
		}
	}
	key := []byte("44d876b6")
	stage := make([]byte, 8)
	for index := range stage {
		stage[index] = 32 + ((suffix[index]-32)+(key[index]-32))%95
	}
	for index := range stage {
		stage[index] = 32 + ((stage[index] - 32 + 54) % 95)
	}
	return base64.StdEncoding.EncodeToString(stage), nil
}

type xorshiftStream struct{ state uint32 }

func newXorshiftStream(seed string) xorshiftStream {
	state := uint32(0x811c9dc5)
	for _, character := range seed {
		state = (state ^ uint32(character)) * 0x01000193
	}
	return xorshiftStream{state: state}
}

func (stream *xorshiftStream) next() uint32 {
	stream.state ^= stream.state << 13
	stream.state ^= stream.state >> 17
	stream.state ^= stream.state << 5
	return stream.state
}

func seededBase64(seed string, length int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	stream := newXorshiftStream(seed)
	output := make([]byte, length)
	for index := range output {
		output[index] = alphabet[stream.next()%64]
	}
	return string(output)
}

func canvasFingerprint(profile Profile) (string, error) {
	lengthStream := newXorshiftStream(profile.CanvasSeed + ":len")
	dataURL := "data:image/png;base64," + seededBase64(profile.CanvasSeed+":canvas", 3400+int(lengthStream.next()%420))
	material, err := json.Marshal(struct {
		Winding  bool   `json:"winding"`
		Geometry string `json:"geometry"`
		Text     string `json:"text"`
	}{Winding: true, Geometry: dataURL, Text: dataURL})
	if err != nil {
		return "", err
	}
	digest := md5.Sum(material)
	return hex.EncodeToString(digest[:]), nil
}

func gpuFingerprint(profile Profile) (string, error) {
	material, err := json.Marshal(struct {
		Vendor   string `json:"fhgjjghdf"`
		Renderer string `json:"xcvdfgfd"`
	}{Vendor: profile.GPU.UnmaskedVendor, Renderer: profile.GPU.UnmaskedRenderer})
	if err != nil {
		return "", err
	}
	digest := md5.Sum(material)
	return hex.EncodeToString(digest[:]), nil
}

func brandList(profile Profile) string {
	brands := make([]string, 0, len(profile.UABrands))
	for _, brand := range profile.UABrands {
		brands = append(brands, brand.Brand)
	}
	return "[" + strings.Join(brands, ",") + "]"
}

func randomAlnum(entropy runtimekit.Entropy, length int) (string, error) {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	if entropy == nil || length < 0 {
		return "", errors.New("invalid alphanumeric random request")
	}
	output := make([]byte, length)
	for index := range output {
		value, err := entropy.Uint64n(uint64(len(alphabet)))
		if err != nil {
			return "", err
		}
		output[index] = alphabet[value]
	}
	return string(output), nil
}

// InteractionEvent 是设备 Log3 接受的逻辑 mousemove 事件。
type InteractionEvent struct {
	Type      string  `json:"type"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	TimeStamp float64 `json:"timeStamp"`
	IsTrusted bool    `json:"isTrusted"`
}

func validateInteractionEvents(events []InteractionEvent) error {
	if len(events) < 1 || len(events) > maxInteractionEvents {
		return fmt.Errorf("interaction events must contain 1..%d values", maxInteractionEvents)
	}
	previous := -1.0
	for index, event := range events {
		if event.Type != "mousemove" || !event.IsTrusted || math.IsNaN(event.X) || math.IsNaN(event.Y) || math.IsNaN(event.TimeStamp) ||
			math.IsInf(event.X, 0) || math.IsInf(event.Y, 0) || math.IsInf(event.TimeStamp, 0) || event.TimeStamp < previous {
			return fmt.Errorf("interaction event %d is invalid", index)
		}
		previous = event.TimeStamp
	}
	return nil
}
