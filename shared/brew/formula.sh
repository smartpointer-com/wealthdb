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
# ./configure, make and make install. It holds no Metabase: the
# dashboards download theirs the first time they start, at the versions
# and checksums web/Dockerfile pins (web/web).
#
# Everything a release's formula installs is held to a checksum: the
# source archive by the formula, the Go modules by go.sum, which the
# build may not add to. So a release's formula has no `head`: nothing
# installs it from a branch. The build uses the Go that Homebrew
# installed and downloads no other.
set -eu

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

  def install
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
