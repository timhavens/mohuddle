// Package releasepack builds and validates MoHuddle release archives.
package releasepack

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

const (
	applicationName      = "mohuddle"
	checksumsName        = "checksums.txt"
	buildVersionVariable = "github.com/timhavens/mohuddle/internal/buildinfo.Version"
)

var (
	stableVersionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	snapshotPattern      = regexp.MustCompile(`^(?:snapshot-)?[0-9a-fA-F]{7,40}$`)
	installCandidates    = []string{
		"INSTALL.md",
		"INSTALLATION.md",
		filepath.Join("docs", "install.md"),
		filepath.Join("docs", "installation.md"),
		filepath.Join("docs", "prerequisites.md"),
	}
)

// Target identifies a release operating system and architecture.
type Target struct {
	GOOS   string
	GOARCH string
}

// Targets is the complete supported release artifact matrix.
var Targets = []Target{
	{GOOS: "linux", GOARCH: "amd64"},
	{GOOS: "linux", GOARCH: "arm64"},
	{GOOS: "darwin", GOARCH: "amd64"},
	{GOOS: "darwin", GOARCH: "arm64"},
	{GOOS: "windows", GOARCH: "amd64"},
	{GOOS: "windows", GOARCH: "arm64"},
}

// BuildRequest describes one cross-platform Go build.
type BuildRequest struct {
	Root    string
	Output  string
	Version string
	Target  Target
}

// Options control packaging or validation.
type Options struct {
	Root      string
	OutputDir string
	Release   string
	DryRun    bool
}

// Result describes a validated artifact set.
type Result struct {
	Release   string
	Snapshot  bool
	OutputDir string
	Archives  []string
	DryRun    bool
}

// Packager allows tests to replace compilation and executable inspection.
type Packager struct {
	Build      func(context.Context, BuildRequest) error
	RunVersion func(context.Context, string) (string, error)
}

// New returns a production release packager.
func New() *Packager {
	return &Packager{
		Build:      buildGoBinary,
		RunVersion: runBinaryVersion,
	}
}

// Package builds, archives, checksums, and validates all release targets.
func (p *Packager) Package(ctx context.Context, opts Options) (Result, error) {
	opts, snapshot, err := normalizeOptions(opts)
	if err != nil {
		return Result{}, err
	}
	if p == nil || p.Build == nil || p.RunVersion == nil {
		return Result{}, errors.New("release packager requires build and version runners")
	}

	workRoot, err := os.MkdirTemp("", "mohuddle-release-")
	if err != nil {
		return Result{}, fmt.Errorf("create release workspace: %w", err)
	}
	defer os.RemoveAll(workRoot)

	artifactDir := filepath.Join(workRoot, "artifacts")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create artifact directory: %w", err)
	}

	documents, err := releaseDocuments(opts.Root)
	if err != nil {
		return Result{}, err
	}
	archives := make([]string, 0, len(Targets))
	for _, target := range Targets {
		base := archiveBase(opts.Release, target)
		stageDir := filepath.Join(workRoot, "stage", base)
		if err := os.MkdirAll(stageDir, 0o755); err != nil {
			return Result{}, fmt.Errorf("create stage for %s/%s: %w", target.GOOS, target.GOARCH, err)
		}
		executable := applicationName
		if target.GOOS == "windows" {
			executable += ".exe"
		}
		binaryPath := filepath.Join(stageDir, executable)
		if err := p.Build(ctx, BuildRequest{
			Root: opts.Root, Output: binaryPath, Version: opts.Release, Target: target,
		}); err != nil {
			return Result{}, fmt.Errorf("build %s/%s: %w", target.GOOS, target.GOARCH, err)
		}
		if err := os.Chmod(binaryPath, 0o755); err != nil {
			return Result{}, fmt.Errorf("set executable mode for %s/%s: %w", target.GOOS, target.GOARCH, err)
		}
		for _, document := range documents {
			destination := filepath.Join(stageDir, filepath.FromSlash(document.archivePath))
			if err := copyFile(document.sourcePath, destination, 0o644); err != nil {
				return Result{}, err
			}
		}

		archiveName := archiveFilename(opts.Release, target)
		archivePath := filepath.Join(artifactDir, archiveName)
		if target.GOOS == "windows" {
			err = writeZip(archivePath, filepath.Dir(stageDir), base)
		} else {
			err = writeTarGz(archivePath, filepath.Dir(stageDir), base)
		}
		if err != nil {
			return Result{}, fmt.Errorf("archive %s/%s: %w", target.GOOS, target.GOARCH, err)
		}
		archives = append(archives, archiveName)
	}
	sort.Strings(archives)
	if err := writeChecksums(artifactDir, archives); err != nil {
		return Result{}, err
	}
	if err := p.validate(ctx, opts.Root, artifactDir, opts.Release); err != nil {
		return Result{}, err
	}

	result := Result{
		Release: opts.Release, Snapshot: snapshot, Archives: archives, DryRun: opts.DryRun,
	}
	if opts.DryRun {
		return result, nil
	}
	if err := installArtifactSet(artifactDir, opts.OutputDir); err != nil {
		return Result{}, err
	}
	result.OutputDir = opts.OutputDir
	return result, nil
}

// Validate verifies an existing complete artifact set without publishing it.
func (p *Packager) Validate(ctx context.Context, opts Options) error {
	opts, _, err := normalizeOptions(opts)
	if err != nil {
		return err
	}
	if p == nil || p.RunVersion == nil {
		return errors.New("release packager requires a version runner")
	}
	return p.validate(ctx, opts.Root, opts.OutputDir, opts.Release)
}

func normalizeOptions(opts Options) (Options, bool, error) {
	if strings.TrimSpace(opts.Root) == "" {
		opts.Root = "."
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return Options{}, false, fmt.Errorf("resolve repository root: %w", err)
	}
	opts.Root = filepath.Clean(root)
	if strings.TrimSpace(opts.OutputDir) == "" {
		opts.OutputDir = filepath.Join(opts.Root, "dist")
	} else if !filepath.IsAbs(opts.OutputDir) {
		opts.OutputDir = filepath.Join(opts.Root, opts.OutputDir)
	}
	opts.OutputDir = filepath.Clean(opts.OutputDir)
	opts.Release = strings.TrimSpace(opts.Release)
	snapshot := snapshotPattern.MatchString(opts.Release)
	if !snapshot && !stableVersionPattern.MatchString(opts.Release) {
		return Options{}, false, fmt.Errorf("release %q must be a semantic version (for example v1.2.3) or a 7-40 character hexadecimal commit SHA optionally prefixed by snapshot-", opts.Release)
	}
	return opts, snapshot, nil
}

type releaseDocument struct {
	sourcePath  string
	archivePath string
}

func releaseDocuments(root string) ([]releaseDocument, error) {
	required := []string{"README.md"}
	if _, err := os.Stat(filepath.Join(root, "LICENSE")); err == nil {
		required = append(required, "LICENSE")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect LICENSE: %w", err)
	}
	for _, candidate := range installCandidates {
		if _, err := os.Stat(filepath.Join(root, candidate)); err == nil {
			required = append(required, candidate)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect %s: %w", candidate, err)
		}
	}
	documents := make([]releaseDocument, 0, len(required))
	for _, relative := range required {
		source := filepath.Join(root, relative)
		info, err := os.Stat(source)
		if err != nil {
			return nil, fmt.Errorf("release document %s: %w", relative, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("release document %s is not a regular file", relative)
		}
		documents = append(documents, releaseDocument{
			sourcePath: source, archivePath: filepath.ToSlash(relative),
		})
	}
	return documents, nil
}

func archiveBase(release string, target Target) string {
	return fmt.Sprintf("%s_%s_%s_%s", applicationName, release, target.GOOS, target.GOARCH)
}

func archiveFilename(release string, target Target) string {
	name := archiveBase(release, target)
	if target.GOOS == "windows" {
		return name + ".zip"
	}
	return name + ".tar.gz"
}

func buildGoBinary(ctx context.Context, request BuildRequest) error {
	command := exec.CommandContext(ctx, "go", buildGoArguments(request)...)
	command.Dir = request.Root
	command.Env = environmentWith(os.Environ(), map[string]string{
		"CGO_ENABLED": "0",
		"GOOS":        request.Target.GOOS,
		"GOARCH":      request.Target.GOARCH,
	})
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func buildGoArguments(request BuildRequest) []string {
	return []string{"build",
		"-trimpath",
		"-ldflags=-s -w -X " + buildVersionVariable + "=" + request.Version,
		"-o", request.Output,
		"./cmd/mohuddle",
	}
}

func environmentWith(base []string, replacements map[string]string) []string {
	result := make([]string, 0, len(base)+len(replacements))
	for _, value := range base {
		key, _, found := strings.Cut(value, "=")
		if found {
			if _, replaced := replacements[key]; replaced {
				continue
			}
		}
		result = append(result, value)
	}
	keys := make([]string, 0, len(replacements))
	for key := range replacements {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+replacements[key])
	}
	return result
}

func runBinaryVersion(ctx context.Context, executable string) (string, error) {
	command := exec.CommandContext(ctx, executable, "--version")
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("run %s --version: %w: %s", executable, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func copyFile(source, destination string, mode os.FileMode) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read %s: %w", source, err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", destination, err)
	}
	if err := os.WriteFile(destination, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", destination, err)
	}
	return nil
}

func writeTarGz(destination, sourceRoot, base string) error {
	file, err := os.Create(destination)
	if err != nil {
		return err
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	walkErr := filepath.Walk(filepath.Join(sourceRoot, base), func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relative)
		header.ModTime = header.ModTime.UTC()
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tarWriter, input)
		closeErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	closeTarErr := tarWriter.Close()
	closeGzipErr := gzipWriter.Close()
	closeFileErr := file.Close()
	return errors.Join(walkErr, closeTarErr, closeGzipErr, closeFileErr)
}

func writeZip(destination, sourceRoot, base string) error {
	file, err := os.Create(destination)
	if err != nil {
		return err
	}
	zipWriter := zip.NewWriter(file)
	walkErr := filepath.Walk(filepath.Join(sourceRoot, base), func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relative)
		header.Method = zip.Deflate
		entry, err := zipWriter.CreateHeader(header)
		if err != nil {
			return err
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(entry, input)
		closeErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	closeZipErr := zipWriter.Close()
	closeFileErr := file.Close()
	return errors.Join(walkErr, closeZipErr, closeFileErr)
}

func writeChecksums(directory string, archives []string) error {
	file, err := os.Create(filepath.Join(directory, checksumsName))
	if err != nil {
		return fmt.Errorf("create checksums: %w", err)
	}
	for _, name := range archives {
		digest, err := fileSHA256(filepath.Join(directory, name))
		if err != nil {
			_ = file.Close()
			return err
		}
		if _, err := fmt.Fprintf(file, "%s  %s\n", digest, name); err != nil {
			_ = file.Close()
			return fmt.Errorf("write checksums: %w", err)
		}
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close checksums: %w", err)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func installArtifactSet(source, destination string) error {
	destinationParent := filepath.Dir(destination)
	if err := os.MkdirAll(destinationParent, 0o755); err != nil {
		return fmt.Errorf("create artifact parent: %w", err)
	}
	sibling, err := os.MkdirTemp(destinationParent, ".mohuddle-artifacts-")
	if err != nil {
		return fmt.Errorf("create artifact staging directory: %w", err)
	}
	defer os.RemoveAll(sibling)
	sourceEntries, err := os.ReadDir(source)
	if err != nil {
		return fmt.Errorf("read completed artifact set: %w", err)
	}
	for _, entry := range sourceEntries {
		if !entry.Type().IsRegular() {
			return fmt.Errorf("completed artifact set contains non-file %s", entry.Name())
		}
		if err := copyFileSynced(filepath.Join(source, entry.Name()), filepath.Join(sibling, entry.Name()), 0o644); err != nil {
			return err
		}
	}

	if info, err := os.Stat(destination); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("artifact destination %s is not a directory", destination)
		}
		entries, err := os.ReadDir(destination)
		if err != nil {
			return fmt.Errorf("inspect artifact destination: %w", err)
		}
		if len(entries) != 0 {
			return fmt.Errorf("artifact destination %s must be empty", destination)
		}
		if err := os.Remove(destination); err != nil {
			return fmt.Errorf("prepare artifact destination: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect artifact destination: %w", err)
	}
	if err := os.Rename(sibling, destination); err != nil {
		return fmt.Errorf("install artifact set: %w", err)
	}
	return nil
}

func copyFileSynced(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open %s: %w", source, err)
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		_ = input.Close()
		return fmt.Errorf("create %s: %w", destination, err)
	}
	_, copyErr := io.Copy(output, input)
	syncErr := output.Sync()
	closeOutputErr := output.Close()
	closeInputErr := input.Close()
	if err := errors.Join(copyErr, syncErr, closeOutputErr, closeInputErr); err != nil {
		return fmt.Errorf("copy %s: %w", filepath.Base(source), err)
	}
	return nil
}

func (p *Packager) validate(ctx context.Context, root, directory, release string) error {
	documents, err := releaseDocuments(root)
	if err != nil {
		return err
	}
	expectedArchives := make(map[string]Target, len(Targets))
	for _, target := range Targets {
		expectedArchives[archiveFilename(release, target)] = target
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read artifact directory: %w", err)
	}
	if len(entries) != len(expectedArchives)+1 {
		return fmt.Errorf("artifact directory has %d entries; expected %d archives and %s", len(entries), len(expectedArchives), checksumsName)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("unexpected directory in artifact set: %s", entry.Name())
		}
		if entry.Name() == checksumsName {
			continue
		}
		target, ok := expectedArchives[entry.Name()]
		if !ok {
			return fmt.Errorf("unexpected artifact: %s", entry.Name())
		}
		if err := validateArchive(filepath.Join(directory, entry.Name()), release, target, documents); err != nil {
			return err
		}
	}
	if err := validateChecksums(directory, expectedArchives); err != nil {
		return err
	}
	native := Target{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	name := archiveFilename(release, native)
	if _, ok := expectedArchives[name]; ok {
		executable, cleanup, err := extractNativeExecutable(filepath.Join(directory, name), release, native)
		if err != nil {
			return err
		}
		defer cleanup()
		got, err := p.RunVersion(ctx, executable)
		if err != nil {
			return err
		}
		want := applicationName + " " + release
		if strings.TrimSpace(got) != want {
			return fmt.Errorf("native artifact version is %q; expected %q", strings.TrimSpace(got), want)
		}
	}
	return nil
}

func validateArchive(path, release string, target Target, documents []releaseDocument) error {
	base := archiveBase(release, target)
	executable := applicationName
	if target.GOOS == "windows" {
		executable += ".exe"
	}
	expected := map[string]bool{base + "/" + executable: false}
	for _, document := range documents {
		expected[base+"/"+document.archivePath] = false
	}
	var names []string
	var err error
	if target.GOOS == "windows" {
		names, err = zipEntryNames(path)
	} else {
		names, err = tarEntryNames(path)
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", filepath.Base(path), err)
	}
	for _, name := range names {
		if strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || strings.Contains(name, "../") {
			return fmt.Errorf("unsafe archive entry %q in %s", name, filepath.Base(path))
		}
		if _, ok := expected[name]; !ok {
			return fmt.Errorf("unexpected archive entry %q in %s", name, filepath.Base(path))
		}
		expected[name] = true
	}
	for name, found := range expected {
		if !found {
			return fmt.Errorf("archive %s is missing %s", filepath.Base(path), name)
		}
	}
	return nil
}

func tarEntryNames(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	var names []string
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("entry %s is not a regular file", header.Name)
		}
		names = append(names, header.Name)
	}
	return names, nil
}

func zipEntryNames(path string) ([]string, error) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	names := make([]string, 0, len(reader.File))
	for _, file := range reader.File {
		if !file.Mode().IsRegular() {
			return nil, fmt.Errorf("entry %s is not a regular file", file.Name)
		}
		names = append(names, file.Name)
	}
	return names, nil
}

func validateChecksums(directory string, expected map[string]Target) error {
	file, err := os.Open(filepath.Join(directory, checksumsName))
	if err != nil {
		return fmt.Errorf("open checksums: %w", err)
	}
	defer file.Close()
	found := make(map[string]bool, len(expected))
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			return fmt.Errorf("invalid checksum line %q", scanner.Text())
		}
		digest, name := strings.ToLower(fields[0]), fields[1]
		if _, ok := expected[name]; !ok {
			return fmt.Errorf("checksum references unexpected artifact %s", name)
		}
		if found[name] {
			return fmt.Errorf("duplicate checksum for %s", name)
		}
		actual, err := fileSHA256(filepath.Join(directory, name))
		if err != nil {
			return err
		}
		if digest != actual {
			return fmt.Errorf("checksum mismatch for %s", name)
		}
		found[name] = true
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read checksums: %w", err)
	}
	for name := range expected {
		if !found[name] {
			return fmt.Errorf("missing checksum for %s", name)
		}
	}
	return nil
}

func extractNativeExecutable(archivePath, release string, target Target) (string, func(), error) {
	temporary, err := os.MkdirTemp("", "mohuddle-version-check-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(temporary) }
	executableName := applicationName
	if target.GOOS == "windows" {
		executableName += ".exe"
	}
	wanted := archiveBase(release, target) + "/" + executableName
	destination := filepath.Join(temporary, executableName)
	var data []byte
	if target.GOOS == "windows" {
		reader, err := zip.OpenReader(archivePath)
		if err != nil {
			cleanup()
			return "", func() {}, err
		}
		for _, file := range reader.File {
			if file.Name != wanted {
				continue
			}
			input, openErr := file.Open()
			if openErr != nil {
				_ = reader.Close()
				cleanup()
				return "", func() {}, openErr
			}
			data, err = io.ReadAll(input)
			_ = input.Close()
			break
		}
		_ = reader.Close()
		if err != nil {
			cleanup()
			return "", func() {}, err
		}
	} else {
		file, err := os.Open(archivePath)
		if err != nil {
			cleanup()
			return "", func() {}, err
		}
		gzipReader, err := gzip.NewReader(file)
		if err != nil {
			_ = file.Close()
			cleanup()
			return "", func() {}, err
		}
		tarReader := tar.NewReader(gzipReader)
		for {
			header, nextErr := tarReader.Next()
			if errors.Is(nextErr, io.EOF) {
				break
			}
			if nextErr != nil {
				err = nextErr
				break
			}
			if header.Name == wanted {
				data, err = io.ReadAll(tarReader)
				break
			}
		}
		_ = gzipReader.Close()
		_ = file.Close()
		if err != nil {
			cleanup()
			return "", func() {}, err
		}
	}
	if len(data) == 0 {
		cleanup()
		return "", func() {}, fmt.Errorf("native executable missing from %s", filepath.Base(archivePath))
	}
	if err := os.WriteFile(destination, data, 0o755); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return destination, cleanup, nil
}
