// Package images is the catalog of guest images vm-manager can boot: the base
// DDIs and UKIs produced by images/ (see images/README.md), the Kubernetes
// sysext versions served next to them and the PCR policy recorded for each
// image.
//
// # Directory layout
//
// The catalog scans one image directory:
//
//	<dir>/giantswarm-vm-base_<version>.efi      UKI, booted directly in the installer phase
//	<dir>/giantswarm-vm-base_<version>.raw      base DDI, attached read-only as the installer disk
//	<dir>/giantswarm-vm-base_<version>.policy.json   optional PCR policy of that image
//	<dir>/policy.json                            optional policy; applies to the image its image_id/image_version name
//	<dir>/sysupdate/base/                        served at /sysupdate/base/ by the IMDS
//	<dir>/sysupdate/kubernetes/SHA256SUMS        lists kubernetes_<kver>.raw, one per available version
//
// An image is the pair of a UKI and a DDI with the same stem <id>_<version>;
// a lone .efi or .raw is skipped with a warning. Image references accept the
// full stem ("giantswarm-vm-base_0.1.0") or the bare id, which selects the
// highest version of that id.
package images

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/giantswarm/vm-manager/internal/apierr"
)

const (
	ukiSuffix    = ".efi"
	diskSuffix   = ".raw"
	policySuffix = ".policy.json"
	policyFile   = "policy.json"
	// SysupdateDirName is the subdirectory holding the sysupdate components.
	SysupdateDirName = "sysupdate"
	// KubernetesComponent is the sysupdate component carrying the Kubernetes sysext.
	KubernetesComponent = "kubernetes"
	kubernetesPrefix    = "kubernetes_"
	sumsFile            = "SHA256SUMS"
)

// Image is one bootable base image.
type Image struct {
	// ID is the image id, the file stem before the version ("giantswarm-vm-base").
	ID string `json:"id"`
	// Version is the image version ("0.1.0").
	Version string `json:"version"`
	// UKI is the path of the unified kernel image (.efi).
	UKI string `json:"uki"`
	// Disk is the path of the base DDI (.raw).
	Disk string `json:"disk"`
	// SysupdateDir is the sysupdate tree served to guests; empty when absent.
	SysupdateDir string `json:"sysupdateDir,omitempty"`
	// KubernetesVersions are the sysext versions available under
	// SysupdateDir/kubernetes, highest first.
	KubernetesVersions []string `json:"kubernetesVersions,omitempty"`
	// Policy is the image's PCR policy as written by `make verify-base`,
	// nil when no policy is present.
	Policy json.RawMessage `json:"policy,omitempty"`
}

// Ref is the image reference, "<id>_<version>".
func (i Image) Ref() string { return i.ID + "_" + i.Version }

// HasKubernetesVersion reports whether the sysext for version is available.
func (i Image) HasKubernetesVersion(version string) bool {
	for _, v := range i.KubernetesVersions {
		if v == version {
			return true
		}
	}
	return false
}

// Catalog lists the images below one directory. It is safe for concurrent use.
type Catalog struct {
	dir string
	log *slog.Logger

	mu     sync.RWMutex
	images []Image // sorted by id, then version descending
}

// New returns a catalog for dir. Call Refresh (or Load) to scan it; a nil
// logger means slog.Default().
func New(dir string, log *slog.Logger) *Catalog {
	if log == nil {
		log = slog.Default()
	}
	return &Catalog{dir: dir, log: log}
}

// Load returns a scanned catalog for dir.
func Load(dir string, log *slog.Logger) (*Catalog, error) {
	c := New(dir, log)
	if err := c.Refresh(); err != nil {
		return nil, err
	}
	return c, nil
}

// Dir is the image directory.
func (c *Catalog) Dir() string { return c.dir }

// SysupdateDir is the sysupdate tree the IMDS serves, whether or not it
// exists yet.
func (c *Catalog) SysupdateDir() string { return filepath.Join(c.dir, SysupdateDirName) }

// Refresh rescans the directory. It fails when the directory cannot be read;
// individual malformed images are logged and skipped.
func (c *Catalog) Refresh() error {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return fmt.Errorf("read image dir: %w", err)
	}
	kversions := c.kubernetesVersions()
	sysupdate := ""
	if st, err := os.Stat(c.SysupdateDir()); err == nil && st.IsDir() {
		sysupdate = c.SysupdateDir()
	}

	var images []Image
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ukiSuffix) {
			continue
		}
		stem := strings.TrimSuffix(name, ukiSuffix)
		id, version, ok := splitStem(stem)
		if !ok {
			c.log.Warn("image name is not <id>_<version>.efi, skipped", "file", name)
			continue
		}
		disk := filepath.Join(c.dir, stem+diskSuffix)
		if _, err := os.Stat(disk); err != nil {
			c.log.Warn("image has no disk next to its UKI, skipped", "file", name, "disk", disk)
			continue
		}
		images = append(images, Image{
			ID:                 id,
			Version:            version,
			UKI:                filepath.Join(c.dir, name),
			Disk:               disk,
			SysupdateDir:       sysupdate,
			KubernetesVersions: kversions,
			Policy:             c.policy(id, version),
		})
	}
	sort.Slice(images, func(i, j int) bool {
		if images[i].ID != images[j].ID {
			return images[i].ID < images[j].ID
		}
		return CompareVersions(images[i].Version, images[j].Version) > 0
	})

	c.mu.Lock()
	c.images = images
	c.mu.Unlock()
	return nil
}

// List returns every image, sorted by id and then by version, highest first.
func (c *Catalog) List() []Image {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Image, len(c.images))
	copy(out, c.images)
	return out
}

// Get resolves an image reference: "<id>_<version>" selects that image,
// a bare "<id>" the highest version of it. It fails with apierr.ErrNotFound.
func (c *Catalog) Get(ref string) (Image, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, img := range c.images {
		if img.Ref() == ref || img.ID == ref {
			return img, nil
		}
	}
	return Image{}, fmt.Errorf("%w: image %q", apierr.ErrNotFound, ref)
}

// Default is the image with the highest version; ties go to the first id.
// It fails with apierr.ErrNotFound on an empty catalog.
func (c *Catalog) Default() (Image, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.images) == 0 {
		return Image{}, fmt.Errorf("%w: no images in %s", apierr.ErrNotFound, c.dir)
	}
	best := c.images[0]
	for _, img := range c.images[1:] {
		if CompareVersions(img.Version, best.Version) > 0 {
			best = img
		}
	}
	return best, nil
}

// kubernetesVersions reads the versions listed in the Kubernetes component's
// SHA256SUMS, highest first.
func (c *Catalog) kubernetesVersions() []string {
	path := filepath.Join(c.SysupdateDir(), KubernetesComponent, sumsFile)
	data, err := os.ReadFile(path) // #nosec G304 -- path is derived from the configured image dir.
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			c.log.Warn("cannot read kubernetes SHA256SUMS", "path", path, "err", err)
		}
		return nil
	}
	var versions []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		file := strings.TrimPrefix(fields[len(fields)-1], "*")
		if !strings.HasPrefix(file, kubernetesPrefix) || !strings.HasSuffix(file, diskSuffix) {
			continue
		}
		versions = append(versions, strings.TrimSuffix(strings.TrimPrefix(file, kubernetesPrefix), diskSuffix))
	}
	sort.Slice(versions, func(i, j int) bool { return CompareVersions(versions[i], versions[j]) > 0 })
	return versions
}

// policy loads <id>_<version>.policy.json, else policy.json when it names
// this image (or names none).
func (c *Catalog) policy(id, version string) json.RawMessage {
	_, raw := c.policySource(id, version)
	return raw
}

// PolicyPath is the file Image.Policy came from, or, for an image without a
// policy, where one would be written: <id>_<version>.policy.json.
func (c *Catalog) PolicyPath(img Image) string {
	if path, _ := c.policySource(img.ID, img.Version); path != "" {
		return path
	}
	return c.perImagePolicy(img.ID, img.Version)
}

func (c *Catalog) perImagePolicy(id, version string) string {
	return filepath.Join(c.dir, id+"_"+version+policySuffix)
}

// policySource returns the policy file that applies to an image and its
// content, or "" and nil when there is none.
func (c *Catalog) policySource(id, version string) (string, json.RawMessage) {
	perImage := c.perImagePolicy(id, version)
	if raw := readJSON(perImage); raw != nil {
		return perImage, raw
	}
	shared := filepath.Join(c.dir, policyFile)
	raw := readJSON(shared)
	if raw == nil {
		return "", nil
	}
	var head struct {
		ImageID      string `json:"image_id"`
		ImageVersion string `json:"image_version"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		c.log.Warn("policy.json is not a JSON object, ignored", "err", err)
		return "", nil
	}
	if (head.ImageID != "" && head.ImageID != id) || (head.ImageVersion != "" && head.ImageVersion != version) {
		return "", nil
	}
	return shared, raw
}

func readJSON(path string) json.RawMessage {
	data, err := os.ReadFile(path) // #nosec G304 -- path is derived from the configured image dir.
	if err != nil || !json.Valid(data) {
		return nil
	}
	return json.RawMessage(data)
}

// splitStem splits "<id>_<version>" at the last underscore.
func splitStem(stem string) (id, version string, ok bool) {
	i := strings.LastIndex(stem, "_")
	if i <= 0 || i == len(stem)-1 {
		return "", "", false
	}
	return stem[:i], stem[i+1:], true
}

// CompareVersions orders dotted versions numerically where both components
// are numbers and lexically otherwise: 0.10.0 > 0.9.0 > 0.9.0-rc1.
func CompareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if c := compareComponent(x, y); c != 0 {
			return c
		}
	}
	return 0
}

func compareComponent(x, y string) int {
	if x == y {
		return 0
	}
	xn, xerr := strconv.Atoi(x)
	yn, yerr := strconv.Atoi(y)
	switch {
	case xerr == nil && yerr == nil:
		return cmpInt(xn, yn)
	case xerr == nil || yerr == nil:
		// A pre-release suffix ("0-rc1") sorts below the plain number; a
		// missing component ("") sorts below anything.
		xp, xrest := splitNumericPrefix(x)
		yp, yrest := splitNumericPrefix(y)
		if xp != yp {
			return cmpInt(xp, yp)
		}
		return -cmpInt(len(xrest), len(yrest))
	default:
		return strings.Compare(x, y)
	}
}

func splitNumericPrefix(s string) (int, string) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	n, _ := strconv.Atoi(s[:i])
	return n, s[i:]
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
