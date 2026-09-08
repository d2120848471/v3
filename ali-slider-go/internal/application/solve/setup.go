package solve

import (
	"errors"
	"time"
	"unicode/utf8"
)

// New 建立无共享挑战状态的求解器；每轮资源由 Factory 创建并独占。
func New(options Options) (*Solver, error) {
	if options.Timeout <= 0 || options.Timeout > 5*time.Minute {
		return nil, errors.New("solver timeout must be within (0,5m]")
	}
	if options.MinimumConfidence < 0 || options.MinimumConfidence > 1 {
		return nil, errors.New("solver confidence must be within 0..1")
	}
	if options.AssetMaxBytes < 1 || options.AssetMaxBytes > 64<<20 || options.AssetMaxDimension < 1 || options.AssetMaxDimension > 16_384 || options.AssetMaxPixels < 1 {
		return nil, errors.New("solver asset limits are invalid")
	}
	if err := options.Sources.Validate(); err != nil {
		return nil, err
	}
	if options.Factory == nil {
		return nil, errors.New("solver round factory is required")
	}
	return &Solver{options: options}, nil
}

func validateRequest(request Request) error {
	if request.SceneID == "" || utf8.RuneCountInString(request.SceneID) > 64 {
		return errors.New("SceneId must contain 1..64 characters")
	}
	if request.Prefix == "" || len(request.Prefix) > 32 {
		return errors.New("prefix must contain 1..32 ASCII alphanumeric characters")
	}
	for _, value := range []byte(request.Prefix) {
		if (value < '0' || value > '9') && (value < 'A' || value > 'Z') && (value < 'a' || value > 'z') {
			return errors.New("prefix must contain 1..32 ASCII alphanumeric characters")
		}
	}
	if utf8.RuneCountInString(request.RPCKeyID) > 128 || len(request.Proxy) > 4096 {
		return errors.New("request field exceeds its size limit")
	}
	return nil
}
