package archiver

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/coalaura/archive/internal/manifest"
	"github.com/coalaura/archive/internal/provider"
	"github.com/coalaura/plain"
	"github.com/klauspost/compress/zstd"
)

type fakeProvider struct {
	files      map[string][]byte
	failPath   string
	openCounts map[string]int
}

type failingReader struct {
	err error
}

func (reader *failingReader) Read(_ []byte) (int, error) {
	return 0, reader.err
}

func (source *fakeProvider) Name() string {
	return "fake"
}

func (source *fakeProvider) Resolve(_ context.Context, reference string, revision string) (*provider.Snapshot, error) {
	files := make([]provider.File, 0, len(source.files))

	paths := []string{"config.json", "weights.bin"}

	for _, path := range paths {
		data := source.files[path]
		sum := sha256.Sum256(data)

		files = append(files, provider.File{
			Path:         path,
			Size:         int64(len(data)),
			SourceSHA256: hex.EncodeToString(sum[:]),
		})
	}

	return &provider.Snapshot{
		Provider:          source.Name(),
		Reference:         reference,
		RequestedRevision: revision,
		ResolvedRevision:  "0123456789abcdef0123456789abcdef01234567",
		Files:             files,
	}, nil
}

func (source *fakeProvider) Open(_ context.Context, _ *provider.Snapshot, file provider.File) (io.ReadCloser, error) {
	if source.openCounts == nil {
		source.openCounts = make(map[string]int)
	}

	source.openCounts[file.Path]++
	data := source.files[file.Path]

	if source.failPath == file.Path {
		middle := len(data) / 2
		reader := io.MultiReader(
			bytes.NewReader(data[:middle]),
			&failingReader{err: errors.New("simulated network failure")},
		)

		return io.NopCloser(reader), nil
	}

	return io.NopCloser(bytes.NewReader(data)), nil
}

func TestArchive(t *testing.T) {
	source := &fakeProvider{
		files: map[string][]byte{
			"config.json": []byte(`{"model":"test"}`),
			"weights.bin": []byte("some model weights"),
		},
	}

	logger := plain.New(plain.WithTarget(io.Discard))
	archiveWriter := New(logger)
	outputDirectory := t.TempDir()

	archivePath, err := archiveWriter.Archive(context.Background(), source, "owner/model", "main", outputDirectory)
	if err != nil {
		t.Fatal(err)
	}

	if filepath.Ext(archivePath) != ".zst" {
		t.Fatalf("unexpected archive path: %s", archivePath)
	}

	entries, err := readArchive(archivePath)
	if err != nil {
		t.Fatal(err)
	}

	if string(entries["repository/config.json"]) != `{"model":"test"}` {
		t.Fatalf("unexpected config.json: %q", entries["repository/config.json"])
	}

	if string(entries["repository/weights.bin"]) != "some model weights" {
		t.Fatalf("unexpected weights.bin: %q", entries["repository/weights.bin"])
	}

	var archiveManifest manifest.Manifest

	err = json.Unmarshal(entries[manifestPath], &archiveManifest)
	if err != nil {
		t.Fatal(err)
	}

	if archiveManifest.ResolvedRevision != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("unexpected resolved revision: %s", archiveManifest.ResolvedRevision)
	}

	if len(archiveManifest.Files) != 2 {
		t.Fatalf("unexpected manifest file count: %d", len(archiveManifest.Files))
	}
}

func TestArchiveResume(t *testing.T) {
	files := map[string][]byte{
		"config.json": []byte(`{"model":"test"}`),
		"weights.bin": []byte("some model weights that fail during the first run"),
	}

	logger := plain.New(plain.WithTarget(io.Discard))
	archiveWriter := New(logger)
	outputDirectory := t.TempDir()

	failingSource := &fakeProvider{
		files:    files,
		failPath: "weights.bin",
	}

	_, err := archiveWriter.Archive(context.Background(), failingSource, "owner/model", "main", outputDirectory)
	if err == nil {
		t.Fatal("expected the first archive attempt to fail")
	}

	if failingSource.openCounts["config.json"] != 1 {
		t.Fatalf("config.json was opened %d times", failingSource.openCounts["config.json"])
	}

	resumedSource := &fakeProvider{files: files}

	archivePath, err := archiveWriter.Archive(context.Background(), resumedSource, "owner/model", "main", outputDirectory)
	if err != nil {
		t.Fatal(err)
	}

	if resumedSource.openCounts["config.json"] != 0 {
		t.Fatalf("resume unexpectedly re-downloaded config.json %d times", resumedSource.openCounts["config.json"])
	}

	if resumedSource.openCounts["weights.bin"] != 1 {
		t.Fatalf("resume opened weights.bin %d times", resumedSource.openCounts["weights.bin"])
	}

	entries, err := readArchive(archivePath)
	if err != nil {
		t.Fatal(err)
	}

	if string(entries["repository/weights.bin"]) != string(files["weights.bin"]) {
		t.Fatal("resumed weights.bin does not match source")
	}
}

func readArchive(path string) (map[string][]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	defer file.Close()

	decoder, err := zstd.NewReader(file)
	if err != nil {
		return nil, err
	}

	defer decoder.Close()

	archiveReader := tar.NewReader(decoder)
	entries := make(map[string][]byte)

	for {
		header, nextErr := archiveReader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}

		if nextErr != nil {
			return nil, nextErr
		}

		data, readErr := io.ReadAll(archiveReader)
		if readErr != nil {
			return nil, readErr
		}

		entries[header.Name] = data
	}

	return entries, nil
}
