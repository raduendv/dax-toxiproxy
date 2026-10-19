SHELL := /bin/bash
TOXIPROXY_VERSION ?= 2.12.0
TOXIPROXY_BINARY ?= bin/toxiproxy-server

.PHONY: build-server start status list reset stop check

build-server:
	TOXIPROXY_VERSION='$(TOXIPROXY_VERSION)' scripts/build-server.sh --output '$(TOXIPROXY_BINARY)'

start: build-server
	TOXIPROXY_BINARY='$(TOXIPROXY_BINARY)' scripts/server.sh start

status:
	scripts/server.sh status

list:
	scripts/toxiproxyctl.sh list

reset:
	scripts/toxiproxyctl.sh reset

stop:
	scripts/server.sh stop

check:
	bash -n scripts/build-server.sh scripts/server.sh scripts/toxiproxyctl.sh
	@if command -v shellcheck >/dev/null 2>&1; then shellcheck scripts/*.sh; fi
