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
| `mkosi.images/kubernetes/` | the Kubernetes sysext (`Format=sysext`, `Overlay=yes`): package list (`kubeadm=%v kubelet=%v kubectl=%v`, pinned to the image version), `mkosi.version` (executable: prints `KUBERNETES_VERSION` from the Makefile, so one configuration builds every listed release), `mkosi.extra/` (drop-ins, containerd.toml, sysctl, tmpfiles, preset), `mkosi.postinst` (version guard, drops `/opt`, seeds extension-release) |
| `mkosi.repart/` | partition definitions of the *base image*: ESP 256M, erofs root (zstd), verity hash, verity signature |
| `mkosi.initrd.conf/` | additions to mkosi's default initrd (`InitrdProfiles=network`): the giantswarm hwdb record compiled into the initrd's `hwdb.bin`, a `systemd-repart.service` drop-in, `persistent-etc.service` with its script `usr/lib/vm-manager/persistent-etc` (see [Persistent state](#persistent-state)), the Ignition binary (`ExtraTrees=../build/ignition/tree`) and the `ignition-*` units, see [Ignition](#ignition) |
| `mkosi.profiles/debug/` | `Autologin=yes`, `RootPassword=hashed:` (unlocked, empty) for local iteration; the default build has neither |
| `mkosi.version` | `ImageVersion=` (0.1.0). Bump it per release; sysupdate orders versions with `strverscmp` |
| `scripts/` | `gen-keys`, `build-ignition` (pinned upstream tag into `build/ignition/`), `fetch-kubernetes-packages` (one release's kubeadm/kubelet/kubectl from the Arch Linux Archive into `build/pkgs/<kv>/`, signatures checked), `hwdb-update` (mkosi postinst), `publish-sysupdate <component>` (accumulates versions), `verify` (base), `verify-kubernetes` (every published version), `smoke-boot` |
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
| `build/policy.json` | written by `make verify-base`: `{image_id, image_version, uki, roothash, partitions, pcr11: {phase_path: hex}}`; `make verify-kubernetes` adds `pcr13: {<kubernetes version>: hex}` for every published version (and drops entries of versions no longer in `SHA256SUMS`); `vm-manager image golden` adds `golden: {sha256: {"0": hex, "2": hex, "3": hex, "4": hex, "6": hex, "7": hex, "13": hex}}` (the golden PCRs of a known-good boot; PCR 1 and 5 differ per VM, see [Attestation](#attestation)), which a re-run of `make verify-base` keeps for the same image version. vm-manager's attestation verifier (`internal/attest`) compares PCR 11 with `pcr11[<phase path>]`, PCRs 0, 2-4, 6, 7 with `golden` and, at the ready stage, PCR 13 with `pcr13[<version>]` or golden 13 |
| `build/base/` | the OS tree (input for the main image and for the sysext) |
| `build/ignition/` | `make ignition`: `tree/usr/bin/ignition` (copied into the initrd), `version`, `stamp-<tag>` |

`make kubernetes` (one version, `KUBERNETES_VERSION`), `make kubernetes-all` (every version of
`KUBERNETES_VERSIONS`) and `make images` (the default version) add, via
`scripts/fetch-kubernetes-packages` and `scripts/publish-sysupdate kubernetes`:

| Artifact | Content |
|---|---|
| `build/pkgs/<kv>/` | the release's `kubeadm`, `kubelet`, `kubectl` `.pkg.tar.zst` from the Arch Linux Archive (`signatures/` next to them, `.verified` stamp), mkosi's local repository for the sysext build |
| `build/kubernetes_<kv>.raw` | the sysext DDI: root (erofs, zstd, `Verity=data`), root-verity, root-verity-sig; no ESP |
| `build/kubernetes_<kv>.roothash` | its verity root hash |
| `build/sysupdate/kubernetes/` | what vm-manager serves at `.../sysupdate/kubernetes/`: one `kubernetes_<kv>.raw` per published version, `SHA256SUMS` over all of them, `SHA256SUMS.gpg`. Publishing a version replaces only that version's file (`publish-sysupdate` accumulates, for `base` too) |

Root and verity partition UUIDs are derived from the root hash (first/last 128 bits),
which is how `roothash=` locates them; that is why the sysupdate file names carry
the partition UUID (`@u`).

Sizes for 0.1.0: `.raw` 651 MB (488 MB used), `.efi` 102 MB (kernel 17 MB, initrd 84 MB
of which 61 MB is mkosi's default initrd, 6.9 MB Ignition and 3.9 MB `vm-agent`), root erofs (zstd) 400 MB, verity 3.2 MB,
signature 1.9 KB. A warm `make base` (nothing changed) takes ~10 s, a cold one with
package downloads ~3 min; `make verify-base` ~5 s; `make smoke-boot` ~30 s wall, the guest
reaches multi-user.target after ~7 s. The kubernetes sysext for 1.36.4 is 206 MB
(erofs zstd; 484 MB uncompressed, of which `/usr/bin` is 411 MB of Go binaries);
`make kubernetes verify-kubernetes` on a built base tree takes ~20 s with a warm package
cache, `make images verify` (everything, warm) ~30 s.

## Image content (mkosi.images/base/mkosi.extra)

- `/usr/lib/udev/hwdb.d/45-imds-giantswarm.hwdb`: `dmi:*:svnGiantSwarm:*` ->
  `IMDS_VENDOR=giantswarm`, `IMDS_DATA_URL=http://169.254.169.254/giantswarm/v1`,
  `IMDS_ADDRESS_IPV4`, `IMDS_KEY_{HOSTNAME,REGION,ZONE,SSH_KEY}`. Compiled
  into `hwdb.bin` of the root file system and of the initrd (`scripts/hwdb-update`).
  `/user-data` is deliberately not an hwdb key: Ignition fetches it through
  `ignition.config.url`, and `systemd-imds --import` must not see the 503 the IMDS answers
  while the key is gated by attestation.
- `/usr/bin/vm-agent` (from `make agent` at the repo root, static, 9.8 MB) with
  `vm-agent-attest.service` (ready stage, enabled by the preset); the initrd carries the
  same binary with the initrd-stage unit. See [Attestation](#attestation).
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
  and var (mounted by the initrd from the next boot on; that boot runs on a tmpfs, see
  [Persistent state](#persistent-state)) and grows var. A drop-in disables `systemd-repart.service` when the
  `vm.install-target` credential is present (installer boot, read-only root disk).
- `/var` is empty in the image (`RemoveFiles=/var/*`): the var partition is mounted over
  it in the initrd (`persistent-etc.service`, see [Persistent state](#persistent-state)),
  together with the `/etc` overlay and the `/root` and `/opt` bind mounts.
  `tmpfiles.d/vm-manager.conf` recreates `/var/empty` (sshd), `/var/lock` and
  `/var/log/journal` on it; the rest comes from systemd's own tmpfiles catalog.
- `tmpfiles.d/vm-manager-exitrd.conf`, `run-initramfs-root.mount` and `run-initramfs-usr.mount`
  populate `/run/initramfs` with the exitrd that lets systemd-shutdown(8) release the `/etc`
  and `/usr` overlays at power-off; `system-shutdown/vm-manager-var` then waits for the
  kernel's asynchronous tear-down of the sysext's loop device to put var's superblock, so
  that var is unmounted cleanly, see [Persistent state](#persistent-state).
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
- `vm-kubernetes.service` (enabled, `ConditionCredential=!vm.install-target`,
  `ConditionKernelCommandLine=!systemd.imds=no`,
  `After=network-online.target systemd-imds-import.service systemd-sysext.service
  systemd-tpm2-setup.service`, wanted by `multi-user.target` so `READY=1` waits for it)
  runs `/usr/lib/vm-manager/kubernetes` once per boot: reads `/kubernetes-version` from
  the IMDS, pulls that sysext with `systemd-sysupdate --component=kubernetes update <v>`,
  merges it with `systemd-sysext refresh` and measures it into PCR 13 with
  `systemd-pcrextend` (details under [Kubernetes sysext](#kubernetes-sysext)).
- `vm-report-upload.timer` (every 15 s) runs
  `systemd-report upload --url=http://169.254.169.254/giantswarm/v1/report --key=- --network-timeout=5`.
- `/usr/lib/systemd/system-preset/10-vm-manager.preset` enables `systemd-networkd`
  (DHCP on every ethernet link), `systemd-resolved`, `systemd-timesyncd`,
  `systemd-imdsd.socket`, `systemd-sysext.service`, `sshd.service` (plus
  `systemd-ssh-generator`'s AF_VSOCK port 22 listener), `vm-sysinstall.service`,
  `vm-kubernetes.service`, `vm-report-upload.timer`, and disables
  `systemd-sysupdate*.timer`, `systemd-homed`,
  `machines.target`. `systemd-pcrphase*` are the systemd defaults.
- Kernel command line: `console=ttyS0,115200 systemd.imds.import=yes systemd.firstboot=off`
  plus `roothash=<hash>` added by mkosi.
- `systemd-modules-load.service.d/` and `systemd-sysctl.service.d/vm-manager.conf`:
  `After=systemd-sysext.service`, so `modules-load.d/` and `sysctl.d/` entries of a
  merged extension are applied (upstream has no such ordering).
- `network/70-kubernetes-cni.network`: `Unmanaged=yes` for veth and dummy devices
  (nspawn's `ve-*`/`vb-*` excepted). They are `Type=ether` like the virtio NIC, and a
  managed link is detached from a master its `.network` does not name (see `KeepMaster=`
  in systemd.network(5)): the first cluster e2e saw every pod veth removed from `cni0`,
  the bridge down and no pod able to reach the API server. CNI bridges, vxlan and ipip
  devices have their own `Type=` and never matched `80-vm-manager.network`.

## Ignition

Ignition v2.27.0 runs in the initrd on the first installed boot and applies the
`user_data` of `create_vm` (CAPI's bootstrap Secret with `format: ignition`: files and
systemd units) to the real root before the switch root. Attestation is not Ignition's
job; it only sees the IMDS answer.

Build: Arch has no Ignition package, so `make ignition` (a prerequisite of `base` and
`images`) runs `scripts/build-ignition build/ignition v2.27.0`: a shallow clone of the
upstream tag, `go build` of `./internal` with the vendored modules,
`-X .../internal/version.Raw=<tag>` and `-X .../internal/distro.selinuxRelabel=false`
(the upstream default assumes an SELinux distro and makes the files stage abort on a root
without `/etc/selinux/config`), cached as `build/ignition/tree/usr/bin/ignition`
with a `stamp-<tag>` (bump `IGNITION_VERSION` in the Makefile to rebuild). The binary is
linked dynamically against glibc, `libblkid` and `libresolv`: `internal/as_user` and the
blkid helper are cgo-only and Arch ships no static libblkid; the initrd is built from the
same Arch packages (systemd depends on glibc and util-linux-libs) and `scripts/verify`
checks every `NEEDED` library against the initrd listing. It reaches the initrd through
`ExtraTrees=../build/ignition/tree:/` in `mkosi.initrd.conf/mkosi.conf` and adds 6.9 MB
to the (zstd) initrd, 23 MB uncompressed.

Units (`mkosi.initrd.conf/mkosi.extra/usr/lib/systemd/system/`), ported from upstream's
`dracut/30ignition/` for a systemd initrd without dracut:

| Unit | Ordering | Does |
|---|---|---|
| `ignition-complete.target` | `Before=initrd.target`, required by it (`initrd.target.requires/`); `Conflicts=initrd-switch-root.target` | synchronization point; requires the four stages below |
| `ignition-fetch.service` | `After=basic.target network-online.target systemd-imds-early-network.service systemd-imds-import.service`, `Wants=network-online.target`, `Before=ignition-disks.service` | `--stage=fetch`: GETs `ignition.config.url`, caches `/run/ignition.json` |
| `ignition-disks.service` | `After=ignition-fetch.service systemd-udevd.service`, `Before=ignition-mount.service` | `--stage=disks`: partitions/file systems of the config (none for CAPI) |
| `ignition-mount.service` | `Requires=initrd-root-fs.target`, `After=initrd-root-fs.target systemd-volatile-root.service network.target`, `Before=ignition-files.service initrd-switch-root.target`, `ExecStop=--stage=umount` | `--stage=mount`: mounts the config's file systems under `/sysroot` |
| `ignition-files.service` | `After=ignition-mount.service initrd-root-fs.target systemd-volatile-root.service sysroot-var.mount`, `Before=initrd-parse-etc.service` | `--stage=files --root=/sysroot`: files, units, presets |

All four services carry `ConditionKernelCommandLine=ignition.firstboot` (the target does
not: a condition on the target would still start its dependencies), pass
`--platform=metal` and `--log-to-stdout` with `StandardOutput=journal+console`, so the
attempts show up in `get_vm_console` and `journalctl -u 'ignition-*'`, and have
`OnFailure=emergency.target` like the rest of the initrd. Not ported: `fetch-offline`
(this initrd always has the network up for the IMDS), `kargs`, the LUKS/cex bits and the
generator (`/run/ignition.env` only carried the platform id). Two deliberate ordering
differences: `ignition-disks.service` is not `Before=sysroot.mount` (the verity root cannot
be reformatted, and that ordering would serialise every boot's root mount behind DHCP,
because ordering also applies to condition-skipped jobs), and there is no
`ignition-remount-sysroot.service` (`/sysroot/etc`, `/sysroot/var`, `/sysroot/root` and
`/sysroot/opt` are writable through the mounts of `persistent-etc.service`).

Command line: the UKI carries `ignition.platform.id=metal
ignition.config.url=http://169.254.169.254/giantswarm/v1/user-data`. `ignition.firstboot`
is not in the image: vm-manager passes it through the stub's
`io.systemd.stub.kernel-cmdline-extra` SMBIOS string on the first installed boot only
(`internal/vm/lifecycle.go`), so every later boot reaches `ignition-complete.target` with
the stages skipped and `/run/ignition.json` absent. `systemd-imds --import` no longer
asks for `/user-data` (the key left the hwdb record).

IMDS status codes (see `internal/imds` and docs/design.md): 200 with the raw Ignition JSON
once released, 503 + `Retry-After` while gated by attestation (Ignition retries every
status >= 500 with 200 ms..5 s backoff for `--fetch-timeout`, 2 min; then the fetch fails
and the boot lands in `emergency.target`), 204 when the VM has no user-data (an empty body
is "no config", the stages finish with nothing to do). The initrd stage of the
attestation agent runs `Before=ignition-fetch.service` (`vm-agent-attest.service` plus the
`ignition-fetch.service.d/10-vm-agent-attest.conf` drop-in, see
[Attestation](#attestation)): its verified quote is what turns the 503 into 200.

What Ignition writes lands on the var partition: `persistent-etc.service` (see
[Persistent state](#persistent-state)) mounts var, the `/etc` overlay and the `/root` and
`/opt` bind mounts under `/sysroot` before `initrd-root-fs.target`, which
`ignition-mount.service` and `ignition-files.service` are ordered after (their
`After=systemd-volatile-root.service` is a no-op now; `persistent-etc.service` took that
slot). Files below `/var`, `/etc`, `/root` and `/opt` therefore persist across reboots;
the rest of the root is the read-only erofs and a file there fails with EROFS.

## Attestation

`vm-agent` (`cmd/vm-agent`, `make agent` at the repo root: `CGO_ENABLED=0`, checked
static, 9.8 MB) is staged by `images/Makefile` (`agent` target, a prerequisite of `base`
and `images`) into `build/agent/tree/usr/bin/vm-agent` and copied by `ExtraTrees=` into
both the base tree (`mkosi.images/base/mkosi.conf`) and the initrd
(`mkosi.initrd.conf/mkosi.conf`); it adds 3.9 MB to the zstd initrd (9.8 MB uncompressed),
84 MB in total for 0.1.0. `go build` is incremental, so every image build rebuilds it; a
changed agent changes the initrd and with it PCR 11. It speaks to the vTPM at
`/dev/tpmrm0` and to the IMDS; `docs/design.md` "Attestation protocol" has the wire
format, `internal/attest` the verifier.

Two units of the same name, `vm-agent-attest.service`, one per stage:

| Where | Ordering | Runs |
|---|---|---|
| initrd (`mkosi.initrd.conf/mkosi.extra`) | `initrd.target.wants/`; `After=basic.target network-online.target systemd-imds-early-network.service systemd-imds-import.service tpm2.target systemd-tpm2-setup-early.service systemd-pcrphase-initrd.service`, `Before=ignition-fetch.service ignition-complete.target`; `ignition-fetch.service.d/10-vm-agent-attest.conf` adds `Wants=`+`After=vm-agent-attest.service` to the fetch stage | `vm-agent attest --stage=initrd --timeout=90s`: nonce, quote of PCRs 0-7 and 11, posted with the firmware and userspace event logs. Every installed boot, not only the first: vm-manager gates user-data again on every boot |
| root (`mkosi.images/base/mkosi.extra`, preset-enabled) | `WantedBy=multi-user.target`, `Before=multi-user.target`; `After=network-online.target systemd-imds-import.service systemd-tpm2-setup.service vm-kubernetes.service systemd-pcrphase.service` | `vm-agent attest --stage=ready --timeout=90s`: PCRs 0-7, 11 and 13 once the sysext is merged and measured and PCR 11 carries the full phase path; `READY=1` follows it |

Why the ordering matters: `systemd-pcrphase-initrd.service` extends PCR 11 with
`enter-initrd` when it starts and with `leave-initrd` when it stops, which happens at the
switch root (`Conflicts=initrd-switch-root.target`), after every unit of `initrd.target`;
the initrd quote therefore sees exactly the `enter-initrd` value `scripts/verify` wrote to
`policy.json`. `systemd-pcrphase.service` extends `ready` in the root, so the ready quote
matches `enter-initrd:leave-initrd:sysinit:ready`. `vm-kubernetes.service` measures the
sysext into PCR 13 before the ready quote.

Conditions, the same on both units: `ConditionCredential=!vm.install-target` (the
installer boot has nothing to attest), `ConditionKernelCommandLine=!systemd.imds=no`
(no IMDS) and `ConditionFirmware=smbios-field(sys_vendor = GiantSwarm)`, the SMBIOS
vendor vm-manager sets and the hwdb record keys on: a boot of the image elsewhere
(`make smoke-boot`, `mkosi vm`) skips the units instead of waiting `--timeout` for an
IMDS nobody serves.

Exit codes and failure: the agent exits 0 when the verifier accepted the quote, 2 when
it rejected it (the reason is printed, e.g. `golden mismatch: pcr 0 expected ..., got
...`), 1 on any other error (no IMDS within `--timeout`, no TPM). 2 and 1 fail the unit,
visibly (`StandardOutput=journal+console`, so the verdict is in `get_vm_console` and
`journalctl -u vm-agent-attest.service`), but not the boot: no `OnFailure=`, no
`Restart=` (re-quoting the same PCRs cannot change the verdict). With
`require_attestation` the IMDS keeps answering `/user-data` with 503, Ignition's fetch
stage retries until its 2-minute timeout and the boot lands in `emergency.target`;
without it user-data was released when the boot started and the boot proceeds with the
failed unit on record. `get_vm_attestation` shows both stages either way.

Golden values: `policy.json` from `make verify` has `pcr11` and `pcr13` only. The
firmware PCRs a golden value can pin across VMs are 0 (firmware code), 2 and 3 (option
ROMs), 4 (boot loader and UKI), 6 (os-separator only) and 7 (Secure Boot policy). PCR 1
and 5 are quoted and recorded but not compared: EDK2 measures the SMBIOS tables into
PCR 1, and the type 11 strings vm-manager passes are per-VM credentials (hostname,
machine ID, SSH key, notify socket), as is the `Boot####` entry with the ESP's partition
GUID; PCR 5 holds the GPT of the installed disk with its per-install partition UUIDs.
Bring-up of a new image or firmware: start vm-manager with
`--attestation-learn-golden`, boot one VM, `vm-manager image golden giantswarm-vm-base
--from-vm <id> --image-dir <dir>`, restart without the flag; from then on the default
`--attestation=verify` rejects a boot on other firmware (`e2e/attestation_test.go` proves
this with `OVMF_CODE.secboot.4m.fd`: `golden mismatch` on PCR 0 and 7, user-data gated,
Ignition in its fetch loop).

## Persistent state

The root stays the read-only, dm-verity protected erofs. What has to be writable lives on
the var partition and is mounted into place by the initrd before it hands over:
`mkosi.initrd.conf/mkosi.extra/usr/lib/vm-manager/persistent-etc`, run by
`persistent-etc.service` (`initrd-root-fs.target.wants/`, `After=sysroot.mount
systemd-repart.service`, `Before=initrd-root-fs.target systemd-sysext-sysroot.service
systemd-confext-sysroot.service`), which is the slot systemd-volatile-root.service(8)
fills for `systemd.volatile=`.

| Path | Mount | Backing |
|---|---|---|
| `/var` | ext4 from `/dev/disk/by-partlabel/var` (systemd-repart labels a partition after its type when the definition sets no `Label=`), `systemd-fsck` before, `systemd-growfs` after (`x-systemd.growfs` is fstab-only) | partition 8 of the installed disk |
| `/etc` | overlayfs, `lowerdir=` the image's `/etc`, `upperdir=`/`workdir=` on var | `/var/lib/etc-overlay/{upper,work}` |
| `/root` | bind mount (systemd's `tmpfiles.d/provision.conf` writes `/root/.ssh/authorized_keys` from `ssh.authorized_keys.root`) | `/var/roothome` |
| `/opt` | bind mount (CNI DaemonSets install into `/opt/cni/bin`) | `/var/opt` |

- `systemd-firstboot` (host, first boot) writes `/etc/hostname` and friends into the
  overlay, PID 1 commits the `system.machine_id` credential to `/etc/machine-id`, sshd's
  host keys (`sshdgenkeys.service`) are generated once; all of it survives reboots, as do
  units enabled with `systemctl enable` and whatever a provisioning stage in the initrd
  (ordered after `persistent-etc.service`) writes to `/sysroot/etc`. `ConditionFirstBoot=`
  holds on the first boot only (`/etc/machine-id` is `uninitialized` in the image), so
  `systemd-firstboot.service` runs once.
- An empty upper hides nothing: the merged `/etc` starts out as the image's `/etc`.
- `/home`, `/srv` and `/usr/local` stay read-only (nothing in the image uses them);
  another writable directory is one `bind` line in the script. `/root` and `/opt` are
  bind mounts rather than symlinks into `/var` because systemd-tmpfiles 261 refuses
  `d` lines on symlinks ("already exists and is not a directory").
- Without a var partition (a plain boot of the base image; the partition is created by
  systemd-repart on the host, so it exists from the second boot on) or with one that
  fails fsck or mount, the script mounts a tmpfs on `/var`, logs it and continues: that
  boot is volatile like the former `systemd.volatile=overlay`, the next one persistent.
  The installer boot (`vm.install-target` credential) skips the unit altogether, see
  Deviations.
- `make e2e` (`e2e/persistent_etc_test.go`) proves the installed system: a file and an
  enabled unit written to `/etc` survive `reboot_vm`, the unit runs on the new boot, SSH
  host key and machine ID are unchanged, `/etc` is the overlay and the file shows up
  under `/var/lib/etc-overlay/upper`, `/root` and `/opt` are bound from the partition.
- systemd-gpt-auto-generator(8) finds `/var` already mounted on the host and does nothing;
  systemd-confext-sysroot.service(8), ordered after the unit, sees `/sysroot/var/lib/confexts`.
- Shutdown: the overlays are what stand between systemd-shutdown(8) and a clean var. The
  service manager treats `/etc` and `/usr` as extrinsic and never unmounts them (it does
  unmount `/var`, `/root` and `/opt` at `umount.target`), and systemd-shutdown's own
  `umount /etc` fails with EBUSY without any open file: glibc keeps `/etc/ld.so.cache`
  mmap'd for the lifetime of every process, a mapping through overlayfs pins the overlay
  mount (the backing file holds the user-visible path since Linux 6.6), and PID 1 is the
  process that survives. The Kubernetes sysext adds a second pin of the same kind: its
  overlay on `/usr` has the image on var as a lower layer (loop device plus dm-verity), and
  PID 1 runs from that overlay, so neither the verity nor the loop device can be detached.
  While an overlay with a layer on var exists, var's ext4 superblock stays alive with a
  dirty journal, and the next boot's fsck logs `var: recovering journal`. The fix is
  systemd's exitrd (bootup(7)): `tmpfiles.d/vm-manager-exitrd.conf` populates
  `/run/initramfs` on every boot with an `/etc/initrd-release` marker, a `lib64` symlink
  and a `shutdown` script that re-executes `/usr/lib/systemd/systemd-shutdown`;
  `run-initramfs-root.mount` and `run-initramfs-usr.mount` (enabled by the preset) bind
  the root file system below its overlays (a private, non-recursive bind of `/`) and its
  `usr/` as the exitrd's `/usr`, so that the exitrd runs the image's own binaries, not the
  sysext overlay's. systemd-shutdown pivots into it after its own unmount loop gave up; the
  fresh instance has no `/etc/ld.so.cache` to map and nothing from the `/usr` overlay, so
  nothing pins the overlays any more: its unmount loop releases them. Mounts below
  `/run/initramfs` are extrinsic to the manager and skipped by systemd-shutdown's first
  instance (`nonunmountable_path`), so the exitrd survives until the pivot.
  `e2e/persistent_etc_test.go` asserts that the rebooted system's fsck output has no
  `recovering journal`. Diagnosed with a debug `systemd-shutdown` and a `system-shutdown/`
  hook dumping `/proc/self/mountinfo` and the survivors in the final phase (only PID 1 and
  kernel threads; `jbd2/vda8-8` still alive), and reproduced on the host: an `mmap` through
  an overlayfs whose fd is closed makes `umount` of the overlay EBUSY, a mapping reached
  through a symlink to a file outside the overlay does not.
- Shutdown, the asynchronous rest (`system-shutdown/vm-manager-var`, exitrd part 3): the
  exitrd's unmount loop releasing the `/usr` overlay is not what puts var's superblock. The
  overlay was the last user of the Kubernetes image's dm-verity device (deferred removal)
  and loop device (autoclear); the kernel tears both down on workqueues, and only that
  closes the image file on var, by then the last reference to var's superblock: `/var`
  itself is unmounted by the service manager at `umount.target` (the loop device was set up
  in systemd-sysext's own mount namespace and never pinned the mount), `/root` and `/opt`
  too. systemd-shutdown does not wait for work it did not start: it logs `All loop devices
  detached`, syncs and calls reboot(2) a few milliseconds later, and when `ext4_put_super`
  has not run by then var's journal stays marked dirty. Measured with a `system-shutdown/`
  hook: right after the loop `/sys/fs/ext4/vda8` still exists and the `jbd2` thread is
  alive, 10-20 ms later both are gone; with the manager's debug log on the console the
  window never showed, which is why PR #30's run passed. `vm-manager-var` is that wait:
  systemd-shutdown runs `system-shutdown/` after its loop and before the sync, in both
  instances, and in the exitrd one (`/etc/initrd-release`) the hook polls for
  `/sys/fs/ext4/<var device>` to disappear (10 ms steps, 5 s bound, a console line when it
  does not). Boots after the first do not need it for the sysext: `/var/lib/extensions/` is
  populated at sysinit, `systemd-sysext.service` merges and its `ExecStop=systemd-sysext
  unmerge` (ordered after everything `After=sysinit.target`) releases the sysext during the
  manager's shutdown; on the first boot `vm-kubernetes.service` merged with `systemd-sysext
  refresh` and the unit is inactive, and on a node with pods the shims exec'd from the
  overlay live until systemd-shutdown kills them, so the exitrd tear-down is the general
  path.

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
  extension (`mkosi.postinst` removes the package's `/opt/cni/bin` copy): `/opt` is a
  bind mount of `/var/opt` (see [Persistent state](#persistent-state)), so CNI DaemonSets
  can install into `/opt/cni/bin` and keep it across reboots, which a sysext overlay on
  `/opt` would make read-only.
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
  (`/etc/cni/net.d`, `/opt/cni/bin`). The links a CNI creates are left to it by the base
  image's `network/70-kubernetes-cni.network` (see [Image content](#image-content-mkosiimagesbasemkosiextra)); the
  cluster e2e checks `networkctl list` reports `cni0`, `flannel.1` and the pod veths as
  unmanaged.
- `usr/lib/extension-release.d/extension-release.kubernetes_<kv>`: `ID=arch`
  (the base has neither `SYSEXT_LEVEL` nor `VERSION_ID`, rolling release, so ID alone
  is matched), `SYSEXT_ID=kubernetes`, `SYSEXT_VERSION_ID=<kv>`, `SYSEXT_SCOPE=system`,
  `ARCHITECTURE=x86-64`, `EXTENSION_RELOAD_MANAGER=1` (daemon-reload after the merge so
  the units above are seen).

Versions: `KUBERNETES_VERSIONS` in the Makefile (newest first, `1.36.4 1.35.4`) lists the
releases that are built; `KUBERNETES_VERSION` (default: the first) is the one `make
kubernetes` builds and `make images` builds in the same mkosi run as the base image, `make
kubernetes-all` builds every one and `make` (all) does both. The version becomes the
sysext's `ImageVersion=` through `mkosi.images/kubernetes/mkosi.version`, an executable that
prints the exported `KUBERNETES_VERSION` (mkosi runs an executable `mkosi.version`), and
with it the file name `kubernetes_<kv>.raw`, `SYSEXT_VERSION_ID` and the `@v` of the
sysupdate transfer. The base image is untouched by any of this.

Where the packages come from: the official Arch repositories carry one Kubernetes release
at a time (`pacman -Si kubeadm`), the [Arch Linux Archive](https://archive.archlinux.org/packages/)
keeps every build ever published. `scripts/fetch-kubernetes-packages <kv> build/pkgs/<kv>`
(target `kubernetes-packages`) reads the archive's index for `kubeadm`, `kubelet` and
`kubectl`, downloads the highest pkgrel of `<kv>` with its `.sig`, and verifies the
signature against the Arch packagers' keyring (`/usr/share/pacman/keyrings/archlinux.gpg`,
the `archlinux-keyring` package; keys on `archlinux-revoked` are refused) because mkosi's
local repository is `SigLevel = Never`. The directory is handed to mkosi as
`--volatile-package-directory=` (a local pacman repository that precedes the official
ones; "volatile" keeps it out of the incremental cache manifest, so the cached base tree
survives a version switch), and the subimage pins `kubeadm=%v kubelet=%v kubectl=%v`: a
release the directory lacks fails the build instead of silently installing what the
repositories offer, and `mkosi.postinst` still refuses an installed kubeadm that is not
`<kv>`. `containerd`, `runc`, `crictl` and `cni-plugins` come from the current repositories
(the same set for every release: both sysexts of this repository state carry
containerd 2.3.5, runc 1.5.1, crictl 1.36.0, cni-plugins 1.9.1; `verify-kubernetes` prints
what a sysext holds).

Adding a release: append it to `KUBERNETES_VERSIONS` (or pass `make kubernetes
KUBERNETES_VERSION=<kv>` once), `make kubernetes-all verify-kubernetes` (or `make`);
`build/sysupdate/kubernetes/` then serves the new file next to the old ones,
`SHA256SUMS` is regenerated over all of them and re-signed, `build/policy.json` gains
`pcr13.<kv>`, and vm-manager's catalog (`internal/images`, `get_image`) lists it. Retiring a
release: remove it from the list, delete `build/sysupdate/kubernetes/kubernetes_<kv>.raw`,
publish any version (`make kubernetes`) so the manifest no longer names it, and
`verify-kubernetes` drops its `pcr13` entry. `make kubernetes` for a version rebuilds only
that sysext (`make clean` if the base tree changed) and replaces only that version's file.

How the VM receives it: vm-manager serves `build/sysupdate/kubernetes/` at
`http://169.254.169.254/giantswarm/v1/sysupdate/kubernetes/` (every published
`kubernetes_<kv>.raw`, `SHA256SUMS`, `SHA256SUMS.gpg`, unfiltered) and `/kubernetes-version`
with the version the VM was created with (`create_vm` accepts only a version the manifest
lists, the newest is the default). `vm-kubernetes.service` (base image, enabled, `WantedBy=multi-user.target`,
`After=network-online.target systemd-imds-import.service systemd-sysext.service
systemd-tpm2-setup.service`, skipped on the installer boot by
`ConditionCredential=!vm.install-target` and on a boot with `systemd.imds=no` on the
kernel command line by `ConditionKernelCommandLine=`, the switch systemd-imds-generator
honours; without it `systemd-imds` would socket-activate `systemd-imdsd` against an
address nobody serves, as the harness-driven `TestInstallBoot` showed) runs
`/usr/lib/vm-manager/kubernetes` once per boot:

1. `systemd-imds /kubernetes-version`. `KeyNotFound` (VM without a Kubernetes version)
   and `NotSupported` (no IMDS provider matched, e.g. `scripts/smoke-boot`) end the unit
   successfully with nothing to do; any other error fails it.
2. Every other `kubernetes_*.raw` in `/var/lib/extensions/` is deleted: systemd-sysext
   merges every image it finds, so exactly one version may be present.
3. `systemd-sysupdate --component=kubernetes update <kv>` (the transfer is
   `/usr/lib/sysupdate.kubernetes.d/50-kubernetes.transfer`, `Verify=yes` against
   `/etc/systemd/import-pubring.pgp`, target `/var/lib/extensions/kubernetes_@v.raw`);
   a present file is a no-op, so a reboot downloads nothing.
4. `systemd-sysext refresh` merges it (the verity signature is checked against
   `/usr/lib/verity.d/verity.crt`; the kernel's "Required key not available" lines in the
   journal are the keyring attempt before that). `EXTENSION_RELOAD_MANAGER=1` makes the
   refresh a daemon-reload, after which `Upholds=` of `multi-user.target` starts
   `containerd.service` and `kubelet.service`. The unit checks that
   `systemd-sysext status` lists exactly `kubernetes_<kv>` on `/usr`.
5. PCR 13 is extended, see below.
6. The node stack is made usable: `systemd-modules-load.service` and
   `systemd-sysctl.service` are restarted, because on a boot that downloaded the
   extension they ran before it existed (the `After=systemd-sysext.service` ordering
   only helps on later boots), so `br_netfilter`, `net.ipv4.ip_forward` and
   `bridge-nf-call-iptables` from the extension's `modules-load.d/` and `sysctl.d/`
   apply on the first boot too; and `systemctl start containerd.service` waits for the
   `Upholds=` start, so the CRI socket is there. Without this step kubeadm's preflight
   on the first boot fails with `[ERROR CRI]` (no `/run/containerd/containerd.sock`
   yet) and `[ERROR FileContent--proc-sys-net-ipv4-ip_forward]`, as
   `e2e/kubernetes_cluster_test.go` showed.

In the e2e (`e2e/kubernetes_sysext_test.go`, `make e2e`) the download of the 206 MB takes
about 2 s, the unit 2.7 s, and the installed boot reaches `READY=1` (which waits for the
unit) after 14 s. Units that need the extension or the measurement order
`After=vm-kubernetes.service`: the kubeadm unit from CAPI's Ignition config
(docs/design.md, boot flow step 7) and the ready-stage attestation quote; step 6 is
what makes that one ordering line enough for `kubeadm init|join`
(`e2e/kubernetes_cluster_test.go` bootstraps a control plane and joins a worker that
way). Until `kubeadm init/join` wrote `/var/lib/kubelet/config.yaml`, `kubelet.service`
exits and `Restart=always` brings it back every 10 s: `activating (auto-restart)`, never
`failed`, so `systemctl --failed` stays empty; the e2e pins that.

Lifecycle: `InstancesMax=2` in the transfer is the minimum sysupdate.d(5) accepts, hence
step 2 (a superseded file is removed before the refresh, never left for a later boot).
Several served versions change nothing for the guest: `systemd-sysupdate update <kv>`
installs the named version only ("Selected update '1.35.4' is not the newest, proceeding
anyway" in the journal when a newer one is served), the vacuum finds nothing to remove on a
fresh VM, and the merge sees the one file. `e2e/kubernetes_versions_test.go` creates one
VM per published version and checks each pulled, merged and measured its own
(`systemd-sysupdate list` shows the other as a candidate, never on disk), and that an
unpublished version is refused by `create_vm`.
A changed `/kubernetes-version` takes effect on the next boot: the early
`systemd-sysext.service` still merges the old file, the unit then replaces it and
refreshes, and PCR 13 carries only the new version because the unit measures once, after
the final merge. A version switch while the node runs is not supported (the running
containerd/kubelet would be swapped underneath).

PCR 13. systemd-stub measures only extension images it loads itself from the ESP
(`<uki>.efi.extra.d/*.sysext.raw`) into PCR 13, and those land in `/.extra/sysext/` of
the *initrd*, a directory systemd-sysext searches only when invoked in the initrd: they
extend the initrd, not the installed root, so delivering the Kubernetes layer that way
would not merge it at all. The measured object would also be the cpio archive the stub
packs (a host would have to reproduce it byte for byte), the 206 MB would sit on the
512 MB ESP next to the UKIs, and the first installed boot would need the installer to
place it. The unit therefore measures after the merge with
`systemd-pcrextend --pcr=13 "<line>"`, where `<line>` is what `sha256sum
kubernetes_<kv>.raw` prints in `/var/lib/extensions`, i.e. the file's line in the
published `SHA256SUMS`: `<sha256>  kubernetes_<kv>.raw` (two spaces, no newline).
The PCR 13 rule, implemented by `scripts/verify-kubernetes` (step 10, Python) and
`attest.SysextPCR` (Go), both checked against the TPM by the e2e:

    PCR13 = 0 (32 zero bytes)
    PCR13 = sha256(PCR13 || sha256("os-separator"))     systemd-pcrosseparator.service, initrd
    PCR13 = sha256(PCR13 || sha256("<sha256>  kubernetes_<kv>.raw"))   vm-kubernetes.service

`systemd-pcrosseparator.service` (initrd, `ConditionSecurity=measured-os`) is the only
other PCR 13 event of a boot; both events are in `/run/log/systemd/tpm2-measure.log`,
`systemd-analyze pcrs 13` shows the result. `verify-kubernetes` writes the value to
`build/policy.json` as `"pcr13": {"<kv>": "<hex>"}` (kept across `verify-base` runs, one
entry per sysext version built); vm-manager compares it at the ready stage against the
VM's Kubernetes version and falls back to `golden.sha256.13` for a VM without one
(`internal/attest`). The trust chain for the content itself stays the verity signature
(`/usr/lib/verity.d/verity.crt`) plus `SHA256SUMS.gpg` for the download; PCR 13 binds
the attested boot to the exact artifact.

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
   repart drop-in, and the giantswarm record in the initrd's `hwdb.bin` (without
   `IMDS_KEY_USERDATA`); `usr/bin/ignition` with the four stage units, their
   `ignition-complete.target` and `initrd.target` wiring, every `NEEDED` library of the
   binary, and `ignition.config.url=` / `ignition.platform.id=metal` on the UKI cmdline
   (and no `ignition.firstboot`); `usr/bin/vm-agent` (statically linked) with
   `vm-agent-attest.service` wanted by `initrd.target`, ordered after
   `systemd-pcrphase-initrd.service` and before `ignition-fetch.service` (the drop-in),
   the ready-stage unit enabled in the root and the same binary size in the root erofs
   (`dump.erofs`).
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
in both phases: it becomes the installed system's persistent `/etc/machine-id` on the
first boot, and systemd-repart derives the var partition UUID from it (the initrd
mounts var by its partition label; systemd-gpt-auto-generator would require the match).

Installed boot (phase B): OVMF -> systemd-boot -> `EFI/Linux/giantswarm-vm-base_<v>.efi`.
In the initrd, `systemd-imds-early-network.service` configures networkd for
169.254.169.254, `systemd-imds-import.service` turns `/hostname` and `/public-keys/0`
into `firstboot.hostname` and `ssh.authorized_keys.root` credentials; the verity root is
set up from `roothash=`, `persistent-etc.service` mounts var from the disk and the
persistent `/etc` overlay, `/root` and `/opt` on it (see [Persistent state](#persistent-state)).
On the first installed boot vm-manager adds `ignition.firstboot` to the command line and
the `ignition-*` units fetch `/user-data` from the IMDS and apply it to `/sysroot` before
the switch root, see [Ignition](#ignition). Serial console is `ttyS0`; ssh is reachable
on AF_VSOCK port 22.

## Deviations from docs/design.md and the systemd/mkosi man pages

- `persistent-etc.service` (initrd) mounts var, the `/etc` overlay and the `/root` and
  `/opt` bind mounts from a script rather than through mount units or an `/etc/fstab`
  entry with `x-initrd.mount`: systemd-gpt-auto-generator(8) mounts var only on the
  host, and a mount unit whose `What=` is a device (static or generated from fstab)
  pulls in the device unit and waits `DefaultDeviceTimeoutSec=` (90 s) for it *before*
  its own `ConditionCredential=` is evaluated, which would stall the installer boot (no
  var partition on the installer image) and the first plain boot of the base image (var
  is created by systemd-repart on the host). The script checks for the partition after
  `udevadm settle` and falls back to a tmpfs instead. Details in
  [Persistent state](#persistent-state).
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
- The installer boot runs on the plain verity root: `persistent-etc.service` has
  `ConditionCredential=!vm.install-target`. The installer image has no var partition and
  nothing needs writing to `/etc` there; `CopyBlocks=auto` resolves the source partitions
  from the block device behind `/usr` (`/dev/mapper/root`, from which systemd-repart walks
  to the data and hash partitions and copies their UUIDs), which an overlay on `/etc`
  alone would not disturb either.
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
  running kernel (`ExecStartPost=` writing `/proc/sys/kernel/hostname`) on the first
  boot: PID 1 read `/etc/hostname` before systemd-firstboot wrote it from
  `firstboot.hostname`, so without the drop-in the first boot would run as
  `DEFAULT_HOSTNAME` (`archlinux`) unless a separate `system.hostname` credential is
  passed. From the second boot on `/etc/hostname` persists, PID 1 reads it itself and
  `systemd-firstboot.service` no longer runs (`ConditionFirstBoot=`).
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
- `systemd-sysext` 261 does not deduplicate versions: two files `kubernetes_<a>.raw`
  and `kubernetes_<b>.raw` in `/var/lib/extensions/` would *both* be merged (the later
  name wins per file), and sysupdate.d(5) rejects `InstancesMax=` below 2, so
  `vm-kubernetes.service` deletes every `kubernetes_*.raw` other than the requested
  version before `systemd-sysext refresh` (see Kubernetes sysext above).
- PCR 13 is only extended by systemd-stub for extensions it loads from the ESP, which
  end up in the initrd, not for `/var/lib/extensions/` merges; `vm-kubernetes.service`
  extends it with `systemd-pcrextend` after the merge (rule under Kubernetes sysext).
  The base image has no awk or bc: the unit's script does its arithmetic in bash.
- `publish-sysupdate` no longer writes an empty signed manifest for the kubernetes
  component on base-only builds; `build/sysupdate/kubernetes/` exists once the sysext
  was built.
