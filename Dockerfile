# vm-manager as a platform pod: the binary next to the runtime a KVM node
# needs — QEMU, swtpm and OVMF from Ubuntu 26.04 LTS, the lineage the e2e tests
# run on (Debian trixie's swtpm is 0.7.1). The image is x86-64 only (qemu-system-x86_64,
# the x64 OVMF), like the guests it boots.
#
# The Go binary is built by CircleCI (architect/go-build) and attached to the
# build context as vm-manager-<os>-<arch>; this image only assembles the
# runtime. Local build: `make docker-build` (TAG=vm-manager:dev) produces the
# binary first.
FROM ubuntu:26.04
# The firmware VMs boot with is pinned: PCR 0 measures it, so another build
# invalidates every recorded golden value (README.md, Attestation). ovmf-generic
# owns /usr/share/OVMF/OVMF_CODE_4M.fd and OVMF_VARS_4M.fd; the ovmf
# metapackage depends on it without a version and would not pin anything.
# Renovate proposes each new build of the archive's updates pocket as its own
# pull request; once the archive supersedes the pinned build, apt no longer
# finds it and the image build fails until that pull request merges.
# renovate: suite=resolute-updates depName=ovmf-generic
ARG OVMF_GENERIC_VERSION=2025.11-3ubuntu7.2
# qemu-system-x86: qemu-system-x86_64 10.2; swtpm 0.10 (the vTPM, one per VM);
# ovmf-generic: the firmware, a location the firmware probe knows;
# openssh-client: exec_vm; ca-certificates: the identity provider's and the
# registry's TLS. No systemd: the pod has no service manager, VMs are plain
# child processes (--launcher process) and volumes plain files.
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      qemu-system-x86 swtpm "ovmf-generic=${OVMF_GENERIC_VERSION}" \
      openssh-client ca-certificates \
 && rm -rf /var/lib/apt/lists/*
ARG TARGETOS
ARG TARGETARCH
COPY vm-manager-${TARGETOS}-${TARGETARCH} /usr/local/bin/vm-manager
# Root, on purpose: the pod runs privileged for the node's devices (the chart
# says why) and /dev/kvm is root:kvm on every node; QEMU and swtpm are its
# children and the state directory is the pod's own volume.
ENTRYPOINT ["/usr/local/bin/vm-manager"]
CMD ["serve"]
