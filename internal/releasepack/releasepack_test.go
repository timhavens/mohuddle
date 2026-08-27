package releasepack

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestPackageStableBuildsAndValidatesCompleteArtifactSet(t *testing.T) {
	root := releaseFixture(t, true)
	output := filepath.Join(t.TempDir(), "dist")
	var builds []BuildRequest
	packager := &Packager{
		Build: func(_ context.Context, request BuildRequest) error {
			builds = append(builds, request)
			return os.WriteFile(request.Output, []byte("binary "+request.Version), 0o755)
		},
		RunVersion: func(_ context.Context, executable string) (string, error) {
			data, err := os.ReadFile(executable)
			if err != nil {
				return "", err
			}
			version, found := strings.CutPrefix(string(data), "binary ")
			if !found {
				return "", fmt.Errorf("unexpected fake binary")
			}
			return "mohuddle " + version, nil
		},
	}

	result, err := packager.Package(context.Background(), Options{
		Root: root, OutputDir: output, Release: "v1.2.3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot || result.DryRun {
		t.Fatalf("unexpected result flags: %+v", result)
	}
	if result.OutputDir != output {
		t.Fatalf("output = %q, want %q", result.OutputDir, output)
	}
	if len(result.Archives) != len(Targets) {
		t.Fatalf("archives = %d, want %d", len(result.Archives), len(Targets))
	}
	if len(builds) != len(Targets) {
		t.Fatalf("builds = %d, want %d", len(builds), len(Targets))
	}
	for index, request := range builds {
		if request.Root != root || request.Version != "v1.2.3" || request.Target != Targets[index] {
			t.Fatalf("build %d = %+v", index, request)
		}
	}

	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	wantNames := append([]string(nil), result.Archives...)
	wantNames = append(wantNames, checksumsName)
	sort.Strings(wantNames)
	if strings.Join(names, "\n") != strings.Join(wantNames, "\n") {
		t.Fatalf("artifact names = %v, want %v", names, wantNames)
	}
	if err := packager.Validate(context.Background(), Options{
		Root: root, OutputDir: output, Release: "v1.2.3",
	}); err != nil {
		t.Fatalf("second validation failed: %v", err)
	}
}

func TestPackageSnapshotUsesCommitSHA(t *testing.T) {
	root := releaseFixture(t, false)
	output := filepath.Join(t.TempDir(), "dist")
	const commit = "snapshot-a1b2c3d4e5f6"
	packager := fakePackager()
	result, err := packager.Package(context.Background(), Options{
		Root: root, OutputDir: output, Release: commit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Snapshot {
		t.Fatal("snapshot release was not identified")
	}
	for _, name := range result.Archives {
		if !strings.Contains(name, "_"+commit+"_") {
			t.Fatalf("archive %q does not contain snapshot SHA", name)
		}
	}
}

func TestBuildGoArgumentsEmbedBuildInfoVersion(t *testing.T) {
	request := BuildRequest{Output: "mohuddle", Version: "abc123456789"}
	arguments := buildGoArguments(request)
	joined := strings.Join(arguments, " ")
	want := "-X " + buildVersionVariable + "=" + request.Version
	if !strings.Contains(joined, want) {
		t.Fatalf("build arguments %q do not contain %q", joined, want)
	}
	if strings.Contains(joined, "main.version") {
		t.Fatalf("build arguments still target obsolete main.version: %q", joined)
	}
}

func TestPackageDryRunLeavesNoArtifacts(t *testing.T) {
	root := releaseFixture(t, false)
	output := filepath.Join(t.TempDir(), "dist")
	result, err := fakePackager().Package(context.Background(), Options{
		Root: root, OutputDir: output, Release: "v2.0.0-rc.1", DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.DryRun || result.OutputDir != "" {
		t.Fatalf("result = %+v", result)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("dry run created output directory: %v", err)
	}
}

func TestValidateRejectsChecksumMismatch(t *testing.T) {
	root := releaseFixture(t, false)
	output := filepath.Join(t.TempDir(), "dist")
	packager := fakePackager()
	if _, err := packager.Package(context.Background(), Options{
		Root: root, OutputDir: output, Release: "v1.0.0",
	}); err != nil {
		t.Fatal(err)
	}
	checksums := filepath.Join(output, checksumsName)
	data, err := os.ReadFile(checksums)
	if err != nil {
		t.Fatal(err)
	}
	if data[0] == '0' {
		data[0] = '1'
	} else {
		data[0] = '0'
	}
	if err := os.WriteFile(checksums, data, 0o644); err != nil {
		t.Fatal(err)
	}
	err = packager.Validate(context.Background(), Options{
		Root: root, OutputDir: output, Release: "v1.0.0",
	})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("validation error = %v, want checksum mismatch", err)
	}
}

func TestPackageRejectsUnsafeReleaseAndNonEmptyDestination(t *testing.T) {
	root := releaseFixture(t, false)
	packager := fakePackager()
	for _, release := range []string{"", "latest", "../../v1.0.0", "abc123"} {
		t.Run(release, func(t *testing.T) {
			_, err := packager.Package(context.Background(), Options{
				Root: root, OutputDir: filepath.Join(t.TempDir(), "dist"), Release: release,
			})
			if err == nil {
				t.Fatalf("release %q unexpectedly accepted", release)
			}
		})
	}
	output := filepath.Join(t.TempDir(), "dist")
	if err := os.MkdirAll(output, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := packager.Package(context.Background(), Options{
		Root: root, OutputDir: output, Release: "v1.0.0",
	})
	if err == nil || !strings.Contains(err.Error(), "must be empty") {
		t.Fatalf("package error = %v, want non-empty destination rejection", err)
	}
	if _, err := os.Stat(filepath.Join(output, "keep.txt")); err != nil {
		t.Fatalf("existing destination content was changed: %v", err)
	}
}

func TestValidateRejectsWrongEmbeddedVersion(t *testing.T) {
	root := releaseFixture(t, false)
	output := filepath.Join(t.TempDir(), "dist")
	packager := fakePackager()
	if _, err := packager.Package(context.Background(), Options{
		Root: root, OutputDir: output, Release: "v1.0.0",
	}); err != nil {
		t.Fatal(err)
	}
	packager.RunVersion = func(context.Context, string) (string, error) {
		return "mohuddle v9.9.9", nil
	}
	err := packager.Validate(context.Background(), Options{
		Root: root, OutputDir: output, Release: "v1.0.0",
	})
	if err == nil || !strings.Contains(err.Error(), "native artifact version") {
		t.Fatalf("validation error = %v, want embedded version failure", err)
	}
}

func fakePackager() *Packager {
	return &Packager{
		Build: func(_ context.Context, request BuildRequest) error {
			return os.WriteFile(request.Output, []byte("binary "+request.Version), 0o755)
		},
		RunVersion: func(_ context.Context, executable string) (string, error) {
			data, err := os.ReadFile(executable)
			if err != nil {
				return "", err
			}
			version, found := strings.CutPrefix(string(data), "binary ")
			if !found {
				return "", fmt.Errorf("unexpected fake binary")
			}
			return "mohuddle " + version, nil
		},
	}
}

func releaseFixture(t *testing.T, withInstall bool) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"README.md": "# MoHuddle\n",
		"LICENSE":   "test license\n",
	}
	if withInstall {
		files[filepath.Join("docs", "installation.md")] = "# Installation\n"
	}
	for name, contents := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestTargetsCoverExpectedMatrix(t *testing.T) {
	want := []Target{
		{GOOS: "linux", GOARCH: "amd64"},
		{GOOS: "linux", GOARCH: "arm64"},
		{GOOS: "darwin", GOARCH: "amd64"},
		{GOOS: "darwin", GOARCH: "arm64"},
		{GOOS: "windows", GOARCH: "amd64"},
		{GOOS: "windows", GOARCH: "arm64"},
	}
	if fmt.Sprint(Targets) != fmt.Sprint(want) {
		t.Fatalf("targets = %v, want %v", Targets, want)
	}
	if runtime.GOOS == "js" {
		t.Skip("release packaging is not supported under js")
	}
}
