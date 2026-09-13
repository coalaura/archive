package archiver

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/coalaura/archive/internal/manifest"
	"github.com/coalaura/archive/internal/provider"
	"github.com/coalaura/plain"
	"github.com/klauspost/compress/zstd"
)

const (
	bytesPerUnit       = int64(1024)
	defaultRetries     = 3
	manifestPath       = ".archive/manifest.json"
	repositoryRoot     = "repository"
	shortRevisionWidth = 12
)

type Archiver struct {
	logger      *plain.Plain
	retries     int
	compression zstd.EncoderLevel
}

func New(logger *plain.Plain) *Archiver {
	return &Archiver{
		logger:      logger,
		retries:     defaultRetries,
		compression: zstd.SpeedBetterCompression,
	}
}

func (archiver *Archiver) Archive(ctx context.Context, source provider.Provider, reference string, revision string, outputDirectory string) (string, error) {
	err := os.MkdirAll(outputDirectory, 0o755)
	if err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}

	progressPath := statePath(outputDirectory, reference, revision)

	progress, err := loadState(progressPath)
	if err != nil {
		return "", err
	}

	resolveRevision := revision

	if progress != nil {
		err = validateState(progress, source.Name(), reference, revision)
		if err != nil {
			return "", err
		}

		resolveRevision = progress.ResolvedRevision

		archiver.logger.Printf("Resuming %s@%s\n", reference, shortRevision(progress.ResolvedRevision))
	} else {
		archiver.logger.Printf("Resolving %s@%s\n", reference, revision)
	}

	snapshot, err := source.Resolve(ctx, reference, resolveRevision)
	if err != nil {
		return "", err
	}

	snapshot.RequestedRevision = revision

	if progress != nil && snapshot.ResolvedRevision != progress.ResolvedRevision {
		return "", fmt.Errorf(
			"saved state resolved to %s, provider resolved to %s",
			progress.ResolvedRevision,
			snapshot.ResolvedRevision,
		)
	}

	if progress != nil {
		err = validateSnapshotState(progress, snapshot)
		if err != nil {
			return "", err
		}
	}

	if progress == nil {
		progress = newState(snapshot)
	}

	finalPath := filepath.Join(outputDirectory, progress.FinalName)
	partialPath := finalPath + ".partial"

	if fileExists(finalPath) {
		removeErr := os.Remove(progressPath)
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			archiver.logger.Warnf("Could not remove stale state: %v\n", removeErr)
		}

		archiver.logger.Printf("Already archived: %s\n", finalPath)

		return finalPath, nil
	}

	output, err := archiver.openPartial(partialPath, progress)
	if err != nil {
		return "", err
	}

	defer output.Close()

	if progress.NextFile == 0 {
		err = saveState(progressPath, progress)
		if err != nil {
			return "", err
		}
	}

	for index := progress.NextFile; index < len(snapshot.Files); index++ {
		file := snapshot.Files[index]

		archiver.logger.Printf(
			"[%d/%d] %s (%s)\n",
			index+1,
			len(snapshot.Files),
			file.Path,
			formatBytes(file.Size),
		)

		archivedFile, archiveErr := archiver.archiveFile(ctx, source, snapshot, file, output)
		if archiveErr != nil {
			return "", archiveErr
		}

		err = output.Sync()
		if err != nil {
			return "", fmt.Errorf("sync archive: %w", err)
		}

		offset, seekErr := output.Seek(0, io.SeekCurrent)
		if seekErr != nil {
			return "", fmt.Errorf("read archive offset: %w", seekErr)
		}

		progress.Files = append(progress.Files, archivedFile)
		progress.NextFile = index + 1
		progress.ArchiveOffset = offset

		err = saveState(progressPath, progress)
		if err != nil {
			return "", err
		}
	}

	archiveManifest := manifest.New(snapshot, progress.Files)

	manifestData, err := json.MarshalIndent(archiveManifest, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode manifest: %w", err)
	}

	manifestData = append(manifestData, '\n')

	err = archiver.writeFinalFrame(output, manifestPath, manifestData)
	if err != nil {
		return "", err
	}

	err = output.Sync()
	if err != nil {
		return "", fmt.Errorf("sync completed archive: %w", err)
	}

	err = output.Close()
	if err != nil {
		return "", fmt.Errorf("close completed archive: %w", err)
	}

	err = os.Rename(partialPath, finalPath)
	if err != nil {
		return "", fmt.Errorf("finalize archive: %w", err)
	}

	removeErr := os.Remove(progressPath)
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		archiver.logger.Warnf("Archive completed, but state cleanup failed: %v\n", removeErr)
	}

	archiver.logger.Printf("Archived to %s\n", finalPath)

	return finalPath, nil
}

func (archiver *Archiver) archiveFile(ctx context.Context, source provider.Provider, snapshot *provider.Snapshot, file provider.File, output *os.File) (manifest.File, error) {
	frameStart, err := output.Seek(0, io.SeekCurrent)
	if err != nil {
		return manifest.File{}, fmt.Errorf("read frame offset: %w", err)
	}

	var lastErr error

	for attempt := 1; attempt <= archiver.retries; attempt++ {
		if ctx.Err() != nil {
			return manifest.File{}, ctx.Err()
		}

		archivedFile, archiveErr := archiver.writeFileFrame(ctx, source, snapshot, file, output)
		if archiveErr == nil {
			return archivedFile, nil
		}

		lastErr = archiveErr

		truncateErr := output.Truncate(frameStart)
		if truncateErr != nil {
			return manifest.File{}, fmt.Errorf("rollback failed frame: %w", truncateErr)
		}

		_, seekErr := output.Seek(frameStart, io.SeekStart)
		if seekErr != nil {
			return manifest.File{}, fmt.Errorf("seek after rollback: %w", seekErr)
		}

		if attempt < archiver.retries {
			archiver.logger.Warnf(
				"Retrying %s after attempt %d/%d failed: %v\n",
				file.Path,
				attempt,
				archiver.retries,
				archiveErr,
			)
		}
	}

	return manifest.File{}, fmt.Errorf("archive %s after %d attempts: %w", file.Path, archiver.retries, lastErr)
}

func (archiver *Archiver) writeFileFrame(ctx context.Context, source provider.Provider, snapshot *provider.Snapshot, file provider.File, output io.Writer) (manifest.File, error) {
	reader, err := source.Open(ctx, snapshot, file)
	if err != nil {
		return manifest.File{}, err
	}

	defer reader.Close()

	encoder, err := archiver.newEncoder(output)
	if err != nil {
		return manifest.File{}, err
	}

	archiveWriter := tar.NewWriter(encoder)

	header := &tar.Header{
		Name:    repositoryRoot + "/" + file.Path,
		Mode:    0o644,
		Size:    file.Size,
		ModTime: time.Unix(0, 0).UTC(),
		Format:  tar.FormatPAX,
	}

	err = archiveWriter.WriteHeader(header)
	if err != nil {
		encoder.Close()

		return manifest.File{}, fmt.Errorf("write tar header for %s: %w", file.Path, err)
	}

	hasher := sha256.New()
	stream := io.TeeReader(reader, hasher)

	written, copyErr := io.Copy(archiveWriter, stream)
	if copyErr != nil {
		encoder.Close()

		return manifest.File{}, fmt.Errorf("stream %s: %w", file.Path, copyErr)
	}

	if written != file.Size {
		encoder.Close()

		return manifest.File{}, fmt.Errorf("stream %s: expected %d bytes, received %d", file.Path, file.Size, written)
	}

	err = archiveWriter.Flush()
	if err != nil {
		encoder.Close()

		return manifest.File{}, fmt.Errorf("flush tar entry for %s: %w", file.Path, err)
	}

	err = encoder.Close()
	if err != nil {
		return manifest.File{}, fmt.Errorf("finish zstd frame for %s: %w", file.Path, err)
	}

	sha256Sum := hex.EncodeToString(hasher.Sum(nil))

	if file.SourceSHA256 != "" && !strings.EqualFold(file.SourceSHA256, sha256Sum) {
		return manifest.File{}, fmt.Errorf(
			"verify %s: SHA-256 mismatch: expected %s, received %s",
			file.Path,
			file.SourceSHA256,
			sha256Sum,
		)
	}

	return manifest.File{
		Path:         file.Path,
		Size:         file.Size,
		SHA256:       sha256Sum,
		SourceSHA256: file.SourceSHA256,
		BlobID:       file.BlobID,
	}, nil
}

func (archiver *Archiver) writeFinalFrame(output io.Writer, name string, data []byte) error {
	encoder, err := archiver.newEncoder(output)
	if err != nil {
		return err
	}

	archiveWriter := tar.NewWriter(encoder)

	header := &tar.Header{
		Name:    name,
		Mode:    0o644,
		Size:    int64(len(data)),
		ModTime: time.Unix(0, 0).UTC(),
		Format:  tar.FormatPAX,
	}

	err = archiveWriter.WriteHeader(header)
	if err != nil {
		encoder.Close()

		return fmt.Errorf("write manifest header: %w", err)
	}

	_, err = archiveWriter.Write(data)
	if err != nil {
		encoder.Close()

		return fmt.Errorf("write manifest: %w", err)
	}

	err = archiveWriter.Close()
	if err != nil {
		encoder.Close()

		return fmt.Errorf("finish tar archive: %w", err)
	}

	err = encoder.Close()
	if err != nil {
		return fmt.Errorf("finish manifest zstd frame: %w", err)
	}

	return nil
}

func (archiver *Archiver) newEncoder(output io.Writer) (*zstd.Encoder, error) {
	encoder, err := zstd.NewWriter(
		output,
		zstd.WithEncoderLevel(archiver.compression),
		zstd.WithEncoderCRC(true),
		zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)),
		zstd.WithConcurrentBlocks(true),
	)

	if err != nil {
		return nil, fmt.Errorf("create zstd encoder: %w", err)
	}

	return encoder, nil
}

func (archiver *Archiver) openPartial(path string, progress *state) (*os.File, error) {
	if progress.NextFile == 0 {
		if fileExists(path) {
			archiver.logger.Warnf("Discarding orphaned partial archive %s\n", path)

			err := os.Remove(path)
			if err != nil {
				return nil, fmt.Errorf("remove orphaned partial archive: %w", err)
			}
		}

		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
		if err != nil {
			return nil, fmt.Errorf("create partial archive: %w", err)
		}

		return file, nil
	}

	file, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open partial archive for resume: %w", err)
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()

		return nil, fmt.Errorf("inspect partial archive: %w", err)
	}

	if info.Size() < progress.ArchiveOffset {
		file.Close()

		return nil, fmt.Errorf(
			"partial archive is shorter than its saved checkpoint: %d < %d",
			info.Size(),
			progress.ArchiveOffset,
		)
	}

	if info.Size() != progress.ArchiveOffset {
		err = file.Truncate(progress.ArchiveOffset)
		if err != nil {
			file.Close()

			return nil, fmt.Errorf("truncate partial archive to checkpoint: %w", err)
		}
	}

	_, err = file.Seek(progress.ArchiveOffset, io.SeekStart)
	if err != nil {
		file.Close()

		return nil, fmt.Errorf("seek partial archive to checkpoint: %w", err)
	}

	return file, nil
}

func newState(snapshot *provider.Snapshot) *state {
	return &state{
		Version:           stateVersion,
		Provider:          snapshot.Provider,
		Reference:         snapshot.Reference,
		RequestedRevision: snapshot.RequestedRevision,
		ResolvedRevision:  snapshot.ResolvedRevision,
		FinalName:         archiveName(snapshot.Reference, snapshot.ResolvedRevision),
		Files:             make([]manifest.File, 0, len(snapshot.Files)),
	}
}

func validateSnapshotState(progress *state, snapshot *provider.Snapshot) error {
	if progress.NextFile > len(snapshot.Files) {
		return fmt.Errorf(
			"saved state contains %d completed files, but the pinned snapshot contains only %d",
			progress.NextFile,
			len(snapshot.Files),
		)
	}

	for index, archivedFile := range progress.Files {
		snapshotFile := snapshot.Files[index]

		if archivedFile.Path != snapshotFile.Path || archivedFile.Size != snapshotFile.Size {
			return fmt.Errorf("saved state no longer matches pinned snapshot at file %d", index+1)
		}

		if archivedFile.SourceSHA256 != "" && snapshotFile.SourceSHA256 != "" &&
			!strings.EqualFold(archivedFile.SourceSHA256, snapshotFile.SourceSHA256) {
			return fmt.Errorf("saved state no longer matches pinned snapshot at %s", archivedFile.Path)
		}
	}

	return nil
}

func validateState(progress *state, providerName string, reference string, revision string) error {
	if progress.Provider != providerName {
		return fmt.Errorf("saved state belongs to provider %q", progress.Provider)
	}

	if progress.Reference != reference {
		return fmt.Errorf("saved state belongs to reference %q", progress.Reference)
	}

	if progress.RequestedRevision != revision {
		return fmt.Errorf("saved state belongs to revision %q", progress.RequestedRevision)
	}

	if progress.ResolvedRevision == "" || progress.FinalName == "" {
		return errors.New("saved state is incomplete")
	}

	if progress.NextFile < 0 || progress.NextFile != len(progress.Files) {
		return errors.New("saved state has an invalid completed-file count")
	}

	if progress.ArchiveOffset < 0 {
		return errors.New("saved state has an invalid archive offset")
	}

	return nil
}

func archiveName(reference string, revision string) string {
	return fmt.Sprintf(
		"%s@%s.tar.zst",
		safeFileName(reference),
		shortRevision(revision),
	)
}

func safeFileName(value string) string {
	var builder strings.Builder

	for _, character := range value {
		switch {
		case unicode.IsLetter(character), unicode.IsDigit(character):
			builder.WriteRune(character)
		case character == '-', character == '_', character == '.', character == '@':
			builder.WriteRune(character)
		default:
			builder.WriteByte('_')
		}
	}

	name := strings.Trim(builder.String(), "._")
	if name == "" {
		return "archive"
	}

	return name
}

func shortRevision(revision string) string {
	if len(revision) <= shortRevisionWidth {
		return revision
	}

	return revision[:shortRevisionWidth]
}

func formatBytes(size int64) string {
	if size < bytesPerUnit {
		return fmt.Sprintf("%d B", size)
	}

	divisor := bytesPerUnit
	exponent := 0

	for value := size / bytesPerUnit; value >= bytesPerUnit && exponent < 5; value /= bytesPerUnit {
		divisor *= bytesPerUnit
		exponent++
	}

	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(divisor), "KMGTPE"[exponent])
}

func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}
