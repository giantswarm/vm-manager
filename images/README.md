# images/ — giantswarm-vm-base and the kubernetes sysext

mkosi 27 project that builds the guest image described in
[docs/design.md](../docs/design.md) ("Guest image", "Boot flow"): Arch Linux,
systemd 261, an erofs root protected by signed dm-verity, a UKI with signed
expected PCR 11 values, systemd-boot on the ESP, plus the Kubernetes node stack
as a separately versioned, signed systemd-sysext (see [Kubernetes sysext](#kubernetes-sysext)).
Everything runs unprivileged (mkosi's own sandbox and user namespaces), no sudo anywhere.

```
make -C images                                 # keys, base image + kubernetes sysext (one mkosi run), verify
make -C images keys base verify-base smoke-boot  # base-only cycle (~5 min cold, ~1 min warm)
make -C images kubernetes verify-kubernetes    # sysext only, base tree reused (~20 s warm)
make -C images PROFILE=debug base smoke-boot   # root autologin + empty root password
make -C images clean                           # drop build/, keep keys/
```

## Layout

| Path | Purpose |
|---|---|
| `mkosi.conf` | main image: `Format=disk`, `Bootable=yes`, systemd-boot, UKI, `Verity=signed`, `SignExpectedPcr=yes`, kernel command line, split artifacts, `mkosi vm` runtime settings |
| `mkosi.images/base/` | the OS tree (`Format=directory`, `Output=base`): package list, `mkosi.extra/` content (units, presets, repart and sysupdate definitions, hwdb), `mkosi.postinst` (hwdb compile, PGP pubring, mask). The main image and the sysext consume it via `BaseTrees=%O/base` |
| `mkosi.images/kubernetes/` | the Kubernetes sysext (`Format=sysext`, `Overlay=yes`): package list, `mkosi.version` (= Kubernetes version), `mkosi.extra/` (drop-ins, containerd.toml, sysctl, tmpfiles, preset), `mkosi.postinst` (version guard, drops `/opt`, seeds extension-release) |
| `mkosi.repart/` | partition definitions of the *base image*: ESP 256M, erofs root (zstd), verity hash, verity signature |
| `mkosi.initrd.conf/` | additions to mkosi's default initrd (`InitrdProfiles=network`): the giantswarm hwdb record compiled into the initrd's `hwdb.bin`, a `systemd-repart.service` drop-in |
| `mkosi.profiles/debug/` | `Autologin=yes`, `RootPassword=hashed:` (unlocked, empty) for local iteration; the default build has neither |
| `mkosi.version` | `ImageVersion=` (0.1.0). Bump it per release; sysupdate orders versions with `strverscmp` |
| `scripts/` | `gen-keys`, `hwdb-update` (mkosi postinst), `publish-sysupdate <component>`, `verify` (base), `verify-kubernetes`, `smoke-boot` |
| `keys/` (git-ignored) | development signing keys, see below |
| `build/` (git-ignored) | outputs |

Make targets: `images` (one `mkosi --dependency=kubernetes build` for base tree, sysext
and disk image), `base` (plain `mkosi build`: base tree and disk image), `kubernetes`
(`mkosi --format=none --bootable=no --dependency=kubernetes build`: sysext on the
existing base tree, without `--force`, which would delete the disk image outputs of the
skipped main image), `verify` = `verify-base` + `verify-kubernetes`, `smoke-boot`, `clean`.

## What is built

`make base` runs `mkosi build` and `scripts/publish-sysupdate base`:

| Artifact | Content |
|---|---|
| `build/giantswarm-vm-base_<v>.raw` | GPT disk image: ESP (systemd-boot + UKI), root (erofs, `Verity=data`), root-verity, root-verity-sig |
| `build/giantswarm-vm-base_<v>.efi` | the UKI (`.linux`, `.initrd`, `.cmdline`, `.osrel`, `.uname`, `.sbat`, `.pcrpkey`, `.pcrsig`) |
| `build/giantswarm-vm-base_<v>.initrd` | the initrd inside the UKI (default mkosi initrd + network profile + kernel modules initrd) |
| `build/giantswarm-vm-base_<v>.roothash` | verity root hash; also on the UKI command line as `roothash=` |
| `build/giantswarm-vm-base_<v>.root-x86-64{,-verity,-verity-sig}.raw` | split partitions (`SplitArtifacts=partitions`) |
| `build/giantswarm-vm-base_<v>.repart.d/` | the repart definitions that were used |
| `build/sysupdate/base/` | what vm-manager serves at `.../sysupdate/base/`: `<id>_<v>_<root-partuuid>.root.raw`, `<id>_<v>_<verity-partuuid>.verity.raw`, `<id>_<v>.verity-sig.raw`, `<id>_<v>.efi`, `SHA256SUMS`, `SHA256SUMS.gpg` |
| `build/policy.json` | written by `make verify-base`: `{image_id, image_version, uki, roothash, partitions, pcr11: {phase_path: hex}}`; `vm-manager image golden` adds `golden: {sha256: {"0": hex, ..., "7": hex, "13": hex}}` (PCRs 0-7 and 13 of a known-good boot), which a re-run of `make verify-base` keeps for the same image version. vm-manager's attestation verifier (`internal/attest`) compares PCR 11 with `pcr11[<phase path>]` and PCRs 0-7 (and 13 at the ready stage) with `golden` |
| `build/base/` | the OS tree (input for the main image and for the sysext) |

`make kubernetes` (or `make images`) adds, via `scripts/publish-sysupdate kubernetes`:

| Artifact | Content |
|---|---|
| `build/kubernetes_<kv>.raw` | the sysext DDI: root (erofs, zstd, `Verity=data`), root-verity, root-verity-sig; no ESP |
| `build/kubernetes_<kv>.roothash` | its verity root hash |
| `build/sysupdate/kubernetes/` | what vm-manager serves at `.../sysupdate/kubernetes/`: `kubernetes_<kv>.raw`, `SHA256SUMS`, `SHA256SUMS.gpg` |

Root and verity partition UUIDs are derived from the root hash (first/last 128 bits),
which is how `roothash=` locates them; that is why the sysupdate file names carry
the partition UUID (`@u`).

Sizes for 0.1.0: `.raw` 646 MB (472 MB used), `.efi` 90 MB (kernel 17 MB, initrd 73 MB
of which 61 MB is mkosi's default initrd), root erofs (zstd) 400 MB, verity 3.2 MB,
signature 1.9 KB. A warm `make base` (nothing changed) takes ~10 s, a cold one with
package downloads ~3 min; `make verify-base` ~5 s; `make smoke-boot` ~30 s wall, the guest
reaches multi-user.target after ~7 s. The kubernetes sysext for 1.36.4 is 206 MB
(erofs zstd; 484 MB uncompressed, of which `/usr/bin` is 411 MB of Go binaries);
`make kubernetes verify-kubernetes` on a built base tree takes ~20 s with a warm package
cache, `make images verify` (everything, warm) ~30 s.

## Image content (mkosi.images/base/mkosi.extra)

- `/usr/lib/udev/hwdb.d/45-imds-giantswarm.hwdb`: `dmi:*:svnGiantSwarm:*` ->
  `IMDS_VENDOR=giantswarm`, `IMDS_DATA_URL=http://169.254.169.254/giantswarm/v1`,
  `IMDS_ADDRESS_IPV4`, `IMDS_KEY_{HOSTNAME,REGION,ZONE,SSH_KEY,USERDATA}`. Compiled
  into `hwdb.bin` of the root file system and of the initrd (`scripts/hwdb-update`).
- `/usr/lib/repart.sysinstall.d/`: target layout for `systemd-sysinstall`: ESP 512M,
  root A / verity A / verity-sig A with `CopyBlocks=auto` and labels `%M_%A[...]`
  (`giantswarm-vm-base_0.1.0`, `_verity`, `_verity_sig`), empty B slots labelled `_empty`
  with the same fixed sizes (2G / 64M; signature partitions are always 16K, see
  "Deviations"), `var` (ext4, `SizeMinBytes=1G`, rest of the disk). systemd-sysinstall
  defers the `_empty` partitions (their space stays reserved, var is partition 8); the
  installed system's first boot creates them from `/usr/lib/repart.d/`.
- `/usr/lib/repart.d/`: boot-time repart on the root disk. `10-`/`11-`/`12-` are
  size-less matchers that claim the existing A slots (repart matches by type in
  definition order), `20-`/`21-`/`22-` and `30-var.conf` are symlinks to the sysinstall
  definitions: on the installed disk the first boot creates the `_empty` B slots that
  systemd-sysinstall deferred; on a plain smoke boot of the base image it creates B slots
  and var (var is then mounted from the next boot on, since systemd-gpt-auto-generator
  already ran) and grows var. A drop-in disables `systemd-repart.service` when the
  `vm.install-target` credential is present (installer boot, read-only root disk).
- `/var` is empty in the image (`RemoveFiles=/var/*`): systemd-gpt-auto-generator mounts
  the var partition (UUID keyed by the machine ID, found through
  `/run/systemd/volatile-root` under the overlay) only over an empty directory.
  `tmpfiles.d/vm-manager.conf` recreates `/var/empty` (sshd), `/var/lock` and
  `/var/log/journal` on it; the rest comes from systemd's own tmpfiles catalog.
- `vm-sysinstall.service` (enabled, `ConditionCredential=vm.install-target`,
  `ImportCredential=vm.* firstboot.* ssh.* system.*`) runs `/usr/lib/vm-manager/sysinstall`:
  reads the target from `$CREDENTIALS_DIRECTORY/vm.install-target`, finds the booted
  UKI on the installer image's ESP (`EFI/Linux/giantswarm-vm-base_<v>.efi`; the ESP is
  mounted read-only by the script itself, see "Deviations"), forwards every
  `firstboot.*`, `ssh.authorized_keys.root`, `system.machine_id` credential with
  `--load-credential=` and executes
  `systemd-sysinstall $TARGET --erase=yes --confirm=no --welcome=no --chrome=no --variables=yes --reboot=yes --kernel=<uki>`.
  The stock interactive `systemd-sysinstall.service` is masked.
- `/usr/lib/sysupdate.d/{50-verity,55-verity-sig,60-root,70-uki}.transfer`: A/B OS
  updates from `http://169.254.169.254/giantswarm/v1/sysupdate/base/` (sysupdate.d(5)
  Example 1 plus the signature partition). `/usr/lib/sysupdate.kubernetes.d/50-kubernetes.transfer`
  pulls `kubernetes_@v.raw` into `/var/lib/extensions/` from `.../sysupdate/kubernetes/`
  (`Verify=yes`, key in `/etc/systemd/import-pubring.pgp`). `systemd-sysupdate.timer` stays off.
- `vm-report-upload.timer` (every 15 s) runs
  `systemd-report upload --url=http://169.254.169.254/giantswarm/v1/report --key=- --network-timeout=5`.
- `/usr/lib/systemd/system-preset/10-vm-manager.preset` enables `systemd-networkd`
  (DHCP on every ethernet link), `systemd-resolved`, `systemd-timesyncd`,
  `systemd-imdsd.socket`, `systemd-sysext.service`, `sshd.service` (plus
  `systemd-ssh-generator`'s AF_VSOCK port 22 listener), `vm-sysinstall.service`,
  `vm-report-upload.timer`, and disables `systemd-sysupdate*.timer`, `systemd-homed`,
  `machines.target`. `systemd-pcrphase*` are the systemd defaults.
- Kernel command line: `console=ttyS0,115200 systemd.volatile=overlay systemd.imds.import=yes systemd.firstboot=off`
  plus `roothash=<hash>` added by mkosi.
- `systemd-modules-load.service.d/` and `systemd-sysctl.service.d/vm-manager.conf`:
  `After=systemd-sysext.service`, so `modules-load.d/` and `sysctl.d/` entries of a
  merged extension are applied (upstream has no such ordering).

## Kubernetes sysext

`mkosi.images/kubernetes/` builds the Kubernetes node stack as a
[systemd-sysext](https://www.freedesktop.org/software/systemd/man/latest/systemd-sysext.html)
image: an erofs `/usr` tree with signed dm-verity that systemd-sysext overlays onto the
read-only base root at boot. It is built as `Overlay=yes` on top of `build/base`, so
only the delta is packed, and signed with the same `keys/verity.crt` the base image
trusts in `/usr/lib/verity.d/` (mkosi installs it there).

Content (Arch packages `kubeadm kubelet kubectl containerd runc crictl cni-plugins
conntrack-tools socat ethtool iptables`): `/usr/bin/{kubeadm,kubelet,kubectl,containerd,
containerd-shim-runc-v2,ctr,runc,crictl,conntrack,socat,ethtool,iptables,...}`, the
reference CNI plugins in `/usr/lib/cni`, and from `mkosi.extra/`:

- `containerd.service.d/10-vm-manager.conf`: `--config /usr/lib/vm-manager/containerd.toml`
  (a sysext cannot ship `/etc`): `SystemdCgroup=true`, CNI `bin_dirs=['/opt/cni/bin',
  '/usr/lib/cni']`, `conf_dir=/etc/cni/net.d`. `/opt` is deliberately *not* in the
  extension (`mkosi.postinst` removes the package's `/opt/cni/bin` copy): with
  `systemd.volatile=overlay` the base root is writable per boot, so CNI DaemonSets can
  install into `/opt/cni/bin`, which would be read-only under a sysext overlay.
- `kubelet.service.d/20-vm-manager.conf`: `KUBELET_ARGS=--container-runtime-endpoint=unix:///run/containerd/containerd.sock`
  (Arch's unit sources it from `/etc/kubernetes/kubelet.env`, which is not shippable),
  `Wants=/After=network-online.target containerd.service`. `10-kubeadm.conf` comes from
  the kubeadm package; kubeadm fills `/var/lib/kubelet/{kubeadm-flags.env,config.yaml}`.
- `multi-user.target.d/10-kubernetes.conf`: `Upholds=containerd.service kubelet.service`.
  This is what starts them: presets and `[Install]` symlinks cannot enable units of an
  extension merged during boot (the boot transaction is computed before
  `systemd-sysext.service` runs, and `.wants/` under `/usr` is read-only).
  `system-preset/50-kubernetes.preset` still records the policy for `systemctl preset`.
- `sysctl.d/90-kubernetes.conf` (bridge-nf-call-ip{,6}tables, IPv6 forwarding; the
  kubelet package brings `ip_forward` and `br_netfilter`), `tmpfiles.d/kubernetes.conf`
  (`/etc/cni/net.d`, `/opt/cni/bin`).
- `usr/lib/extension-release.d/extension-release.kubernetes_<kv>`: `ID=arch`
  (the base has neither `SYSEXT_LEVEL` nor `VERSION_ID`, rolling release, so ID alone
  is matched), `SYSEXT_ID=kubernetes`, `SYSEXT_VERSION_ID=<kv>`, `SYSEXT_SCOPE=system`,
  `ARCHITECTURE=x86-64`, `EXTENSION_RELOAD_MANAGER=1` (daemon-reload after the merge so
  the units above are seen).

Version pinning: `mkosi.images/kubernetes/mkosi.version` is the Kubernetes version and
becomes `ImageVersion=`, the file name `kubernetes_<kv>.raw`, `SYSEXT_VERSION_ID` and the
`@v` of the sysupdate transfer. Packages come from the Arch repositories; `mkosi.postinst`
fails the build when the installed kubeadm is not `<kv>`, `verify-kubernetes` runs the
extracted `kubeadm version`/`kubelet --version`. A new Kubernetes version: bump
`mkosi.version` to what `pacman -Si kubeadm` offers, `make kubernetes verify-kubernetes`
(rebuilds only the sysext; `make clean` if the base tree changed), publish
`build/sysupdate/kubernetes/`. The base image is untouched by a Kubernetes bump.

How the VM receives it: vm-manager serves `build/sysupdate/kubernetes/` at
`http://169.254.169.254/giantswarm/v1/sysupdate/kubernetes/` (the directory listing must
show `kubernetes_<kv>.raw`, `SHA256SUMS`, `SHA256SUMS.gpg`) and runs
`systemd-sysupdate --component=kubernetes update` in the guest (the transfer in the base
image is `/usr/lib/sysupdate.kubernetes.d/50-kubernetes.transfer`, `Verify=yes` against
`/etc/systemd/import-pubring.pgp`, target `/var/lib/extensions/kubernetes_@v.raw`,
`InstancesMax=2`). `systemd-sysext.service` (enabled in the base, `Before=sysinit.target
systemd-tmpfiles-setup.service`) merges it on the next boot, or `systemd-sysext refresh`
does it live; the verity signature is checked against `/usr/lib/verity.d/verity.crt`.
The kubeadm unit from CAPI's Ignition config must be ordered `After=systemd-sysext.service`
(docs/design.md, boot flow step 7); until `kubeadm init/join` ran, `kubelet.service`
exits and restarts every 10 s, which is expected and harmless.

PCR 13: systemd-stub measures extension images it loads from the ESP
(`<uki>.efi.extra.d/*.sysext.raw`) into PCR 13; systemd 261 does *not* measure
extensions merged from `/var/lib/extensions`. With the sysupdate delivery used here the
trust anchor is the verity signature (plus `SHA256SUMS.gpg` for the download); if the
attestation policy must cover the Kubernetes version, vm-manager has to either place the
image next to the UKI on the ESP or extend a PCR itself (`systemd-pcrextend`) after the merge.

## Keys

`make keys` (`scripts/gen-keys`) writes into the git-ignored `images/keys/`:

| File | Used for |
|---|---|
| `verity.key`, `verity.crt` | `VerityKey=`/`VerityCertificate=`: signature partition of the verity root hash |
| `pcr.key`, `pcr.crt` | `SignExpectedPcrKey=`/`SignExpectedPcrCertificate=`: `.pcrsig`/`.pcrpkey` in the UKI |
| `gnupg/`, `import-pubring.pgp` | PGP key that signs `SHA256SUMS`; the public keyring is installed as `/etc/systemd/import-pubring.pgp` (systemd-pull executes `gpg` against it, which is why `gnupg` is in the image) |

Keys are never committed. CI generates them per build; vm-manager only ever serves
signed artifacts and never holds private keys. `make keys` is idempotent.

## Verify and smoke boot

`make verify` runs both scripts below. `make verify-base` (`scripts/verify`, offline, unprivileged):

1. GPT via `sfdisk --json`: ESP, root, root-verity, root-verity-sig present and the
   root/verity partition UUIDs equal the two halves of the roothash.
2. `ukify inspect`: `.pcrsig` and `.pcrpkey` present, `.cmdline` pins the roothash.
3. Expected PCR 11 per phase path (`enter-initrd`, `enter-initrd:leave-initrd`,
   `...:sysinit`, `...:ready`) recomputed with `systemd-measure calculate` from the UKI
   sections (extracted with `pefile`), independent of what ukify embedded.
4. `systemd-hwdb query` against `hwdb.bin` extracted from the root erofs
   (`fsck.erofs --extract`) with a `svnGiantSwarm` modalias: `IMDS_VENDOR=giantswarm`.
5. `systemctl --root is-enabled`: `vm-sysinstall.service` enabled, stock
   `systemd-sysinstall.service` masked, `systemd-sysupdate.timer` off, the rest enabled;
   `/etc/systemd/import-pubring.pgp` matches the dev key.
6. Initrd listing: networkd, imdsd, `systemd-imds` and generator, the imds units, the
   repart drop-in, and the giantswarm record in the initrd's `hwdb.bin`.
7. `gpg --verify SHA256SUMS.gpg` with the dev public keyring and `sha256sum -c`.
8. Writes `build/policy.json`.

`make verify-kubernetes` (`scripts/verify-kubernetes`, offline, unprivileged):

1. GPT via `sfdisk --json`: root, root-verity, root-verity-sig and nothing else;
   root/verity partition UUIDs equal the halves of `kubernetes_<kv>.roothash`.
2. `systemd-dissect --validate --image-policy=root=signed:usr=absent` (works without
   root, unlike plain `systemd-dissect`).
3. Signature partition JSON: `rootHash` is ours, `certificateFingerprint` is
   `keys/verity.crt`, and the base tree carries that certificate in `/usr/lib/verity.d/`.
4. `dump.erofs`: compressed; `fsck.erofs --extract`: only `/usr` (plus an empty `/opt`),
   no `/etc`, `/var`, `/usr/lib/os-release`.
5. Exactly one `extension-release.kubernetes_<kv>`, matched against the base
   `os-release` the way systemd-sysext(8) does (`ID`, `SYSEXT_LEVEL`/`VERSION_ID`,
   `ARCHITECTURE`), `SYSEXT_SCOPE` includes `system`, `EXTENSION_RELOAD_MANAGER=1`.
6. Binaries present; `kubeadm version`/`kubelet --version` (run from the extracted
   tree) report `<kv>`.
7. Units, drop-ins, `containerd.toml` (parsed: `SystemdCgroup`, `bin_dirs`), preset,
   sysctl/tmpfiles files, and the base's modules-load/sysctl ordering drop-ins.
8. Real merge: in an unprivileged user namespace (`unshare -Urm`, private mount
   namespace, `/run` on tmpfs for systemd's lock directory) the base tree is bind-mounted
   read-only, the extracted extension is offered as
   `/var/lib/extensions/kubernetes_<kv>/` and `systemd-sysext --root merge` runs;
   `status` must list it, merged `/usr/bin/kubeadm` and the drop-ins must exist next to
   base files, and `systemd-analyze --root verify containerd.service kubelet.service
   multi-user.target` must be clean. This exercises the directory form of the extension
   (the DDI/verity path needs loop devices, i.e. root; it is covered by 1-3 and by
   `--validate`). Skipped with a message if user namespaces are unavailable.
9. The base image's `50-kubernetes.transfer`: `MatchPattern` with `@v` substituted
   names the published file, `Path` is the `kubernetes` component, `Verify=yes`, target
   `/var/lib/extensions`; `gpg --verify SHA256SUMS.gpg`, `sha256sum -c`.

Not verified without a boot: the verity activation in the guest (kernel keyring /
`verity.d` path), the daemon-reload and `Upholds=` start, containerd/kubelet coming up,
the `modules-load`/`sysctl` ordering. That is what a smoke boot with the sysext in
`/var/lib/extensions/` must watch for (see Deviations).

`make smoke-boot` (`scripts/smoke-boot`): `systemd-vmspawn --image=build/smoke.raw
--firmware=uefi --tpm=yes --console=read-only --network-user-mode` (QEMU, OVMF, swtpm)
on a throw-away sparse copy of the image grown to 8G, with three boot credentials: a
fixed `system.machine_id` and a `systemd.extra-unit.*`/`systemd.unit-dropin.*` pair that
powers the VM off after `systemctl is-system-running --wait`. `timeout 180`, console in
`build/smoke.log`, asserts `Reached target Multi-User System` (or a `login:` prompt with
`PROFILE=debug`).

## How vm-manager boots this image

Installer boot (phase A): QEMU with `-kernel build/giantswarm-vm-base_<v>.efi` (direct
UKI boot under OVMF), the `.raw` attached read-only, the blank target disk, swtpm,
`-smbios type=1,manufacturer=GiantSwarm,product=vm-manager,serial=<vm-id>` (this is
what the hwdb glob `dmi:*:svnGiantSwarm:*` matches; without it no IMDS units start)
and SMBIOS type 11 credentials `io.systemd.credential:vm.install-target=/dev/disk/by-id/...`,
`firstboot.hostname`, `ssh.authorized_keys.root`, `system.machine_id`, `vmm.notify_socket`.
`vm-sysinstall.service` partitions the target from `/usr/lib/repart.sysinstall.d/`,
installs the UKI and TPM-encrypted credentials with `bootctl link`, `bootctl install`,
then reboots (run QEMU with `-no-reboot`). The same `system.machine_id` must be passed
in both phases: systemd-repart derives the var partition UUID from the machine ID and
systemd-gpt-auto-generator only mounts var when it matches.

Installed boot (phase B): OVMF -> systemd-boot -> `EFI/Linux/giantswarm-vm-base_<v>.efi`.
In the initrd, `systemd-imds-early-network.service` configures networkd for
169.254.169.254, `systemd-imds-import.service` turns `/hostname` and `/public-keys/0`
into `firstboot.hostname` and `ssh.authorized_keys.root` credentials; the verity root is
set up from `roothash=`, `/` becomes a tmpfs overlay (`systemd.volatile=overlay`), var
is mounted from the disk. Serial console is `ttyS0`; ssh is reachable on AF_VSOCK port 22.

## Deviations from docs/design.md and the systemd/mkosi man pages

- `systemd.volatile=overlay` was added to the kernel command line. With a verity
  erofs root `/etc` is read-only, and `systemd-firstboot.service` has
  `ConditionPathIsReadWrite=/etc`, so credential-driven provisioning (hostname,
  machine ID, ssh key) would silently be skipped. The overlay makes `/etc` writable
  per boot; persistent state is confined to var. Consequence: SSH host keys are
  regenerated on every boot (`sshdgenkeys.service`) until they are persisted in var.
- `systemd.firstboot=off` on the command line: disables only the interactive prompts
  of systemd-firstboot (the unit still runs and applies credentials), needed for a
  headless VM.
- `UnifiedKernelImages=unsigned`: in mkosi 27 `yes` means `signed`, which searches for
  a distro-prebuilt UKI and fails on Arch ("no kernel was found").
- `SplitArtifacts=pcrs` produces no file for a locally built UKI in mkosi 27; the
  expected PCR 11 values come from `scripts/verify` (`systemd-measure calculate`).
- `systemd-dissect --json` cannot run unprivileged here (it needs a user namespace from
  `systemd-nsresourced`, which is disabled on the host); `scripts/verify` reads the GPT
  with `sfdisk --json` and maps the partition type GUIDs to designators instead.
- `KernelInitrdModules=` restricts the kernel-modules initrd to what a VM needs; the
  mkosi default appends every module of the kernel (150 MB) to the UKI.
- Unit enablement is a preset file, not `systemctl enable` in a post-install script:
  mkosi runs `systemctl preset-all` in every image (base tree *and* disk image), which
  reverts symlinks to what the presets say.
- `make smoke-boot` uses `systemd-vmspawn` instead of `mkosi vm`: mkosi 27 passes
  `file.aio=io_uring` to QEMU unconditionally, which fails on hosts with
  `kernel.io_uring_disabled` (this one); vmspawn probes and falls back.
- `systemd-ukify` is not installed in the guest (it pulls in Python; UKIs are built on
  the host and delivered by sysupdate). `gnupg` is installed because `systemd-pull`
  verifies `SHA256SUMS.gpg` by executing `gpg`.
- `mkosi.images/base/` is a `Format=directory` subimage and the disk image is mkosi's
  main image (`BaseTrees=%O/base`) rather than two sibling images; this is what lets a
  sysext subimage overlay the identical base tree.
- The B slots in `repart.sysinstall.d` and the A slots have fixed sizes (2G root, 64M
  verity; systemd-repart sizes every verity signature partition to its fixed 16K
  `VERITY_SIG_SIZE` whatever the definition says) because sysupdate needs pre-existing,
  equally sized slots;
  `CopyBlocks=auto` sizes are not known before install time.
- `repart.sysinstall.d` assumes that `CopyBlocks=auto` carries the source partition
  UUIDs over to the copies (needed for `roothash=` discovery on the installed disk).
  The two-phase install is verified by `make e2e` at the repo root
  (`e2e/install_boot_test.go`), which also checks these UUIDs, not here.
- The installer boot runs without the volatile overlay: a drop-in in the initrd
  (`systemd-volatile-root.service.d/vm-manager.conf`, `ConditionCredential=!vm.install-target`)
  skips `systemd-volatile-root.service` when the install credential is present.
  `CopyBlocks=auto` makes systemd-repart resolve the source partitions from the block
  device behind `/usr`, and under `systemd.volatile=overlay` that is an overlayfs with
  no block device ("Failed to resolve automatic CopyBlocks= path for partition type
  root"). On the plain verity root it walks from `/dev/mapper/root` to the data and
  hash partitions and copies their UUIDs. The installed system keeps the overlay.
- `/usr/lib/vm-manager/sysinstall` exports `KERNEL_INSTALL_CONF_ROOT=/boot/kernel-install`
  and puts `layout=uki` there on a tmpfs over the installer's (empty) `/boot`.
  systemd-sysinstall dissects the freshly written target (erofs root, target ESP on
  `/boot` because the image's `/boot` and `/efi` are both empty and dissect prefers
  `/boot`; gpt-auto mounts the ESP on `/efi` at runtime) and runs kernel-install and
  `bootctl install` against it. Both read `install.conf` from the conf root on the
  installer side, and bootctl persists the entry token it derived (IMAGE_ID, the image
  has no machine ID) to the same path inside the target root, which under the default
  `/etc/kernel` fails with EROFS; the target ESP is the only writable place there.
  Without `layout=uki` kernel-install assumes Type #1 entries and installs the UKI and
  its sealed credentials into `<esp>/giantswarm-vm-base/` with a `loader/entries/*.conf`
  instead of `EFI/Linux/<uki>.efi` plus `<uki>.efi.extra.d/*.cred`, where sysupdate and
  systemd-stub expect them. Consequence: `<esp>/kernel-install/entry-token` exists on
  installed disks. To be dropped once bootctl tolerates a read-only root for a token
  derived from os-release.
- `systemd-firstboot.service.d/vm-manager.conf` applies the static hostname to the
  running kernel (`ExecStartPost=` writing `/proc/sys/kernel/hostname`): with the volatile
  `/etc`, the `/etc/hostname` that systemd-firstboot writes from `firstboot.hostname`
  never reaches PID 1, which read it before firstboot ran, and the file is gone again by
  the next boot; without the drop-in the hostname stays `DEFAULT_HOSTNAME` (`archlinux`)
  unless a separate `system.hostname` credential is passed on every boot.
- `/usr/lib/vm-manager/sysinstall` passes `--reboot=no` and reboots with
  `systemctl reboot` itself: sysinstall's own reboot asks logind
  (`io.systemd.Shutdown`), which cannot start on the read-only installer root
  (`ReadWritePaths=/etc`, `StateDirectory=` under `/var`); systemctl falls back to PID 1.
  The same read-only root makes `sshd`, `systemd-timesyncd`, `systemd-tpm2-setup` and
  `systemd-networkd-persistent-storage` fail during the installer boot, which is
  harmless there (credential sealing uses the SRK from the initrd's early TPM setup).
- `/usr/lib/vm-manager/sysinstall` mounts the installer image's ESP itself instead of
  relying on `/efi` from systemd-gpt-auto-generator: with `-kernel <uki>` (direct boot)
  systemd-stub has no partition to record in `LoaderDevicePartUUID`, and gpt-auto then
  mounts no ESP rather than one it cannot prove it booted from. The script tries
  `/efi`, `/boot`, `/boot/efi` first (a boot through systemd-boot from the image) and
  otherwise mounts every ESP that is not on the target disk read-only under
  `/run/vm-sysinstall/` and searches it for `EFI/Linux/<IMAGE_ID>_<IMAGE_VERSION>*.efi`.

Kubernetes sysext:

- The units of the extension are started through `Upholds=` in a
  `multi-user.target.d/` drop-in, not through the preset file: presets and
  `[Install]` symlinks have no effect on units that appear via a sysext merged at boot
  (transaction already computed, `/usr` read-only). The preset file is kept as
  documentation of the policy and for images that bake the extension in.
- `Format=sysext` uses mkosi's built-in repart definitions, which ignore
  `RepartDirectories=` and set no `Compression=`; the erofs would be 553 MB. mkosi
  passes the image `Environment=` to systemd-repart, so
  `SYSTEMD_REPART_MKFS_OPTIONS_EROFS="-zzstd -Ededupe"` in the subimage config gets the
  compression in (206 MB). `Format=disk` with own definitions was rejected: mkosi would
  then also edit `/usr/lib/os-release`, run depmod etc. into the overlay upper layer.
- `Dependencies=` given on the mkosi command line *appends* to the value in
  `mkosi.conf` (mkosi 27); hence `Dependencies=base` in the config and
  `--dependency=kubernetes` in the Makefile, not the other way round. `mkosi --force`
  removes the outputs of every image in the run including the main image, even with
  `--format=none`; the `kubernetes` target therefore runs without `--force` and deletes
  the previous `build/kubernetes_<kv>*` itself.
- `systemd-modules-load.service` and `systemd-sysctl.service` are ordered after
  `systemd-sysext.service` by two drop-ins in the *base* image (upstream systemd has no
  such ordering; only `systemd-tmpfiles-setup.service` is). Without it the
  `modules-load.d`/`sysctl.d` files of the extension would race the merge.
- `/opt` is excluded from the extension (`mkosi.postinst` removes the `cni-plugins`
  copy under `/opt/cni/bin`; `/usr/lib/cni` remains) so that `/opt/cni/bin` stays
  writable for CNI DaemonSets. containerd searches `/opt/cni/bin` first, `/usr/lib/cni`
  second.
- The `iptables` package is already part of the base tree (`/usr/bin/iptables ->
  xtables-nft-multi`), so it leaves no files in the overlay delta; it stays in the
  package list to keep the extension self-describing.
- `systemd-sysext` 261 does not deduplicate versions: with `InstancesMax=2` in
  `50-kubernetes.transfer`, two files `kubernetes_<a>.raw` and `kubernetes_<b>.raw` in
  `/var/lib/extensions/` would *both* be merged (verified with directory images on the
  host; the later name wins per file). vm-manager must remove the superseded file (or
  the transfer needs `InstancesMax=1`) before `systemd-sysext refresh`/reboot.
- PCR 13 is only extended for extensions loaded by systemd-stub from the ESP, not for
  `/var/lib/extensions/` merges (see Kubernetes sysext above).
- `publish-sysupdate` no longer writes an empty signed manifest for the kubernetes
  component on base-only builds; `build/sysupdate/kubernetes/` exists once the sysext
  was built.
