package bootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
	"github.com/d2120848471/v3/ali-slider-go/internal/infrastructure/artifact"
)

func TestServiceArtifactPurge(t *testing.T) {
	options := DefaultOptions()
	options.DevicePrewarmCapacity = 0
	options.ArtifactDir = filepath.Join(t.TempDir(), "artifacts")
	application, shared, err := assemble(options)
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	identifier, err := shared.artifacts.SaveFailure(nil, nil, artifact.Metrics{Reason: "fixture", Stage: "test"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(options.ArtifactDir, identifier+"-metrics.json")
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := application.PurgeArtifacts()
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
}

func TestFailureRecorderPreservesSampleFields(t *testing.T) {
	options := DefaultOptions()
	options.ArtifactDir = filepath.Join(t.TempDir(), "artifacts")
	application, shared, err := assemble(options)
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	sample := solve.FailureSample{
		Background: []byte("background fixture"), Shadow: []byte("shadow fixture"),
		Reason: "vision", Stage: "vision", Confidence: 0.25, XPos: 31, SlidePos: 18,
		TimingsMS: map[string]int{"total": 27},
	}
	if err := (failureRecorder{store: shared.artifacts}).SaveFailure(sample); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(options.ArtifactDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("artifact files=%d want=3", len(files))
	}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(options.ArtifactDir, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Ext(file.Name()) != ".json" {
			continue
		}
		var metrics artifact.Metrics
		if err := json.Unmarshal(data, &metrics); err != nil {
			t.Fatal(err)
		}
		if metrics.RecordedAt == "" || metrics.Reason != sample.Reason || metrics.Stage != sample.Stage || metrics.Confidence != sample.Confidence || metrics.XPos != sample.XPos || metrics.SlidePos != sample.SlidePos || metrics.TimingsMS["total"] != 27 {
			t.Fatalf("metrics=%+v", metrics)
		}
	}
}
