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
# test-<x> builds its <x> first when it changed, so testing always runs
# current code.
# `make test` also runs the shared collectorkit library's own suite
# (test-collectorkit), which otherwise runs nowhere.

# The tree's root: this file's folder. It is read before any include,
# while this file is the last one make has read.
ROOT_DIR := $(patsubst %/,%,$(dir $(abspath $(lastword $(MAKEFILE_LIST)))))

# The kind of engine a tree builds and runs is ./configure's choice,
# recorded in config.mk with the tools it found: ENGINE (container, or
# native), PYTHON, GO, JAVA, and PREFIX and BINDIR for `make install`.
# Without a config.mk the engine is the container one, and no target
# needs ./configure first. The wrappers read the same file
# (shared/wrappers/engine-mode.sh), so make and a wrapper never disagree:
# ENGINE and PREFIX are emptied first, so a variable of that name in the
# environment sets neither. Only config.mk and the make command line do.
ENGINE :=
PREFIX :=
-include config.mk
ENGINE := $(or $(strip $(ENGINE)),container)
ifeq ($(filter $(ENGINE),container native),)
$(error config.mk: ENGINE=$(ENGINE) is no kind of engine; run ./configure again)
endif

# Auto-discover collectors: immediate subdirectories of collectors/.
# The `/.` matches directories only (files like collectors/README.md
# have no `.` entry), so non-collector files don't leak into the list.
COLLECTORS := $(sort $(notdir $(patsubst %/.,%,$(wildcard collectors/*/.))))

# The image tag (see the header), as shared/wrappers/image-tag.sh resolves
# it for every wrapper. override: an empty WEALTHDB_IMAGE_TAG= on the
# command line resolves like an unset one. Exported, so the wrappers the
# targets call read the same tag.
override WEALTHDB_IMAGE_TAG := $(shell WEALTHDB_IMAGE_TAG='$(WEALTHDB_IMAGE_TAG)' sh -c \
	'. "$$1/shared/wrappers/image-tag.sh" && wealthdb_image_tag "$$1"' sh '$(ROOT_DIR)')
ifeq ($(WEALTHDB_IMAGE_TAG),)
$(error no image tag: fix .image-tag, or set WEALTHDB_IMAGE_TAG)
endif
export WEALTHDB_IMAGE_TAG
TAG := $(WEALTHDB_IMAGE_TAG)
NCPU = $(shell sysctl -n hw.ncpu 2>/dev/null || nproc 2>/dev/null || echo 4)
# The build locks, the Go caches, the demo test roots and the scratch
# dirs of site-shots and of test-wealthdb's catalogue check live here:
# paths Docker can mount.
CACHE_ROOT := $(or $(XDG_CACHE_HOME),$(HOME)/.cache)/wealthdb
BUILD_LOCK_DIR := $(CACHE_ROOT)/build-locks
# LOCKED NAME: a recipe prefix that holds the build lock NAME-$(TAG)
# for the rest of the recipe's shell (wealthdb_build_lock), so two
# builds of one image never run at once.
LOCKED = . shared/wrappers/image-tag.sh; \
	wealthdb_build_lock "$(BUILD_LOCK_DIR)/$(1)-$(TAG).lock" || exit 1;

WEALTHDB      := wealthdb/wealthdb
WEALTHDB_TEST := wealthdb/wealthdb-test
WEALTHDB_GO   := wealthdb/wealthdb-go

# Host interpreter for every venv the Makefile creates (the host-venv
# collectors' and hybrids', collectorkit's, web's, site's, ruff's) and
# for the host-side scripts (the demo generator and test runner, the
# privacy check, bump-playwright.py, site/images.py). The venvs' deps
# (e.g. schwab-py) need Python >=3.10, but macOS /usr/bin/python3 is 3.9
# and `make` may resolve a bare `python3` to it. Prefer a versioned
# python3.X from PATH, then a Homebrew install, then plain python3.
# Override explicitly:  make build-schwab-api PYTHON=python3.12
# A PYTHON that ./configure recorded stands. ./configure tries the same
# versions: a new one is added to both lists.
ifneq ($(origin PYTHON),file)
PYTHON := $(or \
  $(shell for p in python3.15 python3.14 python3.13 python3.12 python3.11 python3.10; do command -v $$p && break; done 2>/dev/null),\
  $(firstword $(wildcard /opt/homebrew/bin/python3.1[0-9] /usr/local/bin/python3.1[0-9])),\
  python3)
endif

# ENSURE_VENV dir,label,target — create the venv at dir from $(PYTHON),
# unless one on Python >=3.10 is there. label names what needs it in the
# error, and target is the make target to re-run with a newer PYTHON.
define ENSURE_VENV
if [ ! -x $(1)/bin/python ] || \
   ! $(1)/bin/python -c 'import sys; sys.exit(0 if sys.version_info >= (3,10) else 1)' 2>/dev/null; then \
	$(PYTHON) -c 'import sys; sys.exit(0 if sys.version_info >= (3,10) else 1)' 2>/dev/null || { \
		echo "    ERROR: $(2): needs Python >=3.10, and '$(PYTHON)' is older." >&2; \
		echo "    Re-run with a newer interpreter, e.g.: make $(3) PYTHON=python3.14" >&2; \
		exit 1; }; \
	rm -rf $(1); \
	$(PYTHON) -m venv $(1); \
fi
endef

# VENV dir,stamp-files — pip install $(dir)_PIP (see VENVS) into the
# venv at dir, unless its .stamp holds the bytes of stamp-files and then
# those arguments, as the last install that succeeded wrote them. It
# compares contents, not mtimes, because the pre-commit hook lints a
# fresh export of the index against this checkout's venv. A failed pip
# stops the recipe.
define VENV
if ! { cat $(2) && echo '$($(1)_PIP)'; } | cmp -s - $(1)/.stamp 2>/dev/null; then \
	$(1)/bin/pip install -q $($(1)_PIP) && \
	{ cat $(2) && echo '$($(1)_PIP)'; } > $(1)/.stamp || exit 1; \
fi
endef

# CLEAN_PYCACHE dirs — remove the __pycache__ and .pytest_cache dirs under
# dirs, leaving any .venv alone.
define CLEAN_PYCACHE
find $(1) -name .venv -prune -o -type d \( -name __pycache__ -o -name .pytest_cache \) -exec rm -rf {} + 2>/dev/null || true
endef

.DEFAULT_GOAL := help
.PHONY: all build test help install uninstall hooks \
        list-suites list-collectors build-wealthdb test-wealthdb \
        build-web test-web clean-web cleanall-web test-mcp \
        build-collectors test-collectors \
        test-collectorkit clean-collectorkit cleanall-collectorkit \
        test-wrappers test-pii pii pii-history \
        clean cleanall clean-wealthdb cleanall-wealthdb clean-build-locks \
        clean-collectors cleanall-collectors base-images camoufox-archive cleanall-bases \
        update update-venvs update-wealthdb update-playwright update-bases \
        lint lint-go lint-python cleanall-lint \
        demo demo-roll demo-web demo-web-stop demo-mcp demo-mcp-stop test-demo \
        clean-demo site-shots site-images site-publish test-site \
        clean-site cleanall-site test-native cleanall-native

# ---- aggregates --------------------------------------------------------

# `all` builds in parallel: with the -jN it was given, else one job
# per CPU. Each Docker collector's build waits for the bases on its own.
all:
	@$(MAKE) $(if $(findstring jobserver,$(MAKEFLAGS)),,-j$(NCPU)) \
		build-wealthdb build-web build-collectors
build: all

# TEST_SUITES is every suite `make test` runs, in start order: each
# names a test-<name> target. `make list-suites` prints it, and CI runs
# each suite in exactly one job (.github/ci-suites.sh).
TEST_SUITES := wealthdb demo web mcp site collectorkit wrappers pii $(COLLECTORS)
# SUITES narrows `make test` to some of them (e.g. SUITES="web mcp").
SUITES = $(TEST_SUITES)

# `make test` runs the suites together and prints each one's log, with
# its duration in seconds, once all have finished. Each image build
# holds a lock (LOCKED), so two suites never build one image at once.
# `make test-<name>` runs one suite and prints as it goes.
test:
	$(if $(strip $(SUITES)),,$(error SUITES names no suite))
	$(if $(subst $(strip $(TEST_SUITES)),,$(strip $(SUITES))),@echo "make test: running $(words $(SUITES)) of $(words $(TEST_SUITES)) suites (SUITES)")
	@shared/wrappers/fanout.sh -j $(NCPU) --times -- '$(MAKE) --no-print-directory test-"$$1"' $(SUITES)

list-suites:
	@printf '%s\n' $(TEST_SUITES)

list-collectors:
	@printf '%s\n' $(COLLECTORS)

# ---- install -----------------------------------------------------------
# Put the two top-level entry points on PATH so they work from any
# directory:
#   wealthdb         the gold-engine wrapper (wealthdb/wealthdb)
#   wealthdb-collect the collector dispatcher (bin/wealthdb-collect)
# BINDIR defaults to ~/.local/bin (the XDG-conventional user bin dir,
# matching the config default under ~/.config); override e.g.
# `make install BINDIR=/usr/local/bin`.
#
# Without a prefix, they are symlinks into this checkout, and the
# per-collector wrappers stay in the repo (the dispatcher resolves
# them). With ./configure --prefix=DIR, install copies what a run needs
# to DIR/libexec/wealthdb, in the checkout's layout, and links the two
# entry points from DIR/bin. The copy holds:
#   - of INSTALL_TREE, in a git checkout, the files git tracks: nothing
#     git ignores (a session file, a log, a download) leaves the tree.
#     A tree with no .git, such as a source archive, has no such list,
#     and gives every file but a browser's core file and a log;
#   - INSTALL_BUILT: config.mk, .image-tag when there is one, and the
#     engine of a native tree; and the dashboards' two files when they
#     are there;
#   - no test, venv or source of the engine (INSTALL_SKIP).
# The copy runs the engine, both servers and the collectors in plain
# Python that need no package. It builds nothing: a collector in a
# container runs the image the checkout built, under the same tag, and
# a collector that needs Python packages runs from the checkout only.
# An install extracts beside the copy that is there, then takes its
# place as a whole, so a file the tree dropped is gone from the copy.
# uninstall removes the copy. In a native tree it first stops both
# servers, when the command in BINDIR is this tree's: a server's
# LaunchAgent runs that command, and would keep starting a program that
# is gone.
# LIBEXECDIR is where the copy goes (default PREFIX/libexec; a Debian
# package names /usr/lib). DESTDIR stages an install for a package: every
# file goes under it, and the links name the place the package puts them,
# without it. uninstall removes a copy in the default place; a package's
# copy is its package manager's to remove.
BINDIR ?= $(HOME)/.local/bin
LIBEXECDIR ?= $(PREFIX)/libexec
LIBEXEC = $(LIBEXECDIR)/wealthdb
STAGE = $(DESTDIR)$(LIBEXECDIR)
INSTALL_TREE  = LICENSE README.md bin/wealthdb-collect bin/wealthdb-refresh wealthdb/wealthdb \
                mcp/mcp web shared/wrappers shared/collectorkit collectors demo skills
INSTALL_BUILT = config.mk $(wildcard .image-tag) \
                $(if $(filter native,$(ENGINE)),wealthdb/wealthdb-engine)
INSTALL_SKIP  = .venv __pycache__ .pytest_cache tests test_* *.egg-info
INSTALL_SKIP_UNTRACKED = core *.log
# TAR_SKIP names: tar's option that leaves out each of names, at any depth.
TAR_SKIP = $(foreach x,$(1),--exclude='$(x)')

# STOP_SERVERS launcher: stop both servers when the command in BINDIR is
# launcher. A stop that fails does not fail the uninstall.
ifeq ($(ENGINE),native)
define STOP_SERVERS
if [ "$(BINDIR)/wealthdb" -ef "$(1)" ]; then \
	"$(BINDIR)/wealthdb" mcp stop || true; \
	"$(BINDIR)/wealthdb" web stop || true; \
fi
endef
else
STOP_SERVERS = :
endif

ifeq ($(PREFIX),)
install: hooks
	@mkdir -p "$(BINDIR)"
	@ln -sf "$(ROOT_DIR)/wealthdb/wealthdb"     "$(BINDIR)/wealthdb"
	@ln -sf "$(ROOT_DIR)/bin/wealthdb-collect"  "$(BINDIR)/wealthdb-collect"
	@echo "  linked $(BINDIR)/wealthdb         -> $(ROOT_DIR)/wealthdb/wealthdb"
	@echo "  linked $(BINDIR)/wealthdb-collect -> $(ROOT_DIR)/bin/wealthdb-collect"
	@case ":$$PATH:" in *":$(BINDIR):"*) ;; \
	  *) echo "  note: $(BINDIR) is not on your PATH — add it to run these bare";; esac

uninstall:
	@$(call STOP_SERVERS,$(ROOT_DIR)/wealthdb/wealthdb)
	@rm -f "$(BINDIR)/wealthdb" "$(BINDIR)/wealthdb-collect"
	@echo "  removed $(BINDIR)/wealthdb and $(BINDIR)/wealthdb-collect"
	@if [ "$$(git config --get core.hooksPath)" = .githooks ]; then \
		git config --unset core.hooksPath; \
		echo "  removed the git hooks (core.hooksPath)"; \
	fi
else
# The new copy is checked before it takes the old one's place, so a copy
# that came out short (git or tar failed) replaces nothing.
install:
	@for f in $(INSTALL_BUILT) $(INSTALL_TREE); do \
		[ -e "$$f" ] && continue; \
		case "$$f" in \
			config.mk) echo "install: config.mk is missing; run ./configure and make all first" >&2 ;; \
			*) echo "install: $$f is missing; run make all first" >&2 ;; \
		esac; \
		exit 1; \
	done
	@mkdir -p "$(STAGE)" "$(DESTDIR)$(BINDIR)"
	@rm -rf "$(STAGE)/wealthdb.new"
	@mkdir "$(STAGE)/wealthdb.new"
	@if [ -e .git ]; then \
		{ git ls-files -z -- $(INSTALL_TREE) && \
			printf '%s\0' $(INSTALL_BUILT); } | \
			tar -cf - $(call TAR_SKIP,$(INSTALL_SKIP)) --null -T -; \
	else \
		tar -cf - $(call TAR_SKIP,$(INSTALL_SKIP) $(INSTALL_SKIP_UNTRACKED)) \
			$(INSTALL_BUILT) $(INSTALL_TREE); \
	fi | tar -C "$(STAGE)/wealthdb.new" -xf -
	@for f in $(INSTALL_BUILT) $(INSTALL_TREE); do \
		[ -e "$(STAGE)/wealthdb.new/$$f" ] || \
			{ echo "install: the copy lacks $$f; $(DESTDIR)$(LIBEXEC) is left as it was" >&2; exit 1; }; \
	done
	@rm -rf "$(STAGE)/wealthdb.old"
	@if [ -e "$(STAGE)/wealthdb" ]; then mv "$(STAGE)/wealthdb" "$(STAGE)/wealthdb.old"; fi
	@mv "$(STAGE)/wealthdb.new" "$(STAGE)/wealthdb"
	@rm -rf "$(STAGE)/wealthdb.old"
	@ln -sf "$(LIBEXEC)/wealthdb/wealthdb"    "$(DESTDIR)$(BINDIR)/wealthdb"
	@ln -sf "$(LIBEXEC)/bin/wealthdb-collect" "$(DESTDIR)$(BINDIR)/wealthdb-collect"
	@echo "  installed $(DESTDIR)$(LIBEXEC)"
	@echo "  linked $(DESTDIR)$(BINDIR)/wealthdb and $(DESTDIR)$(BINDIR)/wealthdb-collect"

uninstall:
	@$(call STOP_SERVERS,$(LIBEXEC)/wealthdb/wealthdb)
	@rm -f "$(BINDIR)/wealthdb" "$(BINDIR)/wealthdb-collect"
	@rm -rf "$(PREFIX)/libexec/wealthdb" "$(PREFIX)/libexec/wealthdb.new" "$(PREFIX)/libexec/wealthdb.old"
	@echo "  removed $(LIBEXEC), $(BINDIR)/wealthdb and $(BINDIR)/wealthdb-collect"
endif

# The repo's git hooks live in .githooks/, tracked, and git runs them once
# core.hooksPath names that dir. The pre-commit hook runs the privacy check
# (shared/pii) and `make lint` on the staged tree, and refuses a commit
# either fails on or the lint rewrites; the commit-msg hook runs the
# privacy check on the message.
hooks:
	@git config core.hooksPath .githooks
	@echo "  git hooks: core.hooksPath -> .githooks (privacy check and make lint)"

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

# CAMOUFOX_BROWSER is base-camoufox's browser mode: bundle bakes in the
# Camoufox browser, none leaves it out. The tests build with none
# (test-<collector> below), so on a machine without a base-camoufox they
# need neither the local archive nor a download. A none build keeps the
# browser of a base-camoufox that has it (BUILD_BASES). Every other
# build bakes the browser in. bundle takes the
# browser from the local archive in CAMOUFOX_ARCHIVE_DIR when there is
# one, and downloads it otherwise (shared/images/base-camoufox.Dockerfile).
# camoufox-archive writes that archive from the image CAMOUFOX_KEEP.
CAMOUFOX_BROWSER = bundle
CAMOUFOX_ARCHIVE_DIR := $(CACHE_ROOT)/camoufox
CAMOUFOX_BUILD := $(shell sed -n 's/^ARG CAMOUFOX_BUILD=//p' shared/images/base-camoufox.Dockerfile 2>/dev/null)
CAMOUFOX_KEEP = wealthdb/base-camoufox:keep-$(CAMOUFOX_BUILD)

# BUILD_BASES quiet,pull — build the shared bases in order, and stop at
# the first that fails. quiet keeps docker's output in a temporary log
# and prints it only when the build fails, so a failing RUN step shows
# its own error, not only its Dockerfile lines. pull refreshes the
# upstream image a base builds FROM; a base FROM a local wealthdb/ image
# never pulls, and is built only when that image is local
# (wealthdb_require_local_bases), so no registry is asked for a
# wealthdb/ name. Without pull, a base whose wealthdb.inputs label matches
# the hash of its inputs is kept as it is (wealthdb_build_unchanged).
# The inputs are its own Dockerfile, collectorkit, shared/.dockerignore,
# the helper files in shared/images that are not Dockerfiles, and its
# parent image's id. Another base's Dockerfile is not one, so an edit
# to one base rebuilds only that base and the images built on it. A
# hash that fails builds the base without a label. base-camoufox's
# inputs also hold its browser mode, which reaches the build as an
# argument, with the archive directory as a build context in bundle
# mode. Without pull, a none build never replaces a base-camoufox that
# carries the browser: one whose WEALTHDB_CAMOUFOX_BROWSER is not none,
# or that has no such variable. It builds that one in bundle mode,
# stale or not, so a test run never strips the browser from the image
# the live runs start. With no image at the tag (CI), it builds none.
# BUILD_BASES runs after LOCKED, which has sourced image-tag.sh.
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
$(if $(1),log=$$(mktemp) || exit 1; trap 'rm -f "$$log"' EXIT;) \
helpers=; \
for x in shared/images/*; do \
	case "$$x" in *.Dockerfile) ;; *) helpers="$$helpers $$x" ;; esac; \
done; \
inputs() { wealthdb_inputs_hash "$$f" shared/collectorkit $$helpers \
	shared/.dockerignore -- "$$(wealthdb_from_id "$$f")" "$$@"; }; \
for img in $(BASE_IMAGES); do \
	f="shared/images/$$img.Dockerfile"; \
	[ -e "$$f" ] || continue; \
	pull=; \
	if [ -n "$(2)" ] && ! grep -qE '^FROM[[:space:]]+wealthdb/' "$$f"; then \
		pull=--pull; \
		docker pull "$$(wealthdb_from_ref "$$f")" || exit 1; \
	fi; \
	mode=; why=; set --; \
	if [ "$$img" = base-camoufox ]; then \
		mode=$(CAMOUFOX_BROWSER); \
		if [ "$$mode" = none ] && [ -z "$(2)" ] && \
			have=$$(docker image inspect -f '{{range .Config.Env}}{{println .}}{{end}}' \
				"wealthdb/$$img:$(TAG)" 2>/dev/null) && \
			! printf '%s\n' "$$have" | grep -qx 'WEALTHDB_CAMOUFOX_BROWSER=none'; then \
			mode=bundle; why=", as the image there carries it"; \
		fi; \
		set -- --build-arg "CAMOUFOX_BROWSER=$$mode"; \
		if [ "$$mode" = bundle ] && [ -d "$(CAMOUFOX_ARCHIVE_DIR)" ]; then \
			set -- "$$@" --build-context "camoufox-archive=$(CAMOUFOX_ARCHIVE_DIR)"; \
		fi; \
	fi; \
	hash=$$(inputs $${mode:+"browser=$$mode"}) || hash=; \
	if wealthdb_build_unchanged "wealthdb/$$img:$(TAG)" "$$hash" $(if $(2),1,0); then \
		continue; \
	fi; \
	wealthdb_require_local_bases "$$img" "$$f" "Run: make base-images" || exit 1; \
	echo "==> build base image wealthdb/$$img:$(TAG)$${pull:+ ($$pull)}$${mode:+ (browser: $$mode$$why)}"; \
	docker build $(if $(1),--progress=plain) $$pull --provenance=false \
		$${hash:+--label "wealthdb.inputs=$$hash"} \
		--build-arg WEALTHDB_IMAGE_TAG=$(TAG) "$$@" \
		-f "$$f" -t "wealthdb/$$img:$(TAG)" shared/ \
		$(if $(1),>"$$log" 2>&1 || { cat "$$log" >&2; exit 1; },|| exit 1); \
done
endef

base-images:
	@$(call LOCKED,bases) \
	$(call BUILD_BASES,quiet,)

# The local archive of the pinned Camoufox build, written from the
# image CAMOUFOX_KEEP into CAMOUFOX_ARCHIVE_DIR and checked against the
# checksum base-camoufox.Dockerfile pins for its CPU architecture. A
# mismatch keeps the archive that is there and prints the new checksum,
# which is the one to pin when CAMOUFOX_KEEP holds a new build. A
# CAMOUFOX_KEEP built without the browser writes nothing.
camoufox-archive:
	@arch=$$(docker image inspect -f '{{.Architecture}}' "$(CAMOUFOX_KEEP)") || exit 1; \
	if docker image inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$(CAMOUFOX_KEEP)" | \
		grep -qx 'WEALTHDB_CAMOUFOX_BROWSER=none'; then \
		echo "camoufox-archive: $(CAMOUFOX_KEEP) is a build without the browser" >&2; \
		echo "camoufox-archive: (CAMOUFOX_BROWSER=none); keep an image built by make all" >&2; \
		exit 1; \
	fi; \
	out="$(CAMOUFOX_ARCHIVE_DIR)/camoufox-cache-$(CAMOUFOX_BUILD)-lin.$$arch.tgz"; \
	pin=$$(sed -n "s/^ARG CAMOUFOX_SUM_$$(echo "$$arch" | tr a-z A-Z)=//p" \
		shared/images/base-camoufox.Dockerfile); \
	mkdir -p "$(CAMOUFOX_ARCHIVE_DIR)" || exit 1; \
	tmp=$$(mktemp "$$out.XXXXXX") || exit 1; trap 'rm -f "$$tmp"' EXIT; \
	echo "==> write $$out from $(CAMOUFOX_KEEP)"; \
	docker run --rm --pull=never --entrypoint tar "$(CAMOUFOX_KEEP)" -C /opt -czf - camoufox-cache >"$$tmp" || exit 1; \
	sum=$$($(PYTHON) shared/images/camoufox-archive.py sum "$$tmp") || exit 1; \
	if [ "$$sum" != "$$pin" ]; then \
		echo "camoufox-archive: the archive's checksum is $$sum, and" >&2; \
		echo "camoufox-archive: base-camoufox.Dockerfile pins '$$pin' for $$arch" >&2; \
		exit 1; \
	fi; \
	chmod 0644 "$$tmp" && mv "$$tmp" "$$out" && echo "    checksum $$sum, as pinned"

# clean    = build artefacts (pycache, pytest and ruff caches, Go build cache,
#            test-demo's scratch roots, the build locks)
# cleanall = clean + the heavy outputs (docker images, venvs, the native
#            program, the dashboards' downloaded files, test-native's
#            image and cache)
clean:    clean-wealthdb clean-web clean-collectors clean-collectorkit clean-demo \
          clean-site clean-build-locks
	@$(call CLEAN_PYCACHE,shared/update shared/pii)
	@rm -rf .pytest_cache .ruff_cache
cleanall: clean cleanall-wealthdb cleanall-web cleanall-collectors cleanall-bases \
          cleanall-collectorkit cleanall-lint cleanall-site cleanall-native

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

# pip runs only when collectorkit's pyproject.toml or CK_TEST_DEPS
# changes. The editable install picks up source edits without a
# reinstall.
test-collectorkit:
	@echo "==> test collectorkit"
	@$(call ENSURE_VENV,$(CK_VENV),collectorkit,test-collectorkit)
	@$(call VENV,$(CK_VENV),$(CK_DIR)/pyproject.toml)
	@$(CK_VENV)/bin/python -m pytest -q -p no:cacheprovider $(CK_DIR)/tests

# The test files run side by side, and each one's output prints once all
# have finished, in file order. PYTHON names the interpreter the archive
# test runs, so its unpack has tarfile's extraction filters.
test-wrappers:
	@echo "==> test wrappers (shared/wrappers)"
	@PYTHON=$(PYTHON) shared/wrappers/fanout.sh -j $(NCPU) -- 'bash "$$1"' shared/wrappers/tests/test_*.sh

# The privacy check the hooks and CI run (shared/pii): stdlib only, so
# it runs on the host interpreter, no venv.
test-pii:
	@echo "==> test pii (shared/pii/tests)"
	@$(PYTHON) -m unittest discover -s shared/pii/tests

# The privacy check by hand. `pii` reads what a push would publish: every
# commit since PII_BASE (default origin/main; a fork's upstream is often
# upstream/main) and the staged change. `pii-history` reads every commit
# HEAD reaches. Findings print as shapes; SHOW=1 adds the values, for this
# terminal only.
PII_BASE ?= origin/main
SHOW =
PII_CHECK = $(PYTHON) shared/pii/pii_check.py $(if $(SHOW),--show)

pii:
	@echo "==> pii check (the commits since $(PII_BASE), then the index)"
	@$(PII_CHECK) range $(PII_BASE) HEAD && $(PII_CHECK) staged

pii-history:
	@echo "==> pii check (every commit HEAD reaches)"
	@$(PII_CHECK) history HEAD

clean-collectorkit:
	@echo "==> clean collectorkit"
	@$(call CLEAN_PYCACHE,$(CK_DIR))

cleanall-collectorkit: clean-collectorkit
	@echo "==> cleanall collectorkit"
	@rm -rf $(CK_VENV) $(CK_DIR)/collectorkit.egg-info

clean-build-locks:
	@echo "==> clean build locks"
	@. shared/wrappers/image-tag.sh; wealthdb_clean_build_locks "$(BUILD_LOCK_DIR)"

cleanall-lint:
	@echo "==> cleanall lint (ruff venv)"
	@rm -rf $(LINT_VENV)

# ---- lint ----------------------------------------------------------------
# gofmt and go vet run on the image's toolchain like every other Go command
# (gofmt as `go run cmd/gofmt`, since the wrapper's entrypoint is `go`).
# They read the bind-mounted source, so the image only has to exist — it is
# built when missing, never rebuilt here, and linting never touches the
# image the nightly runs. A tree of the native engine runs both on the Go
# that ./configure recorded.
#
# ruff comes from a venv of its own, pinned in shared/lint/requirements.txt
# and refreshed by update-venvs; the rule set is /ruff.toml, which covers
# every Python module in the tree. An edit to the pin reinstalls.
LINT_DIR  := shared/lint
LINT_VENV := $(LINT_DIR)/.venv

lint: lint-go lint-python

lint-go:
	@echo "==> lint wealthdb (gofmt, go vet)"
ifeq ($(ENGINE),container)
	@docker image inspect wealthdb:$(TAG) >/dev/null 2>&1 || { $(call LOCKED,wealthdb) $(WEALTHDB) build; }
endif
	@out="$$($(WEALTHDB_GO) run cmd/gofmt -l .)" || exit 1; \
	if [ -n "$$out" ]; then \
		echo "    gofmt: not formatted:" >&2; echo "$$out" | sed 's/^/      /' >&2; exit 1; \
	fi
	@$(WEALTHDB_GO) vet ./...

lint-python:
	@echo "==> lint python (ruff)"
	@if [ ! -x $(LINT_VENV)/bin/ruff ]; then \
		rm -rf $(LINT_VENV); \
		$(PYTHON) -m venv $(LINT_VENV) || exit 1; \
	fi
	@$(call VENV,$(LINT_VENV),$(LINT_DIR)/requirements.txt)
	@$(LINT_VENV)/bin/ruff check --quiet .

# ---- wealthdb gold engine ---------------------------------------------

# The engine: the image, or the native program wealthdb/wealthdb-engine.
build-wealthdb:
	@$(call LOCKED,wealthdb) \
	echo "==> build wealthdb ($(if $(filter native,$(ENGINE)),native program,docker image))"; \
	$(WEALTHDB) build

# WEALTHDB_PKGS narrows the packages, and GO_TEST_FLAGS adds go test
# flags (CI passes -count=1, so no result comes from the test cache).
WEALTHDB_PKGS = ./...
GO_TEST_FLAGS =

# After the Go tests, the built engine's catalogue (what `wealthdb
# config` offers) is held to the collectors: it names every directory
# under collectors/ that has a wrapper, and nothing else. The engine runs
# on a scratch config and data root of its own, in the cache, removed
# when the check ends.
CATALOGUE_CHECK_ROOT := $(CACHE_ROOT)/catalogue-check-$(TAG)

test-wealthdb: build-wealthdb
	@echo "==> test wealthdb (go test $(strip $(GO_TEST_FLAGS) $(WEALTHDB_PKGS)))"
	$(WEALTHDB_TEST) $(GO_TEST_FLAGS) $(WEALTHDB_PKGS)
	@echo "==> check the engine's catalogue against collectors/"
	@root='$(CATALOGUE_CHECK_ROOT)'; rm -rf "$$root"; mkdir -p "$$root" || exit 1; \
	trap 'rm -rf "$$root"' EXIT; \
	for d in collectors/*/; do n="$${d%/}"; n="$${n##*/}"; \
	    if [ -f "$$d$$n" ] && [ -x "$$d$$n" ]; then echo "$$n"; fi; \
	done | sort > "$$root/want"; \
	XDG_CONFIG_HOME="$$root" WEALTHDB_DATA_ROOT="$$root/data" $(WEALTHDB) config-env collectors \
	    | sort > "$$root/got"; \
	if ! cmp -s "$$root/want" "$$root/got"; then \
	    echo "test-wealthdb: config-env collectors (+) differs from the collectors with a wrapper (-):"; \
	    diff "$$root/want" "$$root/got"; exit 1; \
	fi

# Where wealthdb-go parks the containerised Go toolchain caches
# (must mirror CACHE in wealthdb/wealthdb-go).
GO_CACHE := $(CACHE_ROOT)/go

clean-wealthdb:
	@echo "==> clean wealthdb (go build cache)"
	@chmod -R u+w $(GO_CACHE)/go-build 2>/dev/null || true
	@rm -rf $(GO_CACHE)/go-build

cleanall-wealthdb: clean-wealthdb
	@echo "==> cleanall wealthdb (image, native program, go caches)"
	@docker image rm -f wealthdb:$(TAG) >/dev/null 2>&1 || true
	@rm -f wealthdb/wealthdb-engine
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

# `web refresh` re-materializes the serving tables, returns, gains, exposure,
# yield, the bridge and the cash flow projection, and re-snapshots gold
# (restarting the container when it runs); `web start` then brings it up if
# it does not.
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
# see, holding nothing but the demo's test roots. One per image tag, as
# the images are, so checkouts on tags of their own never share a root.
DEMO_TEST_ROOT := $(CACHE_ROOT)/demo-test-$(TAG)

# The test classes run side by side (demo/run_tests.py), one process
# each, so the engine cases overlap.
test-demo: build-wealthdb
	@echo "==> test demo (generator, then the household through the engine: its first months and to today)"
	@WEALTHDB_DEMO_TEST_ROOT="$(DEMO_TEST_ROOT)" WEALTHDB_BIN="$(abspath $(WEALTHDB))" \
		$(PYTHON) demo/run_tests.py -j $(NCPU)

# This tag's root only, and only when it holds the marker the tests
# write into it (demo/tests/test_engine.py), so a dir of another kind
# at that path is left alone.
clean-demo:
	@echo "==> clean demo (pycache, test-demo's scratch roots of tag $(TAG))"
	@$(call CLEAN_PYCACHE,demo)
	@if [ -f "$(DEMO_TEST_ROOT)/.wealthdb-demo-tests" ]; then rm -rf "$(DEMO_TEST_ROOT)"; fi

# ---- site (the project page) ------------------------------------------
# The page is site/public, deployed by .github/workflows/pages.yml. Its
# screenshots stay out of git: they live in a GitHub release, which the
# workflow downloads into site/public/shots at deploy time
# (site/images.py).
#
# site-shots retakes the screenshots:
#   - it rebuilds the demo at the pinned seed and date, so a reshoot
#     moves only what the code changed;
#   - it shoots every dashboard in both colour schemes in the Camoufox
#     image, as the caller, with the date pickers pinned to that date;
#   - it converts the shots into site/public/shots;
#   - it prints the figures the page's text quotes, each report that
#     takes a day on that date. The income projection takes none and
#     starts at the current month.
# The rebuild resets the demo root to that date; `make demo` or
# `make demo-roll` brings it back to today.
# site-images downloads the published screenshots for a local preview.
# site-publish uploads site/public/shots to the release and starts the
# deploy. Its token comes from WEALTHDB_SITE_TOKEN or the env file
# SITE_ENV, read through bash.
SITE_SEED  := harlow-19
SITE_AS_OF := 2026-10-30
SITE_VENV  := site/.venv
SITE_SHOTS := site/public/shots
SITE_ENV   ?= $(HOME)/.secrets/github-site.env
SITE_TMP   := $(CACHE_ROOT)/site-shots-$(TAG)

site-shots: build-wealthdb build-web base-images
	@$(MAKE) --no-print-directory demo SEED=$(SITE_SEED) AS_OF=$(SITE_AS_OF)
	@$(MAKE) --no-print-directory demo-web
	@$(call ENSURE_VENV,$(SITE_VENV),the site tools,site-shots); \
	$(call VENV,$(SITE_VENV),site/requirements.txt)
	@echo "==> shoot the demo's dashboards"
	@rm -rf "$(SITE_TMP)" && mkdir -p "$(SITE_TMP)" && trap 'rm -rf "$(SITE_TMP)"' EXIT && \
	docker run --rm --pull=never --user "$$(id -u):$$(id -g)" -e HOME=/tmp --add-host=host.docker.internal:host-gateway \
		-v "$(ROOT_DIR):/repo:ro" -v "$(DEMO_ROOT)/web/admin-password.txt:/password:ro" \
		-v "$(SITE_TMP):/out" --entrypoint bash wealthdb/base-camoufox:$(TAG) \
		-c 'source /opt/entrypoint-lib.sh && exec python3 /repo/site/capture.py "$$@"' capture \
		--out /out --as-of $(SITE_AS_OF) --password-file /password && \
	$(SITE_VENV)/bin/python site/images.py convert "$(SITE_TMP)" $(SITE_SHOTS)
	@echo "==> the figures the page quotes (terminal block, then the agent conversation)"
	@$(DEMO_ENGINE) -r exposure countries -d $(SITE_AS_OF)
	@$(DEMO_ENGINE) -r spending categories 2025 --period annual
	@$(DEMO_ENGINE) -r yield global -x EUR -d $(SITE_AS_OF)
	@$(DEMO_ENGINE) -r yield accounts -x EUR -d $(SITE_AS_OF) \
		-C silver_source,account,currency,value,income_yield,income_12m,income_100bp,duration,dv01,quality
	@$(DEMO_ENGINE) -r income projection -x EUR
	@$(DEMO_ENGINE) -r yield cash -x EUR -d $(SITE_AS_OF)

site-images:
	@$(PYTHON) site/images.py fetch $(SITE_SHOTS)

site-publish:
	@bash -c '. shared/wrappers/envfile-lib.sh; f="$$1"; shift; \
		if [ -f "$$f" ] && ! wealthdb_source_bash "$$f"; then \
			echo "site-publish: $$f has a bash syntax error" >&2; exit 1; fi; \
		exec "$$@"' _ "$(SITE_ENV)" $(PYTHON) site/images.py publish $(SITE_SHOTS)

# capture.py's and images.py's unit tests. The image conversion runs
# with Pillow from site/.venv. No Docker, no network.
test-site:
	@echo "==> test site (site/tests)"
	@$(call ENSURE_VENV,$(SITE_VENV),the site tests,test-site); \
	$(call VENV,$(SITE_VENV),site/requirements.txt)
	@$(SITE_VENV)/bin/python -m unittest discover -s site/tests -t site

clean-site:
	@echo "==> clean site (pycache)"
	@$(call CLEAN_PYCACHE,site)

cleanall-site: clean-site
	@echo "==> cleanall site (site tools venv)"
	@rm -rf $(SITE_VENV)

# ---- the native engine, tested in a container ---------------------------
# `make test-native` runs a tree of the native engine through its tests
# without installing Go, Java or anything else on this computer, and
# without touching this checkout's own kind of engine. The image
# wealthdb/native-test holds the tools (shared/images/native-test.Dockerfile),
# at the versions the engine's, the dashboards' and the Python
# collectors' Dockerfiles name. A run mounts the checkout read-only,
# copies it, and inside the container: configures the copy with
# --engine=native, builds it, runs NATIVE_SUITES of `make test`,
# installs to a prefix, walks the installed tree end to end with both
# servers started (shared/native/e2e.sh), and uninstalls. Go's caches
# and the dashboards' two files are kept in NATIVE_CACHE between runs.
# --init gives the container a first process that passes a Ctrl-C on,
# so a run can be interrupted.
NATIVE_TEST_IMAGE := wealthdb/native-test:$(TAG)
NATIVE_CACHE := $(CACHE_ROOT)/native-test
NATIVE_SUITES = wealthdb demo web mcp wrappers collectorkit pii

test-native:
	@echo "==> build $(NATIVE_TEST_IMAGE)"
	@$(call LOCKED,native-test) \
	docker build --quiet --provenance=false \
		--build-arg GO_IMAGE="$$(wealthdb_from_ref wealthdb/Dockerfile)" \
		--build-arg JAVA_IMAGE="$$(wealthdb_from_ref web/Dockerfile)" \
		--build-arg PYTHON_IMAGE="$$(wealthdb_from_ref shared/images/base-python.Dockerfile)" \
		-t "$(NATIVE_TEST_IMAGE)" - < shared/images/native-test.Dockerfile >/dev/null
	@mkdir -p "$(NATIVE_CACHE)"
	@echo "==> test the native engine in a container ($(NATIVE_SUITES), then end to end)"
	@docker run --rm --init --pull=never --user "$$(id -u):$$(id -g)" \
		-v "$(ROOT_DIR):/src:ro" -v "$(NATIVE_CACHE):/cache" \
		"$(NATIVE_TEST_IMAGE)" /src/shared/native/test-in-container.sh $(NATIVE_SUITES)

# The image, and the cache folder. Go's module cache is read-only, so it
# is made writable first.
cleanall-native:
	@echo "==> cleanall native (test-native's image and cache)"
	@docker image rm -f "$(NATIVE_TEST_IMAGE)" >/dev/null 2>&1 || true
	@chmod -R u+w "$(NATIVE_CACHE)" 2>/dev/null || true
	@rm -rf "$(NATIVE_CACHE)"

# ---- mcp (optional MCP server) ----------------------------------------
# The server is the engine itself, its image or the native program, so
# there is nothing to build; the lifecycle script has pure-bash unit
# tests, no Docker needed.
test-mcp:
	@echo "==> test mcp (mcp/test_mcp.sh)"
	@mcp/test_mcp.sh

# ---- web (optional Metabase BI server) --------------------------------

build-web:
	@echo "==> build web ($(if $(filter native,$(ENGINE)),metabase and its driver,metabase image))"
	@$(call LOCKED,web) web/web build

# Pure-bash unit tests for the lifecycle script, then provision.py's
# definitions; no Docker needed. provision.py's tests run the projection
# cards' SQL on DuckDB, so they run in a venv of their own that holds the
# duckdb module, pinned in web/requirements-test.txt and refreshed by
# update-venvs. pip runs only when the requirements change.
WEB_VENV := web/.venv

test-web:
	@echo "==> test web (web/test_web.sh)"
	@$(call ENSURE_VENV,$(WEB_VENV),the web tests,test-web); \
	$(call VENV,$(WEB_VENV),web/requirements-test.txt)
	@WEALTHDB_WEB_PYTHON="$(abspath $(WEB_VENV))/bin/python" web/test_web.sh

clean-web:
	@echo "==> clean web (pycache)"
	@$(call CLEAN_PYCACHE,web)

cleanall-web: clean-web
	@echo "==> cleanall web (image, downloaded files, test venv)"
	@docker image rm -f wealthdb/metabase:$(TAG) >/dev/null 2>&1 || true
	@rm -rf web/lib $(WEB_VENV)

# ---- host venvs ---------------------------------------------------------
# VENVS is every host venv the Makefile installs, and <dir>_PIP what pip
# installs into each: a host-venv or hybrid collector's requirements and
# collectorkit, editable; collectorkit's test deps; ruff; the web tests'
# deps; the site tools. The target that uses a venv installs it with
# VENV, and update-venvs upgrades every one that exists. A host-venv
# collector has a requirements.txt and no Dockerfile; a hybrid has all
# three of requirements.txt, Dockerfile and .host-venv.
HOST_VENV_COLLECTORS := $(foreach c,$(COLLECTORS),$(if $(wildcard collectors/$(c)/requirements.txt),$(if $(wildcard collectors/$(c)/Dockerfile),$(if $(wildcard collectors/$(c)/.host-venv),$(c)),$(c))))
VENVS := $(foreach c,$(HOST_VENV_COLLECTORS),collectors/$(c)/.venv) \
         $(CK_VENV) $(LINT_VENV) $(WEB_VENV) $(SITE_VENV)
$(foreach c,$(HOST_VENV_COLLECTORS),$(eval collectors/$(c)/.venv_PIP := -r collectors/$(c)/requirements.txt -e $(CK_DIR)))
$(CK_VENV)_PIP   := -e $(CK_DIR) $(CK_TEST_DEPS)
$(LINT_VENV)_PIP := -r $(LINT_DIR)/requirements.txt
$(WEB_VENV)_PIP  := -r web/requirements-test.txt
$(SITE_VENV)_PIP := -r site/requirements.txt

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
# A host venv reruns pip only when the collector's requirements.txt or
# collectorkit's pyproject.toml changes. The editable collectorkit picks
# up source edits without a reinstall.
build-$(1): $(if $(wildcard collectors/$(1)/Dockerfile),base-images)
	@echo "==> build collector: $(1)"
	@if [ -x collectors/$(1)/$(1) ] && [ -f collectors/$(1)/Dockerfile ]; then \
		( $(call LOCKED,$(1)) collectors/$(1)/$(1) build ); \
	fi
	@if [ -f collectors/$(1)/requirements.txt ] && { [ ! -f collectors/$(1)/Dockerfile ] || [ -f collectors/$(1)/.host-venv ]; }; then \
		$(call ENSURE_VENV,collectors/$(1)/.venv,$(1),build-$(1)); \
		$(call VENV,collectors/$(1)/.venv,collectors/$(1)/requirements.txt $(CK_DIR)/pyproject.toml); \
	elif [ ! -f collectors/$(1)/Dockerfile ]; then \
		echo "    $(1): nothing to build"; \
	fi

# A host-venv or hybrid collector tests on its venv. A Docker collector
# tests in its image, run directly rather than through the wrapper: no
# secrets or data mount, and a name docker picks, so a test never meets a
# live run's container. The tests come from the checkout, not the image.
# It is mounted read-only at /src, and sh copies it to a tmpfs at /app,
# then runs the collector's entrypoint from the copy, as a live run does,
# so the shared browser setup in entrypoint-lib.sh holds for the tests
# too. The tmpfs starts empty and hides any /app of the image, so sh does
# the copy before any entrypoint runs.
#
# The tests never run on the mount. Inode numbers on a VM's file share
# (virtiofs under Colima) are not stable: a file gets a new one whenever
# the VM drops it from its cache, for example under memory pressure.
# pytest compares inode numbers to check that an imported test module is
# the file it collected, so on the share a drop between its two stat
# calls stops the suite with "import file mismatch".
test-$(1): CAMOUFOX_BROWSER = none
test-$(1): build-$(1)
	@echo "==> test collector: $(1)"
	@if [ ! -d collectors/$(1)/tests ] && ! ls collectors/$(1)/test_*.py >/dev/null 2>&1; then \
		echo "    $(1): no tests"; \
	elif [ -f collectors/$(1)/.host-venv ] || [ ! -f collectors/$(1)/Dockerfile ]; then \
		if [ -x collectors/$(1)/.venv/bin/python ]; then \
			collectors/$(1)/.venv/bin/python -m pytest -q -p no:cacheprovider collectors/$(1); \
		else \
			echo "    $(1): cannot run tests (no venv)" >&2; exit 1; \
		fi; \
	else \
		docker run --rm --pull=never --user "$$$$(id -u):$$$$(id -g)" -e HOME=/tmp \
			-v "$(ROOT_DIR)/collectors/$(1):/src:ro" \
			--tmpfs "/app:exec,uid=$$$$(id -u),gid=$$$$(id -g)" \
			--entrypoint sh "wealthdb/$(1):$(TAG)" \
			-c 'cp -R /src/. /app && exec /app/entrypoint.sh sh -c "cd /app && pytest -q -p no:cacheprovider"'; \
	fi

clean-$(1):
	@echo "==> clean collector: $(1)"
	@$$(call CLEAN_PYCACHE,collectors/$(1))

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
#     (schwab-api, ubs-psn, …) and the tool venvs below
#   - Go modules in wealthdb/ (go get -u + go mod tidy, run by the
#     image's own toolchain — see update-wealthdb)
#   - The Playwright base image tag, moved to the newest release together
#     with the collectors' `playwright` pip pins and their user agents'
#     Chrome major (see update-playwright)
#   - Shared Docker base images, rebuilt with --pull so the underlying
#     OS layers also refresh
#
# `make all` then rebuilds the collector images on the refreshed bases.
# The host venvs and the gold engine pick up their updates at once.
# The one pin set `make update` does not move is the GitHub actions in
# .github/workflows/ and .github/actions/: their major tags move by hand.
#
# update-venvs upgrades each venv of VENVS that exists: its target
# (`make build-<name>`, `make test-collectorkit`, `make lint`,
# `make test-web`, `make test-site`) has to have created it once. Docker
# collectors have no venv on the host; their pip is inside the image and
# refreshes when the image rebuilds.

# UPDATE_VENV venv,args: upgrade pip, setuptools and wheel in venv, then
# `pip install --upgrade args`. A failed pip stops the recipe, so `make
# update` never reports done over a venv that did not update.
define UPDATE_VENV
echo "==> update $(1)"; \
$(1)/bin/python -m pip install --upgrade pip setuptools wheel || exit 1; \
$(1)/bin/python -m pip install --upgrade $(2) || exit 1
endef

update-venvs:
	@if $(foreach v,$(VENVS),[ ! -x $(v)/bin/python ] &&) true; then \
		echo "==> no host venvs to update"; \
	fi
	@$(foreach v,$(VENVS),if [ -x $(v)/bin/python ]; then \
		$(call UPDATE_VENV,$(v),$($(v)_PIP)); \
	fi;) true

# A tree of the native engine has no image whose Go resolves the
# modules. update-wealthdb refuses there, and so does update, as a whole
# and before any layer moves.
ifeq ($(ENGINE),native)
update update-wealthdb:
	@echo "$@: the image's Go resolves the modules, and this tree runs the native engine." >&2
	@echo "$@: run make update in a tree of the container engine (./configure)." >&2
	@exit 1
else
# The base rebuild follows the Playwright bump in one recipe, so a
# parallel make cannot build the base on the old tag.
update: update-venvs update-wealthdb
	@$(MAKE) --no-print-directory update-playwright
	@$(MAKE) --no-print-directory update-bases
	@echo ""
	@echo "==> update done. Run \`make all\` to rebuild collector images on top of the refreshed bases."

# The module graph is refreshed by the IMAGE's Go toolchain, never the
# host's: build first so the pinned toolchain exists, resolve against it,
# then rebuild so the refreshed modules land in the build cache. The image
# ships GOTOOLCHAIN=local, so a dependency that needs a newer Go fails here
# with "go.mod requires go >= X" instead of raising the `go` directive past
# what wealthdb/Dockerfile pins — the fix for that is to bump the base image,
# which is the single place the toolchain version is declared.
update-wealthdb:
	@echo "==> rebuild wealthdb image (its Go toolchain resolves the modules)"
	@$(call LOCKED,wealthdb) $(WEALTHDB) build
	@echo "==> update wealthdb/ go modules inside the image"
	$(WEALTHDB_GO) get -u ./...
	$(WEALTHDB_GO) mod tidy
	@echo "==> rebuild wealthdb image so the refreshed modules land in the build cache"
	@$(call LOCKED,wealthdb) $(WEALTHDB) build
endif

# The newest vX.Y.Z-noble tag of mcr.microsoft.com/playwright/python,
# with the Chrome major its Chromium reports, written to the base
# Dockerfile, both requirements pins and both user agents at once. An
# unreachable registry fails here and changes no file. It runs before
# update-bases, which then builds the base on the new tag.
update-playwright:
	@echo "==> update the Playwright base image tag, pip pins and user agents"
	@$(PYTHON) shared/update/bump-playwright.py

update-bases:
	@$(call LOCKED,bases) \
	$(call BUILD_BASES,,pull)

# ---- help --------------------------------------------------------------

help:
	@echo "wealthdb build/test — run from the repo root:"
	@echo ""
	@echo "  ./configure             choose the kind of engine: --engine=native runs the engine,"
	@echo "                          the dashboards and the MCP server as programs here, not as"
	@echo "                          containers (wealthdb/docs/NATIVE.md). Optional."
	@echo "  make all                build everything (engine + web + all collectors)"
	@echo "  make test               test everything; the suites run together"
	@echo "                          (SUITES=\"a b\" runs some of them)"
	@echo "  make list-suites        the suites make test runs (list-collectors: the collectors)"
	@echo "  make build-wealthdb     build the Go gold engine: its image, or the native program"
	@echo "  make test-wealthdb      run go test ./... on the engine's toolchain"
	@echo "                          (WEALTHDB_PKGS=, GO_TEST_FLAGS=)"
	@echo "  make build-web          build the optional Metabase BI image, or download Metabase"
	@echo "                          for a native tree"
	@echo "  make test-web           run the web unit tests (lifecycle script, provision.py)"
	@echo "  make test-mcp           run the MCP server lifecycle unit tests"
	@echo "  make build-collectors   build every collector"
	@echo "  make test-collectors    test every collector"
	@echo "  make test-collectorkit  run the shared collectorkit test suite"
	@echo "  make test-wrappers      run the shared wrapper tests and the Playwright pin check"
	@echo "  make test-pii           run the privacy check's unit tests"
	@echo "  make test-native        test the native engine in a container that holds its tools:"
	@echo "                          NATIVE_SUITES of make test, then an install and a walk end"
	@echo "                          to end with both servers"
	@echo "  make pii                privacy check of the unpushed commits and the index"
	@echo "                          (PII_BASE=, default origin/main; SHOW=1 prints the values)"
	@echo "  make pii-history        privacy check of every commit"
	@echo "  make lint               gofmt + go vet (engine), ruff (Python)"
	@echo "  make lint-go / lint-python"
	@echo "  make build-<name>       build one collector (e.g. build-schwab-web)"
	@echo "  make test-<name>        test one collector  (e.g. test-schwab-web)"
	@echo "  make base-images        build the shared bases; skip one whose inputs did not change"
	@echo "                          (CAMOUFOX_BROWSER=none: base-camoufox without its browser,"
	@echo "                          unless the one there has it)"
	@echo "  make camoufox-archive   write the local Camoufox browser archive from the image"
	@echo "                          wealthdb/base-camoufox:keep-<build> (CAMOUFOX_KEEP=)"
	@echo ""
	@echo "  make install            symlink wealthdb + wealthdb-collect into BINDIR (~/.local/bin);"
	@echo "                          after ./configure --prefix=DIR, copy the tree under DIR"
	@echo "  make hooks              run the privacy check and make lint on every commit (install runs it too)"
	@echo "  make uninstall          remove those symlinks and the hooks; with a prefix, the links"
	@echo "                          and the copy. A native tree's servers are stopped first"
	@echo ""
	@echo "  make clean              remove build artefacts (pycache, caches, demo test roots,"
	@echo "                          build locks)"
	@echo "  make cleanall           also remove docker images + venvs, the native program,"
	@echo "                          the dashboards' downloaded files, test-native's cache"
	@echo "  make clean-<name> / cleanall-<name>   (incl. -wealthdb, -web, -collectorkit,"
	@echo "                          -collectors, -site; clean-demo, clean-build-locks,"
	@echo "                          cleanall-bases, cleanall-lint, cleanall-native)"
	@echo ""
	@echo "  make demo               build the synthetic demo household (WEALTHDB_DEMO_ROOT,"
	@echo "                          default .demo-root's first line, else ~/wealthdb-demo;"
	@echo "                          AS_OF=, SEED=, FINDINGS=1)"
	@echo "  make demo-roll          append the days since the last demo build and load them"
	@echo "  make demo-web / demo-web-stop   the demo's dashboards, own container and port"
	@echo "  make demo-mcp / demo-mcp-stop   the demo over MCP, own container and port"
	@echo "  make test-demo          test the demo generator, a short demo load and a build to today"
	@echo ""
	@echo "  make site-shots         rebuild the demo at the pinned seed and date (resets the"
	@echo "                          demo root), shoot its dashboards, print the page's figures"
	@echo "  make site-images        download the published screenshots for a local preview"
	@echo "  make site-publish       upload the screenshots to their GitHub release and deploy"
	@echo "                          (token: WEALTHDB_SITE_TOKEN or ~/.secrets/github-site.env)"
	@echo "  make test-site          run the page tools' unit tests"
	@echo ""
	@echo "  WEALTHDB_IMAGE_TAG=…    the tag of every image built and run (default:"
	@echo "                          .image-tag's first line, else latest)"
	@echo ""
	@echo "  make update             refresh all tooling: pip in every host venv,"
	@echo "                          go modules in wealthdb/, the Playwright pins,"
	@echo "                          shared Docker bases"
	@echo "  make update-venvs / update-wealthdb / update-playwright / update-bases"
	@echo "                          (one layer at a time; update-bases pulls and"
	@echo "                          rebuilds, and does not skip)"
	@echo ""
	@echo "  collectors: $(COLLECTORS)"
