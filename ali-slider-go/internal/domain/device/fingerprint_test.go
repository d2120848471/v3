package device

import "testing"

func TestSessionProbePythonFixture(t *testing.T) {
	value, err := SessionProbe("offline-session-ABCDEFGH")
	if err != nil {
		t.Fatal(err)
	}
	if value != "LC1eMzMzYDU=" {
		t.Fatalf("probe=%q", value)
	}
}

func TestSeededFingerprintsAreStable(t *testing.T) {
	profile := Profile{
		CanvasSeed: "00112233445566778899aabbccddeeff",
		GPU:        GPUProfile{UnmaskedVendor: "Google Inc. (ARM)", UnmaskedRenderer: "ANGLE (ARM, Mali-G710 MC10, OpenGL ES 3.2)"},
		UABrands:   []UABrand{{Brand: "Chromium", Version: "150"}, {Brand: "Google Chrome", Version: "150"}},
	}
	leftCanvas, err := CanvasFingerprint(profile)
	if err != nil {
		t.Fatal(err)
	}
	rightCanvas, _ := CanvasFingerprint(profile)
	if leftCanvas != rightCanvas || len(leftCanvas) != 32 {
		t.Fatalf("canvas fingerprints %q / %q", leftCanvas, rightCanvas)
	}
	leftGPU, err := GPUFingerprint(profile)
	if err != nil {
		t.Fatal(err)
	}
	rightGPU, _ := GPUFingerprint(profile)
	if leftGPU != rightGPU || len(leftGPU) != 32 {
		t.Fatalf("GPU fingerprints %q / %q", leftGPU, rightGPU)
	}
	if BrandList(profile) != "[Chromium,Google Chrome]" {
		t.Fatalf("brand list %q", BrandList(profile))
	}
}

func TestValidateInteractionEvents(t *testing.T) {
	valid := []InteractionEvent{
		{Type: "mousemove", X: 1, Y: 2, TimeStamp: 3, IsTrusted: true},
		{Type: "mousemove", X: 2, Y: 3, TimeStamp: 3, IsTrusted: true},
	}
	if err := ValidateInteractionEvents(valid); err != nil {
		t.Fatal(err)
	}
	invalid := append([]InteractionEvent(nil), valid...)
	invalid[1].TimeStamp = 2
	if err := ValidateInteractionEvents(invalid); err == nil {
		t.Fatal("expected monotonic error")
	}
}
