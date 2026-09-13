package git

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coalaura/archive/internal/provider"
)

func TestProvider(t *testing.T) {
	_, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is not installed")
	}

	repositoryDirectory := t.TempDir()

	runTestGit(t, "", "init", "--quiet", "--initial-branch=main", repositoryDirectory)
	runTestGit(t, "", "-C", repositoryDirectory, "config", "user.name", "Archive Test")
	runTestGit(t, "", "-C", repositoryDirectory, "config", "user.email", "archive@example.com")

	readmePath := filepath.Join(repositoryDirectory, "README.md")

	err = os.WriteFile(readmePath, []byte("repository contents\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(repositoryDirectory, "script.sh")

	err = os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 0\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	runTestGit(t, "", "-C", repositoryDirectory, "add", "README.md", "script.sh")
	runTestGit(t, "", "-C", repositoryDirectory, "update-index", "--chmod=+x", "script.sh")

	linkTarget := "README.md"

	linkBlob := runTestGit(t, linkTarget, "-C", repositoryDirectory, "hash-object", "-w", "--stdin")

	runTestGit(t, "", "-C", repositoryDirectory, "update-index", "--add", "--cacheinfo", "120000,"+linkBlob+",readme-link")
	runTestGit(t, "", "-C", repositoryDirectory, "commit", "--quiet", "-m", "initial")

	outputDirectory := t.TempDir()
	gitProvider := New(outputDirectory)

	snapshot, err := gitProvider.Resolve(context.Background(), repositoryDirectory, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	if snapshot.Provider != providerName {
		t.Fatalf("unexpected provider: %s", snapshot.Provider)
	}

	if snapshot.ResolvedRevision == "" || snapshot.ResolvedRevision == "HEAD" {
		t.Fatalf("unexpected resolved revision: %q", snapshot.ResolvedRevision)
	}

	if len(snapshot.Files) != 3 {
		t.Fatalf("unexpected file count: %d", len(snapshot.Files))
	}

	assertGitFile(t, gitProvider, snapshot, snapshot.Files[0], "README.md", provider.FileTypeRegular, 0o644, "repository contents\n")
	assertGitFile(t, gitProvider, snapshot, snapshot.Files[1], "readme-link", provider.FileTypeSymlink, 0o777, linkTarget)
	assertGitFile(t, gitProvider, snapshot, snapshot.Files[2], "script.sh", provider.FileTypeRegular, 0o755, "#!/bin/sh\nexit 0\n")

	temporaryDirectory := gitProvider.repositoryDirectory
	expectedPartialDirectory := filepath.Join(outputDirectory, partialDirectoryName)

	if filepath.Dir(temporaryDirectory) != expectedPartialDirectory {
		t.Fatalf("temporary repository is outside partial directory: %s", temporaryDirectory)
	}

	err = gitProvider.Close()
	if err != nil {
		t.Fatal(err)
	}

	_, err = os.Stat(temporaryDirectory)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary repository still exists: %v", err)
	}

	partialEntries, err := os.ReadDir(expectedPartialDirectory)
	if err != nil {
		t.Fatal(err)
	}

	if len(partialEntries) != 0 {
		t.Fatalf("partial directory is not empty after cleanup: %v", partialEntries)
	}
}

func assertGitFile(t *testing.T, gitProvider *Provider, snapshot *provider.Snapshot, file provider.File, path string, fileType provider.FileType, mode int64, contents string) {
	t.Helper()

	if file.Path != path || file.Type != fileType || file.Mode != mode {
		t.Fatalf("unexpected file metadata: %+v", file)
	}

	if file.Type == provider.FileTypeSymlink {
		if file.LinkTarget != contents {
			t.Fatalf("unexpected link target for %s: %q", file.Path, file.LinkTarget)
		}

		return
	}

	reader, err := gitProvider.Open(context.Background(), snapshot, file)
	if err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		reader.Close()

		t.Fatal(err)
	}

	err = reader.Close()
	if err != nil {
		t.Fatal(err)
	}

	if string(data) != contents {
		t.Fatalf("unexpected contents for %s: %q", file.Path, data)
	}
}

func runTestGit(t *testing.T, standardInput string, arguments ...string) string {
	t.Helper()

	command := exec.Command("git", arguments...)

	if standardInput != "" {
		command.Stdin = io.NopCloser(strings.NewReader(standardInput))
	}

	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}

	return strings.TrimSpace(string(output))
}
