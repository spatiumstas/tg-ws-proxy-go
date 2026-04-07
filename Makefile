-include .config

SHELL := /bin/bash

PKG_NAME := tg-ws-proxy
PKG_DESCRIPTION := Telegram MTProto WS bridge proxy (Go binary)
PKG_LICENSE := MIT
PKG_SECTION := net
PKG_MAINTAINER := tg-ws-proxy maintainers

PKG_VERSION := $(shell cat VERSION)
PKG_REVISION ?= 1

PLATFORM ?=
TARGET ?=
GOOS ?=
GOARCH ?=
GOARM ?=
GOMIPS ?=
CGO_ENABLED ?= 0
GO_PROXY_DIR ?= src

ifeq ($(PLATFORM),entware)
	PKG_DEPENDS := ca-certificates
else ifeq ($(PLATFORM),openwrt)
	PKG_DEPENDS := ca-certificates
else
	$(error Unsupported PLATFORM='$(PLATFORM)'; expected entware or openwrt)
endif

BUILDS_DIR := ./.build
BUILD_DIR := $(BUILDS_DIR)/$(PLATFORM)_$(TARGET)
COMPILE_DIR := $(BUILD_DIR)/compile
ROOT_DIR := $(BUILD_DIR)/root
ROOT_APK_DIR := $(BUILD_DIR)/root_apk
CONTROL_DIR := $(BUILD_DIR)/control
APK_DIR := $(BUILD_DIR)/apk

APK_ARCH ?= $(TARGET)

BUILD_KEY_APK_SEC ?=

ifeq ($(PLATFORM),entware)
BIN_DIR := $(ROOT_DIR)/opt/bin
ETC_DIR := $(ROOT_DIR)/opt/etc
VAR_DIR := $(ROOT_DIR)/opt/var
else
BIN_DIR := $(ROOT_DIR)/usr/bin
ETC_DIR := $(ROOT_DIR)/etc
VAR_DIR := $(ROOT_DIR)/var
endif

define _copy_files
	if [ -d $(1)/_ipk/control ]; then mkdir -p "$(CONTROL_DIR)"; cp -r $(1)/_ipk/control/* "$(CONTROL_DIR)"; fi
	if [ -d $(1)/_apk ]; then mkdir -p "$(APK_DIR)"; cp -r $(1)/_apk/* "$(APK_DIR)"; fi
	if [ -d $(1)/bin ]; then mkdir -p "$(BIN_DIR)"; cp -r $(1)/bin/* "$(BIN_DIR)"; fi
	if [ -d $(1)/etc ]; then mkdir -p "$(ETC_DIR)"; cp -r $(1)/etc/* "$(ETC_DIR)"; fi
	if [ -d $(1)/var ]; then mkdir -p "$(VAR_DIR)"; cp -r $(1)/var/* "$(VAR_DIR)"; fi
endef

PACKAGE_FILE := $(BUILDS_DIR)/$(PKG_NAME)_$(PKG_VERSION)-$(PKG_REVISION)_$(PLATFORM)_$(TARGET).ipk
APK_PACKAGE_FILE := $(BUILDS_DIR)/$(PKG_NAME)_$(PKG_VERSION)-r$(PKG_REVISION)_$(PLATFORM)_$(TARGET).apk

.PHONY: all clean build prepare_files package package_ipk package_apk

all: build package

clean:
	rm -rf $(BUILDS_DIR)

build:
	mkdir -p "$(COMPILE_DIR)"
	cd "$(GO_PROXY_DIR)" && \
		GOOS="$(GOOS)" GOARCH="$(GOARCH)" GOARM="$(GOARM)" GOMIPS="$(GOMIPS)" CGO_ENABLED="$(CGO_ENABLED)" \
		go build -trimpath -o "$(abspath $(COMPILE_DIR))/tg-ws-proxy" .

prepare_files: build
	rm -rf "$(ROOT_DIR)" "$(CONTROL_DIR)"
	mkdir -p "$(BIN_DIR)" "$(CONTROL_DIR)"

	cp "$(COMPILE_DIR)/tg-ws-proxy" "$(BIN_DIR)/tg-ws-proxy"
	$(call _copy_files,./files/common)
	$(if $(filter entware,$(PLATFORM)), $(call _copy_files,./files/entware))
	$(if $(filter openwrt,$(PLATFORM)), $(call _copy_files,./files/openwrt))

	echo "Package: $(PKG_NAME)" > "$(CONTROL_DIR)/control"
	echo "Version: $(PKG_VERSION)-$(PKG_REVISION)" >> "$(CONTROL_DIR)/control"
	echo "Depends: $(PKG_DEPENDS)" >> "$(CONTROL_DIR)/control"
	echo "Section: $(PKG_SECTION)" >> "$(CONTROL_DIR)/control"
	echo "Architecture: $(TARGET)" >> "$(CONTROL_DIR)/control"
	echo "License: $(PKG_LICENSE)" >> "$(CONTROL_DIR)/control"
	echo "Maintainer: $(PKG_MAINTAINER)" >> "$(CONTROL_DIR)/control"
	echo "Description: $(PKG_DESCRIPTION)" >> "$(CONTROL_DIR)/control"

	chmod +x "$(BIN_DIR)/tg-ws-proxy"
	if [ -d "$(ETC_DIR)/init.d" ]; then chmod +x "$(ETC_DIR)/init.d"/*; fi
	if [ -f "$(CONTROL_DIR)/prerm" ]; then chmod +x "$(CONTROL_DIR)/prerm"; fi
	if [ -f "$(CONTROL_DIR)/postinst" ]; then chmod +x "$(CONTROL_DIR)/postinst"; fi
	if [ -f "$(CONTROL_DIR)/postrm" ]; then chmod +x "$(CONTROL_DIR)/postrm"; fi

package:
	$(MAKE) package_ipk
	@if [ "$(PLATFORM)" = "openwrt" ]; then $(MAKE) package_apk; fi

package_ipk: prepare_files
	mkdir -p "$(BUILDS_DIR)"
	echo 2.0 > "$(BUILD_DIR)/debian-binary"
	tar -C "$(CONTROL_DIR)" -czf "$(BUILD_DIR)/control.tar.gz" --owner=0 --group=0 .
	tar -C "$(ROOT_DIR)" -czf "$(BUILD_DIR)/data.tar.gz" --owner=0 --group=0 .
	tar -C "$(BUILD_DIR)" -czf "$(PACKAGE_FILE)" --owner=0 --group=0 debian-binary control.tar.gz data.tar.gz
	@echo "Built: $(PACKAGE_FILE)"

package_apk: prepare_files
	rm -rf "$(ROOT_APK_DIR)"
	mkdir -p "$(ROOT_APK_DIR)"
	cp -r "$(ROOT_DIR)/." "$(ROOT_APK_DIR)/"

	mkdir -p "$(ROOT_APK_DIR)/lib/apk/packages"
	if [ -f "$(APK_DIR)/conffiles" ]; then \
		cp "$(APK_DIR)/conffiles" "$(ROOT_APK_DIR)/lib/apk/packages/$(PKG_NAME).conffiles"; \
		for file in $$(cat "$(ROOT_APK_DIR)/lib/apk/packages/$(PKG_NAME).conffiles"); do \
			[ -f "$(ROOT_APK_DIR)/$$file" ] || continue; \
			csum=$$(sha256sum "$(ROOT_APK_DIR)/$$file" | cut -d' ' -f1); \
			echo "$$file $$csum" >> "$(ROOT_APK_DIR)/lib/apk/packages/$(PKG_NAME).conffiles_static"; \
		done; \
	fi
	(cd "$(ROOT_APK_DIR)" && find . -type f,l -printf "/%P\\n") > "$(ROOT_APK_DIR)/lib/apk/packages/$(PKG_NAME).list"

	APK_SIGN_ARG=""; \
	APK_SCRIPT_ARGS=""; \
	if [ -n "$(BUILD_KEY_APK_SEC)" ] && [ -f "$(BUILD_KEY_APK_SEC)" ]; then \
		APK_SIGN_ARG="--sign $(BUILD_KEY_APK_SEC)"; \
	fi; \
	if [ -f "$(APK_DIR)/post-install.sh" ]; then \
		APK_SCRIPT_ARGS="$$APK_SCRIPT_ARGS -s post-install:$(APK_DIR)/post-install.sh"; \
	fi; \
	if [ -f "$(APK_DIR)/pre-deinstall.sh" ]; then \
		APK_SCRIPT_ARGS="$$APK_SCRIPT_ARGS -s pre-deinstall:$(APK_DIR)/pre-deinstall.sh"; \
	fi; \
	if [ -f "$(APK_DIR)/post-upgrade.sh" ]; then \
		APK_SCRIPT_ARGS="$$APK_SCRIPT_ARGS -s post-upgrade:$(APK_DIR)/post-upgrade.sh"; \
	fi; \
	apk mkpkg \
		-I "name:$(PKG_NAME)" \
		-I "version:$(PKG_VERSION)-r$(PKG_REVISION)" \
		-I "description:$(PKG_DESCRIPTION)" \
		-I "arch:$(APK_ARCH)" \
		-I "license:$(PKG_LICENSE)" \
		-I "origin:feeds/packages/feeds/tg-ws-proxy/net/$(PKG_NAME)" \
		-I "maintainer:$(PKG_MAINTAINER)" \
		-I "provider-priority:100" \
		-F "$(ROOT_APK_DIR)" \
		-o "$(APK_PACKAGE_FILE)" \
		$$APK_SCRIPT_ARGS \
		$$APK_SIGN_ARG
	@echo "Built: $(APK_PACKAGE_FILE)"
