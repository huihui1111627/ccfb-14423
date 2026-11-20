// Package repo implements a content-addressable image repository with
// blobs, manifests and a simulated range-request remote.
package repo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Descriptor references a blob by digest.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Manifest is an OCI-style image manifest.
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// ImageConfig is the OCI config subset we use.
type ImageConfig struct {
	Architecture string    `json:"architecture"`
	OS           string    `json:"os"`
	Config       RunConfig `json:"config"`
	RootFS       RootFS    `json:"rootfs"`
}

// RunConfig is the process configuration.
type RunConfig struct {
	Env        []string `json:"Env,omitempty"`
	Cmd        []string `json:"Cmd,omitempty"`
	WorkingDir string   `json:"WorkingDir,omitempty"`
}

// RootFS holds layer diff IDs in application order.
type RootFS struct {
	Type    string   `json:"type"`
	DiffIDs []string `json:"diff_ids"`
}

// Repo is an on-disk blob store.
type Repo struct {
	Root string
	mu   sync.Mutex
}

// New creates or opens a repository at root.
func New(root string) (*Repo, error) {
	for _, d := range []string{"blobs/sha256", "manifests", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Repo{Root: root}, nil
}

func (r *Repo) blobPath(hexDigest string) string {
	return filepath.Join(r.Root, "blobs", "sha256", hexDigest)
}

// PutBlob stores data and returns its digest.
func (r *Repo) PutBlob(data []byte) (string, error) {
	h := sha256.Sum256(data)
	hexd := hex.EncodeToString(h[:])
	dg := "sha256:" + hexd
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.blobPath(hexd)
	if _, err := os.Stat(p); err == nil {
		return dg, nil
	}
	tmp := filepath.Join(r.Root, "tmp", hexd+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, p); err != nil {
		return "", err
	}
	return dg, nil
}

// PutBlobFromFile moves a file into the store. src must live on the same
// filesystem or be plain data.
func (r *Repo) PutBlobFromFile(src string) (string, int64, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	hexd := hex.EncodeToString(h.Sum(nil))
	dg := "sha256:" + hexd
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.blobPath(hexd)
	if _, err := os.Stat(p); err == nil {
		return dg, n, nil
	}
	if err := copyFile(src, p); err != nil {
		return "", 0, err
	}
	return dg, n, nil
}

// GetBlob returns a reader over a blob.
func (r *Repo) GetBlob(digest string) (io.ReadCloser, int64, error) {
	hexd, err := hexOf(digest)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(r.blobPath(hexd))
	if err != nil {
		return nil, 0, fmt.Errorf("blob %s: %w", digest, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

// GetBlobBytes reads a whole blob.
func (r *Repo) GetBlobBytes(digest string) ([]byte, error) {
	rc, _, err := r.GetBlob(digest)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// HasBlob reports whether a blob exists.
func (r *Repo) HasBlob(digest string) bool {
	hexd, err := hexOf(digest)
	if err != nil {
		return false
	}
	_, err = os.Stat(r.blobPath(hexd))
	return err == nil
}

// BlobStat returns size and whether the blob exists.
func (r *Repo) BlobStat(digest string) (int64, bool) {
	hexd, err := hexOf(digest)
	if err != nil {
		return 0, false
	}
	fi, err := os.Stat(r.blobPath(hexd))
	if err != nil {
		return 0, false
	}
	return fi.Size(), true
}

// PutManifest stores a manifest under its digest.
func (r *Repo) PutManifest(data []byte) (string, error) {
	dg, err := r.PutBlob(data)
	if err != nil {
		return "", err
	}
	hexd, _ := hexOf(dg)
	if err := os.MkdirAll(filepath.Join(r.Root, "manifests"), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(r.Root, "manifests", hexd+".json"), data, 0o644); err != nil {
		return "", err
	}
	return dg, nil
}

// GetManifestBytes reads a manifest blob.
func (r *Repo) GetManifestBytes(digest string) ([]byte, error) {
	hexd, err := hexOf(digest)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(r.Root, "manifests", hexd+".json"))
	if err == nil {
		return b, nil
	}
	return r.GetBlobBytes(digest)
}

// ListManifests returns the digests of all stored manifests sorted by name.
func (r *Repo) ListManifests() ([]string, error) {
	ents, err := os.ReadDir(filepath.Join(r.Root, "manifests"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			out = append(out, "sha256:"+strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	sort.Strings(out)
	return out, nil
}

// TagLink points a friendly name at a manifest digest.
type TagLink struct {
	Name     string    `json:"name"`
	Digest   string    `json:"digest"`
	Modified time.Time `json:"modified"`
}

// SetTag creates or replaces a tag.
func (r *Repo) SetTag(name, digest string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	dir := filepath.Join(r.Root, "manifests", "tags")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(TagLink{Name: name, Digest: digest, Modified: time.Now().UTC()})
	return os.WriteFile(filepath.Join(dir, safeTag(name)+".json"), b, 0o644)
}

// ResolveTag returns the digest for a tag.
func (r *Repo) ResolveTag(name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(r.Root, "manifests", "tags", safeTag(name)+".json"))
	if err != nil {
		return "", fmt.Errorf("tag %q not found", name)
	}
	var t TagLink
	if err := json.Unmarshal(b, &t); err != nil {
		return "", err
	}
	return t.Digest, nil
}

// ListTags returns all tags.
func (r *Repo) ListTags() ([]TagLink, error) {
	dir := filepath.Join(r.Root, "manifests", "tags")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []TagLink
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var t TagLink
		if json.Unmarshal(b, &t) == nil {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// IsTag reports whether a reference is a tag (not a digest).
func IsTag(ref string) bool { return !strings.HasPrefix(ref, "sha256:") }

func safeTag(name string) string {
	return strings.NewReplacer("/", "_", ":", "_").Replace(name)
}

func hexOf(digest string) (string, error) {
	hexd := digest
	if strings.HasPrefix(hexd, "sha256:") {
		hexd = hexd[len("sha256:"):]
	}
	if len(hexd) != 64 {
		return "", fmt.Errorf("invalid digest %q", digest)
	}
	return hexd, nil
}

// HexOf exposes digest normalization.
func HexOf(d string) (string, error) { return hexOf(d) }

// NewRemote builds a simulated remote over the repository's blobs.
func (r *Repo) NewRemote() *Remote { return &Remote{repo: r} }

// Remote serves blob ranges like a registry/CDN would, with per-request
// bookkeeping so tests can assert on range coalescing.
type Remote struct {
	repo *Repo
	mu   sync.Mutex
	log  []RangeRequest
	// Latency is added per request to simulate satellite links.
	Latency time.Duration
	// Bandwidth caps the simulated link in bytes per second (0 = unlimited).
	Bandwidth int64
	// Corrupt, when set, mutates returned bytes for tests.
	Corrupt func(digest string, off int64, data []byte) []byte
	// ForcedErr, when set, makes matching requests fail.
	ForcedErr func(digest string, off int64) error
	// LogHook is called after each request is recorded (for dashboards).
	LogHook func(RangeRequest)
}

// RangeRequest records one HTTP-style range request.
type RangeRequest struct {
	At     time.Time `json:"at"`
	Digest string    `json:"digest"`
	Off    int64     `json:"off"`
	Len    int64     `json:"len"`
	Bytes  int64     `json:"bytes"`
	Note   string    `json:"note,omitempty"`
}

// FetchRange returns bytes [off, off+length) of a blob.
func (rm *Remote) FetchRange(digest string, off, length int64) ([]byte, error) {
	if rm.ForcedErr != nil {
		if err := rm.ForcedErr(digest, off); err != nil {
			return nil, err
		}
	}
	rc, size, err := rm.repo.GetBlob(digest)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if off < 0 || length < 0 || off > size {
		return nil, fmt.Errorf("range [%d,%d) out of bounds for %s", off, off+length, digest)
	}
	if off+length > size {
		length = size - off
	}
	buf := make([]byte, length)
	n, err := io.ReadFull(io.NewSectionReader(rc.(io.ReaderAt), off, length), buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	buf = buf[:n]
	if rm.Latency > 0 || rm.Bandwidth > 0 {
		wait := rm.Latency
		if rm.Bandwidth > 0 {
			wait += time.Duration(float64(len(buf)) / float64(rm.Bandwidth) * float64(time.Second))
		}
		time.Sleep(wait)
	}
	if rm.Corrupt != nil {
		buf = rm.Corrupt(digest, off, buf)
	}
	rr := RangeRequest{At: time.Now(), Digest: digest, Off: off, Len: length, Bytes: int64(len(buf))}
	rm.mu.Lock()
	rm.log = append(rm.log, rr)
	hook := rm.LogHook
	rm.mu.Unlock()
	if hook != nil {
		hook(rr)
	}
	return buf, nil
}

// Stat returns blob size.
func (rm *Remote) Stat(digest string) (int64, error) {
	sz, ok := rm.repo.BlobStat(digest)
	if !ok {
		return 0, fmt.Errorf("remote: blob %s not found", digest)
	}
	return sz, nil
}

// Requests returns a copy of the request log.
func (rm *Remote) Requests() []RangeRequest {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	out := make([]RangeRequest, len(rm.log))
	copy(out, rm.log)
	return out
}

// ResetLog clears the request log.
func (rm *Remote) ResetLog() {
	rm.mu.Lock()
	rm.log = nil
	rm.mu.Unlock()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// BytesReaderAt adapts a byte slice for RangeAt-style use.
type BytesReaderAt []byte

// ReadAt implements io.ReaderAt.
func (b BytesReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// CorruptBlob rewrites bytes of a stored blob in place, simulating a damaged
// or malicious server. Used by tests.
func (r *Repo) CorruptBlob(digest string, off int64, mutate func([]byte)) error {
	hexd, err := hexOf(digest)
	if err != nil {
		return err
	}
	p := r.blobPath(hexd)
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if off < 0 || off >= int64(len(b)) {
		return fmt.Errorf("corrupt offset %d out of range", off)
	}
	mutate(b[off:])
	return os.WriteFile(p, b, 0o644)
}

// EncodeJSON marshals v without HTML escaping.
func EncodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}
