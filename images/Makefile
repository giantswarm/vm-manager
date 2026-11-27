# giantswarm-vm-base image, kubernetes and cni sysext build. Everything runs
# unprivileged.
#
#   make                                     keys, all images (every Kubernetes version and CNI release), verify
#   make keys base verify-base smoke-boot    base-only cycle
#   make kubernetes verify-kubernetes        one sysext (KUBERNETES_VERSION), base tree is reused
#   make kubernetes KUBERNETES_VERSION=1.35.4   another release, published next to the others
#   make kubernetes-all                      every release of KUBERNETES_VERSIONS
#   make cni verify-cni                      one CNI sysext (CNI_VERSION), base tree is reused
#   make cni-all                             every release of CNI_VERSIONS
#   make ignition                            Ignition binary for the initrd (cached)
#   make agent                               vm-agent binary for base and initrd (make -C .. agent)
#   make e2e-sysext                          the e2e tests' firmware sysext, base tree is reused
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
# CNI releases a cni sysext is built for, one per entry of cni/ (the entry
# pins the release, the image and its digest; Renovate bumps both):
# "<entry>-<release without v>". The first is the default of `make cni` and
# the one `make images` builds in the same mkosi run as the base image;
# `make cni-all` (part of `make`) builds every one, and build/sysupdate/cni/
# accumulates them. scripts/fetch-cni-plugins stages the release's plugins
# and cni.extra/<entry> into $(CNI_TREE), ExtraTrees= of mkosi.images/cni/mkosi.conf, and the
# subimage's executable mkosi.version turns the exported CNI_VERSION into the
# image version.
CNI_VERSIONS ?= $(shell for f in cni/*; do (. "./$$f" && echo "$${f##*/}-$${release#v}"); done)
CNI_VERSION  ?= $(firstword $(CNI_VERSIONS))
export CNI_VERSION
CNI_CACHE    := $(BUILD)/cni/$(CNI_VERSION)
CNI_TREE     := $(BUILD)/cni/tree
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
# The IPE policy (ipe/giantswarm-vm.policy) names the root's verity root hash,
# so the disk image is built twice (mkosi-ipe below): the first run yields the
# root hash, scripts/sign-ipe-policy writes the signed policy to $(IPE_P7B),
# which ExtraTrees= of mkosi.initrd.conf/mkosi.conf copies into the initrd, and
# the second run builds the same root and the UKI with that initrd. See
# README.md, IPE.
IPE_POLICY := ipe/giantswarm-vm.policy
IPE_DIR    := $(BUILD)/ipe
IPE_TREE   := $(IPE_DIR)/tree
IPE_P7B    := $(IPE_TREE)/usr/lib/vm-manager/ipe/giantswarm-vm.p7b
ROOTHASH   := $(BUILD)/$(IMAGE_ID)_$(VERSION).roothash
# One timestamp for every file of the image, so that both runs of mkosi-ipe
# produce the root byte for byte: mkosi clamps the tree's mtimes to it and
# hands it to systemd-repart and mkfs.erofs (Seed= of mkosi.conf fixes the
# UUIDs and the verity salt). The last commit's time keeps it stable across
# rebuilds of one checkout.
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct 2>/dev/null || date +%s)
export SOURCE_DATE_EPOCH
# The certificate the image's verity root hash, its sysexts and its IPE policy
# are signed with, published next to the UKI for vm-manager to enroll into
# each VM's UEFI db (internal/uefivars).
CERT := $(BUILD)/$(IMAGE_ID)_$(VERSION).crt

MKOSI_ARGS := $(if $(PROFILE),--profile=$(PROFILE))

.PHONY: all keys ignition agent images base kubernetes kubernetes-all kubernetes-packages cni cni-all cni-plugins e2e-sysext verify verify-base verify-kubernetes verify-cni smoke-boot clean summary

# `images` builds the default Kubernetes version and CNI release; the other
# listed releases follow, one `make kubernetes` or `make cni` each.
all: keys images $(addprefix kubernetes-,$(filter-out $(KUBERNETES_VERSION),$(KUBERNETES_VERSIONS))) \
	$(addprefix cni-,$(filter-out $(CNI_VERSION),$(CNI_VERSIONS))) verify

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

# $(call mkosi-ipe,<arguments of the second mkosi run>): the disk image with
# the IPE policy in its initrd. The first run builds the base tree and the disk
# image without a policy, for the root hash only; the second rebuilds both (and
# whatever the arguments add, the kubernetes sysext) with the policy signed for
# that root in the initrd, and must arrive at the same root hash: a root that
# differs from the one the policy names would load no kernel module.
define mkosi-ipe
	rm -f $(IPE_P7B) $(IPE_DIR)/roothash
	mkdir -p $(IPE_TREE)
	$(MKOSI) $(MKOSI_ARGS) --force build
	$(SCRIPTS)/sign-ipe-policy $(IPE_POLICY) $(VERSION) $(ROOTHASH) $(KEYS) $(IPE_DIR)
	$(MKOSI) $(MKOSI_ARGS) $(1) --force build
	test "$$(tr -d '\n' <$(IPE_DIR)/roothash)" = "$$(tr -d '\n' <$(ROOTHASH))" || \
		{ echo "mkosi-ipe: the second run built root $$(cat $(ROOTHASH)), the IPE policy names $$(cat $(IPE_DIR)/roothash): the root is not reproducible" >&2; exit 1; }
endef

# The node packages of $(KUBERNETES_VERSION) from the Arch Linux Archive; the
# stamp records a complete, verified set (the script re-verifies what is
# present, so a missing stamp costs no download).
kubernetes-packages: $(KUBERNETES_PKGS)/.verified

$(KUBERNETES_PKGS)/.verified: $(SCRIPTS)/fetch-kubernetes-packages
	$(SCRIPTS)/fetch-kubernetes-packages $(KUBERNETES_VERSION) $(KUBERNETES_PKGS)
	touch $@

# The plugins of $(CNI_VERSION), staged as the cni sysext's tree. Always
# restaged: the tree is shared by every release (the files exported from the
# image are cached per digest in $(CNI_CACHE)).
cni-plugins:
	$(SCRIPTS)/fetch-cni-plugins $(CNI_VERSION) $(CNI_CACHE) $(CNI_TREE)

# Two mkosi runs (mkosi-ipe): the second builds the base tree, the kubernetes
# and cni sysexts (--dependency= appends to Dependencies=base of mkosi.conf)
# and the disk image with the IPE policy for the first one's root, then it
# lays out the sysupdate directories (renamed artifacts, SHA256SUMS,
# SHA256SUMS.gpg) and the e2e tests' sysext.
images: keys ignition agent kubernetes-packages cni-plugins
	$(call mkosi-ipe,$(KUBERNETES_MKOSI_ARGS) --dependency=kubernetes --dependency=cni)
	$(SCRIPTS)/publish-sysupdate base $(BUILD) $(IMAGE_ID) $(VERSION) $(KEYS)
	$(SCRIPTS)/publish-sysupdate kubernetes $(BUILD) kubernetes $(KUBERNETES_VERSION) $(KEYS)
	$(SCRIPTS)/publish-sysupdate cni $(BUILD) cni $(CNI_VERSION) $(KEYS)
	$(SCRIPTS)/build-e2e-sysext $(BUILD) $(KEYS)
	install -m0644 $(KEYS)/verity.crt $(CERT)

# Base tree and disk image only (two mkosi runs, mkosi-ipe).
base: keys ignition agent
	$(call mkosi-ipe,)
	$(SCRIPTS)/publish-sysupdate base $(BUILD) $(IMAGE_ID) $(VERSION) $(KEYS)
	install -m0644 $(KEYS)/verity.crt $(CERT)

# Sysext only: the main image is skipped (--format=none, no initrd), an existing
# base tree is reused. Without --force, which would also delete the disk image
# outputs of the skipped main image; the previous build of this version is
# removed by hand, other versions stay.
kubernetes: keys kubernetes-packages
	rm -f $(BUILD)/kubernetes_$(KUBERNETES_VERSION) $(BUILD)/kubernetes_$(KUBERNETES_VERSION).*
	$(MKOSI) $(MKOSI_ARGS) $(KUBERNETES_MKOSI_ARGS) --format=none --bootable=no --dependency=kubernetes build
	$(SCRIPTS)/publish-sysupdate kubernetes $(BUILD) kubernetes $(KUBERNETES_VERSION) $(KEYS)

# build/e2e/ipe-e2e.raw, the test-only sysext e2e/kubernetes_sysext_test.go
# merges to prove that IPE refuses firmware from a signed sysext: signed with
# the verity key, ID and version from the base tree's os-release. `images`
# builds it too; nothing publishes it.
e2e-sysext: keys
	$(SCRIPTS)/build-e2e-sysext $(BUILD) $(KEYS)

# Every release of KUBERNETES_VERSIONS, published side by side:
#   make kubernetes-all KUBERNETES_VERSIONS="1.36.4 1.35.4"
kubernetes-all: $(addprefix kubernetes-,$(KUBERNETES_VERSIONS))

kubernetes-%:
	$(MAKE) kubernetes KUBERNETES_VERSION=$*

# One CNI sysext, like `kubernetes`: the main image is skipped, the base tree
# reused, the previous build of this release removed by hand.
cni: keys cni-plugins
	rm -f $(BUILD)/cni_$(CNI_VERSION) $(BUILD)/cni_$(CNI_VERSION).*
	$(MKOSI) $(MKOSI_ARGS) --format=none --bootable=no --dependency=cni build
	$(SCRIPTS)/publish-sysupdate cni $(BUILD) cni $(CNI_VERSION) $(KEYS)

# Every release of CNI_VERSIONS, published side by side.
cni-all: $(addprefix cni-,$(CNI_VERSIONS))

cni-%:
	$(MAKE) cni CNI_VERSION=$*

# Offline checks of the built artifacts, no root, no boot.
verify: verify-base verify-kubernetes verify-cni

# Writes $(BUILD)/policy.json.
verify-base:
	$(SCRIPTS)/verify $(BUILD) $(IMAGE_ID) $(VERSION) $(KEYS)

# Every version published in $(BUILD)/sysupdate/<component>/; records the
# sha256 of each in $(BUILD)/policy.json (sysexts.<component>), from which
# vm-manager computes PCR 13.
verify-kubernetes:
	$(SCRIPTS)/verify-sysext kubernetes $(BUILD) $(KEYS)

verify-cni:
	$(SCRIPTS)/verify-sysext cni $(BUILD) $(KEYS)

# Headless boot with a vTPM; console log in $(BUILD)/smoke.log.
smoke-boot:
	$(SCRIPTS)/smoke-boot $(BUILD) $(IMAGE_ID) $(VERSION) "$(PROFILE)"

# All images, the sysexts for $(KUBERNETES_VERSION) and $(CNI_VERSION).
summary: kubernetes-packages
	$(MKOSI) $(MKOSI_ARGS) $(KUBERNETES_MKOSI_ARGS) --dependency=kubernetes --dependency=cni summary

clean:
	-$(MKOSI) $(MKOSI_ARGS) clean
	rm -rf $(BUILD)
