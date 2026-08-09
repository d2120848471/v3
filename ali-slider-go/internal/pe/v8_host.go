package pe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/d2120848471/v3/ali-slider-go/internal/runtimekit"
	"github.com/d2120848471/v3/ali-slider-go/internal/v8runtime"
)

const (
	v8HostRequestMaxBytes = 4 << 20
	v8HostHeaderMaxBytes  = 64 << 10
	v8RandomMaxBytes      = 65_536
)

type v8HostRequest struct {
	Op      string             `json:"op"`
	Length  *int               `json:"length,omitempty"`
	Request *v8HostHTTPRequest `json:"request,omitempty"`
}

type v8HostHTTPRequest struct {
	URL      string            `json:"url"`
	Method   string            `json:"method"`
	Headers  map[string]string `json:"headers"`
	Body     *string           `json:"body"`
	Redirect string            `json:"redirect"`
}

type v8HostHTTPResponse struct {
	Status     int               `json:"status"`
	StatusText string            `json:"statusText"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	URL        string            `json:"url"`
	Redirected bool              `json:"redirected"`
}

func newV8HostHandler(
	transport http.RoundTripper,
	entropy runtimekit.Entropy,
	networkEnabled bool,
) v8runtime.HostFunc {
	return func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		return handleV8HostRequest(ctx, transport, entropy, networkEnabled, raw)
	}
}

func handleV8HostRequest(
	ctx context.Context,
	transport http.RoundTripper,
	entropy runtimekit.Entropy,
	networkEnabled bool,
	raw json.RawMessage,
) (json.RawMessage, error) {
	if ctx == nil {
		return nil, errors.New("V8 host context is nil")
	}
	if len(raw) == 0 || len(raw) > v8HostRequestMaxBytes {
		return nil, errors.New("V8 host request size is invalid")
	}
	var request v8HostRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return nil, errors.New("V8 host request is invalid")
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, errors.New("V8 host request is invalid")
	}

	switch request.Op {
	case "random":
		if request.Length == nil || *request.Length < 0 || *request.Length > v8RandomMaxBytes {
			return nil, errors.New("V8 random byte count is invalid")
		}
		if request.Request != nil || entropy == nil {
			return nil, errors.New("V8 random host is unavailable")
		}
		value := make([]byte, *request.Length)
		if _, err := entropy.Read(value); err != nil {
			return nil, errors.New("V8 random source failed")
		}
		encoded, err := json.Marshal(map[string]string{
			"base64": base64.StdEncoding.EncodeToString(value),
		})
		return encoded, err
	case "http":
		if request.Length != nil || request.Request == nil {
			return nil, errors.New("V8 HTTP request is invalid")
		}
		if !networkEnabled || transport == nil {
			return nil, errors.New("V8 HTTP host is disabled")
		}
		response, err := roundTripV8HostRequest(ctx, transport, *request.Request)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(response)
		return encoded, err
	default:
		return nil, errors.New("V8 host operation is unsupported")
	}
}

func allowedV8NetworkURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return nil, errors.New("V8 HTTP URL is invalid")
	}
	hostname := strings.ToLower(parsed.Hostname())
	hostAllowed := hostname == keyPEHost || strings.HasSuffix(hostname, ".aliyuncs.com")
	if parsed.Port() != "" || parsed.User != nil || !hostAllowed {
		return nil, errors.New("V8 HTTP URL is outside the allowlist")
	}
	return parsed, nil
}

func roundTripV8HostRequest(
	ctx context.Context,
	transport http.RoundTripper,
	input v8HostHTTPRequest,
) (v8HostHTTPResponse, error) {
	parsed, err := allowedV8NetworkURL(input.URL)
	if err != nil {
		return v8HostHTTPResponse{}, err
	}
	method := strings.ToUpper(strings.TrimSpace(input.Method))
	if method == "" {
		method = http.MethodGet
	}
	if input.Redirect != "" && input.Redirect != "manual" {
		return v8HostHTTPResponse{}, errors.New("V8 HTTP redirects must be handled by the JS bridge")
	}
	var body io.Reader
	if input.Body != nil {
		if len(*input.Body) > int(keyScriptMaxBytes) {
			return v8HostHTTPResponse{}, errors.New("V8 HTTP request body is too large")
		}
		body = strings.NewReader(*input.Body)
	}
	request, err := http.NewRequestWithContext(ctx, method, parsed.String(), body)
	if err != nil {
		return v8HostHTTPResponse{}, errors.New("V8 HTTP request is invalid")
	}
	headerBytes := 0
	for name, value := range input.Headers {
		headerBytes += len(name) + len(value)
		if headerBytes > v8HostHeaderMaxBytes {
			return v8HostHTTPResponse{}, errors.New("V8 HTTP headers are too large")
		}
		request.Header.Set(name, value)
	}

	response, err := transport.RoundTrip(request)
	if err != nil {
		if ctx.Err() != nil {
			return v8HostHTTPResponse{}, ctx.Err()
		}
		return v8HostHTTPResponse{}, errors.New("V8 HTTP transport failed")
	}
	if response == nil || response.Body == nil {
		return v8HostHTTPResponse{}, errors.New("V8 HTTP response is invalid")
	}
	defer response.Body.Close()
	if response.StatusCode < 100 || response.StatusCode > 999 {
		return v8HostHTTPResponse{}, errors.New("V8 HTTP response status is invalid")
	}
	limited := io.LimitReader(response.Body, keyScriptMaxBytes+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return v8HostHTTPResponse{}, errors.New("V8 HTTP response body failed")
	}
	if int64(len(payload)) > keyScriptMaxBytes {
		return v8HostHTTPResponse{}, errors.New("V8 HTTP response body is too large")
	}
	finalURL := parsed.String()
	if response.Request != nil && response.Request.URL != nil {
		validated, validateErr := allowedV8NetworkURL(response.Request.URL.String())
		if validateErr != nil {
			return v8HostHTTPResponse{}, errors.New("V8 HTTP response URL is outside the allowlist")
		}
		finalURL = validated.String()
	}
	headers := make(map[string]string, len(response.Header))
	for name, values := range response.Header {
		headers[strings.ToLower(name)] = strings.Join(values, ", ")
	}
	statusText := http.StatusText(response.StatusCode)
	if statusText == "" {
		statusText = fmt.Sprintf("HTTP %d", response.StatusCode)
	}
	return v8HostHTTPResponse{
		Status:     response.StatusCode,
		StatusText: statusText,
		Headers:    headers,
		Body:       string(payload),
		URL:        finalURL,
		Redirected: false,
	}, nil
}
