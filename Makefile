# wealthdb build & test orchestrator.
#
# Run every target from the repo root — no cd-ing into subdirectories,
# so a `git pull` or an agent refactor is followed by a single command.
#
#   make                  show this help
#   make all              build everything (gold engine + all collectors)
#   make test             test everything
#   make build-wealthdb   build the Go gold-engine image
#   make test-wealthdb    run `go test ./...` in the wealthdb container
#   make build-collectors build every collector
#   make test-collectors  test every collector
#   make build-<name>     build one collector   (e.g. make build-schwab-web)
#   make test-<name>      test one collector    (e.g. make test-schwab-web)
#
# A collector with a Docker wrapper (collectors/<name>/<name>) builds via
# `<wrapper> build` and tests with pytest inside the container; a host-venv
# collector (schwab-api, ubs-psn) builds its .venv from requirements.txt.
# Collectors that ship no tests are a no-op for the test target. Each
# test-<x> rebuilds its <x> first, so testing always runs current code.

# Auto-discover collectors: immediate subdirectories of collectors/.
# The `/.` matches directories only (files like collectors/README.md
# have no `.` entry), so non-collector files don't leak into the list.
COLLECTORS := $(sort $(notdir $(patsubst %/.,%,$(wildcard collectors/*/.))))

WEALTHDB      := wealthdb/wealthdb
WEALTHDB_TEST := wealthdb/wealthdb-test

# Interpreter for the host-venv collectors (schwab-api, ubs-psn). Their
# deps (e.g. schwab-py) need Python >=3.10, but macOS /usr/bin/python3 is
# 3.9 and `make` may resolve a bare `python3` to it. Prefer a versioned
# python3.X from PATH, then a Homebrew install, then plain python3.
# Override explicitly:  make build-schwab-api PYTHON=python3.12
PYTHON := $(or \
  $(shell command -v python3.14 python3.13 python3.12 python3.11 python3.10 2>/dev/null | head -n1),\
  $(firstword $(wildcard /opt/homebrew/bin/python3.1[0-9] /usr/local/bin/python3.1[0-9])),\
  python3)

.DEFAULT_GOAL := help
.PHONY: all build test help \
        build-wealthdb test-wealthdb \
        build-collectors test-collectors \
        clean cleanall clean-wealthdb cleanall-wealthdb \
        clean-collectors cleanall-collectors

# ---- aggregates --------------------------------------------------------

all: build-wealthdb build-collectors
build: all
test: test-wealthdb test-collectors

build-collectors: $(addprefix build-,$(COLLECTORS))
test-collectors:  $(addprefix test-,$(COLLECTORS))

# clean    = build artefacts (pycache, pytest cache, Go build cache)
# cleanall = clean + the heavy outputs (docker images, venvs)
clean:    clean-wealthdb clean-collectors
cleanall: cleanall-wealthdb cleanall-collectors

clean-collectors:    $(addprefix clean-,$(COLLECTORS))
cleanall-collectors: $(addprefix cleanall-,$(COLLECTORS))

# ---- wealthdb gold engine ---------------------------------------------

build-wealthdb:
	@echo "==> build wealthdb (docker image)"
	$(WEALTHDB) build

test-wealthdb: build-wealthdb
	@echo "==> test wealthdb (go test ./...)"
	$(WEALTHDB_TEST) ./...

clean-wealthdb:
	@echo "==> clean wealthdb (go build cache)"
	@chmod -R u+w $(HOME)/.cache/wealthdb-test/go-build 2>/dev/null || true
	@rm -rf $(HOME)/.cache/wealthdb-test/go-build

cleanall-wealthdb: clean-wealthdb
	@echo "==> cleanall wealthdb (image + go caches)"
	@docker image rm -f wealthdb:latest >/dev/null 2>&1 || true
	@chmod -R u+w $(HOME)/.cache/wealthdb-test 2>/dev/null || true
	@rm -rf $(HOME)/.cache/wealthdb-test

# ---- per-collector rules (generated for each discovered collector) ----

define COLLECTOR_RULES
.PHONY: build-$(1) test-$(1) clean-$(1) cleanall-$(1)

build-$(1):
	@echo "==> build collector: $(1)"
	@if [ -x collectors/$(1)/$(1) ] && [ -f collectors/$(1)/Dockerfile ]; then \
		collectors/$(1)/$(1) build; \
	elif [ -f collectors/$(1)/requirements.txt ]; then \
		if [ ! -x collectors/$(1)/.venv/bin/python ] || \
		   ! collectors/$(1)/.venv/bin/python -c 'import sys; sys.exit(0 if sys.version_info >= (3,10) else 1)' 2>/dev/null; then \
			$(PYTHON) -c 'import sys; sys.exit(0 if sys.version_info >= (3,10) else 1)' 2>/dev/null || { \
				echo "    ERROR: '$(PYTHON)' is older than 3.10; $(1) needs >=3.10." >&2; \
				echo "    Re-run with a newer interpreter, e.g.: make build-$(1) PYTHON=python3.14" >&2; \
				exit 1; }; \
			rm -rf collectors/$(1)/.venv; \
			$(PYTHON) -m venv collectors/$(1)/.venv; \
		fi; \
		collectors/$(1)/.venv/bin/pip install -q -r collectors/$(1)/requirements.txt; \
	else \
		echo "    $(1): nothing to build"; \
	fi

test-$(1): build-$(1)
	@echo "==> test collector: $(1)"
	@if [ ! -d collectors/$(1)/tests ] && ! ls collectors/$(1)/test_*.py >/dev/null 2>&1; then \
		echo "    $(1): no tests"; \
	elif [ -x collectors/$(1)/$(1) ] && [ -f collectors/$(1)/Dockerfile ]; then \
		collectors/$(1)/$(1) sh -c "cd /app && pytest -q"; \
	elif [ -x collectors/$(1)/.venv/bin/python ]; then \
		collectors/$(1)/.venv/bin/python -m pytest -q collectors/$(1); \
	else \
		echo "    $(1): cannot run tests (no image or venv)"; \
	fi

clean-$(1):
	@echo "==> clean collector: $(1)"
	@find collectors/$(1) -name .venv -prune -o -type d \( -name __pycache__ -o -name .pytest_cache \) -exec rm -rf {} + 2>/dev/null || true

cleanall-$(1): clean-$(1)
	@echo "==> cleanall collector: $(1)"
	@if [ -f collectors/$(1)/Dockerfile ]; then \
		echo "    rm image wealthdb/$(1):latest"; \
		docker image rm -f wealthdb/$(1):latest >/dev/null 2>&1 || true; \
	fi
	@if [ -d collectors/$(1)/.venv ]; then \
		echo "    rm collectors/$(1)/.venv"; \
		rm -rf collectors/$(1)/.venv; \
	fi
endef

$(foreach c,$(COLLECTORS),$(eval $(call COLLECTOR_RULES,$(c))))

# ---- help --------------------------------------------------------------

help:
	@echo "wealthdb build/test — run from the repo root:"
	@echo ""
	@echo "  make all                build everything (engine + all collectors)"
	@echo "  make test               test everything"
	@echo "  make build-wealthdb     build the Go gold-engine image"
	@echo "  make test-wealthdb      run go test ./... in the wealthdb container"
	@echo "  make build-collectors   build every collector"
	@echo "  make test-collectors    test every collector"
	@echo "  make build-<name>       build one collector (e.g. build-schwab-web)"
	@echo "  make test-<name>        test one collector  (e.g. test-schwab-web)"
	@echo ""
	@echo "  make clean              remove build artefacts (pycache, caches)"
	@echo "  make cleanall           also remove docker images + venvs"
	@echo "  make clean-<name> / cleanall-<name>   (incl. -wealthdb, -collectors)"
	@echo ""
	@echo "  collectors: $(COLLECTORS)"
