package speech

import (
	"path/filepath"
	"testing"
)

func TestDiagnoseRuntimeReportsEveryKokoroDependency(t *testing.T) {
	dir := t.TempDir()
	result := DiagnoseRuntime(Config{
		Provider:     ProviderKokoro,
		PythonBinary: filepath.Join(dir, "missing-python"),
		ModelPath:    filepath.Join(dir, "missing-model.onnx"),
		VoicesPath:   filepath.Join(dir, "missing-voices.bin"),
		PlayerBinary: filepath.Join(dir, "missing-player"),
	})
	if result.Status != "unavailable" || len(result.Dependencies) != 4 {
		t.Fatalf("diagnostic=%+v", result)
	}
	want := []string{"python", "model", "voices", "player"}
	for index, dependency := range result.Dependencies {
		if dependency.Name != want[index] || dependency.Status != "unavailable" || dependency.Detail == "" {
			t.Fatalf("dependency[%d]=%+v", index, dependency)
		}
	}
	if summary := result.UnavailableSummary(); summary == "" {
		t.Fatal("unavailable runtime has no summary")
	}
}
