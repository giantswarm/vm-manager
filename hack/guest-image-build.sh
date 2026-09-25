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
# root.
#
# Every run starts without images/mkosi.cache, so mkosi installs the current
# Arch repositories, as .github/workflows/image.yml (the e2e image) does:
# a cache left by an earlier run would freeze the package set it was built
# from. Within the run the incremental cache keeps the second mkosi run (the
# other KUBERNETES_VERSIONS) on the first one's metadata and base tree.
#
#   hack/guest-image-build.sh            # -> images/build/
#   ARCH_IMAGE=archlinux:base-20260901.0.412345 hack/guest-image-build.sh
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
workspace=${GUEST_IMAGE_WORKSPACE:-$(mktemp -d "${TMPDIR:-/tmp}/mkosi-workspace.XXXXXX")}
mkdir -p "$workspace"

docker run --rm --privileged \
  -v "$repo:/src" -v "$workspace:/workspace" -w /src \
  "${ARCH_IMAGE:-archlinux:latest}" bash -euo pipefail -c '
    images/scripts/install-build-deps
    # The bind-mounted checkout belongs to the host user, the container runs
    # as root: without safe.directory, git (Make'"'"'s rev-parse, Go'"'"'s VCS
    # stamping of vm-agent) refuses it with "dubious ownership".
    git config --global --add safe.directory /src
    rm -rf images/mkosi.cache
    make -C images MKOSI="mkosi --workspace-dir=/workspace" all
    # Hand the outputs and the spent cache back to the host user.
    chown -R "$(stat -c %u:%g /src/images/Makefile)" /src/images/build /src/images/keys /src/images/mkosi.cache /src/bin 2>/dev/null || true
  '
ls -lh "$repo"/images/build/*.efi "$repo"/images/build/*.raw "$repo"/images/build/policy.json
