package device

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	model "github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
)

const (
	testStartMS   = int64(1_700_000_000_000)
	testConfigKey = "fedcba9876543210"
)

type fixedSessionClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newFixedSessionClock() *fixedSessionClock {
	return &fixedSessionClock{now: time.UnixMilli(testStartMS)}
}

func (clock *fixedSessionClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fixedSessionClock) Sleep(ctx context.Context, duration time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.sleeps = append(clock.sleeps, duration)
	clock.now = clock.now.Add(duration)
	return nil
}

type deterministicSessionEntropy struct {
	mu      sync.Mutex
	read    byte
	integer uint64
}

func (entropy *deterministicSessionEntropy) Read(buffer []byte) (int, error) {
	entropy.mu.Lock()
	defer entropy.mu.Unlock()
	for index := range buffer {
		buffer[index] = entropy.read
		entropy.read++
	}
	return len(buffer), nil
}

func (entropy *deterministicSessionEntropy) Uint64n(limit uint64) (uint64, error) {
	if limit == 0 {
		return 0, errors.New("zero random limit")
	}
	entropy.mu.Lock()
	defer entropy.mu.Unlock()
	value := entropy.integer % limit
	entropy.integer++
	return value, nil
}

type recordedDeviceRPC struct {
	action string
	data   string
	body   string
	url    string
	header http.Header
}

type recordingDeviceTransport struct {
	mu       sync.Mutex
	clock    interface{ Now() time.Time }
	secrets  protocol.FrontendSecrets
	requests []recordedDeviceRPC
	log1s    int
	failure  error
}

func (transport *recordingDeviceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.failure != nil {
		return nil, transport.failure
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	if request.GetBody != nil {
		return nil, errors.New("device RPC request exposed a replayable GetBody")
	}
	if request.ContentLength != int64(len(body)) {
		return nil, errors.New("device RPC request lost its ContentLength")
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	action := form.Get("Action")
	transport.mu.Lock()
	transport.requests = append(transport.requests, recordedDeviceRPC{
		action: action, data: form.Get("Data"), body: string(body),
		url: request.URL.String(), header: request.Header.Clone(),
	})
	index := transport.log1s
	if action == "Log1" {
		transport.log1s++
	}
	transport.mu.Unlock()

	response := map[string]any{"Code": "200"}
	if action == "Log1" {
		sessionIDs := []string{"offline-session-ABCDEFGH", "offline-session-IJKLMNOP"}
		if index >= len(sessionIDs) {
			index = len(sessionIDs) - 1
		}
		plaintext := strings.Join([]string{
			base64.StdEncoding.EncodeToString([]byte(testConfigKey)),
			base64.StdEncoding.EncodeToString([]byte("1")),
			sessionIDs[index], "1.5.1", "", "", "",
			fmt.Sprint(transport.clock.Now().UnixMilli() + 10), "203.0.113.8", "1",
		}, "#")
		ciphertext, encryptErr := protocol.AESCBCEncryptBase64([]byte(plaintext), transport.secrets.DeviceResponseKey())
		if encryptErr != nil {
			return nil, encryptErr
		}
		response["ResultObject"] = map[string]string{"DeviceConfig": ciphertext}
	} else {
		// 真实服务的非 Log1 action 不保证 ResultObject 为对象。
		// 设备合同只对 Log1 解析 DeviceConfig，其他 action 只检查 Code。
		response["ResultObject"] = "accepted"
	}
	encoded, _ := json.Marshal(response)
	header := make(http.Header)
	if action == "Log1" {
		header.Add("Set-Cookie", "feilin-session=fixture-cookie; Path=/; Secure; HttpOnly")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(string(encoded))),
		Request:    request,
	}, nil
}

func TestDeviceSessionOfflineProtocolAndStateMachine(t *testing.T) {
	client, transport, clock := newSessionTestClient(t)
	session, err := client.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	initial, err := session.InitialResult()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(initial.RequestActions, []string{"Log1", "Log2", "Log3"}) || initial.RequestCount != 3 || initial.FingerprintFieldCount != 111 {
		t.Fatalf("initial result: %+v", initial.RequestActions)
	}
	if !reflect.DeepEqual(clock.sleeps, []time.Duration{100 * time.Millisecond, 44 * time.Millisecond, 6 * time.Millisecond}) {
		t.Fatalf("initial timing sleeps: %v", clock.sleeps)
	}
	if initial.CollectorStartedMS != testStartMS || initial.TokenTimeMS != testStartMS+144 || initial.GatherCost != 200 {
		t.Fatalf("initial timing result: %#v", initial)
	}
	firstAge, err := session.TargetFirstTouchAgeMS()
	if err != nil {
		t.Fatal(err)
	}
	secondAge, _ := session.TargetFirstTouchAgeMS()
	if firstAge != secondAge || firstAge < 700 || firstAge > 702 {
		t.Fatalf("first touch age changed: %d / %d", firstAge, secondAge)
	}

	events := []model.InteractionEvent{
		{Type: "mousemove", X: 10, Y: 20, TimeStamp: 700, IsTrusted: true},
		{Type: "mousemove", X: 20, Y: 21, TimeStamp: 720, IsTrusted: true},
	}
	completed, err := session.Complete(context.Background(), "certify-id-1234", events, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(completed.RequestActions, []string{"Log1", "Log2", "Log3", "Log2"}) || completed.GetterArgumentCount != 1 || completed.InteractionEventCount != 2 {
		t.Fatalf("completed result: actions=%v getter=%d events=%d", completed.RequestActions, completed.GetterArgumentCount, completed.InteractionEventCount)
	}
	if _, err := session.Complete(context.Background(), "again", events, 0); err == nil {
		t.Fatal("second complete was accepted")
	}
	if _, err := session.TargetFirstTouchAgeMS(); err == nil {
		t.Fatal("completed session returned first touch age")
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", completed, completed), completed.VerifyToken) {
		t.Fatal("Result formatting leaked token")
	}

	transport.mu.Lock()
	requests := append([]recordedDeviceRPC(nil), transport.requests...)
	transport.mu.Unlock()
	if len(requests) != 4 {
		t.Fatalf("request count=%d", len(requests))
	}
	for index, request := range requests {
		if request.url != "https://device.invalid" || request.header.Get("Origin") != deviceOrigin || request.header.Get("User-Agent") != testProfile().UserAgent {
			t.Fatalf("request %d route/profile changed", index)
		}
		assertOrderedRPCForm(t, request.body)
		assertRPCSignature(t, request.body, client.secrets.DeviceRPCKeySecret())
	}
	if requests[0].header.Get("Cookie") != "" || requests[1].header.Get("Cookie") != "feilin-session=fixture-cookie" || requests[2].header.Get("Cookie") != "feilin-session=fixture-cookie" {
		t.Fatalf("device session cookie continuity failed: %q %q %q", requests[0].header.Get("Cookie"), requests[1].header.Get("Cookie"), requests[2].header.Get("Cookie"))
	}
	assertLog1(t, requests[0].data, client.secrets)

	initFields := tokenFields(t, initial.InitToken, client.secrets.DeviceTokenSalt())
	verifyFields := tokenFields(t, completed.VerifyToken, client.secrets.DeviceTokenSalt())
	if got := nonEmptyIndexes(initFields); !reflect.DeepEqual(got, []int{0, 5, 6, 7, 21, 22, 32, 34, 36, 37, 42, 43, 44, 45, 47, 49, 53, 63, 64, 67, 68, 71, 72, 73, 74, 75, 78, 80, 85, 86, 87, 110}) {
		t.Fatalf("non-empty fingerprint indexes: %v", got)
	}
	if initFields[21] != "LC1eMzMzYDU=" || initFields[77] != "" || verifyFields[77] != "certify-id-1234" {
		t.Fatalf("session probe/getter fields: %q %q %q", initFields[21], initFields[77], verifyFields[77])
	}
	if initFields[73] != verifyFields[73] || initFields[71] == verifyFields[71] {
		t.Fatal("stable/rotating fingerprint fields violated")
	}
	if initFields[43] != "10-0|11-10|20-11|23-100|30-101|40-105|90-141|91-144|92-144|93-148|94-152" {
		t.Fatalf("initial timing=%q", initFields[43])
	}
	if verifyFields[43] != "10-0|11-10|20-11|23-100|30-101|40-105|90-147|91-150|92-150|93-152|94-152" {
		t.Fatalf("verify timing=%q", verifyFields[43])
	}
	assertLog2Snapshot(t, requests[1].data, initFields, client.secrets)
	assertLog3(t, requests[2].data, client.secrets)
	assertLog2Snapshot(t, requests[3].data, verifyFields, client.secrets)
}

func TestSafeResponseCode(t *testing.T) {
	for input, expected := range map[string]string{
		`"400"`: "400", `"Risk-01"`: "Risk-01", `null`: "null", `"secret text"`: "non-success", `"`: "non-success",
	} {
		if actual := safeResponseCode(json.RawMessage(input)); actual != expected {
			t.Fatalf("safeResponseCode(%q)=%q, want %q", input, actual, expected)
		}
	}
}

func TestSafeResponseMetadataIsBoundedAndRedacted(t *testing.T) {
	response := &http.Response{
		Proto:        "HTTP/2.0",
		Header:       make(http.Header),
		Uncompressed: false,
	}
	response.Header.Set("Content-Type", "text/html; charset=utf-8")
	response.Header.Set("Content-Encoding", "br")
	actual := safeResponseMetadata(response, []byte("<html>secret-token</html>"))
	want := "proto=HTTP/2.0 contentType=text/html contentEncoding=br uncompressed=false bodyBytes=25 prefixHex=3c68746d"
	if actual != want {
		t.Fatalf("metadata=%q, want %q", actual, want)
	}
	if strings.Contains(actual, "secret-token") {
		t.Fatal("response metadata leaked body text")
	}

	response.Proto = "attacker-proto secret"
	response.Header.Set("Content-Type", "text/plain; boundary=secret-token")
	response.Header.Set("Content-Encoding", "secret-token")
	actual = safeResponseMetadata(response, nil)
	if actual != "proto=unknown contentType=text/plain contentEncoding=unknown uncompressed=false bodyBytes=0 prefixHex=empty" {
		t.Fatalf("sanitized metadata=%q", actual)
	}
}

func TestSessionsKeepIndependentState(t *testing.T) {
	client, transport, _ := newSessionTestClient(t)
	first, err := client.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	firstToken, _ := first.InitToken()
	secondToken, _ := second.InitToken()
	if firstToken == secondToken || first.config.SessionID == second.config.SessionID {
		t.Fatal("sessions shared token/config state")
	}
	first.Close()
	if _, err := first.InitToken(); err == nil {
		t.Fatal("closed session remained usable")
	}
	if got, err := second.InitToken(); err != nil || got != secondToken {
		t.Fatal("closing first session changed second session")
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.requests) != 6 {
		t.Fatalf("shared transport request count=%d", len(transport.requests))
	}
}

type cancelSessionClock struct {
	now     time.Time
	started chan struct{}
	once    sync.Once
}

func (clock *cancelSessionClock) Now() time.Time { return clock.now }
func (clock *cancelSessionClock) Sleep(ctx context.Context, _ time.Duration) error {
	clock.once.Do(func() { close(clock.started) })
	<-ctx.Done()
	return ctx.Err()
}

func TestOpenCancelsProtocolSleep(t *testing.T) {
	clock := &cancelSessionClock{now: time.UnixMilli(testStartMS), started: make(chan struct{})}
	client, _ := newSessionClient(t, clock)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.Open(ctx)
		result <- err
	}()
	<-clock.started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestClientAndCompleteRejectInvalidInputsWithoutLeakingCause(t *testing.T) {
	profile := testProfile()
	sources := runtimekit.Sources{Clock: newFixedSessionClock(), Entropy: &deterministicSessionEntropy{}}
	valid := DefaultClientOptions(profile, sources)
	valid.Endpoint = "https://device.invalid"
	for _, mutate := range []func(*ClientOptions){
		func(value *ClientOptions) { value.Endpoint = "ftp://device.invalid" },
		func(value *ClientOptions) { value.Prefix = "" },
		func(value *ClientOptions) { value.Timeout = 0 },
		func(value *ClientOptions) { value.GatherCostMin = 2; value.GatherCostMax = 1 },
		func(value *ClientOptions) { value.FirstTouchAgeMin = 0 },
		func(value *ClientOptions) { value.Profile = model.Profile{} },
		func(value *ClientOptions) { value.Sources = runtimekit.Sources{} },
	} {
		options := valid
		mutate(&options)
		if _, err := NewClient(options, http.DefaultTransport); err == nil {
			t.Fatal("invalid client options accepted")
		}
	}
	if _, err := NewClient(valid, nil); err == nil {
		t.Fatal("nil transport accepted")
	}

	transport := &recordingDeviceTransport{failure: errors.New("upstream password=secret")}
	client, err := NewClient(valid, transport)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Open(context.Background())
	if err == nil || strings.Contains(err.Error(), "password=secret") {
		t.Fatalf("transport error leaked: %v", err)
	}

	client, _, _ = newSessionTestClient(t)
	session, err := client.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	validEvents := []model.InteractionEvent{{Type: "mousemove", IsTrusted: true}}
	if _, err := session.Complete(context.Background(), "", validEvents, 0); err == nil {
		t.Fatal("empty getter accepted")
	}
	if _, err := session.Complete(context.Background(), "getter", nil, 0); err == nil {
		t.Fatal("empty events accepted")
	}
	if _, err := session.Complete(context.Background(), "getter", validEvents, 501); err == nil {
		t.Fatal("large post delay accepted")
	}
}

func newSessionTestClient(t *testing.T) (*Client, *recordingDeviceTransport, *fixedSessionClock) {
	t.Helper()
	clock := newFixedSessionClock()
	client, transport := newSessionClient(t, clock)
	return client, transport, clock
}

func newSessionClient(t *testing.T, clock runtimekit.Clock) (*Client, *recordingDeviceTransport) {
	t.Helper()
	secrets, err := protocol.ResolveFrontendSecrets()
	if err != nil {
		t.Fatal(err)
	}
	transport := &recordingDeviceTransport{clock: clock, secrets: secrets}
	options := DefaultClientOptions(testProfile(), runtimekit.Sources{Clock: clock, Entropy: &deterministicSessionEntropy{}})
	options.Endpoint = "https://device.invalid"
	options.GatherCostMin, options.GatherCostMax = 200, 200
	options.FirstTouchAgeMin, options.FirstTouchAgeMax = 700, 702
	client, err := NewClient(options, transport)
	if err != nil {
		t.Fatal(err)
	}
	return client, transport
}

func testProfile() model.Profile {
	return model.Profile{
		ProfileID: "0011223344556677", Family: "android-mali",
		UserAgent:  "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.7871.187 Mobile Safari/537.36",
		AppVersion: "5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.7871.187 Mobile Safari/537.36",
		Platform:   "Linux armv8l", Vendor: "Google Inc.", Mobile: true,
		UABrands:   []model.UABrand{{Brand: "Chromium", Version: "150"}, {Brand: "Google Chrome", Version: "150"}},
		UAPlatform: "Android", UAPlatformVersion: "14.0.0", UAFullVersion: "150.0.7871.187",
		Screen:              model.ScreenProfile{Width: 393, Height: 852},
		GPU:                 model.GPUProfile{UnmaskedVendor: "Google Inc. (ARM)", UnmaskedRenderer: "ANGLE (ARM, Mali-G710 MC10, OpenGL ES 3.2)", MaxTextureSize: 16_384},
		HardwareConcurrency: 8, DeviceMemory: 8, MaxTouchPoints: 5,
		CanvasSeed: "00112233445566778899aabbccddeeff",
	}
}

func assertOrderedRPCForm(t *testing.T, body string) {
	t.Helper()
	parts := strings.Split(body, "&")
	keys := make([]string, len(parts))
	for index, part := range parts {
		keys[index], _, _ = strings.Cut(part, "=")
	}
	expected := []string{"AaduaneId", "Version", "SignatureMethod", "SignatureVersion", "Format", "Action", "Data", "SignatureNonce", "Signature"}
	if !reflect.DeepEqual(keys, expected) {
		t.Fatalf("RPC form order=%v", keys)
	}
}

func assertRPCSignature(t *testing.T, body, secret string) {
	t.Helper()
	form, err := url.ParseQuery(body)
	if err != nil {
		t.Fatal(err)
	}
	params := make(map[string]string, len(form))
	for key, values := range form {
		params[key] = values[0]
	}
	expected, err := protocol.RPCV1Signature(params, secret)
	if err != nil || form.Get("Signature") != expected {
		t.Fatalf("RPC signature mismatch: %v", err)
	}
}

func assertLog1(t *testing.T, data string, secrets protocol.FrontendSecrets) {
	t.Helper()
	outer, err := protocol.AESCBCDecryptBase64(data, secrets.DeviceRequestKey())
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(outer), "#")
	if len(parts) != 6 || parts[0] != deviceAPIKey || parts[1] != "W" || parts[3] != deviceAppVersion || parts[4] != "CLOUD" || parts[5] != "" {
		t.Fatalf("Log1 outer=%q", outer)
	}
	inner, err := protocol.AESCBCDecryptBase64(parts[2], secrets.DeviceFlagKey())
	if err != nil || string(inner) != "W.10001.c#saf-captcha#scene#captcha-front#fsgtmi#cn" {
		t.Fatalf("Log1 inner=%q err=%v", inner, err)
	}
}

func tokenFields(t *testing.T, token, salt string) []string {
	t.Helper()
	parsed, err := protocol.ParseDeviceToken(token, salt)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := protocol.AESCBCDecryptBase64(parsed.FingerprintCipher, testConfigKey)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(string(plaintext), "#")
	if len(fields) != 111 {
		t.Fatalf("fingerprint fields=%d", len(fields))
	}
	return fields
}

func nonEmptyIndexes(fields []string) []int {
	result := make([]int, 0, len(fields))
	for index, value := range fields {
		if value != "" {
			result = append(result, index)
		}
	}
	return result
}

func assertLog2Snapshot(t *testing.T, data string, tokenFields []string, secrets protocol.FrontendSecrets) {
	t.Helper()
	outerText, err := protocol.AESCBCDecryptBase64(data, secrets.DeviceUploadKey())
	if err != nil {
		t.Fatal(err)
	}
	outer := strings.Split(string(outerText), "#")
	if len(outer) != 8 || outer[5] != "0" || outer[6] != "501" {
		t.Fatalf("Log2 outer=%q", outerText)
	}
	innerText, err := base64.StdEncoding.Strict().DecodeString(outer[7])
	if err != nil {
		t.Fatal(err)
	}
	inner := strings.Split(string(innerText), "#")
	if len(inner) != 6 || inner[0] == "" {
		t.Fatalf("Log2 inner=%q", innerText)
	}
	snapshotText, err := protocol.AESCBCDecryptBase64(inner[1], testConfigKey)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := strings.Split(string(snapshotText), "#")
	if len(snapshot) != 111 {
		t.Fatalf("Log2 fields=%d", len(snapshot))
	}
	for index := range snapshot {
		if index == 43 {
			continue
		}
		if snapshot[index] != tokenFields[index] {
			t.Fatalf("Log2 changed field %d", index)
		}
	}
	expectedTiming := make([]string, 0, 9)
	for _, part := range strings.Split(tokenFields[43], "|") {
		if !strings.HasPrefix(part, "93-") && !strings.HasPrefix(part, "94-") {
			expectedTiming = append(expectedTiming, part)
		}
	}
	if snapshot[43] != strings.Join(expectedTiming, "|") || strings.Contains(snapshot[43], "93-") || strings.Contains(snapshot[43], "94-") {
		t.Fatalf("Log2 timing snapshot=%q", snapshot[43])
	}
}

func assertLog3(t *testing.T, data string, secrets protocol.FrontendSecrets) {
	t.Helper()
	outerText, err := protocol.AESCBCDecryptBase64(data, secrets.DeviceUploadKey())
	if err != nil {
		t.Fatal(err)
	}
	outer := strings.Split(string(outerText), "#")
	if len(outer) != 7 || outer[5] != "" {
		t.Fatalf("Log3 outer=%q", outerText)
	}
	stageBytes, err := base64.StdEncoding.Strict().DecodeString(outer[6])
	if err != nil {
		t.Fatal(err)
	}
	stage := strings.TrimPrefix(string(stageBytes), "511#")
	markerText, combatText, found := strings.Cut(stage, "-504#")
	if !found {
		t.Fatalf("Log3 stage=%q", stageBytes)
	}
	markerBytes, err := base64.StdEncoding.Strict().DecodeString(markerText)
	if err != nil {
		t.Fatal(err)
	}
	combatBytes, err := base64.StdEncoding.Strict().DecodeString(combatText)
	if err != nil {
		t.Fatal(err)
	}
	marker := strings.Split(string(markerBytes), "#")
	combat := strings.Split(string(combatBytes), "#")
	if len(marker) != 6 || len(combat) != 6 || marker[0] != "offline-session-ABCDEFGH" || combat[0] != marker[0] {
		t.Fatalf("Log3 headers marker=%q combat=%q", markerBytes, combatBytes)
	}
	eventJSON, err := protocol.AESCBCDecryptBase64(combat[1], testConfigKey)
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"mousemove":[],"mouseclick":[],"keyup":[],"scrollTop":[],"scrollLeft":[],"pointerEvent":[],"clientType":"mobile","startTime":1700000000100,"timestamp":"1700000000010"}`
	if string(eventJSON) != expected {
		t.Fatalf("Log3 events=%s", eventJSON)
	}
}
