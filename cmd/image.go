package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/giantswarm/vm-manager/internal/api"
	"github.com/giantswarm/vm-manager/internal/attest"
	"github.com/giantswarm/vm-manager/internal/images"
	"github.com/giantswarm/vm-manager/internal/vm"
	"github.com/giantswarm/vm-manager/pkg/guestimage"
)

// goldenHTTPTimeout bounds the attestation lookup on the running server.
const goldenHTTPTimeout = 10 * time.Second

type imageGoldenOptions struct {
	imageDirOptions
	fromVM string
	server string
	token  string
}

// imageDirOptions resolve the image directory the image subcommands work on:
// --image-dir, else <state-dir>/images like the server.
type imageDirOptions struct {
	stateDir string
	imageDir string
}

func (o *imageDirOptions) addFlags(f *pflag.FlagSet) {
	f.StringVar(&o.stateDir, "state-dir", envOr("VM_MANAGER_STATE_DIR", defaultStateDir()), "State directory of the server; only its images subdirectory is used unless --image-dir is set (VM_MANAGER_STATE_DIR)")
	f.StringVar(&o.imageDir, "image-dir", envOr("VM_MANAGER_IMAGE_DIR", ""), "Image directory; default: <state-dir>/images (VM_MANAGER_IMAGE_DIR)")
}

func (o *imageDirOptions) dir() string {
	if o.imageDir != "" {
		return o.imageDir
	}
	return filepath.Join(o.stateDir, imagesSubdir)
}

type imageArtifactOptions struct {
	imageDirOptions
	plainHTTP bool
}

func (o *imageArtifactOptions) addFlags(f *pflag.FlagSet) {
	o.imageDirOptions.addFlags(f)
	f.BoolVar(&o.plainHTTP, "plain-http", envBool("VM_MANAGER_REGISTRY_PLAIN_HTTP", false), "Reach the registry over HTTP instead of HTTPS (a lab registry) (VM_MANAGER_REGISTRY_PLAIN_HTTP)")
}

func (o *imageArtifactOptions) options() guestimage.Options {
	return guestimage.Options{PlainHTTP: o.plainHTTP, Logger: slog.Default()}
}

func newImageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Image catalog operations",
	}
	cmd.AddCommand(newImagePullCmd(), newImagePushCmd(), newImageGoldenCmd())
	return cmd
}

func newImagePullCmd() *cobra.Command {
	o := &imageArtifactOptions{}
	cmd := &cobra.Command{
		Use:   "pull <reference>",
		Short: "Fetch a published guest image artifact into the image directory",
		Long: `Fetch the guest image artifact <registry>/<repository>:<tag> (or @<digest>) — the
UKIs, disks, policy.json and sysupdate tree ` + "`vm-manager image push`" + ` published — into
the image directory the server reads. A directory that already holds that artifact
(by manifest digest, recorded in ` + guestimage.MarkerFile + `) is left as it is, golden PCR
values recorded since included; another digest replaces its contents. Registry
credentials come from the Docker config ($DOCKER_CONFIG/config.json or
~/.docker/config.json) when one exists; without one the pull is anonymous.

The pod runs this as its init container.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := guestimage.Pull(cmd.Context(), args[0], o.dir(), o.options())
			if err != nil {
				return err
			}
			verb := "already present"
			if res.Fetched {
				verb = "pulled"
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s (%s, %d files) in %s\n", verb, args[0], res.Descriptor.Digest, res.Files, o.dir())
			return nil
		},
	}
	o.addFlags(cmd.Flags())
	return cmd
}

func newImagePushCmd() *cobra.Command {
	o := &imageArtifactOptions{}
	cmd := &cobra.Command{
		Use:   "push <reference>",
		Short: "Publish the image directory as a guest image artifact",
		Long: `Publish the guest image in the image directory (` + "`make -C images`" + ` output: every
<id>_<version>.efi with its .raw disk, .roothash and .repart.d/, policy.json and the
sysupdate tree — nothing else of a build directory) as the OCI artifact
<registry>/<repository>:<tag>. Registry credentials come from the Docker config
($DOCKER_CONFIG/config.json or ~/.docker/config.json). The release pipeline pushes
one per release; a lab pushes a local build to its own registry.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			desc, err := guestimage.Push(cmd.Context(), o.dir(), args[0], o.options())
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "pushed %s (%s)\n", args[0], desc.Digest)
			return nil
		},
	}
	o.addFlags(cmd.Flags())
	return cmd
}

func newImageGoldenCmd() *cobra.Command {
	o := &imageGoldenOptions{}
	cmd := &cobra.Command{
		Use:   "golden <image-ref> --from-vm <id>",
		Short: "Record the golden PCR values of an attested VM into the image's policy.json",
		Long: `Read the verified ready-stage quote of a VM from the running server and write
its PCRs 0, 2-4, 6, 7 and 13 as the golden values into the policy.json of the image, so
the verifier (--attestation=verify) can compare every later boot of that image
against a known-good one. The VM must have attested with --attestation=verify;
during bring-up start the server with --attestation-learn-golden so the first
boot is accepted without golden values.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImageGolden(cmd.Context(), o, args[0], cmd.OutOrStdout())
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.fromVM, "from-vm", "", "ID of the VM whose verified ready-stage quote supplies the values")
	f.StringVar(&o.server, "server", envOr("VM_MANAGER_SERVER", "http://127.0.0.1:8080"), "Base URL of the running vm-manager server (VM_MANAGER_SERVER)")
	f.StringVar(&o.token, "token", os.Getenv("VM_MANAGER_TOKEN"), "Bearer token for a server started with --enable-oauth (VM_MANAGER_TOKEN)")
	o.addFlags(f)
	_ = cmd.MarkFlagRequired("from-vm")
	return cmd
}

func runImageGolden(ctx context.Context, o *imageGoldenOptions, ref string, out io.Writer) error {
	o.imageDir = o.dir()
	catalog, err := images.Load(o.imageDir, slog.Default())
	if err != nil {
		return fmt.Errorf("load images: %w", err)
	}
	img, err := catalog.Get(ref)
	if err != nil {
		return err
	}
	if img.Policy == nil {
		return fmt.Errorf("image %s has no policy.json in %s; run make image-verify and copy build/policy.json next to the image", img.Ref(), o.imageDir)
	}

	att, err := fetchAttestation(ctx, o, o.fromVM)
	if err != nil {
		return err
	}
	if att.Ready == nil || !att.Ready.Verified {
		return fmt.Errorf("vm %s has no verified ready-stage quote (initrd verified: %v); boot it with --attestation=verify", o.fromVM, att.Initrd != nil && att.Initrd.Verified)
	}
	golden := attest.GoldenFromPCRs(att.Ready.PCRs)
	if len(golden) != len(attest.GoldenIndexes) {
		return fmt.Errorf("ready quote of vm %s carries %d of the %d golden PCRs; the server must run with --attestation=verify", o.fromVM, len(golden), len(attest.GoldenIndexes))
	}

	path := catalog.PolicyPath(img)
	if err := writeGolden(path, golden); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "recorded golden sha256 PCRs %s of vm %s (ak %s) into %s\n", indexes(golden), o.fromVM, att.Ready.AKFingerprint, path)
	return nil
}

// fetchAttestation reads GET /api/v1/vms/<id>/attestation from the server.
func fetchAttestation(ctx context.Context, o *imageGoldenOptions, vmID string) (vm.Attestation, error) {
	url := strings.TrimRight(o.server, "/") + api.Prefix + "/vms/" + vmID + "/attestation"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return vm.Attestation{}, err
	}
	if o.token != "" {
		req.Header.Set("Authorization", "Bearer "+o.token)
	}
	resp, err := (&http.Client{Timeout: goldenHTTPTimeout}).Do(req)
	if err != nil {
		return vm.Attestation{}, fmt.Errorf("query %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return vm.Attestation{}, fmt.Errorf("read %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return vm.Attestation{}, fmt.Errorf("%s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	var att vm.Attestation
	if err := json.Unmarshal(body, &att); err != nil {
		return vm.Attestation{}, fmt.Errorf("decode %s: %w", url, err)
	}
	return att, nil
}

// writeGolden sets golden.sha256 in the policy file, keeping every other
// field, and validates the result before replacing the file.
func writeGolden(path string, golden map[int]string) error {
	raw, err := os.ReadFile(path) // #nosec G304 -- the path comes from the image catalog.
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	doc["golden"], err = json.Marshal(map[string]map[int]string{attest.Bank: golden})
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if _, err := attest.ParsePolicy(data); err != nil {
		return fmt.Errorf("%s would not be a valid policy: %w", path, err)
	}
	tmp := path + ".tmp"
	// The policy holds public measurements and is read by whoever runs the
	// server, possibly another user than the one recording it.
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil { // #nosec G306 -- public data, see above.
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func indexes(golden map[int]string) string {
	parts := make([]string, 0, len(golden))
	for _, i := range attest.GoldenIndexes {
		if _, ok := golden[i]; ok {
			parts = append(parts, fmt.Sprint(i))
		}
	}
	return strings.Join(parts, ",")
}
