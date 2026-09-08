// Package device 定义移动 Chromium 设备画像、指纹规则和设备会话数据合同。
package device

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
)

type family struct {
	name            string
	models          []string
	gpus            [][2]string
	screens         []screenChoice
	concurrency     []int
	memory          []int
	maxTextureSizes []int
}

type screenChoice struct {
	width, height int
	ratio         float64
}

var androidScreens = []screenChoice{
	{393, 852, 2.75}, {412, 915, 2.625}, {360, 800, 3.0}, {384, 854, 2.8125},
	{412, 892, 3.5}, {360, 780, 3.0}, {393, 873, 2.75}, {411, 914, 2.625},
}

var families = []family{
	{
		name:   "android-adreno",
		models: []string{"SM-S9110", "SM-S9210", "SM-G9980", "2211133C", "23013RK75C", "PJD110", "2304FPN6DC"},
		gpus: [][2]string{
			{"Google Inc. (Qualcomm)", "ANGLE (Qualcomm, Adreno (TM) 730, OpenGL ES 3.2)"},
			{"Google Inc. (Qualcomm)", "ANGLE (Qualcomm, Adreno (TM) 740, OpenGL ES 3.2)"},
			{"Google Inc. (Qualcomm)", "ANGLE (Qualcomm, Adreno (TM) 750, OpenGL ES 3.2)"},
		},
		screens: androidScreens, concurrency: []int{8, 8, 6}, memory: []int{8, 8, 4}, maxTextureSizes: []int{16_384},
	},
	{
		name:   "android-mali",
		models: []string{"Pixel 7", "Pixel 8", "Pixel 7a", "V2309A", "22041216C", "CPH2531"},
		gpus: [][2]string{
			{"Google Inc. (ARM)", "ANGLE (ARM, Mali-G610 MC6, OpenGL ES 3.2)"},
			{"Google Inc. (ARM)", "ANGLE (ARM, Mali-G710 MC10, OpenGL ES 3.2)"},
			{"Google Inc. (ARM)", "ANGLE (ARM, Mali-G715-Immortalis MC11, OpenGL ES 3.2)"},
		},
		screens: androidScreens, concurrency: []int{8, 8, 6}, memory: []int{8, 8, 4}, maxTextureSizes: []int{8192, 16_384},
	},
}

var (
	androidVersions = []string{"13.0.0", "14.0.0", "15.0.0"}
	chromeBuilds    = [][2]string{{"148", "148.0.7647.212"}, {"149", "149.0.7742.96"}, {"150", "150.0.7871.187"}, {"151", "151.0.7955.63"}}
	greaseBrands    = []string{"Not;A=Brand", "Not)A;Brand", "Not.A/Brand", "Not/A)Brand", "Not_A Brand", "Not-A.Brand", "Not A(Brand", "Not?A_Brand"}
	greaseVersions  = []string{"8", "24", "99"}
	languageSets    = [][]string{{"zh-CN", "zh"}, {"zh-CN", "zh", "en-US", "en"}, {"zh-CN", "en-US", "en", "zh"}}
)

// ScreenProfile 是同一台移动设备的屏幕与视口组合。
type ScreenProfile struct {
	Width            int     `json:"width"`
	Height           int     `json:"height"`
	AvailWidth       int     `json:"availWidth"`
	AvailHeight      int     `json:"availHeight"`
	AvailLeft        int     `json:"availLeft"`
	AvailTop         int     `json:"availTop"`
	ColorDepth       int     `json:"colorDepth"`
	OrientationType  string  `json:"orientationType"`
	OrientationAngle int     `json:"orientationAngle"`
	DevicePixelRatio float64 `json:"devicePixelRatio"`
	InnerWidth       int     `json:"innerWidth"`
	InnerHeight      int     `json:"innerHeight"`
	OuterWidth       int     `json:"outerWidth"`
	OuterHeight      int     `json:"outerHeight"`
}

// GPUProfile 是 WEBGL_debug_renderer_info 对应的移动 GPU。
type GPUProfile struct {
	UnmaskedVendor   string `json:"unmaskedVendor"`
	UnmaskedRenderer string `json:"unmaskedRenderer"`
	MaxTextureSize   int    `json:"maxTextureSize"`
}

// UABrand 保持 UA-CH 品牌顺序。
type UABrand struct {
	Brand   string `json:"brand"`
	Version string `json:"version"`
}

// Profile 在一轮 HTTP 头、设备指纹和 PE 计算间共享，创建后只读。
type Profile struct {
	ProfileID           string
	Family              string
	UserAgent           string
	AppVersion          string
	Platform            string
	Vendor              string
	Mobile              bool
	PDFViewer           bool
	UABrands            []UABrand
	UAFullVersions      []UABrand
	UAPlatform          string
	UAPlatformVersion   string
	UAModel             string
	UAFullVersion       string
	UAArchitecture      string
	UABitness           string
	Language            string
	Languages           []string
	Screen              ScreenProfile
	GPU                 GPUProfile
	HardwareConcurrency int
	DeviceMemory        int
	MaxTouchPoints      int
	CanvasSeed          string
	TextMetricScale     float64
}

// Clone 返回不共享 slice 底层数组的只读画像副本。
func (p Profile) Clone() Profile {
	p.UABrands = append([]UABrand(nil), p.UABrands...)
	p.UAFullVersions = append([]UABrand(nil), p.UAFullVersions...)
	p.Languages = append([]string(nil), p.Languages...)
	return p
}

// GenerateProfile 从同一家族抽取自洽画像。所有随机字段均来自注入熵。
func GenerateProfile(entropy runtimekit.Entropy) (Profile, error) {
	if entropy == nil {
		return Profile{}, errors.New("profile entropy is required")
	}
	random := profileRandom{entropy: entropy}
	familyValue, err := choose(&random, families)
	if err != nil {
		return Profile{}, err
	}
	chrome, err := choose(&random, chromeBuilds)
	if err != nil {
		return Profile{}, err
	}
	platformVersion, err := choose(&random, androidVersions)
	if err != nil {
		return Profile{}, err
	}
	model, err := choose(&random, familyValue.models)
	if err != nil {
		return Profile{}, err
	}
	brands, fullVersions, err := buildBrands(&random, chrome[0], chrome[1])
	if err != nil {
		return Profile{}, err
	}
	gpu, err := choose(&random, familyValue.gpus)
	if err != nil {
		return Profile{}, err
	}
	languages, err := choose(&random, languageSets)
	if err != nil {
		return Profile{}, err
	}
	profileID, err := runtimekit.Hex(entropy, 8)
	if err != nil {
		return Profile{}, fmt.Errorf("profile id: %w", err)
	}
	screen, err := buildScreen(&random, familyValue)
	if err != nil {
		return Profile{}, err
	}
	texture, err := choose(&random, familyValue.maxTextureSizes)
	if err != nil {
		return Profile{}, err
	}
	concurrency, err := choose(&random, familyValue.concurrency)
	if err != nil {
		return Profile{}, err
	}
	memory, err := choose(&random, familyValue.memory)
	if err != nil {
		return Profile{}, err
	}
	canvasSeed, err := runtimekit.Hex(entropy, 16)
	if err != nil {
		return Profile{}, fmt.Errorf("canvas seed: %w", err)
	}
	metric, err := random.uniform(5.62, 6.48)
	if err != nil {
		return Profile{}, err
	}
	userAgent := fmt.Sprintf("Mozilla/5.0 (Linux; Android %s; %s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s.0.0.0 Mobile Safari/537.36", strings.SplitN(platformVersion, ".", 2)[0], model, chrome[0])
	return Profile{
		ProfileID: profileID, Family: familyValue.name, UserAgent: userAgent,
		AppVersion: strings.TrimPrefix(userAgent, "Mozilla/"), Platform: "Linux armv8l",
		Vendor: "Google Inc.", Mobile: true, PDFViewer: false, UABrands: brands,
		UAFullVersions: fullVersions, UAPlatform: "Android", UAPlatformVersion: platformVersion,
		UAModel: model, UAFullVersion: chrome[1], UAArchitecture: "", UABitness: "64",
		Language: languages[0], Languages: append([]string(nil), languages...), Screen: screen,
		GPU:                 GPUProfile{UnmaskedVendor: gpu[0], UnmaskedRenderer: gpu[1], MaxTextureSize: texture},
		HardwareConcurrency: concurrency, DeviceMemory: memory, MaxTouchPoints: 5,
		CanvasSeed: canvasSeed, TextMetricScale: math.RoundToEven(metric*10_000) / 10_000,
	}, nil
}

// SecCHUA 返回 Chrome 的有序品牌头。
func (p Profile) SecCHUA() string {
	parts := make([]string, 0, len(p.UABrands))
	for _, brand := range p.UABrands {
		parts = append(parts, fmt.Sprintf("\"%s\";v=\"%s\"", brand.Brand, brand.Version))
	}
	return strings.Join(parts, ", ")
}

func (p Profile) SecCHUAMobile() string {
	if p.Mobile {
		return "?1"
	}
	return "?0"
}

func (p Profile) SecCHUAPlatform() string { return fmt.Sprintf("\"%s\"", p.UAPlatform) }

// AcceptLanguage 使用 Chrome 的递减 q 值格式。
func (p Profile) AcceptLanguage() string {
	if len(p.Languages) == 0 {
		return ""
	}
	parts := []string{p.Languages[0]}
	quality := 10
	for _, language := range p.Languages[1:] {
		quality--
		parts = append(parts, fmt.Sprintf("%s;q=0.%d", language, quality))
	}
	return strings.Join(parts, ",")
}

// BrowserHeaders 构造与画像一致的跨域浏览器头。
func (p Profile) BrowserHeaders(referer, origin, destination, mode string, includeOrigin bool) map[string]string {
	headers := map[string]string{
		"Accept": "*/*", "Accept-Language": p.AcceptLanguage(), "Referer": referer,
		"Sec-CH-UA": p.SecCHUA(), "Sec-CH-UA-Mobile": p.SecCHUAMobile(),
		"Sec-CH-UA-Platform": p.SecCHUAPlatform(), "Sec-Fetch-Dest": destination,
		"Sec-Fetch-Mode": mode, "Sec-Fetch-Site": "cross-site", "Priority": "u=1, i",
		"User-Agent": p.UserAgent,
	}
	if includeOrigin {
		headers["Origin"] = origin
	}
	return headers
}

type profileRandom struct{ entropy runtimekit.Entropy }

func (r *profileRandom) index(length int) (int, error) {
	if length < 1 {
		return 0, errors.New("cannot choose from empty set")
	}
	value, err := r.entropy.Uint64n(uint64(length))
	return int(value), err
}

func (r *profileRandom) integer(minimum, maximum int) (int, error) {
	value, err := r.entropy.Uint64n(uint64(maximum - minimum + 1))
	return minimum + int(value), err
}

func (r *profileRandom) uniform(minimum, maximum float64) (float64, error) {
	value, err := r.entropy.Uint64n(1 << 53)
	if err != nil {
		return 0, err
	}
	return minimum + (maximum-minimum)*float64(value)/float64(uint64(1)<<53), nil
}

func choose[T any](random *profileRandom, values []T) (T, error) {
	var zero T
	index, err := random.index(len(values))
	if err != nil {
		return zero, err
	}
	return values[index], nil
}

func buildBrands(random *profileRandom, chromeMajor, chromeFull string) ([]UABrand, []UABrand, error) {
	greaseBrand, err := choose(random, greaseBrands)
	if err != nil {
		return nil, nil, err
	}
	greaseVersion, err := choose(random, greaseVersions)
	if err != nil {
		return nil, nil, err
	}
	type entry struct{ brand, short, full string }
	entries := []entry{{greaseBrand, greaseVersion, greaseVersion + ".0.0.0"}, {"Chromium", chromeMajor, chromeFull}, {"Google Chrome", chromeMajor, chromeFull}}
	for index := len(entries) - 1; index > 0; index-- {
		chosen, chooseErr := random.integer(0, index)
		if chooseErr != nil {
			return nil, nil, chooseErr
		}
		entries[index], entries[chosen] = entries[chosen], entries[index]
	}
	brands := make([]UABrand, len(entries))
	full := make([]UABrand, len(entries))
	for index, value := range entries {
		brands[index] = UABrand{Brand: value.brand, Version: value.short}
		full[index] = UABrand{Brand: value.brand, Version: value.full}
	}
	return brands, full, nil
}

func buildScreen(random *profileRandom, familyValue family) (ScreenProfile, error) {
	choice, err := choose(random, familyValue.screens)
	if err != nil {
		return ScreenProfile{}, err
	}
	chromeHeight, err := random.integer(112, 190)
	if err != nil {
		return ScreenProfile{}, err
	}
	return ScreenProfile{
		Width: choice.width, Height: choice.height, AvailWidth: choice.width, AvailHeight: choice.height,
		ColorDepth: 24, OrientationType: "portrait-primary", DevicePixelRatio: choice.ratio,
		InnerWidth: choice.width, InnerHeight: max(400, choice.height-chromeHeight),
		OuterWidth: choice.width, OuterHeight: choice.height,
	}, nil
}
