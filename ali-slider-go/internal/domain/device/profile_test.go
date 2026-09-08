package device

import (
	"strings"
	"sync"
	"testing"
)

type deterministicEntropy struct {
	mu   sync.Mutex
	next uint64
}

func (d *deterministicEntropy) Read(buffer []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for index := range buffer {
		buffer[index] = byte(d.next)
		d.next++
	}
	return len(buffer), nil
}

func (d *deterministicEntropy) Uint64n(limit uint64) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	value := d.next % limit
	d.next++
	return value, nil
}

func TestGenerateProfileIsSelfConsistent(t *testing.T) {
	profile, err := GenerateProfile(&deterministicEntropy{})
	if err != nil {
		t.Fatal(err)
	}
	if !profile.Mobile || profile.MaxTouchPoints != 5 || profile.PDFViewer {
		t.Fatalf("invalid mobile profile: %+v", profile)
	}
	if !strings.Contains(profile.UserAgent, profile.UAModel) || !strings.Contains(profile.UserAgent, "Mobile Safari") {
		t.Fatalf("UA/model mismatch: %s / %s", profile.UserAgent, profile.UAModel)
	}
	if profile.Family == "android-adreno" && !strings.Contains(profile.GPU.UnmaskedRenderer, "Adreno") {
		t.Fatal("Adreno family has non-Adreno GPU")
	}
	if profile.Family == "android-mali" && !strings.Contains(profile.GPU.UnmaskedRenderer, "Mali") {
		t.Fatal("Mali family has non-Mali GPU")
	}
	if profile.Screen.InnerHeight < 400 || profile.DeviceMemory > 8 || len(profile.ProfileID) != 16 || len(profile.CanvasSeed) != 32 {
		t.Fatalf("invalid derived values: %+v", profile)
	}
	if profile.SecCHUAMobile() != "?1" || !strings.Contains(profile.AcceptLanguage(), "zh-CN") {
		t.Fatalf("invalid headers: %s / %s", profile.SecCHUAMobile(), profile.AcceptLanguage())
	}
}

func TestGenerateProfileDeterministicWithInjectedEntropy(t *testing.T) {
	left, err := GenerateProfile(&deterministicEntropy{next: 7})
	if err != nil {
		t.Fatal(err)
	}
	right, err := GenerateProfile(&deterministicEntropy{next: 7})
	if err != nil {
		t.Fatal(err)
	}
	if left.ProfileID != right.ProfileID || left.UserAgent != right.UserAgent || left.CanvasSeed != right.CanvasSeed || left.TextMetricScale != right.TextMetricScale {
		t.Fatalf("profiles differ:\n%+v\n%+v", left, right)
	}
}

func TestProfileCloneDoesNotAliasSlices(t *testing.T) {
	original := Profile{
		UABrands:       []UABrand{{Brand: "brand", Version: "1"}},
		UAFullVersions: []UABrand{{Brand: "brand", Version: "1.2.3"}},
		Languages:      []string{"zh-CN", "zh"},
	}
	cloned := original.Clone()
	cloned.UABrands[0].Brand = "changed"
	cloned.UAFullVersions[0].Version = "changed"
	cloned.Languages[0] = "changed"
	if original.UABrands[0].Brand != "brand" || original.UAFullVersions[0].Version != "1.2.3" || original.Languages[0] != "zh-CN" {
		t.Fatalf("Clone mutated original profile: %+v", original)
	}
}
