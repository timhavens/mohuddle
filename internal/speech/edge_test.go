package speech

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEdgeProviderUsesSeparateArgumentsAndListsVoices(t *testing.T) {
	dir := t.TempDir()
	argumentsPath := filepath.Join(dir, "arguments.txt")
	injectedPath := filepath.Join(dir, "injected")
	playback := filepath.Join(dir, "edge-playback")
	edgeTTS := filepath.Join(dir, "edge-tts")
	mpv := filepath.Join(dir, "mpv")
	writeExecutable(t, playback, `#!/bin/sh
printf '<%s>\n' "$@" > "$MOHUDDLE_ARGUMENTS"
`)
	writeExecutable(t, edgeTTS, `#!/bin/sh
printf '%s\n' 'Name Gender ContentCategories VoicePersonalities'
printf '%s\n' '--------------------------------- -------- --------------------- --------------------------------------'
printf '%s\n' 'en-US-AndrewMultilingualNeural Male General Warm'
printf '%s\n' 'en-GB-SoniaNeural Female General Friendly'
`)
	writeExecutable(t, mpv, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", dir)
	t.Setenv("MOHUDDLE_ARGUMENTS", argumentsPath)

	provider := NewEdgeProvider(playback)
	if err := provider.Validate(); err != nil {
		t.Fatal(err)
	}
	payload := "hello; touch " + injectedPath
	if err := provider.Play(context.Background(), "en-US-AndrewMultilingualNeural", []string{"hello;", "touch " + injectedPath}); err != nil {
		t.Fatal(err)
	}
	arguments, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(arguments)
	for _, want := range []string{"<--voice>", "<en-US-AndrewMultilingualNeural>", "<--text>", "<" + payload + ">"} {
		if !strings.Contains(got, want) {
			t.Fatalf("arguments missing %q: %q", want, got)
		}
	}
	if _, err := os.Stat(injectedPath); !os.IsNotExist(err) {
		t.Fatalf("message text was interpreted by a shell: %v", err)
	}
	voices, err := provider.ListVoices(context.Background(), "andrew")
	if err != nil {
		t.Fatal(err)
	}
	if len(voices) != 1 || voices[0].Name != "en-US-AndrewMultilingualNeural" {
		t.Fatalf("voices=%+v", voices)
	}
}

func TestEdgeProviderFindsPythonUserInstall(t *testing.T) {
	home := t.TempDir()
	userBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(userBin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(userBin, "edge-playback"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(userBin, "edge-tts"), "#!/bin/sh\nexit 0\n")
	pathBin := t.TempDir()
	writeExecutable(t, filepath.Join(pathBin, "mpv"), "#!/bin/sh\nexit 0\n")
	t.Setenv("HOME", home)
	t.Setenv("PATH", pathBin)

	if err := NewEdgeProvider("").Validate(); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}
