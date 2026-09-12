# images/ — giantswarm-vm-base

mkosi 27 project that builds the guest image described in
[docs/design.md](../docs/design.md) ("Guest image", "Boot flow"): Arch Linux,
systemd 261, an erofs root protected by signed dm-verity, a UKI with signed
expected PCR 11 values, systemd-boot on the ESP. Everything runs unprivileged
(mkosi's own sandbox and user namespaces), no sudo anywhere.

```
make -C images keys base verify smoke-boot     # full local cycle (~5 min cold, ~1 min warm)
make -C images PROFILE=debug base smoke-boot   # root autologin + empty root password
make -C images clean                           # drop build/, keep keys/
```

## Layout

| Path | Purpose |
|---|---|
| `mkosi.conf` | main image: `Format=disk`, `Bootable=yes`, systemd-boot, UKI, `Verity=signed`, `SignExpectedPcr=yes`, kernel command line, split artifacts, `mkosi vm` runtime settings |
| `mkosi.images/base/` | the OS tree (`Format=directory`, `Output=base`): package list, `mkosi.extra/` content (units, presets, repart and sysupdate definitions, hwdb), `mkosi.postinst` (hwdb compile, PGP pubring, mask). The main image consumes it via `BaseTrees=%O/base` |
| `mkosi.repart/` | partition definitions of the *base image*: ESP 256M, erofs root (zstd), verity hash, verity signature |
| `mkosi.initrd.conf/` | additions to mkosi's default initrd (`InitrdProfiles=network`): the giantswarm hwdb record compiled into the initrd's `hwdb.bin`, a `systemd-repart.service` drop-in |
| `mkosi.profiles/debug/` | `Autologin=yes`, `RootPassword=hashed:` (unlocked, empty) for local iteration; the default build has neither |
| `mkosi.version` | `ImageVersion=` (0.1.0). Bump it per release; sysupdate orders versions with `strverscmp` |
| `scripts/` | `gen-keys`, `hwdb-update` (mkosi postinst), `publish-sysupdate`, `verify`, `smoke-boot` |
| `keys/` (git-ignored) | development signing keys, see below |
| `build/` (git-ignored) | outputs |

Adding the Kubernetes sysext later: create `mkosi.images/kubernetes/mkosi.conf` with
`Format=sysext`, `BaseTrees=%O/base`, `Overlay=yes`, `Dependencies=base`, its
package list (kubeadm, kubelet, containerd, ...), an `extension-release.kubernetes`
matching the base `ID`/`IMAGE_ID`, and add `kubernetes` to `Dependencies=` in the main
`mkosi.conf`. Verity/PCR keys and `ImageId`/`ImageVersion`/`Profiles` are passed down
to subimages by mkosi; nothing in the base image has to move. `scripts/publish-sysupdate`
then only needs to add `kubernetes_<kv>.raw` to `build/sysupdate/kubernetes/`.

## What is built

`make base` runs `mkosi build` and `scripts/publish-sysupdate`:

| Artifact | Content |
|---|---|
| `build/giantswarm-vm-base_<v>.raw` | GPT disk image: ESP (systemd-boot + UKI), root (erofs, `Verity=data`), root-verity, root-verity-sig |
| `build/giantswarm-vm-base_<v>.efi` | the UKI (`.linux`, `.initrd`, `.cmdline`, `.osrel`, `.uname`, `.sbat`, `.pcrpkey`, `.pcrsig`) |
| `build/giantswarm-vm-base_<v>.initrd` | the initrd inside the UKI (default mkosi initrd + network profile + kernel modules initrd) |
| `build/giantswarm-vm-base_<v>.roothash` | verity root hash; also on the UKI command line as `roothash=` |
| `build/giantswarm-vm-base_<v>.root-x86-64{,-verity,-verity-sig}.raw` | split partitions (`SplitArtifacts=partitions`) |
| `build/giantswarm-vm-base_<v>.repart.d/` | the repart definitions that were used |
| `build/sysupdate/base/` | what vm-manager serves at `.../sysupdate/base/`: `<id>_<v>_<root-partuuid>.root.raw`, `<id>_<v>_<verity-partuuid>.verity.raw`, `<id>_<v>.verity-sig.raw`, `<id>_<v>.efi`, `SHA256SUMS`, `SHA256SUMS.gpg` |
| `build/sysupdate/kubernetes/` | empty signed manifest until the sysext exists |
| `build/policy.json` | written by `make verify`: `{image_id, image_version, uki, roothash, partitions, pcr11: {phase_path: hex}}` |
| `build/base/` | the OS tree (input for the main image and for sysexts) |

Root and verity partition UUIDs are derived from the root hash (first/last 128 bits),
which is how `roothash=` locates them; that is why the sysupdate file names carry
the partition UUID (`@u`).

Sizes for 0.1.0: `.raw` 646 MB (472 MB used), `.efi` 90 MB (kernel 17 MB, initrd 73 MB
of which 61 MB is mkosi's default initrd), root erofs (zstd) 400 MB, verity 3.2 MB,
signature 1.9 KB. A warm `make base` (nothing changed) takes ~10 s, a cold one with
package downloads ~3 min; `make verify` ~5 s; `make smoke-boot` ~30 s wall, the guest
reaches multi-user.target after ~7 s.

## Image content (mkosi.images/base/mkosi.extra)

- `/usr/lib/udev/hwdb.d/45-imds-giantswarm.hwdb`: `dmi:*:svnGiantSwarm:*` ->
  `IMDS_VENDOR=giantswarm`, `IMDS_DATA_URL=http://169.254.169.254/giantswarm/v1`,
  `IMDS_ADDRESS_IPV4`, `IMDS_KEY_{HOSTNAME,REGION,ZONE,SSH_KEY,USERDATA}`. Compiled
  into `hwdb.bin` of the root file system and of the initrd (`scripts/hwdb-update`).
- `/usr/lib/repart.sysinstall.d/`: target layout for `systemd-sysinstall`: ESP 512M,
  root A / verity A / verity-sig A with `CopyBlocks=auto` and labels `%M_%A[...]`
  (`giantswarm-vm-base_0.1.0`, `_verity`, `_verity_sig`), empty B slots labelled `_empty`
  with the same fixed sizes (2G / 64M / 4M), `var` (ext4, `SizeMinBytes=1G`, rest of the disk).
- `/usr/lib/repart.d/30-var.conf`: boot-time repart creates var if missing (plain
  smoke boot of the base image; it is then mounted from the next boot on, since
  systemd-gpt-auto-generator already ran) or grows it. On the installed disk var exists
  before the first boot. A drop-in disables `systemd-repart.service` when the
  `vm.install-target` credential is present (installer boot, read-only root disk).
- `vm-sysinstall.service` (enabled, `ConditionCredential=vm.install-target`,
  `ImportCredential=vm.* firstboot.* ssh.* system.*`) runs `/usr/lib/vm-manager/sysinstall`:
  reads the target from `$CREDENTIALS_DIRECTORY/vm.install-target`, finds the booted
  UKI on the ESP (`/efi/EFI/Linux/giantswarm-vm-base_<v>.efi`), forwards every
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

`make verify` (`scripts/verify`, offline, unprivileged):

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
  verity, 4M signature) because sysupdate needs pre-existing, equally sized slots;
  `CopyBlocks=auto` sizes are not known before install time.
- `repart.sysinstall.d` assumes that `CopyBlocks=auto` carries the source partition
  UUIDs over to the copies (needed for `roothash=` discovery on the installed disk).
  The two-phase install is verified by the vm-manager runtime tests, not here.
