# Layer signing

A node that enforces IPE runs a container only from a layer the kernel verified itself:
containerd opens each tar layer as an EROFS tar index under dm-verity, and with
`dm_verity.require_signatures=1` the kernel refuses a device whose root hash carries no
signature by a certificate it trusts. `vm-manager layers sign` produces those signatures
for an image in a registry, `vm-manager layers verify` checks them the way a node does.

## What is signed

For every layer of every platform manifest, the signer:

1. fetches the layer and decompresses it (gzip, zstd or none) to the uncompressed tar, the
   bytes of the config's `diff_id`;
2. builds the tar index with `mkfs.erofs --tar=i --aufs --quiet -T0 --mkfs-time -U <uuid>`,
   where `<uuid>` is UUIDv5(URL, `erofs:blobs/<layer digest>`), so the index is
   reproducible;
3. takes the device as index ‖ the whole tar, zero-padded to 512 bytes, and computes its
   dm-verity hash tree: format 1, SHA-256, data and hash blocks of 512 bytes, a salt of 32
   zero bytes; the superblock UUID is derived from the layer digest too;
4. signs the root hash, its 64 lowercase hex characters without a newline: CMS
   SignedData, DER, detached, content type id-data, SHA-256, **no signed attributes**, the
   signer certificate embedded (what `openssl cms -sign -binary -noattr` writes; openssl's
   default `smimeCapabilities` attribute makes the kernel refuse the signature).

It pushes one referrer per platform manifest: artifact type
`application/vnd.containerd.erofs.dmverity.v1`, an empty config, the
`org.opencontainers.image.created` annotation and the erofs-utils version
(`io.giantswarm.vm-manager.layers/erofs-utils-version`), and three blobs per layer, each
annotated `io.containerd.erofs.v1/dmverity.source-layer-digest: <layer digest>`:

| Media type | Content |
|---|---|
| `application/vnd.containerd.erofs.dmverity.layer-signature.v1+pkcs7` | the signature, annotated `io.containerd.erofs.v1/dmverity.root-hash: <hex>` |
| `application/vnd.containerd.erofs.metadata.v1` | the tar index |
| `application/vnd.containerd.erofs.dmverity.merkle-tree.v1` | the dm-verity superblock, then the tree |

Both commands fail closed. An index is walked, and only an entry for the platform
`unknown/unknown` (how BuildKit marks build attestations) is skipped, and reported. Any
other manifest that is no container image (another config media type, an artifact), a
layer that is no OCI or Docker tar layer (native EROFS `application/vnd.erofs.layer*`,
encrypted, or anything else) and a digest that is not sha256 or sha512 fail the whole
image before any work, so nothing a node would unpack goes unsigned or unchecked.

Signing the same image with the same key file again gives the same blobs, which the
registry deduplicates; only the referrer manifest is new. An ECDSA key in a KMS signs
differently on every run, so there only the index and tree repeat. A layer shared between
images with the same digest has the same root hash. The root hash depends on the
*compressed* layer digest (through the index UUID): a mirror that recompresses a layer
must sign it again. A node needs neither the same signature nor the same root hash for a
layer images share: it keeps the layer as the first image unpacked it and runs the
others on it ([images/README.md](../images/README.md#containerd)). A layer is kept only
after the node's kernel accepted its signature: an image signed by a key the node does
not trust yet fails at pull, naming the layer, and leaves nothing that would break the
images sharing it.

## Keys

RSA (signed with PKCS#1 v1.5) or ECDSA P-256/P-384; RSA-PSS is refused, the kernel does not
verify it. The certificate is an X.509 certificate with `keyUsage=digitalSignature`; the
kernel checks neither key usage nor expiry. A development key:

    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 365 \
      -subj /CN=layer-signing -addext keyUsage=critical,digitalSignature \
      -keyout layers.key -out layers.crt

`--key` reads PKCS#8, PKCS#1 or SEC 1 PEM. Go programs sign with any `crypto.Signer`
(`pkg/layersign`, `SignOptions.Signer`), a KMS key among them; a raw r‖s ECDSA signature is
DER-encoded before it is embedded.

## Commands

    vm-manager layers sign <registry>/<repository>:<tag> --key layers.key --cert layers.crt
    vm-manager layers verify <registry>/<repository>:<tag> --cert layers.crt [--cert next.crt]

Both take `@<digest>` instead of a tag, `--plain-http` for a lab registry and `--temp-dir`
for the work files (a layer's tar, index, device and tree; the default is `$TMPDIR`).
Registry credentials come from the Docker config (`$DOCKER_CONFIG/config.json` or
`~/.docker/config.json`), as for `vm-manager image pull`. `sign` needs `mkfs.erofs` from an
erofs-utils with `--tar=i` and `--mkfs-time` on `PATH` (or `--mkfs-erofs`); the vm-manager
container image carries it, so a pipeline runs

    docker run --rm -v "$PWD/keys:/keys:ro" -v "$HOME/.docker:/root/.docker:ro" \
      --entrypoint vm-manager gsoci.azurecr.io/giantswarm/vm-manager:<version> \
      layers sign <image> --key /keys/layers.key --cert /keys/layers.crt

`verify` exits non-zero and names the manifest and layer digest on the first failure.

## What a node checks

The node builds nothing. Per platform manifest it takes the newest referrer of the artifact
type (by `org.opencontainers.image.created`, ties to the lowest digest) and per layer:

- all three blobs are present, otherwise the pull fails naming the layer;
- the device, the published index ‖ the layer's uncompressed tar zero-padded to 512 bytes,
  verifies against the published tree, and the tree's root equals the root-hash annotation;
- the kernel opens dm-verity with the signature and checks it against its `.dm-verity`
  keyring (the certificates enrolled on the node).

`layers verify` runs the same checks, the signature against the `--cert` certificates by
the kernel's rules (detached, id-data, SHA-256, no signed attributes, RSA PKCS#1 v1.5 or
ECDSA).

## Rotation

Nodes trust every certificate in their keyring, so a key rotates by overlap: enroll the new
certificate next to the old one on the nodes, sign the images with the new key (a newer
referrer, which the nodes then use), check them with `layers verify --cert new.crt`, and
remove the old certificate from the nodes once no image they run relies on it. Expiry does
not revoke a signature; removing the certificate does.
