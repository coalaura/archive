package archiver

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/coalaura/archive/internal/manifest"
)

const stateVersion = 1

type state struct {
	Version           int             `json:"version"`
	Provider          string          `json:"provider"`
	Reference         string          `json:"reference"`
	RequestedRevision string          `json:"requested_revision"`
	ResolvedRevision  string          `json:"resolved_revision"`
	FinalName         string          `json:"final_name"`
	NextFile          int             `json:"next_file"`
	ArchiveOffset     int64           `json:"archive_offset"`
	Files             []manifest.File `json:"files"`
}

func loadState(path string) (*state, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}

	var value state

	err = json.Unmarshal(data, &value)
	if err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}

	if value.Version != stateVersion {
		return nil, fmt.Errorf("unsupported state version %d", value.Version)
	}

	return &value, nil
}

func saveState(path string, value *state) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}

	data = append(data, '\n')
	temporaryPath := path + ".tmp"

	file, err := os.OpenFile(temporaryPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create state: %w", err)
	}

	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}

	closeErr := file.Close()

	if writeErr != nil {
		os.Remove(temporaryPath)

		return fmt.Errorf("write state: %w", writeErr)
	}

	if closeErr != nil {
		os.Remove(temporaryPath)

		return fmt.Errorf("close state: %w", closeErr)
	}

	err = replaceFile(temporaryPath, path)
	if err != nil {
		os.Remove(temporaryPath)

		return fmt.Errorf("replace state: %w", err)
	}

	return nil
}

func replaceFile(source string, destination string) error {
	err := os.Rename(source, destination)
	if err == nil {
		return nil
	}

	_, statErr := os.Stat(destination)
	if errors.Is(statErr, os.ErrNotExist) {
		return err
	}

	if statErr != nil {
		return err
	}

	removeErr := os.Remove(destination)
	if removeErr != nil {
		return err
	}

	return os.Rename(source, destination)
}

func statePath(outputDirectory string, reference string, revision string) string {
	name := "." + safeFileName(reference) + "@" + safeFileName(revision) + ".state"

	return filepath.Join(outputDirectory, name)
}
