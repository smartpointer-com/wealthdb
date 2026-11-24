#!/bin/sh
# formula.sh [VERSION SHA256] — print the Homebrew formula of this tree,
# for the tap smartpointer-com/homebrew-tap (Formula/wealthdb.rb).
#
# VERSION is a release tag without its v, and SHA256 the checksum of that
# release's source archive on GitHub, 64 hex digits. Both come together;
# release-formula.sh beside this file finds the checksum, after it has
# held the archive to the tag. Without them the formula installs from
# the main branch alone (brew install --HEAD).
#
# The formula builds the native engine (wealthdb/docs/NATIVE.md) with
# ./configure, make and make install. The dashboards' two files are
# resources at the versions and checksums web/Dockerfile pins, read from
# it here, so the formula and the image never state two pins.
#
# Everything a release's formula installs is held to a checksum: the
# source archive and the two resources by the formula, the Go modules by
# go.sum, which the build may not add to. So a release's formula has no
# `head`: nothing installs it from a branch. The build uses the Go that
# Homebrew installed and downloads no other.
set -eu

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
. "$ROOT/shared/wrappers/image-tag.sh"
arg() { wealthdb_dockerfile_arg "$ROOT/web/Dockerfile" "$1"; }

usage() { echo "usage: formula.sh [VERSION SHA256]" >&2; exit 2; }

VERSION="${1:-}" SHA256="${2:-}"
case "$#" in
    0) ;;
    2)
        case "$VERSION" in
            v*) echo "formula.sh: the version is the tag without its v" >&2; exit 2 ;;
            *[!0-9A-Za-z.-]* | [!0-9]* | '') echo "formula.sh: '$VERSION' is no version" >&2; usage ;;
        esac
        case "$SHA256" in
            *[!0-9a-f]* | '') SHA256= ;;
        esac
        if [ "${#SHA256}" -ne 64 ]; then
            echo "formula.sh: the checksum is not 64 hex digits" >&2
            usage
        fi
        ;;
    *) usage ;;
esac

mb_version="$(arg MB_VERSION)" mb_sha="$(arg MB_SHA256)"
driver_url="$(arg DRIVER_URL)" driver_sha="$(arg DRIVER_SHA256)"
for v in "$mb_version" "$mb_sha" "$driver_url" "$driver_sha"; do
    [ -n "$v" ] || { echo "formula.sh: web/Dockerfile lacks a pin" >&2; exit 1; }
done
# The pins go into the formula between double quotes, as they are. One
# with a quote, a comment, a variable or a blank would print a formula
# that holds a file to no real checksum, so each is held to its shape.
hex64() { case "$1" in *[!0-9a-f]*) return 1 ;; esac; [ "${#1}" -eq 64 ]; }
plain() { case "$1" in *[!0-9A-Za-z._/:+-]*) return 1 ;; esac; }
pin_fails() { echo "formula.sh: web/Dockerfile pins $1 as '$2', which is $3" >&2; exit 1; }
hex64 "$mb_sha" || pin_fails MB_SHA256 "$mb_sha" "not 64 hex digits"
hex64 "$driver_sha" || pin_fails DRIVER_SHA256 "$driver_sha" "not 64 hex digits"
plain "$mb_version" || pin_fails MB_VERSION "$mb_version" "no plain version"
plain "$driver_url" || pin_fails DRIVER_URL "$driver_url" "no plain address"
case "$driver_url" in
    https://*) ;;
    *) pin_fails DRIVER_URL "$driver_url" "no https address" ;;
esac

cat <<RUBY
# Written by shared/brew/formula.sh of the wealthdb repository; edit it
# there, and write this file again.
class Wealthdb < Formula
  desc "Household finances from every bank and broker, in one local database"
  homepage "https://github.com/smartpointer-com/wealthdb"
RUBY
if [ -n "$VERSION" ]; then
    cat <<RUBY
  url "https://github.com/smartpointer-com/wealthdb/archive/refs/tags/v$VERSION.tar.gz"
  sha256 "$SHA256"
  license "MIT"
RUBY
else
    cat <<RUBY
  license "MIT"
  head "https://github.com/smartpointer-com/wealthdb.git", branch: "main"
RUBY
fi
cat <<RUBY

  depends_on "go" => :build
  depends_on arch: :arm64
  depends_on "colima"
  depends_on "docker"
  depends_on :macos
  depends_on "openjdk@21"
  depends_on "python@3.13"

  # The dashboards: Metabase and its DuckDB driver, as the image holds them.
  resource "metabase" do
    url "https://downloads.metabase.com/$mb_version/metabase.jar"
    sha256 "$mb_sha"
  end

  resource "duckdb-driver" do
    url "$driver_url"
    sha256 "$driver_sha"
  end

  def install
    resource("metabase").stage { (buildpath/"web/lib").install "metabase.jar" }
    resource("duckdb-driver").stage { (buildpath/"web/lib").install "duckdb.metabase-driver.jar" }

    # The build downloads the Go modules go.sum names, each held to its
    # checksum there, and nothing else: no module go.sum lacks, and no
    # other Go toolchain than the one Homebrew installed.
    ENV["GOFLAGS"] = "-mod=readonly"
    ENV["GOTOOLCHAIN"] = "local"

    system "./configure", "--engine=native", "--prefix=#{prefix}",
           "GO=#{formula_opt_bin("go")}/go",
           "PYTHON=#{formula_opt_bin("python@3.13")}/python3.13",
           "JAVA=#{formula_opt_bin("openjdk@21")}/java"
    system "make", "build-wealthdb"
    system "make", "install"
  end

  def caveats
    <<~EOS
      To set up wealthdb, run:
        wealthdb config
      It asks a few questions, and names the next command at each step.
    EOS
  end

  test do
    assert_match "wealthdb", shell_output("#{bin}/wealthdb version")
    # No config yet: validate says so, and names the setup.
    assert_match "wealthdb config", shell_output("#{bin}/wealthdb config validate 2>&1", 2)
  end
end
RUBY
