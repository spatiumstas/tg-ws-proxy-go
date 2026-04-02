SHELL := /bin/bash
PACKAGE := tg-ws-proxy
ROOT_DIR := /opt
DEPENDENCIES := ca-certificates xxd
GOOS ?= linux
GOARCH ?= arm64
GOARM ?=
GOMIPS ?=
CGO_ENABLED ?= 0
GO_PROXY_DIR ?= src
GO_BIN_NAME ?= tg-ws-proxy
GO_BIN ?= out/$(GO_BIN_NAME)-$(GOARCH)

DEFAULT_PKG_ARCH := $(GOARCH)
ifeq ($(GOARCH),amd64)
DEFAULT_PKG_ARCH := x64
endif
ifeq ($(GOARCH),arm64)
DEFAULT_PKG_ARCH := aarch64
endif
ifeq ($(GOARCH),arm)
ifeq ($(GOARM),7)
DEFAULT_PKG_ARCH := armv7
else
DEFAULT_PKG_ARCH := arm
endif
endif
ifeq ($(GOARCH),mipsle)
DEFAULT_PKG_ARCH := mipsel
endif

PKG_ARCH ?= $(DEFAULT_PKG_ARCH)
VERSION := $(shell cat VERSION)

.PHONY: clean _build-go _pkg-clean _pkg-control _pkg-scripts _pkg-data _pkg-ipk tg-ws-proxy-ipk

clean:
	rm -rf out

_build-go:
	mkdir -p out
	cd "$(GO_PROXY_DIR)" && \
		GOOS=$(GOOS) GOARCH=$(GOARCH) GOARM=$(GOARM) GOMIPS=$(GOMIPS) CGO_ENABLED=$(CGO_ENABLED) \
		go build -o "$(abspath $(GO_BIN))" .
	echo "$(VERSION)" > out/VERSION

_pkg-clean:
	rm -rf out/pkg
	mkdir -p out/pkg/control
	mkdir -p out/pkg/data

_pkg-control:
	version="$$(cat out/VERSION)"; \
	echo "Package: $(PACKAGE)" > out/pkg/control/control; \
	echo "Version: $$version-1" >> out/pkg/control/control; \
	echo "Depends: $(DEPENDENCIES)" >> out/pkg/control/control; \
	echo "Section: net" >> out/pkg/control/control; \
	echo "Architecture: $(PKG_ARCH)" >> out/pkg/control/control; \
	echo "License: MIT" >> out/pkg/control/control; \
	echo "Description: Telegram MTProto WS bridge proxy (Go binary)" >> out/pkg/control/control

_pkg-scripts:
	cp common/ipk/prerm out/pkg/control/prerm
	cp common/ipk/postinst out/pkg/control/postinst
	cp common/ipk/postrm out/pkg/control/postrm
	cp common/ipk/conffiles out/pkg/control/conffiles
	find out/pkg/control -type f -print0 | xargs -0 dos2unix
	chmod +x out/pkg/control/prerm out/pkg/control/postinst out/pkg/control/postrm

_pkg-data:
	mkdir -p out/pkg/data$(ROOT_DIR)/etc/init.d
	mkdir -p out/pkg/data$(ROOT_DIR)/etc
	mkdir -p out/pkg/data$(ROOT_DIR)/bin
	cp "$(GO_BIN)" out/pkg/data$(ROOT_DIR)/bin/tg-ws-proxy
	cat common/tg-ws-proxy-common.sh > out/pkg/data$(ROOT_DIR)/etc/init.d/S61tg-ws-proxy
	awk '/^start\(\)/{p=1} p{print}' common/S61tg-ws-proxy >> out/pkg/data$(ROOT_DIR)/etc/init.d/S61tg-ws-proxy
	cp common/tg-ws-proxy.conf out/pkg/data$(ROOT_DIR)/etc/tg-ws-proxy.conf
	find out/pkg/data -type f -print0 | xargs -0 dos2unix
	chmod +x out/pkg/data$(ROOT_DIR)/bin/tg-ws-proxy
	chmod +x out/pkg/data$(ROOT_DIR)/etc/init.d/S61tg-ws-proxy

_pkg-ipk: _pkg-clean _pkg-control _pkg-scripts _pkg-data
	cd out/pkg/control; tar czf ../control.tar.gz .; cd ../../..
	cd out/pkg/data; tar czf ../data.tar.gz .; cd ../../..
	echo 2.0 > out/pkg/debian-binary
	version="$$(cat out/VERSION)"; \
	cd out/pkg; tar czf ../$(PACKAGE)_$$version-1_$(PKG_ARCH).ipk control.tar.gz data.tar.gz debian-binary; cd ../..

tg-ws-proxy-ipk: _build-go _pkg-ipk
	@echo "Built: out/$(PACKAGE)_$$(cat out/VERSION)-1_$(PKG_ARCH).ipk"
