# vm-manager as a platform pod: the binary next to the runtime a KVM node
# needs — QEMU, swtpm and OVMF from Ubuntu 26.04 LTS, the lineage the e2e tests
# run on (Debian trixie's swtpm is 0.7.1, below the 0.8 internal/tpm needs for
# the terminate ctrl option). The image is x86-64 only (qemu-system-x86_64,
# the x64 OVMF), like the guests it boots.
#
# The build stage compiles the binary the way the release binaries are built
# (CGO_ENABLED=0, static, the version stamped into main); the runtime stage
# holds no compiler. Local build: `make docker-build` (TAG=vm-manager:dev).
FROM golang:1.27.1-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
      -o /out/vm-manager .

FROM ubuntu:26.04
# qemu-system-x86: qemu-system-x86_64 10.2; swtpm 0.10 (the vTPM, one per VM);
# ovmf: /usr/share/OVMF/OVMF_CODE_4M.fd and OVMF_VARS_4M.fd, a location the
# firmware probe knows; openssh-client: exec_vm; ca-certificates: the identity
# provider's TLS. No systemd: the pod has no service manager, VMs are plain
# child processes (--launcher process) and volumes plain files.
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      qemu-system-x86 swtpm ovmf openssh-client ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/vm-manager /usr/local/bin/vm-manager
# Root, on purpose: the pod runs privileged for the device cgroup (the chart
# says why) and /dev/kvm is root:kvm on every node; QEMU and swtpm are its
# children and the state directory is the pod's own volume.
ENTRYPOINT ["/usr/local/bin/vm-manager"]
CMD ["serve"]
