package manifest

import "github.com/coalaura/archive/internal/provider"

const Version = 1

type File struct {
	Path         string            `json:"path"`
	Size         int64             `json:"size"`
	Mode         int64             `json:"mode,omitempty"`
	Type         provider.FileType `json:"type,omitempty"`
	SHA256       string            `json:"sha256"`
	SourceSHA256 string            `json:"source_sha256,omitempty"`
	BlobID       string            `json:"blob_id,omitempty"`
}

type Manifest struct {
	Version           int    `json:"version"`
	Provider          string `json:"provider"`
	Reference         string `json:"reference"`
	RequestedRevision string `json:"requested_revision"`
	ResolvedRevision  string `json:"resolved_revision"`
	Files             []File `json:"files"`
}

func New(snapshot *provider.Snapshot, files []File) Manifest {
	return Manifest{
		Version:           Version,
		Provider:          snapshot.Provider,
		Reference:         snapshot.Reference,
		RequestedRevision: snapshot.RequestedRevision,
		ResolvedRevision:  snapshot.ResolvedRevision,
		Files:             files,
	}
}
