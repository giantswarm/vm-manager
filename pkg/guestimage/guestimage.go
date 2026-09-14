// Package guestimage publishes the guest image directory `make -C images`
// builds as an OCI artifact and fetches it back into an image directory
// `vm-manager serve --image-dir` reads.
//
// The artifact is one OCI image manifest of ArtifactType whose layers are the
// files of the image directory, one layer per file, each named by its path
// relative to the directory (the org.opencontainers.image.title annotation):
// the UKI and disk of every image (<id>_<version>.efi and .raw, the
// .roothash and .repart.d/ next to them, a per-image .policy.json), the
// shared policy.json with the expected PCR values, and the sysupdate/ tree
// the guests update from. Nothing else of a build directory travels.
//
// Pull writes the files into the image directory and records what it fetched
// in MarkerFile; a pull of the same manifest digest is a no-op, so the
// policy.json `vm-manager image golden` amended afterwards survives pod
// restarts. A different digest replaces the directory's contents: the golden
// PCR values are the image's, not the directory's.
package guestimage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const (
	// ArtifactType identifies a guest image artifact; Pull refuses anything
	// else, so a mistyped reference cannot empty an image directory.
	ArtifactType = "application/vnd.giantswarm.vm-manager.guest-image.v1"
	// FileMediaType is the media type of every layer: one file of the image
	// directory, uncompressed (the disks are erofs with zstd already).
	FileMediaType = "application/vnd.giantswarm.vm-manager.guest-image.file.v1"
	// MarkerFile in the image directory records the reference and manifest
	// digest of the last pull.
	MarkerFile = ".guest-image.json"

	// PolicyFile is the shared policy the build writes next to the images.
	PolicyFile = "policy.json"
	// SysupdateDir is the update tree the guests fetch from.
	SysupdateDir = "sysupdate"

	// pullConcurrency bounds concurrent blob downloads.
	pullConcurrency = 3

	// manifestCreated is the fixed org.opencontainers.image.created of every
	// manifest: oras stamps the time of packing otherwise, and then two pushes
	// of the same build differ in digest — a pod pinned to the digest would
	// roll for nothing. With it fixed the digest is the content's.
	manifestCreated = "1970-01-01T00:00:00Z"
)

// Options configure the registry access.
type Options struct {
	// PlainHTTP talks to the registry over HTTP, for a lab registry.
	PlainHTTP bool
	// Logger receives a line per file; nil means slog.Default().
	Logger *slog.Logger
}

// Marker is the content of MarkerFile.
type Marker struct {
	Reference string    `json:"reference"`
	Digest    string    `json:"digest"`
	Files     int       `json:"files"`
	PulledAt  time.Time `json:"pulledAt"`
}

// Result describes a Pull.
type Result struct {
	// Descriptor of the artifact manifest.
	Descriptor ocispec.Descriptor
	// Files in the artifact.
	Files int
	// Fetched is false when the directory already held this digest.
	Fetched bool
}

// Push publishes the image directory dir as the artifact reference names
// (<registry>/<repository>:<tag>) and returns the manifest descriptor.
func Push(ctx context.Context, dir, reference string, opts Options) (ocispec.Descriptor, error) {
	log := opts.logger()
	files, err := Contents(dir)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	repo, err := repository(reference, opts)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	if err := repo.Reference.ValidateReferenceAsTag(); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("push needs a tag: %w", err)
	}
	store, err := file.New(dir)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer func() { _ = store.Close() }()

	layers := make([]ocispec.Descriptor, 0, len(files))
	for _, rel := range files {
		desc, err := store.Add(ctx, rel, FileMediaType, filepath.Join(dir, rel))
		if err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("add %s: %w", rel, err)
		}
		log.Info("packed", "file", rel, "bytes", desc.Size)
		layers = append(layers, desc)
	}
	manifest, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, ArtifactType, oras.PackManifestOptions{
		Layers:              layers,
		ManifestAnnotations: map[string]string{ocispec.AnnotationCreated: manifestCreated},
	})
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("pack manifest: %w", err)
	}
	tag := repo.Reference.Reference
	if err := store.Tag(ctx, manifest, tag); err != nil {
		return ocispec.Descriptor{}, err
	}
	copyOpts := oras.DefaultCopyOptions
	copyOpts.Concurrency = pullConcurrency
	copyOpts.PostCopy = func(_ context.Context, desc ocispec.Descriptor) error {
		if name := desc.Annotations[ocispec.AnnotationTitle]; name != "" {
			log.Info("pushed", "file", name, "bytes", desc.Size)
		}
		return nil
	}
	copyOpts.OnCopySkipped = func(_ context.Context, desc ocispec.Descriptor) error {
		if name := desc.Annotations[ocispec.AnnotationTitle]; name != "" {
			log.Info("already in the registry", "file", name)
		}
		return nil
	}
	if _, err := oras.Copy(ctx, store, tag, repo, tag, copyOpts); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("push %s: %w", reference, err)
	}
	return manifest, nil
}

// Pull fetches the artifact reference names into the image directory dir,
// creating it when missing. A directory that already holds the referenced
// manifest digest (MarkerFile) is left untouched; otherwise its contents are
// replaced by the artifact's files.
func Pull(ctx context.Context, reference, dir string, opts Options) (Result, error) {
	log := opts.logger()
	repo, err := repository(reference, opts)
	if err != nil {
		return Result{}, err
	}
	tag := repo.Reference.ReferenceOrDefault()
	desc, err := repo.Resolve(ctx, tag)
	if err != nil {
		return Result{}, fmt.Errorf("resolve %s: %w", reference, err)
	}
	files, err := artifactFiles(ctx, repo, desc)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", reference, err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Result{}, err
	}
	if marker, err := ReadMarker(dir); err == nil && marker.Digest == desc.Digest.String() {
		log.Info("guest image already present", "reference", reference, "digest", desc.Digest, "dir", dir)
		return Result{Descriptor: desc, Files: len(files)}, nil
	}
	if err := clear(dir); err != nil {
		return Result{}, err
	}

	store, err := file.New(dir)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = store.Close() }()
	copyOpts := oras.DefaultCopyOptions
	copyOpts.Concurrency = pullConcurrency
	copyOpts.PostCopy = func(_ context.Context, desc ocispec.Descriptor) error {
		if name := desc.Annotations[ocispec.AnnotationTitle]; name != "" {
			log.Info("fetched", "file", name, "bytes", desc.Size)
		}
		return nil
	}
	if _, err := oras.Copy(ctx, repo, tag, store, tag, copyOpts); err != nil {
		return Result{}, fmt.Errorf("pull %s: %w", reference, err)
	}
	marker := Marker{Reference: reference, Digest: desc.Digest.String(), Files: len(files), PulledAt: time.Now().UTC()}
	if err := writeMarker(dir, marker); err != nil {
		return Result{}, err
	}
	log.Info("guest image pulled", "reference", reference, "digest", desc.Digest, "files", len(files), "dir", dir)
	return Result{Descriptor: desc, Files: len(files), Fetched: true}, nil
}

// Contents lists, relative to dir and sorted, the files a guest image
// artifact carries: for every <id>_<version>.efi with its .raw disk the two,
// plus the .roothash, the .repart.d/ tree and the .policy.json next to them
// when present; policy.json; and the sysupdate/ tree. At least one image and
// the policy are required.
func Contents(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	images := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".efi") {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".efi")
		if _, err := os.Stat(filepath.Join(dir, stem+".raw")); err != nil {
			continue
		}
		images++
		files = append(files, stem+".efi", stem+".raw")
		for _, opt := range []string{stem + ".roothash", stem + ".policy.json"} {
			if fileExists(filepath.Join(dir, opt)) {
				files = append(files, opt)
			}
		}
		tree, err := walk(dir, stem+".repart.d")
		if err != nil {
			return nil, err
		}
		files = append(files, tree...)
	}
	if images == 0 {
		return nil, fmt.Errorf("%s holds no image (<id>_<version>.efi with a .raw disk next to it)", dir)
	}
	if !fileExists(filepath.Join(dir, PolicyFile)) {
		return nil, fmt.Errorf("%s has no %s (make -C images verify writes it)", dir, PolicyFile)
	}
	files = append(files, PolicyFile)
	tree, err := walk(dir, SysupdateDir)
	if err != nil {
		return nil, err
	}
	files = append(files, tree...)
	return files, nil
}

// ReadMarker returns the record of the last pull into dir.
func ReadMarker(dir string) (Marker, error) {
	raw, err := os.ReadFile(filepath.Join(dir, MarkerFile)) // #nosec G304 -- the operator's image directory.
	if err != nil {
		return Marker{}, err
	}
	var m Marker
	if err := json.Unmarshal(raw, &m); err != nil {
		return Marker{}, fmt.Errorf("%s: %w", MarkerFile, err)
	}
	return m, nil
}

func writeMarker(dir string, m Marker) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, MarkerFile), append(data, '\n'), 0o644) // #nosec G306 -- a public record of what was pulled.
}

// artifactFiles reads the manifest and returns the layer names, refusing a
// manifest that is not a guest image artifact or names a file outside dir.
func artifactFiles(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor) ([]string, error) {
	raw, err := content.FetchAll(ctx, fetcher, desc)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if manifest.ArtifactType != ArtifactType {
		return nil, fmt.Errorf("not a guest image artifact: artifactType %q, want %q", manifest.ArtifactType, ArtifactType)
	}
	files := make([]string, 0, len(manifest.Layers))
	for _, layer := range manifest.Layers {
		name := layer.Annotations[ocispec.AnnotationTitle]
		if name == "" {
			return nil, fmt.Errorf("layer %s has no file name", layer.Digest)
		}
		if filepath.IsAbs(name) || name != filepath.ToSlash(filepath.Clean(name)) || strings.HasPrefix(name, "..") {
			return nil, fmt.Errorf("layer %s names %q, not a path inside the image directory", layer.Digest, name)
		}
		files = append(files, name)
	}
	return files, nil
}

// clear empties dir, MarkerFile included: a directory holds one artifact.
func clear(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// walk lists the regular files under dir/sub relative to dir; a missing sub
// is no error and yields nothing.
func walk(dir, sub string) ([]string, error) {
	root := filepath.Join(dir, sub)
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, err
}

func fileExists(path string) bool {
	st, err := os.Stat(path) // #nosec G703 -- paths are the operator's image directory and the Docker config.
	return err == nil && st.Mode().IsRegular()
}

func repository(reference string, opts Options) (*remote.Repository, error) {
	repo, err := remote.NewRepository(reference)
	if err != nil {
		return nil, fmt.Errorf("reference %q: %w", reference, err)
	}
	repo.PlainHTTP = opts.PlainHTTP
	client := &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache()}
	if store := dockerCredentials(); store != nil {
		client.Credential = credentials.Credential(store)
	}
	repo.Client = client
	return repo, nil
}

// dockerCredentials returns the Docker config credential store when a config
// file exists ($DOCKER_CONFIG/config.json or ~/.docker/config.json), else nil
// for anonymous access — the store is never created, so a read-only root
// file system is fine.
func dockerCredentials() credentials.Store {
	configDir := os.Getenv("DOCKER_CONFIG")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		configDir = filepath.Join(home, ".docker")
	}
	path := filepath.Join(configDir, "config.json")
	if !fileExists(path) {
		return nil
	}
	store, err := credentials.NewStore(path, credentials.StoreOptions{})
	if err != nil {
		return nil
	}
	return store
}

func (o Options) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}
