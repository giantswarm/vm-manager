# giantswarm-vm-base image build. Everything runs unprivileged.
#
#   make keys base verify smoke-boot      full local cycle
#   make PROFILE=debug base smoke-boot    root autologin, empty root password
#   make clean                            drop build/ (keys/ are kept)

MKOSI    ?= mkosi
PROFILE  ?=
IMAGE_ID := giantswarm-vm-base
VERSION  := $(strip $(shell cat mkosi.version))
BUILD    := build
KEYS     := keys
SCRIPTS  := scripts

MKOSI_ARGS := $(if $(PROFILE),--profile=$(PROFILE))

.PHONY: all keys base verify smoke-boot clean summary

all: keys base verify

keys: $(KEYS)/import-pubring.pgp

$(KEYS)/import-pubring.pgp: $(SCRIPTS)/gen-keys
	$(SCRIPTS)/gen-keys $(KEYS)

# Build the disk image and its split artifacts, then lay out the sysupdate
# directory (renamed partition files, SHA256SUMS, SHA256SUMS.gpg).
base: keys
	$(MKOSI) $(MKOSI_ARGS) --force build
	$(SCRIPTS)/publish-sysupdate $(BUILD) $(IMAGE_ID) $(VERSION) $(KEYS)

# Offline checks of the built artifacts; writes $(BUILD)/policy.json.
verify:
	$(SCRIPTS)/verify $(BUILD) $(IMAGE_ID) $(VERSION) $(KEYS)

# Headless boot with a vTPM; console log in $(BUILD)/smoke.log.
smoke-boot:
	$(SCRIPTS)/smoke-boot $(BUILD) $(IMAGE_ID) $(VERSION) "$(PROFILE)"

summary:
	$(MKOSI) $(MKOSI_ARGS) summary

clean:
	-$(MKOSI) $(MKOSI_ARGS) clean
	rm -rf $(BUILD)
