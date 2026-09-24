#!/usr/bin/env bash
# Record the golden PCR values of a guest image build for a vm-manager
# container image and prove them: the release pipeline (the guest-image job of
# .circleci/custom.yml) runs this between the build and `vm-manager image
# push`, so the published artifact carries the values a pod of the same
# release measures.
#
#   hack/guest-image-golden.sh <vm-manager-image> [<image-dir>]
#
# <image-dir> is a `make -C images` output (default images/build); its
# policy.json gains golden.sha256 (PCRs 0, 2-4, 6, 7, 13) and
# golden_firmware, the SHA-256 of the firmware build they belong to.
#
# What a boot measures into those PCRs are files: the firmware code and
# variable store (PCRs 0 and 7), the virtio-net option ROM (PCR 2, QEMU's
# efi-virtio.rom), the boot loader, UKI and sysext of the guest image (PCRs 4
# and 13), separators (3, 6). All of them come from <vm-manager-image> — the
# image's OVMF_CODE_4M.fd, OVMF_VARS_4M.fd and efi-virtio.rom, mounted where
# the pod has them, and the image's own vm-manager binary. The emulator that
# boots them is not measured, and the image's QEMU 10.2 loses a firmware TPM
# command on most boots under nested virtualization (giantswarm/vm-manager#84),
# so the VMs run on the e2e runner's stack instead: Ubuntu 24.04's QEMU 8.2 and
# the swtpm PPA the e2e workflow installs, in a recorder container.
#
#  1. learn: a server with --attestation-learn-golden over a policy without
#     golden values boots one VM with attestation required to ready;
#     `vm-manager image golden --from-vm` writes its verified ready-stage PCRs
#     and the server's firmware build into policy.json.
#  2. verify: a server without learn mode boots a fresh VM against the recorded
#     values; both quotes must verify with nothing learned.
#
# A boot that does not attest is tried again with a fresh VM, up to
# GOLDEN_ATTEMPTS times; the server's install and boot timeouts
# (GOLDEN_INSTALL_TIMEOUT, GOLDEN_BOOT_TIMEOUT) end a stuck one early. A boot
# whose TPM measurements failed cannot be learned: PCR 11 is compared against
# the image's own policy before any golden value is taken.
#
# The host needs docker, /dev/kvm, /dev/vhost-vsock (the vhost_vsock module)
# and no AppArmor profile confining swtpm (Ubuntu's usr.bin.swtpm refuses the
# container's sockets); vsock CIDs are host-global: never run this next to
# another vm-manager on the same host.
set -euo pipefail

image=${1:?usage: $0 <vm-manager-image> [<image-dir>]}
dir=$(cd "${2:-images/build}" && pwd)
port=${GOLDEN_PORT:-18080}
attempts=${GOLDEN_ATTEMPTS:-3}
# Seconds, with the s suffix.
install_timeout=${GOLDEN_INSTALL_TIMEOUT:-150s}
boot_timeout=${GOLDEN_BOOT_TIMEOUT:-150s}
# The server settles every VM within its timeouts; the poll gives it a minute
# more.
ready_within=$(( ${install_timeout%s} + ${boot_timeout%s} + 60 ))
name="vm-manager-golden"
volume="vm-manager-golden-state"
recorder="vm-manager-golden-recorder"
api="http://127.0.0.1:${port}/api/v1"

policy="$dir/policy.json"
test -f "$policy" || { echo "no policy.json in $dir: run make -C images verify" >&2; exit 1; }
ref=$(jq -er '"\(.image_id)_\(.image_version)"' "$policy")
work=$(mktemp -d "${TMPDIR:-/tmp}/vm-manager-golden.XXXXXX")

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }

stop_server() {
  docker stop -t 90 "$name" >/dev/null 2>&1 || true
  docker rm -f "$name" >/dev/null 2>&1 || true
}
cleanup() {
  stop_server
  docker volume rm -f "$volume" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

# The measured inputs and the binary, from the image.
inputs=(/usr/local/bin/vm-manager /usr/share/OVMF/OVMF_CODE_4M.fd /usr/share/OVMF/OVMF_VARS_4M.fd /usr/share/qemu/efi-virtio.rom)
# A registry reference is pulled, a local build (make docker-build) used as is.
docker image inspect "$image" >/dev/null 2>&1 || docker pull -q "$image" >/dev/null
cid=$(docker create "$image")
for f in "${inputs[@]}"; do docker cp -L "$cid:$f" "$work/" >/dev/null; done
docker rm "$cid" >/dev/null
log "recording the golden PCR values of $ref with the inputs of $(docker image inspect -f '{{index .RepoDigests 0}}' "$image" 2>/dev/null || echo "$image"):"
(cd "$work" && sha256sum OVMF_CODE_4M.fd OVMF_VARS_4M.fd efi-virtio.rom) >&2

# The recorder: the e2e runner's userspace (.github/workflows/e2e.yml installs
# the same packages), on 24.04 until giantswarm/vm-manager#84 is fixed. Its
# own OVMF (a recommendation) stays out, so only the image's firmware exists.
docker build -q -t "$recorder" - >/dev/null <<'EOF'
FROM ubuntu:24.04
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends software-properties-common gpg-agent \
 && add-apt-repository -y ppa:stefanberger/swtpm-noble \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends qemu-system-x86 swtpm \
 && rm -rf /var/lib/apt/lists/*
EOF
docker run --rm "$recorder" sh -c 'qemu-system-x86_64 --version | head -1; swtpm --version' >&2

# start_server [serve flags...]: one server in a fresh recorder container, the
# image's inputs where the pod has them, the state (networks, VM records and
# disks) on a volume that outlives it.
start_server() {
  stop_server
  docker run -d --name "$name" --privileged \
    -p "127.0.0.1:${port}:8080" \
    -v "$dir:/images" -v "$volume:/state" \
    -v "$work/vm-manager:/usr/local/bin/vm-manager:ro" \
    -v "$work/OVMF_CODE_4M.fd:/usr/share/OVMF/OVMF_CODE_4M.fd:ro" \
    -v "$work/OVMF_VARS_4M.fd:/usr/share/OVMF/OVMF_VARS_4M.fd:ro" \
    -v "$work/efi-virtio.rom:/usr/share/qemu/efi-virtio.rom:ro" \
    "$recorder" vm-manager serve --listen=:8080 --launcher=process \
    --state-dir=/state --image-dir=/images --attestation=verify \
    --ovmf-code=/usr/share/OVMF/OVMF_CODE_4M.fd --ovmf-vars=/usr/share/OVMF/OVMF_VARS_4M.fd \
    --install-timeout="$install_timeout" --boot-timeout="$boot_timeout" "$@" >/dev/null
  for _ in $(seq 60); do
    curl -fsS "http://127.0.0.1:${port}/readyz" >/dev/null 2>&1 && return 0
    sleep 1
  done
  docker logs --tail 50 "$name" >&2
  log "the server did not become ready"; return 1
}

# call <method> <path> [<json body>]: one API request; prints the response
# body, fails with the status and body on anything but 2xx. boot runs as an
# `if` condition, where bash ignores set -e, so every step checks its result.
call() {
  local out code
  out=$(curl -sS -X "$1" -H 'Content-Type: application/json' ${3:+-d "$3"} -w '\n%{http_code}' "$api$2") || return 1
  code=${out##*$'\n'}
  out=${out%$'\n'*}
  if [ "${code:0:1}" != 2 ]; then
    log "$1 $2: HTTP $code: $out"; return 1
  fi
  printf '%s' "$out"
}

# boot <name>: create a VM with attestation required and wait until it is
# ready with both quotes verified; prints its id. On failure the VM's state,
# attestation, console tail and the server log go to stderr and the VM is
# deleted.
boot() {
  local vm=$1 id state seen="" started=$SECONDS att
  if ! id=$(call POST /vms "{\"name\":\"$vm\",\"require_attestation\":true,\"wait_for\":\"none\"}" | jq -er .id); then
    docker logs --tail 30 "$name" >&2 2>&1 || true
    return 1
  fi
  while :; do
    state=$(call GET "/vms/$id" | jq -r .state) || state="unknown"
    [ "$state" = "$seen" ] || log "vm $vm: $state after $((SECONDS - started))s"
    seen=$state
    # running: no READY=1 within the boot timeout.
    case "$state" in
      ready | running | failed | stopped) break ;;
    esac
    if [ $((SECONDS - started)) -ge "$ready_within" ]; then state="not ready within ${ready_within}s ($state)"; break; fi
    sleep 5
  done
  att=$(call GET "/vms/$id/attestation") || att='{}'
  if [ "$state" = ready ] && jq -e '.initrd.verified and .ready.verified' >/dev/null <<<"$att"; then
    jq -r '"initrd: \(.initrd.message)\nready:  \(.ready.message)"' <<<"$att" >&2
    echo "$id"; return 0
  fi
  log "vm $vm ($id): $state"
  call GET "/vms/$id" | jq -r '"lastError: \(.lastError // "")"' >&2 || true
  jq -r '"initrd: \(.initrd.message // "no quote")\nready:  \(.ready.message // "no quote")"' <<<"$att" >&2 || true
  call GET "/vms/$id/console?lines=40" | jq -r .console >&2 || true
  docker logs --tail 30 "$name" >&2 2>&1 || true
  call DELETE "/vms/$id" >/dev/null || true
  return 1
}

# boot_attested <name>: boot, a fresh VM per attempt.
boot_attested() {
  local id
  for attempt in $(seq "$attempts"); do
    if id=$(boot "$1-$attempt"); then echo "$id"; return 0; fi
    log "attempt $attempt of $attempts failed"
  done
  return 1
}

docker volume create "$volume" >/dev/null
# The server reads the policy at start: learn mode accepts only PCRs without
# a value.
"$work/vm-manager" image golden "$ref" --clear --image-dir="$dir" >&2

start_server --attestation-learn-golden
call GET /host | jq -e '
  if (.missing // []) != [] then error("host lacks \(.missing | join(", "))") else . end
  | "firmware sha256 \(.firmware.sha256), qemu \(.qemu.version // "?"), swtpm \(.swtpm.version // "?")"' >&2
id=$(boot_attested golden-learn)
"$work/vm-manager" image golden "$ref" --from-vm "$id" --server="http://127.0.0.1:${port}" --image-dir="$dir" >&2
call DELETE "/vms/$id" >/dev/null

start_server
id=$(boot_attested golden-verify)
att=$(call GET "/vms/$id/attestation")
jq -e '(.initrd.learned // []) == [] and (.ready.learned // []) == []' >/dev/null <<<"$att" \
  || { log "the verify boot learned PCRs: $(jq -c '[.initrd.learned, .ready.learned]' <<<"$att")"; exit 1; }
call DELETE "/vms/$id" >/dev/null
log "verified a fresh VM against the recorded values"
jq '{golden, golden_firmware}' "$policy"
