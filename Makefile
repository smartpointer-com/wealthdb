# wealthdb build & test orchestrator.
#
# Run every target from the repo root — no cd-ing into subdirectories,
# so a `git pull` or an agent refactor is followed by a single command.
# `make` with no target lists the targets.
#
# WEALTHDB_IMAGE_TAG is the tag of every image built, run and removed:
# when it is unset, the first line of the checkout's git-ignored
# .image-tag, else latest. A second checkout writes its tag there once
# and works on images of its own.
#
# A collector with a Docker wrapper (collectors/<name>/<name>) builds via
# `<wrapper> build` and tests with pytest inside the container; a host-venv
# collector (curves, fred, manual, plaid, refdata, svb, ubs-psn) builds its
# .venv from requirements.txt. schwab-api and fidelity-web are hybrids:
# they Docker-build AND carry a host venv (`.host-venv` marker), and test
# on that venv.
# Collectors that ship no tests are a no-op for the test target. Each
# test-<x> rebuilds its <x> first, so testing always runs current code.
# `make test` also runs the shared collectorkit library's own suite
# (test-collectorkit), which otherwise runs nowhere.

# Auto-discover collectors: immediate subdirectories of collectors/.
# The `/.` matches directories only (files like collectors/README.md
# have no `.` entry), so non-collector files don't leak into the list.
COLLECTORS := $(sort $(notdir $(patsubst %/.,%,$(wildcard collectors/*/.))))

# The image tag (see the header), as shared/wrappers/image-tag.sh resolves
# it for every wrapper. override: an empty WEALTHDB_IMAGE_TAG= on the
# command line resolves like an unset one. Exported, so the wrappers the
# targets call read the same tag.
ROOT_DIR := $(patsubst %/,%,$(dir $(abspath $(lastword $(MAKEFILE_LIST)))))
override WEALTHDB_IMAGE_TAG := $(shell WEALTHDB_IMAGE_TAG='$(WEALTHDB_IMAGE_TAG)' sh -c \
	'. "$$1/shared/wrappers/image-tag.sh" && wealthdb_image_tag "$$1"' sh '$(ROOT_DIR)')
ifeq ($(WEALTHDB_IMAGE_TAG),)
$(error no image tag: fix .image-tag, or set WEALTHDB_IMAGE_TAG)
endif
export WEALTHDB_IMAGE_TAG
TAG := $(WEALTHDB_IMAGE_TAG)

WEALTHDB      := wealthdb/wealthdb
WEALTHDB_TEST := wealthdb/wealthdb-test
WEALTHDB_GO   := wealthdb/wealthdb-go

# Interpreter for the host-venv collectors and the hybrids' host venvs
# (curves, fred, manual, plaid, refdata, svb, ubs-psn; schwab-api,
# fidelity-web). Their deps (e.g. schwab-py) need Python >=3.10, but macOS
# /usr/bin/python3 is 3.9 and `make` may resolve a bare `python3` to it.
# Prefer a versioned python3.X from PATH, then a Homebrew install, then
# plain python3.
# Override explicitly:  make build-schwab-api PYTHON=python3.12
PYTHON := $(or \
  $(shell for p in python3.14 python3.13 python3.12 python3.11 python3.10; do command -v $$p && break; done 2>/dev/null),\
  $(firstword $(wildcard /opt/homebrew/bin/python3.1[0-9] /usr/local/bin/python3.1[0-9])),\
  python3)

# ENSURE_VENV dir,label,target — create the venv at dir from $(PYTHON),
# unless one on Python >=3.10 is there. label names what needs it in the
# error, and target is the make target to re-run with a newer PYTHON.
define ENSURE_VENV
if [ ! -x $(1)/bin/python ] || \
   ! $(1)/bin/python -c 'import sys; sys.exit(0 if sys.version_info >= (3,10) else 1)' 2>/dev/null; then \
	$(PYTHON) -c 'import sys; sys.exit(0 if sys.version_info >= (3,10) else 1)' 2>/dev/null || { \
		echo "    ERROR: '$(PYTHON)' is older than 3.10; $(2) needs >=3.10." >&2; \
		echo "    Re-run with a newer interpreter, e.g.: make $(3) PYTHON=python3.14" >&2; \
		exit 1; }; \
	rm -rf $(1); \
	$(PYTHON) -m venv $(1); \
fi
endef

.DEFAULT_GOAL := help
.PHONY: all build test help install uninstall hooks \
        build-wealthdb test-wealthdb \
        build-web test-web clean-web cleanall-web test-mcp \
        build-collectors test-collectors \
        test-collectorkit clean-collectorkit cleanall-collectorkit \
        test-wrappers \
        clean cleanall clean-wealthdb cleanall-wealthdb \
        clean-collectors cleanall-collectors base-images cleanall-bases \
        update update-venvs update-wealthdb update-bases \
        lint lint-go lint-python cleanall-lint \
        demo demo-roll demo-web demo-web-stop demo-mcp demo-mcp-stop test-demo

# ---- aggregates --------------------------------------------------------

all: build-wealthdb build-web build-collectors
build: all
test: test-wealthdb test-web test-mcp test-collectors test-collectorkit test-wrappers test-demo

# ---- install -----------------------------------------------------------
# Symlink the two top-level entry points onto PATH so they work from any
# directory, without the repo dir on PATH:
#   wealthdb         the gold-engine wrapper (wealthdb/wealthdb)
#   wealthdb-collect the collector dispatcher (bin/wealthdb-collect)
# BINDIR defaults to ~/.local/bin (the XDG-conventional user bin dir,
# matching the config default under ~/.config); override e.g.
# `make install BINDIR=/usr/local/bin`.
# Per-collector wrappers stay in the repo (the dispatcher resolves them).
BINDIR ?= $(HOME)/.local/bin

install: hooks
	@mkdir -p "$(BINDIR)"
	@ln -sf "$(ROOT_DIR)/wealthdb/wealthdb"     "$(BINDIR)/wealthdb"
	@ln -sf "$(ROOT_DIR)/bin/wealthdb-collect"  "$(BINDIR)/wealthdb-collect"
	@echo "  linked $(BINDIR)/wealthdb         -> $(ROOT_DIR)/wealthdb/wealthdb"
	@echo "  linked $(BINDIR)/wealthdb-collect -> $(ROOT_DIR)/bin/wealthdb-collect"
	@case ":$$PATH:" in *":$(BINDIR):"*) ;; \
	  *) echo "  note: $(BINDIR) is not on your PATH — add it to run these bare";; esac

uninstall:
	@rm -f "$(BINDIR)/wealthdb" "$(BINDIR)/wealthdb-collect"
	@echo "  removed $(BINDIR)/wealthdb and $(BINDIR)/wealthdb-collect"
	@if [ "$$(git config --get core.hooksPath)" = .githooks ]; then \
		git config --unset core.hooksPath; \
		echo "  removed the git hooks (core.hooksPath)"; \
	fi

# The repo's git hooks live in .githooks/, tracked, and git runs them once
# core.hooksPath names that dir. The pre-commit hook runs `make lint` on the
# staged tree and refuses a commit it fails on or rewrites.
hooks:
	@git config core.hooksPath .githooks
	@echo "  git hooks: core.hooksPath -> .githooks (pre-commit runs make lint)"

build-collectors: $(addprefix build-,$(COLLECTORS))
test-collectors:  $(addprefix test-,$(COLLECTORS))

# Shared Docker base images: shared/images/*.Dockerfile, built with
# context=shared/ so they can bake in collectorkit. Docker collectors
# FROM these.
#
# The order matters: base-camoufox FROMs base-playwright, so the parent
# has to be built first. Explicit list rather than the alphabetised glob
# (which would put camoufox before playwright).
BASE_IMAGES := base-python base-playwright base-camoufox

# BUILD_BASES quiet,pull — build the shared bases in order, and stop at
# the first that fails. quiet hides docker's output. pull refreshes the
# upstream image a base builds FROM; a base FROM a local wealthdb/ image
# never pulls.
#
# --provenance=false on every docker build: buildx attaches a provenance
# attestation by default, and it embeds build metadata, so the image digest
# changes on EVERY build even when all layers are CACHED. A base image whose
# digest moves invalidates `FROM wealthdb/base-*` in every Docker
# collector, so each one would re-run its whole Dockerfile (pip install
# and all) on every build.
# These images are local-only and never pushed; nothing consumes the
# attestation.
define BUILD_BASES
@for img in $(BASE_IMAGES); do \
	f="shared/images/$$img.Dockerfile"; \
	[ -e "$$f" ] || continue; \
	pull=; \
	if [ -n "$(2)" ] && ! grep -qE '^FROM[[:space:]]+wealthdb/' "$$f"; then pull=--pull; fi; \
	echo "==> build base image wealthdb/$$img:$(TAG)$${pull:+ ($$pull)}"; \
	docker build $(if $(1),-q) $$pull --provenance=false \
		--build-arg WEALTHDB_IMAGE_TAG=$(TAG) \
		-f "$$f" -t "wealthdb/$$img:$(TAG)" shared/ $(if $(1),>/dev/null) || exit 1; \
done
endef

base-images:
	$(call BUILD_BASES,quiet,)

# clean    = build artefacts (pycache, pytest cache, Go build cache)
# cleanall = clean + the heavy outputs (docker images, venvs)
clean:    clean-wealthdb clean-web clean-collectors clean-collectorkit
cleanall: cleanall-wealthdb cleanall-web cleanall-collectors cleanall-bases \
          cleanall-collectorkit cleanall-lint

clean-collectors:    $(addprefix clean-,$(COLLECTORS))
cleanall-collectors: $(addprefix cleanall-,$(COLLECTORS))

# Child first: docker frees an image's layers only once no image builds
# on it. cleanall runs this after the collectors, whose images build on
# the bases.
cleanall-bases:
	@echo "==> cleanall bases (shared base images)"
	@order=; for img in $(BASE_IMAGES); do order="$$img $$order"; done; \
	for img in $$order; do \
		echo "    rm image wealthdb/$$img:$(TAG)"; \
		docker image rm -f "wealthdb/$$img:$(TAG)" >/dev/null 2>&1 || true; \
	done

# ---- collectorkit (shared library) tests ------------------------------
# collectorkit is a dependency-free library installed editable into every
# collector's venv, but its own test suite (shared/collectorkit/tests) runs
# nowhere else, so `make test` runs it here. Its venv adds pytest plus
# zstandard so the compress / recompress suites execute rather than skip.
CK_DIR  := shared/collectorkit
CK_VENV := $(CK_DIR)/.venv
# Test-only deps of the collectorkit venv (the library itself is
# dependency-free). Shared with update-venvs so the two can't drift.
CK_TEST_DEPS := pytest zstandard

# pip runs only when the venv's .stamp is older than collectorkit's
# pyproject.toml, or names other test deps than CK_TEST_DEPS. The
# editable install picks up source edits without a reinstall.
test-collectorkit:
	@echo "==> test collectorkit"
	@$(call ENSURE_VENV,$(CK_VENV),collectorkit,test-collectorkit)
	@if [ ! $(CK_VENV)/.stamp -nt $(CK_DIR)/pyproject.toml ] || \
	   [ "$$(cat $(CK_VENV)/.stamp)" != "$(CK_TEST_DEPS)" ]; then \
		$(CK_VENV)/bin/pip install -q -e $(CK_DIR) $(CK_TEST_DEPS) && \
		echo "$(CK_TEST_DEPS)" > $(CK_VENV)/.stamp || exit 1; \
	fi
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

cleanall-lint:
	@echo "==> cleanall lint (ruff venv)"
	@rm -rf $(LINT_VENV)

# ---- lint ----------------------------------------------------------------
# gofmt and go vet run on the image's toolchain like every other Go command
# (gofmt as `go run cmd/gofmt`, since the wrapper's entrypoint is `go`).
# They read the bind-mounted source, so the image only has to exist — it is
# built when missing, never rebuilt here, and linting never touches the
# image the nightly runs.
#
# ruff comes from a venv of its own, pinned in shared/lint/requirements.txt
# and refreshed by update-venvs; the rule set is /ruff.toml, which covers
# every Python module in the tree.
LINT_DIR  := shared/lint
LINT_VENV := $(LINT_DIR)/.venv

lint: lint-go lint-python

lint-go:
	@echo "==> lint wealthdb (gofmt, go vet)"
	@docker image inspect wealthdb:$(TAG) >/dev/null 2>&1 || $(WEALTHDB) build
	@out="$$($(WEALTHDB_GO) run cmd/gofmt -l .)" || exit 1; \
	if [ -n "$$out" ]; then \
		echo "    gofmt: not formatted:" >&2; echo "$$out" | sed 's/^/      /' >&2; exit 1; \
	fi
	@$(WEALTHDB_GO) vet ./...

lint-python:
	@echo "==> lint python (ruff)"
	@if [ ! -x $(LINT_VENV)/bin/ruff ]; then \
		rm -rf $(LINT_VENV); \
		$(PYTHON) -m venv $(LINT_VENV) && \
		$(LINT_VENV)/bin/pip install -q -r $(LINT_DIR)/requirements.txt || exit 1; \
	fi
	@$(LINT_VENV)/bin/ruff check --quiet .

# ---- wealthdb gold engine ---------------------------------------------

build-wealthdb:
	@echo "==> build wealthdb (docker image)"
	$(WEALTHDB) build

test-wealthdb: build-wealthdb
	@echo "==> test wealthdb (go test ./...)"
	$(WEALTHDB_TEST) ./...

# Where wealthdb-go parks the containerised Go toolchain caches
# (must mirror CACHE in wealthdb/wealthdb-go).
GO_CACHE := $(or $(XDG_CACHE_HOME),$(HOME)/.cache)/wealthdb/go

clean-wealthdb:
	@echo "==> clean wealthdb (go build cache)"
	@chmod -R u+w $(GO_CACHE)/go-build 2>/dev/null || true
	@rm -rf $(GO_CACHE)/go-build

cleanall-wealthdb: clean-wealthdb
	@echo "==> cleanall wealthdb (image + go caches)"
	@docker image rm -f wealthdb:$(TAG) >/dev/null 2>&1 || true
	@chmod -R u+w $(GO_CACHE) 2>/dev/null || true
	@rm -rf $(GO_CACHE)

# ---- demo household ----------------------------------------------------
# A fully synthetic household, built through the real load path, for
# trying wealthdb without a bank (demo/README.md). Everything lives under
# WEALTHDB_DEMO_ROOT: the engine is pointed at that directory as its data
# root and its config dir, so it sees nothing else, and the demo's
# Metabase runs as its own container on the port its config names. The
# containers are named after the demo root: wealthdb-metabase-demo for
# ~/wealthdb-demo, wealthdb-metabase-other-demo for ~/wealthdb-other-demo.
#
#   make demo [WEALTHDB_DEMO_ROOT=~/wealthdb-demo] [AS_OF=YYYY-MM-DD] [SEED=…] [FINDINGS=1]
#
# WEALTHDB_DEMO_ROOT resolves as shared/wrappers/demo-root.sh says, and,
# as with the image tag, an empty one on the command line reads as unset.
#
# `demo` rebuilds silver and gold from scratch; `demo-roll` appends the
# days since the last build to silver and loads only those. The generator
# writes only into a new, empty or marked demo root, and the web targets
# run only against a marked one.
# AS_OF, SEED and FINDINGS are plain assignments, so only the command line
# sets them: a variable of that name exported for another tool is ignored.
override WEALTHDB_DEMO_ROOT := $(shell WEALTHDB_DEMO_ROOT='$(WEALTHDB_DEMO_ROOT)' sh -c \
	'. "$$1/shared/wrappers/demo-root.sh" && wealthdb_demo_root "$$1"' sh '$(ROOT_DIR)')
ifeq ($(WEALTHDB_DEMO_ROOT),)
$(error no demo root: fix .demo-root, or set WEALTHDB_DEMO_ROOT)
endif
AS_OF    =
SEED     = harlow-19
FINDINGS =
# Absolute and with a leading ~ expanded, so the generator and the engine
# container see the same directory.
DEMO_ROOT := $(abspath $(patsubst ~/%,$(HOME)/%,$(patsubst ~,$(HOME),$(WEALTHDB_DEMO_ROOT))))
# The demo's containers take their name from the last part of its root.
# Demo roots whose last parts differ get their own containers. Every demo
# serves on the same ports, so one runs at a time.
DEMO_NAME := $(shell sh -c '. "$$1/shared/wrappers/demo-root.sh" && wealthdb_demo_name "$$2"' \
	sh '$(ROOT_DIR)' '$(DEMO_ROOT)')
DEMO_ENV := WEALTHDB_DATA_ROOT="$(DEMO_ROOT)" XDG_CONFIG_HOME="$(DEMO_ROOT)"
DEMO_ENGINE := $(DEMO_ENV) $(WEALTHDB) -c "$(DEMO_ROOT)/wealthdb.cfg"
DEMO_WEB := $(DEMO_ENV) WEALTHDB_CONFIG="$(DEMO_ROOT)/wealthdb.cfg" \
            WEALTHDB_WEB_CONTAINER=wealthdb-metabase-$(DEMO_NAME) \
            WEALTHDB_WEB_DATA_DIR="$(DEMO_ROOT)/web" \
            WEALTHDB_WEB_ENV_FILE="$(DEMO_ROOT)/web.env" $(WEALTHDB) web
DEMO_MCP := $(DEMO_ENV) WEALTHDB_CONFIG="$(DEMO_ROOT)/wealthdb.cfg" \
            WEALTHDB_MCP_CONTAINER=wealthdb-mcp-$(DEMO_NAME) \
            WEALTHDB_MCP_DATA_DIR="$(DEMO_ROOT)/mcp" \
            WEALTHDB_MCP_ENV_FILE="$(DEMO_ROOT)/mcp.env" $(WEALTHDB) mcp
DEMO_GEN_ARGS = --root "$(DEMO_ROOT)" --seed "$(SEED)" $(if $(AS_OF),--as-of $(AS_OF))
DEMO_ROOT_MARKED = @test -f "$(DEMO_ROOT)/.wealthdb-demo" || \
	{ echo "$@: $(DEMO_ROOT) is not a demo root; run make demo first" >&2; exit 2; }

demo: build-wealthdb
	@echo "==> generate the demo household into $(DEMO_ROOT)"
	@$(PYTHON) demo/generate.py $(DEMO_GEN_ARGS) $(if $(FINDINGS),--with-findings)
	@$(DEMO_ENGINE) init
	@$(DEMO_ENGINE) load -a

demo-roll: build-wealthdb
	@echo "==> append the days since the last demo build"
	@$(PYTHON) demo/generate.py $(DEMO_GEN_ARGS) --append
	@$(DEMO_ENGINE) load -a

# `web refresh` re-materializes the serving tables, returns, gains and exposure and
# re-snapshots gold (restarting the container when it runs); `web start` then
# brings it up if it does not.
demo-web: build-web
	$(DEMO_ROOT_MARKED)
	@$(DEMO_WEB) refresh
	@$(DEMO_WEB) status | grep -q '^web: running' || $(DEMO_WEB) start

demo-web-stop:
	$(DEMO_ROOT_MARKED)
	@$(DEMO_WEB) stop

# The MCP server reads live gold, so there is nothing to refresh: a
# rebuilt image needs a restart, and `restart` also starts a stopped one.
demo-mcp: build-wealthdb
	$(DEMO_ROOT_MARKED)
	@$(DEMO_MCP) restart
	@$(DEMO_MCP) url

demo-mcp-stop:
	$(DEMO_ROOT_MARKED)
	@$(DEMO_MCP) stop

# The generator's own suite (stdlib unittest), then the demo loaded
# through the engine image, its first months and its whole history to
# today, in scratch roots under the cache dir — a directory Docker can
# see, holding nothing but the demo's test roots.
DEMO_TEST_ROOT := $(or $(XDG_CACHE_HOME),$(HOME)/.cache)/wealthdb/demo-test

test-demo: build-wealthdb
	@echo "==> test demo (generator, then the household through the engine: its first months and to today)"
	@WEALTHDB_DEMO_TEST_ROOT="$(DEMO_TEST_ROOT)" WEALTHDB_BIN="$(abspath $(WEALTHDB))" \
		$(PYTHON) -m unittest discover -s demo/tests -t demo

# ---- mcp (optional MCP server) ----------------------------------------
# The server is the engine image, so there is nothing to build; the
# lifecycle script has pure-bash unit tests, no Docker needed.
test-mcp:
	@echo "==> test mcp (mcp/test_mcp.sh)"
	@mcp/test_mcp.sh

# ---- web (optional Metabase BI server) --------------------------------

build-web:
	@echo "==> build web (metabase image)"
	web/web build

# Pure-bash unit tests for the lifecycle script, then provision.py's
# definitions; no Docker needed. provision.py's tests run the projection
# cards' SQL on DuckDB, so they run in a venv of their own that holds the
# duckdb module, pinned in web/requirements-test.txt and refreshed by
# update-venvs. pip runs only when the requirements are newer than the
# venv's .stamp.
WEB_VENV := web/.venv

test-web:
	@echo "==> test web (web/test_web.sh)"
	@$(call ENSURE_VENV,$(WEB_VENV),the web tests,test-web)
	@if [ ! $(WEB_VENV)/.stamp -nt web/requirements-test.txt ]; then \
		$(WEB_VENV)/bin/pip install -q -r web/requirements-test.txt && touch $(WEB_VENV)/.stamp || exit 1; \
	fi
	@WEALTHDB_WEB_PYTHON="$(abspath $(WEB_VENV))/bin/python" web/test_web.sh

clean-web:
	@echo "==> clean web (nothing to clean)"

cleanall-web: clean-web
	@echo "==> cleanall web (image, test venv)"
	@docker image rm -f wealthdb/metabase:$(TAG) >/dev/null 2>&1 || true
	@rm -rf $(WEB_VENV)

# ---- per-collector rules (generated for each discovered collector) ----

define COLLECTOR_RULES
.PHONY: build-$(1) test-$(1) clean-$(1) cleanall-$(1)

# A Dockerfile makes a collector Docker-built; a requirements.txt makes
# it host-venv-built. A HYBRID collector has both a Dockerfile AND a
# `.host-venv` marker (e.g. schwab-api: a Camoufox/VNC `login` image plus
# a host venv for download/load) — it builds both, and tests on the host
# venv.
# A Docker collector depends on base-images: collectorkit is BAKED INTO
# the base images, so a collector container only sees a collectorkit
# change once the bases are rebuilt. Hanging this off the per-collector
# build (rather than the build-collectors aggregate) is what makes a bare
# `make test-<one>` correct too — otherwise it silently exercises whatever
# collectorkit the image was last built with, and a stale green looks
# exactly like a real one. Make updates a prerequisite at most once per
# invocation, so the fleet-wide targets still pay for it only once.
#
# A host venv reruns pip only when its .stamp is older than the
# collector's requirements.txt or collectorkit's pyproject.toml. The
# editable collectorkit picks up source edits without a reinstall.
build-$(1): $(if $(wildcard collectors/$(1)/Dockerfile),base-images)
	@echo "==> build collector: $(1)"
	@if [ -x collectors/$(1)/$(1) ] && [ -f collectors/$(1)/Dockerfile ]; then \
		collectors/$(1)/$(1) build; \
	fi
	@if [ -f collectors/$(1)/requirements.txt ] && { [ ! -f collectors/$(1)/Dockerfile ] || [ -f collectors/$(1)/.host-venv ]; }; then \
		$(call ENSURE_VENV,collectors/$(1)/.venv,$(1),build-$(1)); \
		if [ ! collectors/$(1)/.venv/.stamp -nt collectors/$(1)/requirements.txt ] || \
		   [ ! collectors/$(1)/.venv/.stamp -nt $(CK_DIR)/pyproject.toml ]; then \
			collectors/$(1)/.venv/bin/pip install -q -r collectors/$(1)/requirements.txt && \
			collectors/$(1)/.venv/bin/pip install -q -e $(CK_DIR) && \
			touch collectors/$(1)/.venv/.stamp || exit 1; \
		fi; \
	elif [ ! -f collectors/$(1)/Dockerfile ]; then \
		echo "    $(1): nothing to build"; \
	fi

# A host-venv or hybrid collector tests on its venv. A Docker collector
# tests in its image, run directly rather than through the wrapper: no
# secrets or data mount, and a name docker picks, so a test never meets a
# live run's container. The checkout is mounted at /app, since not every
# image copies its tests.
test-$(1): build-$(1)
	@echo "==> test collector: $(1)"
	@if [ ! -d collectors/$(1)/tests ] && ! ls collectors/$(1)/test_*.py >/dev/null 2>&1; then \
		echo "    $(1): no tests"; \
	elif [ -f collectors/$(1)/.host-venv ] || [ ! -f collectors/$(1)/Dockerfile ]; then \
		if [ -x collectors/$(1)/.venv/bin/python ]; then \
			collectors/$(1)/.venv/bin/python -m pytest -q -p no:cacheprovider collectors/$(1); \
		else \
			echo "    $(1): cannot run tests (no venv)"; \
		fi; \
	else \
		docker run --rm --user "$$$$(id -u):$$$$(id -g)" -e HOME=/tmp \
			-v "$(ROOT_DIR)/collectors/$(1):/app" "wealthdb/$(1):$(TAG)" \
			sh -c "cd /app && pytest -q -p no:cacheprovider"; \
	fi

clean-$(1):
	@echo "==> clean collector: $(1)"
	@find collectors/$(1) -name .venv -prune -o -type d \( -name __pycache__ -o -name .pytest_cache \) -exec rm -rf {} + 2>/dev/null || true

cleanall-$(1): clean-$(1)
	@echo "==> cleanall collector: $(1)"
	@if [ -f collectors/$(1)/Dockerfile ]; then \
		echo "    rm image wealthdb/$(1):$(TAG)"; \
		docker image rm -f wealthdb/$(1):$(TAG) >/dev/null 2>&1 || true; \
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
#   - pip + project deps in every host venv: the collectors' own
#     (schwab-api, ubs-psn, …) plus collectorkit's test venv
#   - Go modules in wealthdb/ (go get -u + go mod tidy, run by the
#     image's own toolchain — see update-wealthdb)
#   - Shared Docker base images, rebuilt with --pull so the underlying
#     OS layers also refresh
#
# `make all` then rebuilds the collector images on the refreshed bases.
# The host venvs and the gold engine pick up their updates at once.
#
# Host venvs are auto-discovered as any collectors/<name>/.venv that
# already exists — the venv has to have been created by `make build-<name>`
# at least once. Docker collectors have no .venv on the host; their pip
# is inside the image and refreshes when the image rebuilds. The venvs
# outside collectors/ are collectorkit's test venv
# (`make test-collectorkit`), the web tests' (`make test-web`) and
# ruff's (`make lint`), each updated alongside them once it exists.
HOST_VENV_COLLECTORS := $(patsubst collectors/%/.venv,%,$(wildcard collectors/*/.venv))

update: update-venvs update-wealthdb update-bases
	@echo ""
	@echo "==> update done. Run \`make all\` to rebuild collector images on top of the refreshed bases."

update-venvs:
	@if [ -z "$(HOST_VENV_COLLECTORS)" ] && [ ! -x $(CK_VENV)/bin/python ]; then \
		echo "==> no host venvs to update"; \
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
	@if [ -x $(CK_VENV)/bin/python ]; then \
		echo "==> update $(CK_VENV)"; \
		$(CK_VENV)/bin/python -m pip install --upgrade pip setuptools wheel; \
		$(CK_VENV)/bin/python -m pip install --upgrade -e $(CK_DIR) $(CK_TEST_DEPS); \
	fi
	@if [ -x $(LINT_VENV)/bin/python ]; then \
		echo "==> update $(LINT_VENV)"; \
		$(LINT_VENV)/bin/python -m pip install --upgrade pip; \
		$(LINT_VENV)/bin/python -m pip install --upgrade -r $(LINT_DIR)/requirements.txt; \
	fi
	@if [ -x $(WEB_VENV)/bin/python ]; then \
		echo "==> update $(WEB_VENV)"; \
		$(WEB_VENV)/bin/python -m pip install --upgrade pip; \
		$(WEB_VENV)/bin/python -m pip install --upgrade -r web/requirements-test.txt; \
	fi

# The module graph is refreshed by the IMAGE's Go toolchain, never the
# host's: build first so the pinned toolchain exists, resolve against it,
# then rebuild so the refreshed modules land in the build cache. The image
# ships GOTOOLCHAIN=local, so a dependency that needs a newer Go fails here
# with "go.mod requires go >= X" instead of raising the `go` directive past
# what wealthdb/Dockerfile pins — the fix for that is to bump the base image,
# which is the single place the toolchain version is declared.
update-wealthdb:
	@echo "==> rebuild wealthdb image (its Go toolchain resolves the modules)"
	$(WEALTHDB) build
	@echo "==> update wealthdb/ go modules inside the image"
	$(WEALTHDB_GO) get -u ./...
	$(WEALTHDB_GO) mod tidy
	@echo "==> rebuild wealthdb image so the refreshed modules land in the build cache"
	$(WEALTHDB) build

update-bases:
	$(call BUILD_BASES,,pull)

# ---- help --------------------------------------------------------------

help:
	@echo "wealthdb build/test — run from the repo root:"
	@echo ""
	@echo "  make all                build everything (engine + web + all collectors)"
	@echo "  make test               test everything"
	@echo "  make build-wealthdb     build the Go gold-engine image"
	@echo "  make test-wealthdb      run go test ./... in the wealthdb container"
	@echo "  make build-web          build the optional Metabase BI image"
	@echo "  make test-web           run the web unit tests (lifecycle script, provision.py)"
	@echo "  make test-mcp           run the MCP server lifecycle unit tests"
	@echo "  make build-collectors   build every collector"
	@echo "  make test-collectors    test every collector"
	@echo "  make test-collectorkit  run the shared collectorkit test suite"
	@echo "  make test-wrappers      run the shared wrapper unit tests"
	@echo "  make lint               gofmt + go vet (engine), ruff (Python)"
	@echo "  make lint-go / lint-python"
	@echo "  make build-<name>       build one collector (e.g. build-schwab-web)"
	@echo "  make test-<name>        test one collector  (e.g. test-schwab-web)"
	@echo "  make base-images        build the shared Docker bases the collector images FROM"
	@echo ""
	@echo "  make install            symlink wealthdb + wealthdb-collect into BINDIR (~/.local/bin)"
	@echo "  make hooks              run make lint before every commit (install runs it too)"
	@echo "  make uninstall          remove those symlinks and the hooks"
	@echo ""
	@echo "  make clean              remove build artefacts (pycache, caches)"
	@echo "  make cleanall           also remove docker images + venvs"
	@echo "  make clean-<name> / cleanall-<name>   (incl. -wealthdb, -collectors; cleanall-bases)"
	@echo ""
	@echo "  make demo               build the synthetic demo household (WEALTHDB_DEMO_ROOT,"
	@echo "                          default .demo-root's first line, else ~/wealthdb-demo;"
	@echo "                          AS_OF=, SEED=, FINDINGS=1)"
	@echo "  make demo-roll          append the days since the last demo build and load them"
	@echo "  make demo-web / demo-web-stop   the demo's dashboards, own container and port"
	@echo "  make demo-mcp / demo-mcp-stop   the demo over MCP, own container and port"
	@echo "  make test-demo          test the demo generator, a short demo load and a build to today"
	@echo ""
	@echo "  WEALTHDB_IMAGE_TAG=…    the tag of every image built and run (default:"
	@echo "                          .image-tag's first line, else latest)"
	@echo ""
	@echo "  make update             refresh all tooling: pip in every host venv,"
	@echo "                          go modules in wealthdb/, shared Docker bases"
	@echo "  make update-venvs / update-wealthdb / update-bases     (one layer at a time)"
	@echo ""
	@echo "  collectors: $(COLLECTORS)"
