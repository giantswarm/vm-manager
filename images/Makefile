# giantswarm-vm-base image and kubernetes sysext build. Everything runs
# unprivileged.
#
#   make                                     keys, all images, verify
#   make keys base verify-base smoke-boot    base-only cycle
#   make kubernetes verify-kubernetes        sysext only (base tree is reused)
#   make ignition                            Ignition binary for the initrd (cached)
#   make PROFILE=debug base smoke-boot       root autologin, empty root password
#   make clean                               drop build/ (keys/ are kept)

MKOSI    ?= mkosi
PROFILE  ?=
IMAGE_ID := giantswarm-vm-base
VERSION  := $(strip $(shell cat mkosi.version))
# The sysext is versioned by the Kubernetes release it ships. Bump the file,
# not this variable: mkosi reads the file, mkosi.postinst enforces the match.
KUBERNETES_VERSION := $(strip $(shell cat mkosi.images/kubernetes/mkosi.version))
BUILD    := build
KEYS     := keys
SCRIPTS  := scripts
# Ignition for the initrd, built from the upstream tag (Arch has no package)
# into $(IGNITION_DIR)/tree and copied in through ExtraTrees= of
# mkosi.initrd.conf/mkosi.conf. See scripts/build-ignition.
IGNITION_VERSION := v2.27.0
IGNITION_DIR     := $(BUILD)/ignition
IGNITION_STAMP   := $(IGNITION_DIR)/stamp-$(IGNITION_VERSION)

MKOSI_ARGS := $(if $(PROFILE),--profile=$(PROFILE))

.PHONY: all keys ignition images base kubernetes verify verify-base verify-kubernetes smoke-boot clean summary

all: keys images verify

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

# One mkosi run builds the base tree, the kubernetes sysext (--dependency=
# appends to Dependencies=base of mkosi.conf) and the disk image, then lays out
# both sysupdate directories (renamed artifacts, SHA256SUMS, SHA256SUMS.gpg).
images: keys ignition
	$(MKOSI) $(MKOSI_ARGS) --force --dependency=kubernetes build
	$(SCRIPTS)/publish-sysupdate base $(BUILD) $(IMAGE_ID) $(VERSION) $(KEYS)
	$(SCRIPTS)/publish-sysupdate kubernetes $(BUILD) kubernetes $(KUBERNETES_VERSION) $(KEYS)

# Base tree and disk image only.
base: keys ignition
	$(MKOSI) $(MKOSI_ARGS) --force build
	$(SCRIPTS)/publish-sysupdate base $(BUILD) $(IMAGE_ID) $(VERSION) $(KEYS)

# Sysext only: the main image is skipped (--format=none, no initrd), an existing
# base tree is reused. Without --force, which would also delete the disk image
# outputs of the skipped main image; the previous sysext is removed by hand.
kubernetes: keys
	rm -f $(BUILD)/kubernetes_$(KUBERNETES_VERSION) $(BUILD)/kubernetes_$(KUBERNETES_VERSION).*
	$(MKOSI) $(MKOSI_ARGS) --format=none --bootable=no --dependency=kubernetes build
	$(SCRIPTS)/publish-sysupdate kubernetes $(BUILD) kubernetes $(KUBERNETES_VERSION) $(KEYS)

# Offline checks of the built artifacts, no root, no boot.
verify: verify-base verify-kubernetes

# Writes $(BUILD)/policy.json.
verify-base:
	$(SCRIPTS)/verify $(BUILD) $(IMAGE_ID) $(VERSION) $(KEYS)

verify-kubernetes:
	$(SCRIPTS)/verify-kubernetes $(BUILD) $(KUBERNETES_VERSION) $(KEYS)

# Headless boot with a vTPM; console log in $(BUILD)/smoke.log.
smoke-boot:
	$(SCRIPTS)/smoke-boot $(BUILD) $(IMAGE_ID) $(VERSION) "$(PROFILE)"

summary:
	$(MKOSI) $(MKOSI_ARGS) summary

clean:
	-$(MKOSI) $(MKOSI_ARGS) clean
	rm -rf $(BUILD)
