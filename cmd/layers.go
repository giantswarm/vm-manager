package cmd

import (
	"crypto/x509"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/giantswarm/vm-manager/internal/registry"
	"github.com/giantswarm/vm-manager/pkg/layersign"
)

type layersOptions struct {
	plainHTTP bool
	tempDir   string
}

func (o *layersOptions) addFlags(f *pflag.FlagSet) {
	f.BoolVar(&o.plainHTTP, "plain-http", envBool("VM_MANAGER_REGISTRY_PLAIN_HTTP", false), "Reach the registry over HTTP instead of HTTPS (a lab registry) (VM_MANAGER_REGISTRY_PLAIN_HTTP)")
	f.StringVar(&o.tempDir, "temp-dir", "", "Directory for a layer's uncompressed tar, index and tree while it is worked on; default: $TMPDIR")
}

func newLayersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "layers",
		Short: "Sign and verify container image layers for IPE-enforcing nodes",
		Long: `Container images run on nodes that enforce IPE need a dm-verity signature for every
layer: containerd opens each layer as an EROFS tar index under dm-verity, and the
kernel refuses a device whose root hash is not signed by a certificate in its
.dm-verity keyring. docs/layer-signing.md describes the format, the keys and rotation.`,
	}
	cmd.AddCommand(newLayersSignCmd(), newLayersVerifyCmd())
	return cmd
}

func newLayersSignCmd() *cobra.Command {
	o := &layersOptions{}
	var keyPath, certPath, mkfs string
	cmd := &cobra.Command{
		Use:   "sign <reference> --key <PEM> --cert <PEM>",
		Short: "Sign every layer of an image and push the signatures as referrers",
		Long: `Sign every layer of every platform manifest of the image <registry>/<repository>:<tag>
(or @<digest>; an index or a single manifest): build the layer's EROFS tar index with
mkfs.erofs, its dm-verity hash tree, and the CMS signature of the root hash with --key,
and push one referrer per platform manifest (artifact type
` + layersign.ArtifactType + `) holding the signature, the index and the tree of each
layer. Signing the same image with the same key again gives the same blobs.
Registry credentials come from the Docker config ($DOCKER_CONFIG/config.json or
~/.docker/config.json) when one exists.

--key is a PEM private key (PKCS#8, PKCS#1 or SEC 1): RSA, or ECDSA P-256/P-384; RSA-PSS
keys are refused. --cert is its PEM certificate, the one the nodes trust.
mkfs.erofs (erofs-utils with --tar=i and --mkfs-time) must be on PATH or given with
--mkfs-erofs; the vm-manager image carries it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			keyPEM, err := os.ReadFile(keyPath) //nolint:gosec // G304: the --key the operator names
			if err != nil {
				return err
			}
			signer, err := layersign.ParsePrivateKey(keyPEM)
			if err != nil {
				return fmt.Errorf("--key %s: %w", keyPath, err)
			}
			certs, err := readCertificates(certPath)
			if err != nil {
				return err
			}
			cert := layersign.CertificateFor(signer, certs)
			if cert == nil {
				return fmt.Errorf("--cert %s holds no certificate of --key %s", certPath, keyPath)
			}
			repo, err := registry.Repository(args[0], o.plainHTTP)
			if err != nil {
				return err
			}
			signed, err := layersign.Sign(cmd.Context(), repo, repo.Reference.ReferenceOrDefault(), layersign.SignOptions{
				Signer:      signer,
				Certificate: cert,
				MkfsErofs:   mkfs,
				TempDir:     o.tempDir,
			})
			if err != nil {
				return err
			}
			for _, s := range signed {
				if s.Skipped != "" {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "skipped %s: %s\n", s.Manifest.Digest, s.Skipped)
					continue
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "signed %s: %d layers, referrer %s\n", s.Manifest.Digest, len(s.Layers), s.Referrer.Digest)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&keyPath, "key", "", "PEM private key that signs the layers' root hashes")
	cmd.Flags().StringVar(&certPath, "cert", "", "PEM certificate of --key, embedded in each signature")
	cmd.Flags().StringVar(&mkfs, "mkfs-erofs", "", "mkfs.erofs to run; default: the one on PATH")
	_ = cmd.MarkFlagRequired("key")
	_ = cmd.MarkFlagRequired("cert")
	o.addFlags(cmd.Flags())
	return cmd
}

func newLayersVerifyCmd() *cobra.Command {
	o := &layersOptions{}
	var certPaths []string
	cmd := &cobra.Command{
		Use:   "verify <reference> --cert <PEM>...",
		Short: "Check an image's layer signatures the way a node does",
		Long: `Check, for every platform manifest of the image <registry>/<repository>:<tag> (or
@<digest>), what an IPE-enforcing node checks before it opens a layer: the newest
referrer of artifact type ` + layersign.ArtifactType + ` holds a signature, an index
and a tree for every layer; the signature verifies against one of the --cert
certificates by the kernel's rules (detached, id-data, no signed attributes); the
tree's root is the signed root hash; and the device built from the published index
and the layer's tar verifies against the tree. Exits non-zero naming the layer on
the first failure.

--cert may be repeated, or name a PEM file of several certificates: during a key
rotation the nodes trust both.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var certs []*x509.Certificate
			for _, p := range certPaths {
				c, err := readCertificates(p)
				if err != nil {
					return err
				}
				certs = append(certs, c...)
			}
			repo, err := registry.Repository(args[0], o.plainHTTP)
			if err != nil {
				return err
			}
			verified, err := layersign.Verify(cmd.Context(), repo, repo.Reference.ReferenceOrDefault(), layersign.VerifyOptions{
				Certificates: certs,
				TempDir:      o.tempDir,
			})
			if err != nil {
				return err
			}
			for _, v := range verified {
				if v.Skipped != "" {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "skipped %s: %s\n", v.Manifest.Digest, v.Skipped)
					continue
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "verified %s: %d layers, referrer %s\n", v.Manifest.Digest, len(v.Layers), v.Referrer.Digest)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&certPaths, "cert", nil, "PEM certificate a layer signature may be made with (repeatable)")
	_ = cmd.MarkFlagRequired("cert")
	o.addFlags(cmd.Flags())
	return cmd
}

func readCertificates(path string) ([]*x509.Certificate, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the --cert the operator names
	if err != nil {
		return nil, err
	}
	certs, err := layersign.ParseCertificates(data)
	if err != nil {
		return nil, fmt.Errorf("--cert %s: %w", path, err)
	}
	return certs, nil
}
