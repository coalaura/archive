package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/coalaura/archive/internal/provider"
)

const (
	providerName         = "git"
	partialDirectoryName = ".partial"
	regularMode          = int64(0o644)
	executableMode       = int64(0o755)
)

type Provider struct {
	partialDirectory    string
	repositoryDirectory string
}

func (gitProvider *Provider) Name() string {
	return providerName
}

func (gitProvider *Provider) Resolve(ctx context.Context, reference string, revision string) (*provider.Snapshot, error) {
	reference = strings.TrimSpace(reference)
	revision = strings.TrimSpace(revision)

	if reference == "" {
		return nil, errors.New("repository cannot be empty")
	}

	if revision == "" {
		return nil, errors.New("revision cannot be empty")
	}

	err := gitProvider.Close()
	if err != nil {
		return nil, fmt.Errorf("clean previous Git repository: %w", err)
	}

	err = os.MkdirAll(gitProvider.partialDirectory, 0o755)
	if err != nil {
		return nil, fmt.Errorf("create partial directory: %w", err)
	}

	repositoryDirectory, err := os.MkdirTemp(gitProvider.partialDirectory, "git-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary Git repository: %w", err)
	}

	gitProvider.repositoryDirectory = repositoryDirectory

	_, err = runGit(ctx, "init", "--bare", "--quiet", repositoryDirectory)
	if err != nil {
		resolveErr := fmt.Errorf("initialize temporary Git repository: %w", err)

		return nil, gitProvider.closeAfterError(resolveErr)
	}

	_, err = runGit(
		ctx,
		"-C", repositoryDirectory,
		"fetch", "--quiet", "--depth=1", "--no-tags", "--no-recurse-submodules", "--", reference, revision,
	)

	if err != nil {
		resolveErr := fmt.Errorf("fetch %s@%s: %w", reference, revision, err)

		return nil, gitProvider.closeAfterError(resolveErr)
	}

	resolvedRevisionData, err := runGit(ctx, "-C", repositoryDirectory, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		resolveErr := fmt.Errorf("resolve %s@%s: %w", reference, revision, err)

		return nil, gitProvider.closeAfterError(resolveErr)
	}

	resolvedRevision := strings.TrimSpace(string(resolvedRevisionData))
	if resolvedRevision == "" {
		resolveErr := fmt.Errorf("resolve %s@%s: Git returned an empty commit ID", reference, revision)

		return nil, gitProvider.closeAfterError(resolveErr)
	}

	files, err := gitProvider.listFiles(ctx, resolvedRevision)
	if err != nil {
		return nil, gitProvider.closeAfterError(err)
	}

	sort.Slice(files, func(left int, right int) bool {
		return files[left].Path < files[right].Path
	})

	return &provider.Snapshot{
		Provider:          providerName,
		Reference:         reference,
		RequestedRevision: revision,
		ResolvedRevision:  resolvedRevision,
		Files:             files,
	}, nil
}

func (gitProvider *Provider) Open(ctx context.Context, _ *provider.Snapshot, file provider.File) (io.ReadCloser, error) {
	if gitProvider.repositoryDirectory == "" {
		return nil, errors.New("git repository is not resolved")
	}

	if file.BlobID == "" {
		return nil, fmt.Errorf("open %s: Git blob ID is empty", file.Path)
	}

	command := gitCommand(ctx, "-C", gitProvider.repositoryDirectory, "cat-file", "blob", file.BlobID)
	reader, writer := io.Pipe()

	var standardError bytes.Buffer

	command.Stdout = writer
	command.Stderr = &standardError

	err := command.Start()
	if err != nil {
		reader.Close()
		writer.Close()

		return nil, fmt.Errorf("open Git blob for %s: %w", file.Path, err)
	}

	go func() {
		waitErr := command.Wait()
		if waitErr != nil {
			waitErr = gitError(waitErr, standardError.Bytes())
			waitErr = fmt.Errorf("read Git blob for %s: %w", file.Path, waitErr)
		}

		writer.CloseWithError(waitErr)
	}()

	return reader, nil
}

func (gitProvider *Provider) Close() error {
	if gitProvider.repositoryDirectory == "" {
		return nil
	}

	err := os.RemoveAll(gitProvider.repositoryDirectory)
	if err != nil {
		return err
	}

	gitProvider.repositoryDirectory = ""

	return nil
}

func (gitProvider *Provider) closeAfterError(resolveErr error) error {
	closeErr := gitProvider.Close()
	if closeErr != nil {
		return errors.Join(resolveErr, fmt.Errorf("remove temporary Git repository: %w", closeErr))
	}

	return resolveErr
}

func (gitProvider *Provider) listFiles(ctx context.Context, revision string) ([]provider.File, error) {
	output, err := runGit(ctx, "-C", gitProvider.repositoryDirectory, "ls-tree", "-r", "-z", "--long", revision)
	if err != nil {
		return nil, fmt.Errorf("list repository files: %w", err)
	}

	entries := bytes.Split(output, []byte{0})
	files := make([]provider.File, 0, len(entries)-1)

	for _, entry := range entries {
		if len(entry) == 0 {
			continue
		}

		file, include, parseErr := gitProvider.parseTreeEntry(ctx, entry)
		if parseErr != nil {
			return nil, parseErr
		}

		if include {
			files = append(files, file)
		}
	}

	return files, nil
}

func (gitProvider *Provider) parseTreeEntry(ctx context.Context, entry []byte) (provider.File, bool, error) {
	header, filePath, found := bytes.Cut(entry, []byte{'\t'})
	if !found {
		return provider.File{}, false, fmt.Errorf("parse Git tree entry %q: missing path", entry)
	}

	fields := bytes.Fields(header)
	if len(fields) != 4 {
		return provider.File{}, false, fmt.Errorf("parse Git tree entry for %q: unexpected metadata", filePath)
	}

	objectType := string(fields[1])
	if objectType == "commit" {
		return provider.File{}, false, nil
	}

	if objectType != "blob" {
		return provider.File{}, false, fmt.Errorf("parse Git tree entry for %q: unsupported object type %q", filePath, objectType)
	}

	path := string(filePath)

	err := validateFilePath(path)
	if err != nil {
		return provider.File{}, false, err
	}

	size, err := strconv.ParseInt(string(fields[3]), 10, 64)
	if err != nil || size < 0 {
		return provider.File{}, false, fmt.Errorf("parse Git tree entry for %q: invalid size %q", path, fields[3])
	}

	file := provider.File{
		Path:   path,
		Size:   size,
		Mode:   regularMode,
		Type:   provider.FileTypeRegular,
		BlobID: string(fields[2]),
	}

	switch string(fields[0]) {
	case "100644":
	case "100755":
		file.Mode = executableMode
	case "120000":
		linkTarget, readErr := runGit(ctx, "-C", gitProvider.repositoryDirectory, "cat-file", "blob", file.BlobID)
		if readErr != nil {
			return provider.File{}, false, fmt.Errorf("read symlink target for %s: %w", path, readErr)
		}

		if int64(len(linkTarget)) != size {
			return provider.File{}, false, fmt.Errorf("read symlink target for %s: expected %d bytes, received %d", path, size, len(linkTarget))
		}

		if bytes.IndexByte(linkTarget, 0) >= 0 {
			return provider.File{}, false, fmt.Errorf("read symlink target for %s: target contains a null byte", path)
		}

		file.Mode = 0o777
		file.Type = provider.FileTypeSymlink
		file.LinkTarget = string(linkTarget)
	default:
		return provider.File{}, false, fmt.Errorf("parse Git tree entry for %q: unsupported mode %q", path, fields[0])
	}

	return file, true, nil
}

func New(outputDirectory string) *Provider {
	return &Provider{
		partialDirectory: filepath.Join(outputDirectory, partialDirectoryName),
	}
}

func runGit(ctx context.Context, arguments ...string) ([]byte, error) {
	command := gitCommand(ctx, arguments...)

	output, err := command.Output()
	if err != nil {
		exitError, ok := errors.AsType[*exec.ExitError](err)
		if ok {
			err = gitError(err, exitError.Stderr)
		}

		return nil, err
	}

	return output, nil
}

func gitCommand(ctx context.Context, arguments ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=Never")

	return command
}

func gitError(err error, standardError []byte) error {
	message := strings.TrimSpace(string(standardError))
	if message == "" {
		return err
	}

	return fmt.Errorf("%w: %s", err, message)
}

func validateFilePath(path string) error {
	if path == "" {
		return errors.New("empty file path returned by Git")
	}

	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, "\\") {
		return fmt.Errorf("absolute file path returned by Git: %q", path)
	}

	parts := strings.Split(strings.ReplaceAll(path, "\\", "/"), "/")
	if slices.Contains(parts, "..") {
		return fmt.Errorf("unsafe file path returned by Git: %q", path)
	}

	return nil
}
