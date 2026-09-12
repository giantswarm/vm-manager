# giantswarm-vm-base image and kubernetes sysext build. Everything runs
# unprivileged.
#
#   make                                     keys, all images (every Kubernetes version), verify
#   make keys base verify-base smoke-boot    base-only cycle
#   make kubernetes verify-kubernetes        one sysext (KUBERNETES_VERSION), base tree is reused
#   make kubernetes KUBERNETES_VERSION=1.35.4   another release, published next to the others
#   make kubernetes-all                      every release of KUBERNETES_VERSIONS
#   make ignition                            Ignition binary for the initrd (cached)
#   make agent                               vm-agent binary for base and initrd (make -C .. agent)
#   make PROFILE=debug base smoke-boot       root autologin, empty root password
#   make clean                               drop build/ (keys/ are kept)

MKOSI    ?= mkosi
PROFILE  ?=
IMAGE_ID := giantswarm-vm-base
VERSION  := $(strip $(shell cat mkosi.version))
BUILD    := build
KEYS     := keys
SCRIPTS  := scripts
# Kubernetes releases the sysext is built for, newest first. The first is the
# default of `make kubernetes` and the one `make images` builds in the same
# mkosi run as the base image; `make kubernetes-all` (part of `make`) builds
# every one, and build/sysupdate/kubernetes/ accumulates them. Any release the
# Arch Linux Archive carries can be listed: scripts/fetch-kubernetes-packages
# downloads its kubeadm/kubelet/kubectl builds into $(PKGS)/<version>
# (signatures checked against archlinux-keyring), mkosi serves that directory
# as a local repository ahead of the official ones, the kubernetes subimage
# pins kubeadm=%v kubelet=%v kubectl=%v to it, and its executable mkosi.version
# turns the exported KUBERNETES_VERSION into the image version.
KUBERNETES_VERSIONS ?= 1.36.4 1.35.4
KUBERNETES_VERSION  ?= $(firstword $(KUBERNETES_VERSIONS))
export KUBERNETES_VERSION
PKGS                := $(BUILD)/pkgs
KUBERNETES_PKGS     := $(PKGS)/$(KUBERNETES_VERSION)
# Volatile: the package directory is not part of mkosi's incremental cache
# manifest, so switching versions leaves the cached base tree alone (the base
# installs nothing from it; the sysext's pinned package list changes anyway).
KUBERNETES_MKOSI_ARGS := --volatile-package-directory=$(KUBERNETES_PKGS)
# Ignition for the initrd, built from the upstream tag (Arch has no package)
# into $(IGNITION_DIR)/tree and copied in through ExtraTrees= of
# mkosi.initrd.conf/mkosi.conf. See scripts/build-ignition.
IGNITION_VERSION := v2.27.0
IGNITION_DIR     := $(BUILD)/ignition
IGNITION_STAMP   := $(IGNITION_DIR)/stamp-$(IGNITION_VERSION)
# vm-agent (attestation) for the initrd and the root file system, built static
# by the repo root's `make agent` into ../bin/vm-agent and staged as a tree
# that ExtraTrees= of mkosi.images/base/mkosi.conf and mkosi.initrd.conf/
# mkosi.conf copy in as /usr/bin/vm-agent.
AGENT_DIR  := $(BUILD)/agent
AGENT_TREE := $(AGENT_DIR)/tree
AGENT_BIN  := $(AGENT_TREE)/usr/bin/vm-agent

MKOSI_ARGS := $(if $(PROFILE),--profile=$(PROFILE))

.PHONY: all keys ignition agent images base kubernetes kubernetes-all kubernetes-packages verify verify-base verify-kubernetes smoke-boot clean summary

# `images` builds the default Kubernetes version; the other listed releases
# follow, one `make kubernetes` each.
all: keys images $(addprefix kubernetes-,$(filter-out $(KUBERNETES_VERSION),$(KUBERNETES_VERSIONS))) verify

keys: $(KEYS)/import-pubring.pgp

$(KEYS)/import-pubring.pgp: $(SCRIPTS)/gen-keys
	$(SCRIPTS)/gen-keys $(KEYS)

# The stamp carries the version: bumping IGNITION_VERSION rebuilds, a clean
# checkout builds once (git clone of the tag, go build, ~40 s).
ignition: $(IGNITION_STAMP)

$(IGNITION_STAMP): $(SCRIPTS)/build-ignition
	$(SCRIPTS)/build-ignition $(IGNITION_DIR) $(IGNITION_VERSION)
	rm -f $(IGNITION_DIR)/stamp-*
	touch $@

# go build is incremental, so the agent is rebuilt on every image build; a
# changed binary changes the base tree and the initrd (and with them PCR 11).
agent: $(AGENT_BIN)

$(AGENT_BIN): FORCE
	$(MAKE) -C .. agent
	install -Dm0755 ../bin/vm-agent $@

FORCE:

# The node packages of $(KUBERNETES_VERSION) from the Arch Linux Archive; the
# stamp records a complete, verified set (the script re-verifies what is
# present, so a missing stamp costs no download).
kubernetes-packages: $(KUBERNETES_PKGS)/.verified

$(KUBERNETES_PKGS)/.verified: $(SCRIPTS)/fetch-kubernetes-packages
	$(SCRIPTS)/fetch-kubernetes-packages $(KUBERNETES_VERSION) $(KUBERNETES_PKGS)
	touch $@

# One mkosi run builds the base tree, the kubernetes sysext (--dependency=
# appends to Dependencies=base of mkosi.conf) and the disk image, then lays out
# both sysupdate directories (renamed artifacts, SHA256SUMS, SHA256SUMS.gpg).
images: keys ignition agent kubernetes-packages
	$(MKOSI) $(MKOSI_ARGS) $(KUBERNETES_MKOSI_ARGS) --force --dependency=kubernetes build
	$(SCRIPTS)/publish-sysupdate base $(BUILD) $(IMAGE_ID) $(VERSION) $(KEYS)
	$(SCRIPTS)/publish-sysupdate kubernetes $(BUILD) kubernetes $(KUBERNETES_VERSION) $(KEYS)

# Base tree and disk image only.
base: keys ignition agent
	$(MKOSI) $(MKOSI_ARGS) --force build
	$(SCRIPTS)/publish-sysupdate base $(BUILD) $(IMAGE_ID) $(VERSION) $(KEYS)

# Sysext only: the main image is skipped (--format=none, no initrd), an existing
# base tree is reused. Without --force, which would also delete the disk image
# outputs of the skipped main image; the previous build of this version is
# removed by hand, other versions stay.
kubernetes: keys kubernetes-packages
	rm -f $(BUILD)/kubernetes_$(KUBERNETES_VERSION) $(BUILD)/kubernetes_$(KUBERNETES_VERSION).*
	$(MKOSI) $(MKOSI_ARGS) $(KUBERNETES_MKOSI_ARGS) --format=none --bootable=no --dependency=kubernetes build
	$(SCRIPTS)/publish-sysupdate kubernetes $(BUILD) kubernetes $(KUBERNETES_VERSION) $(KEYS)

# Every release of KUBERNETES_VERSIONS, published side by side:
#   make kubernetes-all KUBERNETES_VERSIONS="1.36.4 1.35.4"
kubernetes-all: $(addprefix kubernetes-,$(KUBERNETES_VERSIONS))

kubernetes-%:
	$(MAKE) kubernetes KUBERNETES_VERSION=$*

# Offline checks of the built artifacts, no root, no boot.
verify: verify-base verify-kubernetes

# Writes $(BUILD)/policy.json.
verify-base:
	$(SCRIPTS)/verify $(BUILD) $(IMAGE_ID) $(VERSION) $(KEYS)

# Every version published in $(BUILD)/sysupdate/kubernetes/; adds their
# pcr13 values to $(BUILD)/policy.json.
verify-kubernetes:
	$(SCRIPTS)/verify-kubernetes $(BUILD) $(KEYS)

# Headless boot with a vTPM; console log in $(BUILD)/smoke.log.
smoke-boot:
	$(SCRIPTS)/smoke-boot $(BUILD) $(IMAGE_ID) $(VERSION) "$(PROFILE)"

# All images, the sysext for $(KUBERNETES_VERSION).
summary: kubernetes-packages
	$(MKOSI) $(MKOSI_ARGS) $(KUBERNETES_MKOSI_ARGS) --dependency=kubernetes summary

clean:
	-$(MKOSI) $(MKOSI_ARGS) clean
	rm -rf $(BUILD)
