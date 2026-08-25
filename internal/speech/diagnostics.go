package speech

import (
	"fmt"
	"path/filepath"
)

// RuntimeDiagnostic describes the optional speech runtime without starting a
// worker, opening an audio device, or making a network request.
type RuntimeDiagnostic struct {
	Provider     ProviderName
	Status       string
	Dependencies []DependencyDiagnostic
}

type DependencyDiagnostic struct {
	Name           string
	ConfiguredPath string
	ResolvedPath   string
	Status         string
	Detail         string
}

func DiagnoseRuntime(config Config) RuntimeDiagnostic {
	config = config.WithDefaults()
	result := RuntimeDiagnostic{Provider: config.Provider, Status: "ready"}
	if config.Provider == ProviderKokoro {
		root, err := defaultKokoroRoot()
		if err != nil {
			result.Status = "unavailable"
			result.Dependencies = append(result.Dependencies, DependencyDiagnostic{Name: "kokoro_data_root", Status: "unavailable", Detail: err.Error()})
			return result
		}
		python := config.PythonBinary
		if python == "" {
			python = filepath.Join(root, "venv", "bin", "python")
		}
		model := config.ModelPath
		if model == "" {
			model = filepath.Join(root, "kokoro-v1.0.onnx")
		}
		voices := config.VoicesPath
		if voices == "" {
			voices = filepath.Join(root, "voices-v1.0.bin")
		}
		player := config.PlayerBinary
		if player == "" {
			player = "mpv"
		}
		result.Dependencies = append(result.Dependencies,
			diagnoseExecutableDependency("python", python, nil),
			diagnoseFileDependency("model", model),
			diagnoseFileDependency("voices", voices),
			diagnoseExecutableDependency("player", player, nil),
		)
	} else {
		playback := config.PlaybackBinary
		var playbackFallback func() (string, error)
		if playback == "" {
			playback = "edge-playback"
			playbackFallback = func() (string, error) { return resolveUserLocalExecutable("edge-playback") }
		}
		result.Dependencies = append(result.Dependencies,
			diagnoseExecutableDependency("edge_playback", playback, playbackFallback),
			diagnoseExecutableDependency("edge_tts", "edge-tts", func() (string, error) { return resolveUserLocalExecutable("edge-tts") }),
			diagnoseExecutableDependency("player", "mpv", nil),
		)
	}
	for _, dependency := range result.Dependencies {
		if dependency.Status != "found" {
			result.Status = "unavailable"
			break
		}
	}
	return result
}

func diagnoseExecutableDependency(name, configured string, fallback func() (string, error)) DependencyDiagnostic {
	result := DependencyDiagnostic{Name: name, ConfiguredPath: configured, Status: "found"}
	resolved, err := resolveExecutable(configured)
	if err != nil && fallback != nil {
		resolved, err = fallback()
	}
	if err != nil {
		result.Status = "unavailable"
		result.Detail = err.Error()
		return result
	}
	result.ResolvedPath = resolved
	return result
}

func diagnoseFileDependency(name, configured string) DependencyDiagnostic {
	result := DependencyDiagnostic{Name: name, ConfiguredPath: configured, Status: "found"}
	resolved, err := resolveRegularFile(configured)
	if err != nil {
		result.Status = "unavailable"
		result.Detail = err.Error()
		return result
	}
	result.ResolvedPath = resolved
	return result
}

func (d RuntimeDiagnostic) UnavailableSummary() string {
	for _, dependency := range d.Dependencies {
		if dependency.Status != "found" {
			return fmt.Sprintf("%s is unavailable: %s", dependency.Name, dependency.Detail)
		}
	}
	return ""
}
