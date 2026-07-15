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
#   make test-collectorkit test the shared collectorkit library
#   make build-<name>     build one collector   (e.g. make build-schwab-web)
#   make test-<name>      test one collector    (e.g. make test-schwab-web)
#   make install          symlink wealthdb + wealthdb-collect into ~/bin
#
# A collector with a Docker wrapper (collectors/<name>/<name>) builds via
# `<wrapper> build` and tests with pytest inside the container; a host-venv
# collector (schwab-api, ubs-psn) builds its .venv from requirements.txt.
# Collectors that ship no tests are a no-op for the test target. Each
# test-<x> rebuilds its <x> first, so testing always runs current code.
# `make test` also runs the shared collectorkit library's own suite
# (test-collectorkit), which otherwise runs nowhere.

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
.PHONY: all build test help install uninstall \
        build-wealthdb test-wealthdb \
        build-web test-web clean-web cleanall-web \
        build-collectors test-collectors \
        test-collectorkit clean-collectorkit cleanall-collectorkit \
        test-wrappers \
        clean cleanall clean-wealthdb cleanall-wealthdb \
        clean-collectors cleanall-collectors base-images \
        update update-venvs update-wealthdb update-bases

# ---- aggregates --------------------------------------------------------

all: build-wealthdb build-web build-collectors
build: all
test: test-wealthdb test-web test-collectors test-collectorkit test-wrappers

# ---- install -----------------------------------------------------------
# Symlink the two top-level entry points onto PATH so they work from any
# directory (and the repo dir no longer needs to be on PATH):
#   wealthdb         the gold-engine wrapper (wealthdb/wealthdb)
#   wealthdb-collect the collector dispatcher (bin/wealthdb-collect)
# BINDIR defaults to ~/bin; override e.g. `make install BINDIR=/usr/local/bin`.
# Per-collector wrappers stay in the repo (the dispatcher resolves them);
# personal orchestration (wealthdb-nightly / wealthdb-refresh) is not
# installed here.
BINDIR    ?= $(HOME)/bin
REPO_ROOT := $(abspath .)

install:
	@mkdir -p "$(BINDIR)"
	@ln -sf "$(REPO_ROOT)/wealthdb/wealthdb"     "$(BINDIR)/wealthdb"
	@ln -sf "$(REPO_ROOT)/bin/wealthdb-collect"  "$(BINDIR)/wealthdb-collect"
	@echo "  linked $(BINDIR)/wealthdb         -> $(REPO_ROOT)/wealthdb/wealthdb"
	@echo "  linked $(BINDIR)/wealthdb-collect -> $(REPO_ROOT)/bin/wealthdb-collect"
	@case ":$$PATH:" in *":$(BINDIR):"*) ;; \
	  *) echo "  note: $(BINDIR) is not on your PATH — add it to run these bare";; esac

uninstall:
	@rm -f "$(BINDIR)/wealthdb" "$(BINDIR)/wealthdb-collect"
	@echo "  removed $(BINDIR)/wealthdb and $(BINDIR)/wealthdb-collect"

build-collectors: $(addprefix build-,$(COLLECTORS))
test-collectors:  $(addprefix test-,$(COLLECTORS))

# Shared Docker base images: shared/images/*.Dockerfile, built with
# context=shared/ so they can bake in collectorkit. Docker collectors
# FROM these. Building one Docker collector on its own? run this first.
#
# The order matters: base-camoufox FROMs base-playwright, so the parent
# has to be built first. Explicit list rather than the alphabetised glob
# (which would put camoufox before playwright).
BASE_IMAGES := base-python base-playwright base-camoufox

# --provenance=false on every docker build: buildx attaches a provenance
# attestation by default, and it embeds build metadata, so the image digest
# changes on EVERY build even when all layers are CACHED. A base image whose
# digest moves invalidates `FROM wealthdb/base-*` in all 15 collectors, so
# each one re-ran its whole Dockerfile (pip install and all) on every build.
# These images are local-only and never pushed; nothing consumes the
# attestation.
base-images:
	@for img in $(BASE_IMAGES); do \
		f="shared/images/$$img.Dockerfile"; \
		[ -e "$$f" ] || continue; \
		echo "==> build base image wealthdb/$$img:latest"; \
		docker build -q --provenance=false -f "$$f" -t "wealthdb/$$img:latest" shared/ >/dev/null; \
	done

# clean    = build artefacts (pycache, pytest cache, Go build cache)
# cleanall = clean + the heavy outputs (docker images, venvs)
clean:    clean-wealthdb clean-web clean-collectors clean-collectorkit
cleanall: cleanall-wealthdb cleanall-web cleanall-collectors cleanall-collectorkit

clean-collectors:    $(addprefix clean-,$(COLLECTORS))
cleanall-collectors: $(addprefix cleanall-,$(COLLECTORS))

# ---- collectorkit (shared library) tests ------------------------------
# collectorkit is a dependency-free library installed editable into every
# collector's venv, but its own test suite (shared/collectorkit/tests) runs
# nowhere else, so `make test` runs it here. Its venv adds pytest plus
# zstandard so the compress / recompress suites execute rather than skip.
CK_DIR  := shared/collectorkit
CK_VENV := $(CK_DIR)/.venv

test-collectorkit:
	@echo "==> test collectorkit"
	@if [ ! -x $(CK_VENV)/bin/python ] || \
	   ! $(CK_VENV)/bin/python -c 'import sys; sys.exit(0 if sys.version_info >= (3,10) else 1)' 2>/dev/null; then \
		$(PYTHON) -c 'import sys; sys.exit(0 if sys.version_info >= (3,10) else 1)' 2>/dev/null || { \
			echo "    ERROR: '$(PYTHON)' is older than 3.10; collectorkit needs >=3.10." >&2; \
			echo "    Re-run with a newer interpreter, e.g.: make test-collectorkit PYTHON=python3.14" >&2; \
			exit 1; }; \
		rm -rf $(CK_VENV); \
		$(PYTHON) -m venv $(CK_VENV); \
	fi
	@$(CK_VENV)/bin/pip install -q -e $(CK_DIR) pytest zstandard
	@$(CK_VENV)/bin/python -m pytest -q -p no:cacheprovider $(CK_DIR)/tests

test-wrappers:
	@echo "==> test wrappers (shared/wrappers)"
	@for t in shared/wrappers/tests/*.sh; do bash "$$t" || exit 1; done

clean-collectorkit:
	@echo "==> clean collectorkit"
	@find $(CK_DIR) -name .venv -prune -o -type d \( -name __pycache__ -o -name .pytest_cache \) -exec rm -rf {} + 2>/dev/null || true

cleanall-collectorkit: clean-collectorkit
	@echo "==> cleanall collectorkit"
	@rm -rf $(CK_VENV) $(CK_DIR)/collectorkit.egg-info

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

# ---- web (optional Metabase BI server) --------------------------------

build-web:
	@echo "==> build web (metabase image)"
	web/web build

# Pure-bash unit tests for the lifecycle script; no Docker needed.
test-web:
	@echo "==> test web (web/test_web.sh)"
	@web/test_web.sh

clean-web:
	@echo "==> clean web (nothing to clean)"

cleanall-web: clean-web
	@echo "==> cleanall web (image)"
	@docker image rm -f wealthdb/metabase:latest >/dev/null 2>&1 || true

# ---- per-collector rules (generated for each discovered collector) ----

define COLLECTOR_RULES
.PHONY: build-$(1) test-$(1) clean-$(1) cleanall-$(1)

# A Dockerfile makes a collector Docker-built; a requirements.txt makes
# it host-venv-built. A HYBRID collector has both a Dockerfile AND a
# `.host-venv` marker (e.g. schwab-api: a Camoufox/VNC `login` image plus
# a host venv for download/load) — it builds and tests both.
# base-images first: collectorkit is BAKED INTO the base images, so a
# collector container only sees a collectorkit change once the bases are
# rebuilt. Hanging this off the per-collector build (rather than the
# build-collectors aggregate) is what makes a bare `make test-<one>` correct
# too — otherwise it silently exercises whatever collectorkit the image was
# last built with, and a stale green looks exactly like a real one. Make
# updates a prerequisite at most once per invocation, so the fleet-wide
# targets still pay for it only once.
build-$(1): base-images
	@echo "==> build collector: $(1)"
	@if [ -x collectors/$(1)/$(1) ] && [ -f collectors/$(1)/Dockerfile ]; then \
		collectors/$(1)/$(1) build; \
	fi
	@if [ -f collectors/$(1)/requirements.txt ] && { [ ! -f collectors/$(1)/Dockerfile ] || [ -f collectors/$(1)/.host-venv ]; }; then \
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
		collectors/$(1)/.venv/bin/pip install -q -e shared/collectorkit; \
	elif [ ! -f collectors/$(1)/Dockerfile ]; then \
		echo "    $(1): nothing to build"; \
	fi

test-$(1): build-$(1)
	@echo "==> test collector: $(1)"
	@if [ ! -d collectors/$(1)/tests ] && ! ls collectors/$(1)/test_*.py >/dev/null 2>&1; then \
		echo "    $(1): no tests"; \
	elif [ -f collectors/$(1)/.host-venv ] && [ -x collectors/$(1)/.venv/bin/python ]; then \
		collectors/$(1)/.venv/bin/python -m pytest -q -p no:cacheprovider collectors/$(1); \
	elif [ -x collectors/$(1)/$(1) ] && [ -f collectors/$(1)/Dockerfile ]; then \
		collectors/$(1)/$(1) sh -c "cd /app && pytest -q -p no:cacheprovider"; \
	elif [ -x collectors/$(1)/.venv/bin/python ]; then \
		collectors/$(1)/.venv/bin/python -m pytest -q -p no:cacheprovider collectors/$(1); \
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

# ---- update: refresh all tooling --------------------------------------
#
# `make update` brings every layer of the toolchain forward in one shot:
#
#   - pip + project deps in every host venv (schwab-api, ubs-psn, …)
#   - Go modules in wealthdb/ (go get -u all + go mod tidy)
#   - Shared Docker base images, rebuilt with --pull so the underlying
#     OS layers also refresh
#
# After this, run `make all` if you want the collector Docker images
# rebuilt on top of the refreshed bases (the host venvs and the gold
# engine pick up their updates immediately).
#
# Host venvs are auto-discovered as any collectors/<name>/.venv that
# already exists — the venv has to have been created by `make build-<name>`
# at least once. Docker collectors have no .venv on the host; their pip
# is inside the image and refreshes when the image rebuilds.
HOST_VENV_COLLECTORS := $(patsubst collectors/%/.venv,%,$(wildcard collectors/*/.venv))

update: update-venvs update-wealthdb update-bases
	@echo ""
	@echo "==> update done. Run \`make all\` to rebuild collector images on top of the refreshed bases."

update-venvs:
	@if [ -z "$(HOST_VENV_COLLECTORS)" ]; then \
		echo "==> no host venvs to update (collectors/*/.venv)"; \
	fi
	@for c in $(HOST_VENV_COLLECTORS); do \
		venv=collectors/$$c/.venv; \
		echo "==> update collectors/$$c/.venv"; \
		"$$venv/bin/python" -m pip install --upgrade pip setuptools wheel; \
		if [ -f "collectors/$$c/requirements.txt" ]; then \
			"$$venv/bin/python" -m pip install --upgrade -r "collectors/$$c/requirements.txt"; \
		fi; \
		"$$venv/bin/python" -m pip install --upgrade -e shared/collectorkit; \
	done

update-wealthdb:
	@echo "==> update wealthdb/ go modules"
	@cd wealthdb && go get -u ./... && go mod tidy
	@echo "==> rebuild wealthdb image so the refreshed modules land in the build cache"
	$(WEALTHDB) build

update-bases:
	@for img in $(BASE_IMAGES); do \
		f="shared/images/$$img.Dockerfile"; \
		[ -e "$$f" ] || continue; \
		if grep -qE '^FROM[[:space:]]+wealthdb/' "$$f"; then \
			echo "==> rebuild base wealthdb/$$img:latest (FROM is local, no --pull)"; \
			docker build --provenance=false -f "$$f" -t "wealthdb/$$img:latest" shared/; \
		else \
			echo "==> rebuild base wealthdb/$$img:latest (--pull)"; \
			docker build --pull --provenance=false -f "$$f" -t "wealthdb/$$img:latest" shared/; \
		fi; \
	done

# ---- help --------------------------------------------------------------

help:
	@echo "wealthdb build/test — run from the repo root:"
	@echo ""
	@echo "  make all                build everything (engine + all collectors)"
	@echo "  make test               test everything"
	@echo "  make build-wealthdb     build the Go gold-engine image"
	@echo "  make test-wealthdb      run go test ./... in the wealthdb container"
	@echo "  make build-web          build the optional Metabase BI image"
	@echo "  make test-web           run the web lifecycle unit tests"
	@echo "  make build-collectors   build every collector"
	@echo "  make test-collectors    test every collector"
	@echo "  make test-collectorkit  run the shared collectorkit test suite"
	@echo "  make build-<name>       build one collector (e.g. build-schwab-web)"
	@echo "  make test-<name>        test one collector  (e.g. test-schwab-web)"
	@echo ""
	@echo "  make install            symlink wealthdb + wealthdb-collect into BINDIR (~/bin)"
	@echo "  make uninstall          remove those symlinks"
	@echo ""
	@echo "  make clean              remove build artefacts (pycache, caches)"
	@echo "  make cleanall           also remove docker images + venvs"
	@echo "  make clean-<name> / cleanall-<name>   (incl. -wealthdb, -collectors)"
	@echo ""
	@echo "  make update             refresh all tooling: pip in every host venv,"
	@echo "                          go modules in wealthdb/, shared Docker bases"
	@echo "  make update-venvs / update-wealthdb / update-bases     (one layer at a time)"
	@echo ""
	@echo "  collectors: $(COLLECTORS)"
