package provider

import (
	"context"
	"io"
)

type File struct {
	Path         string
	Size         int64
	BlobID       string
	SourceSHA256 string
}

type Snapshot struct {
	Provider          string
	Reference         string
	RequestedRevision string
	ResolvedRevision  string
	Files             []File
}

type Provider interface {
	Name() string
	Resolve(ctx context.Context, reference string, revision string) (*Snapshot, error)
	Open(ctx context.Context, snapshot *Snapshot, file File) (io.ReadCloser, error)
}
