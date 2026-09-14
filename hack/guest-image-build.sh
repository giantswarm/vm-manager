#!/usr/bin/env bash
# Build the guest image (`make -C images all`: keys, base image, every
# Kubernetes sysext, verify) inside a privileged archlinux container, for a
# host that is not Arch: the CircleCI guest-image job (.circleci/custom.yml)
# and a developer's machine alike. images/README.md documents the Arch host
# itself; .github/workflows/image.yml runs the same steps as a container job.
#
# --privileged is what mkosi's sandbox needs for its mount namespaces and the
# sysext's overlayfs; the mkosi workspace lives on a bind-mounted host
# directory because overlayfs cannot stack on the container's own overlay
# root. images/mkosi.cache persists mkosi's incremental cache across runs.
#
#   hack/guest-image-build.sh            # -> images/build/
#   ARCH_IMAGE=archlinux:base-20260901.0.412345 hack/guest-image-build.sh
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
workspace=${GUEST_IMAGE_WORKSPACE:-$(mktemp -d "${TMPDIR:-/tmp}/mkosi-workspace.XXXXXX")}
mkdir -p "$workspace" "$repo/images/mkosi.cache"

docker run --rm --privileged \
  -v "$repo:/src" -v "$workspace:/workspace" -w /src \
  "${ARCH_IMAGE:-archlinux:latest}" bash -euo pipefail -c '
    images/scripts/install-build-deps
    # The bind-mounted checkout belongs to the host user, the container runs
    # as root: without safe.directory, git (Make'"'"'s rev-parse, Go'"'"'s VCS
    # stamping of vm-agent) refuses it with "dubious ownership".
    git config --global --add safe.directory /src
    make -C images MKOSI="mkosi --workspace-dir=/workspace" all
    # Hand the outputs back to the host user.
    chown -R "$(stat -c %u:%g /src/images/Makefile)" /src/images/build /src/images/keys /src/images/mkosi.cache /src/bin 2>/dev/null || true
  '
ls -lh "$repo"/images/build/*.efi "$repo"/images/build/*.raw "$repo"/images/build/policy.json
