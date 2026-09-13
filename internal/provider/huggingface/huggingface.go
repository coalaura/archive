package huggingface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/coalaura/archive/internal/provider"
)

const (
	defaultEndpoint = "https://huggingface.co"
	providerName    = "huggingface"
	userAgent       = "coalaura/archive"
)

type Provider struct {
	client   *http.Client
	endpoint string
	token    string
}

type modelInfo struct {
	SHA string `json:"sha"`
}

type treeEntry struct {
	Type string   `json:"type"`
	Path string   `json:"path"`
	Size int64    `json:"size"`
	OID  string   `json:"oid"`
	LFS  *lfsInfo `json:"lfs"`
}

type lfsInfo struct {
	SHA256 string `json:"sha256"`
	OID    string `json:"oid"`
	Size   int64  `json:"size"`
}

func New(token string) *Provider {
	return &Provider{
		client:   http.DefaultClient,
		endpoint: defaultEndpoint,
		token:    strings.TrimSpace(token),
	}
}

func (huggingFace *Provider) Name() string {
	return providerName
}

func (huggingFace *Provider) Resolve(ctx context.Context, reference string, revision string) (*provider.Snapshot, error) {
	reference = strings.TrimSpace(reference)
	revision = strings.TrimSpace(revision)

	if reference == "" {
		return nil, errors.New("repository cannot be empty")
	}

	if revision == "" {
		return nil, errors.New("revision cannot be empty")
	}

	resolvedRevision, err := huggingFace.resolveRevision(ctx, reference, revision)
	if err != nil {
		return nil, err
	}

	files, err := huggingFace.listFiles(ctx, reference, resolvedRevision)
	if err != nil {
		return nil, err
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

func (huggingFace *Provider) Open(ctx context.Context, snapshot *provider.Snapshot, file provider.File) (io.ReadCloser, error) {
	downloadURL := huggingFace.fileURL(snapshot.Reference, snapshot.ResolvedRevision, file.Path)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create download request: %w", err)
	}

	huggingFace.applyHeaders(request)
	request.Header.Set("Accept-Encoding", "identity")

	response, err := huggingFace.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", file.Path, err)
	}

	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()

		message := readErrorBody(response.Body)

		if message == "" {
			return nil, fmt.Errorf("download %s: %s", file.Path, response.Status)
		}

		return nil, fmt.Errorf("download %s: %s: %s", file.Path, response.Status, message)
	}

	if response.ContentLength >= 0 && response.ContentLength != file.Size {
		response.Body.Close()

		return nil, fmt.Errorf(
			"download %s: expected %d bytes, server reported %d",
			file.Path,
			file.Size,
			response.ContentLength,
		)
	}

	return response.Body, nil
}

func (huggingFace *Provider) resolveRevision(ctx context.Context, reference string, revision string) (string, error) {
	requestURL := fmt.Sprintf(
		"%s/api/models/%s/revision/%s",
		huggingFace.endpoint,
		escapePath(reference),
		url.PathEscape(revision),
	)

	var info modelInfo

	err := huggingFace.getJSON(ctx, requestURL, &info)
	if err != nil {
		return "", fmt.Errorf("resolve %s@%s: %w", reference, revision, err)
	}

	if info.SHA == "" {
		return "", fmt.Errorf("resolve %s@%s: Hugging Face returned an empty commit SHA", reference, revision)
	}

	return info.SHA, nil
}

func (huggingFace *Provider) listFiles(ctx context.Context, reference string, revision string) ([]provider.File, error) {
	requestURL := fmt.Sprintf(
		"%s/api/models/%s/tree/%s?recursive=true&expand=false",
		huggingFace.endpoint,
		escapePath(reference),
		url.PathEscape(revision),
	)

	var files []provider.File

	for requestURL != "" {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
		if err != nil {
			return nil, fmt.Errorf("create tree request: %w", err)
		}

		huggingFace.applyHeaders(request)

		response, err := huggingFace.client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("list repository files: %w", err)
		}

		if response.StatusCode != http.StatusOK {
			message := readErrorBody(response.Body)
			response.Body.Close()

			if message == "" {
				return nil, fmt.Errorf("list repository files: %s", response.Status)
			}

			return nil, fmt.Errorf("list repository files: %s: %s", response.Status, message)
		}

		var entries []treeEntry

		err = json.NewDecoder(response.Body).Decode(&entries)
		response.Body.Close()

		if err != nil {
			return nil, fmt.Errorf("decode repository tree: %w", err)
		}

		for _, entry := range entries {
			if entry.Type != "file" {
				continue
			}

			err = validateFilePath(entry.Path)
			if err != nil {
				return nil, err
			}

			sourceSHA256 := ""

			if entry.LFS != nil {
				sourceSHA256 = entry.LFS.SHA256

				if sourceSHA256 == "" && len(entry.LFS.OID) == 64 {
					sourceSHA256 = entry.LFS.OID
				}
			}

			files = append(files, provider.File{
				Path:         entry.Path,
				Size:         entry.Size,
				BlobID:       entry.OID,
				SourceSHA256: sourceSHA256,
			})
		}

		requestURL = nextPageURL(requestURL, response.Header.Get("Link"))
	}

	return files, nil
}

func (huggingFace *Provider) getJSON(ctx context.Context, requestURL string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	huggingFace.applyHeaders(request)

	response, err := huggingFace.client.Do(request)
	if err != nil {
		return err
	}

	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		message := readErrorBody(response.Body)

		if message == "" {
			return errors.New(response.Status)
		}

		return fmt.Errorf("%s: %s", response.Status, message)
	}

	err = json.NewDecoder(response.Body).Decode(target)
	if err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	return nil
}

func (huggingFace *Provider) applyHeaders(request *http.Request) {
	request.Header.Set("User-Agent", userAgent)

	if huggingFace.token != "" {
		request.Header.Set("Authorization", "Bearer "+huggingFace.token)
	}
}

func (huggingFace *Provider) fileURL(reference string, revision string, filePath string) string {
	return fmt.Sprintf(
		"%s/%s/resolve/%s/%s",
		huggingFace.endpoint,
		escapePath(reference),
		url.PathEscape(revision),
		escapePath(filePath),
	)
}

func escapePath(path string) string {
	parts := strings.Split(path, "/")

	for index, part := range parts {
		parts[index] = url.PathEscape(part)
	}

	return strings.Join(parts, "/")
}

func validateFilePath(path string) error {
	if path == "" {
		return errors.New("empty file path returned by Hugging Face")
	}

	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, "\\") {
		return fmt.Errorf("absolute file path returned by Hugging Face: %q", path)
	}

	if slices.Contains(strings.Split(strings.ReplaceAll(path, "\\", "/"), "/"), "..") {
		return fmt.Errorf("unsafe file path returned by Hugging Face: %q", path)
	}

	return nil
}

func nextPageURL(currentURL string, link string) string {
	for part := range strings.SplitSeq(link, ",") {
		part = strings.TrimSpace(part)

		if !strings.Contains(part, `rel="next"`) {
			continue
		}

		left := strings.IndexByte(part, '<')
		right := strings.IndexByte(part, '>')

		if left < 0 || right <= left {
			continue
		}

		nextURL := part[left+1 : right]

		parsedNextURL, err := url.Parse(nextURL)
		if err != nil {
			return ""
		}

		if parsedNextURL.IsAbs() {
			return parsedNextURL.String()
		}

		baseURL, err := url.Parse(currentURL)
		if err != nil {
			return ""
		}

		return baseURL.ResolveReference(parsedNextURL).String()
	}

	return ""
}

func readErrorBody(reader io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(reader, 8<<10))
	if err != nil {
		return ""
	}

	message := strings.TrimSpace(string(data))
	if len(message) > 512 {
		message = message[:512]
	}

	quoted, err := strconv.Unquote(message)
	if err == nil {
		return quoted
	}

	return message
}
