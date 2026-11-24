#!/usr/bin/env bash
# Behaviour test of shared/brew/formula.sh: the formula it prints states
# the dashboards' two files at the pins web/Dockerfile holds, installs
# from the main branch without a release and from the release's archive
# alone with one, keeps the build to what a checksum names, refuses half
# a release, and is Ruby that parses. And of release-formula.sh: it
# prints a release's formula only for an archive that is the tag's tree.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
F="$ROOT/shared/brew/formula.sh"
pin() { sed -n "s/^ARG $1=//p" "$ROOT/web/Dockerfile"; }

echo "the formula:"
head="$("$F")" || bad "formula.sh failed"
has "it is the wealthdb formula" "class Wealthdb < Formula" "$head"
has "Metabase at the Dockerfile's version" "https://downloads.metabase.com/$(pin MB_VERSION)/metabase.jar" "$head"
has "and checksum" "sha256 \"$(pin MB_SHA256)\"" "$head"
has "the driver at the Dockerfile's address" "url \"$(pin DRIVER_URL)\"" "$head"
has "and checksum" "sha256 \"$(pin DRIVER_SHA256)\"" "$head"
has "it configures the native engine" '"./configure", "--engine=native", "--prefix=#{prefix}"' "$head"
has "it installs from the main branch" 'head "https://github.com/smartpointer-com/wealthdb.git", branch: "main"' "$head"
lacks "with no release, it names no archive" "archive/refs/tags" "$head"
has "it names the first command to run" "wealthdb config" "$head"
has "the build may add no module to go.sum" 'ENV["GOFLAGS"] = "-mod=readonly"' "$head"
has "and downloads no other Go" 'ENV["GOTOOLCHAIN"] = "local"' "$head"

# A checksum's shape, made here: 64 hex digits.
sum="$(printf '%032d' 0 | tr 0 a)$(printf '%032d' 0)"
rel="$("$F" 1.2.3 "$sum")" || bad "formula.sh 1.2.3 SUM failed"
has "a release names its archive" 'url "https://github.com/smartpointer-com/wealthdb/archive/refs/tags/v1.2.3.tar.gz"' "$rel"
has "and its checksum" "sha256 \"$sum\"" "$rel"
lacks "a release installs from no branch" "head " "$rel"
if [ "$(grep -c 'sha256 "' <<<"$rel")" = 3 ] && [ "$(grep -c ' url "' <<<"$rel")" = 3 ]; then ok "every download of a release has a checksum"
else bad "a download of a release has no checksum"; fi

# refused DESC NEEDLE ARGS...: formula.sh ARGS fails, prints no formula,
# and its message holds NEEDLE.
refused() {
    local desc="$1" needle="$2" out err; shift 2
    if out="$("$F" "$@" 2>"$TMP/err")"; then bad "$desc: accepted"; return; fi
    err="$(cat "$TMP/err")"
    if [ -n "$out" ]; then bad "$desc: a formula is printed"; return; fi
    has "$desc: refused" "$needle" "$err"
}
refused "a version with no checksum" "usage: formula.sh" 1.2.3
refused "a version with an empty checksum" "not 64 hex digits" 1.2.3 ""
refused "a checksum that is too short" "not 64 hex digits" 1.2.3 abc123
refused "a checksum with another character" "not 64 hex digits" 1.2.3 "${sum%?}g"
refused "a checksum with no version" "is no version" "" "$sum"
refused "a version that is no number" "is no version" latest "$sum"
refused "a version with its v" "the tag without its v" v1.2.3 "$sum"

if command -v ruby >/dev/null 2>&1; then
    printf '%s\n' "$head" > "$TMP/head.rb"
    printf '%s\n' "$rel" > "$TMP/rel.rb"
    if ruby -c "$TMP/head.rb" >/dev/null 2>&1 && ruby -c "$TMP/rel.rb" >/dev/null 2>&1; then ok "both forms parse as Ruby"
    else bad "the formula does not parse as Ruby"; fi
fi

echo "a release's formula, from its archive:"
# A repository of its own with the two scripts, a Dockerfile's pins and
# one tag. No user or system git config, and none of a calling hook's
# variables.
unset GIT_DIR GIT_INDEX_FILE GIT_WORK_TREE
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid
R="$TMP/repo"
mkdir -p "$R/shared/brew" "$R/shared/wrappers" "$R/web" "$R/wealthdb"
cp "$ROOT/shared/brew/formula.sh" "$ROOT/shared/brew/release-formula.sh" \
    "$ROOT/shared/brew/same_tree.py" "$R/shared/brew/"
cp "$ROOT/shared/wrappers/image-tag.sh" "$R/shared/wrappers/"
cp "$ROOT/web/Dockerfile" "$R/web/"
echo "package main" > "$R/wealthdb/main.go"
# As the real tree has them: a link, and a file git fills in on export.
echo "rules" > "$R/AGENTS.md" && ln -s AGENTS.md "$R/CLAUDE.md"
# The placeholder is put together here, so that no export of this test
# file fills it in.
printf 'commit: $%s:%%H$\n' Format > "$R/wealthdb/.archive-version"
echo "wealthdb/.archive-version export-subst" > "$R/.gitattributes"
git -C "$R" init -q && git -C "$R" add -A && git -C "$R" commit -qm one && git -C "$R" tag v1.2.3
RF="$R/shared/brew/release-formula.sh"
git -C "$R" archive --format=tar.gz --prefix=wealthdb-1.2.3/ -o "$TMP/good.tar.gz" v1.2.3
if command -v sha256sum >/dev/null 2>&1; then good_sum="$(sha256sum "$TMP/good.tar.gz" | cut -d' ' -f1)"
else good_sum="$(shasum -a 256 "$TMP/good.tar.gz" | cut -d' ' -f1)"; fi
# Every run keeps its files in a folder of its own, which must end empty.
export TMPDIR="$TMP/t"
mkdir "$TMPDIR"
left_nothing() {
    if [ -z "$(ls -A "$TMPDIR")" ]; then ok "$1"; else bad "$1: $(ls -A "$TMPDIR")"; fi
}
out="$("$RF" 1.2.3 "$TMP/good.tar.gz" 2>"$TMP/err")" || bad "an archive of the tag is refused: $(cat "$TMP/err")"
has "an archive that is the tag's tree gives the formula" 'archive/refs/tags/v1.2.3.tar.gz' "$out"
has "with that archive's checksum" "sha256 \"$good_sum\"" "$out"
left_nothing "and the run leaves no file behind"

# Without ARCHIVE the archive is downloaded: a curl of this test's own
# hands over the good one and notes how it was called.
mkdir "$TMP/bin"
cat > "$TMP/bin/curl" <<EOF
#!/bin/sh
printf '%s\n' "\$*" > "$TMP/curl-args"
while [ \$# -gt 0 ]; do
    [ "\$1" = -o ] && cp "$TMP/good.tar.gz" "\$2"
    shift
done
EOF
chmod +x "$TMP/bin/curl"
out="$(PATH="$TMP/bin:$PATH" "$RF" 1.2.3 2>"$TMP/err")" || bad "the downloaded archive is refused: $(cat "$TMP/err")"
has "the downloaded archive gives the formula too" "sha256 \"$good_sum\"" "$out"
has "it is GitHub's archive of the tag" "https://github.com/smartpointer-com/wealthdb/archive/refs/tags/v1.2.3.tar.gz" "$(cat "$TMP/curl-args")"
has "asked for over https alone" "--proto =https --tlsv1.2" "$(cat "$TMP/curl-args")"

# held_back DESC NEEDLE ARGS...: release-formula.sh ARGS fails, prints no
# formula, and its message holds NEEDLE.
held_back() {
    local desc="$1" needle="$2" out; shift 2
    if out="$("$RF" "$@" 2>"$TMP/err")"; then bad "$desc: a formula is printed"; return; fi
    if [ -n "$out" ]; then bad "$desc: output on a refusal"; return; fi
    has "$desc: refused" "$needle" "$(cat "$TMP/err")"
}
# The same tree with one file changed, one added, one left out.
for case in changed added missing; do
    rm -rf "$TMP/x" && mkdir "$TMP/x" && tar -xzf "$TMP/good.tar.gz" -C "$TMP/x"
    case "$case" in
        changed) echo "package evil" > "$TMP/x/wealthdb-1.2.3/wealthdb/main.go" ;;
        added)   echo "x" > "$TMP/x/wealthdb-1.2.3/wealthdb/extra.go" ;;
        missing) rm "$TMP/x/wealthdb-1.2.3/wealthdb/main.go" ;;
    esac
    tar -czf "$TMP/$case.tar.gz" -C "$TMP/x" wealthdb-1.2.3
    held_back "an archive with a file $case" "is not the tree of v1.2.3" 1.2.3 "$TMP/$case.tar.gz"
done
rm -rf "$TMP/x" && mkdir -p "$TMP/x/wealthdb-1.2.3" "$TMP/x/other" && echo x > "$TMP/x/other/f"
tar -czf "$TMP/outside.tar.gz" -C "$TMP/x" wealthdb-1.2.3 other
held_back "an archive with a member outside its top folder" "outside its top folder" 1.2.3 "$TMP/outside.tar.gz"

# What unpacking would hide: the good archive, written again member by
# member with one thing changed. Each has the tag's bytes wherever a
# comparison of two unpacked trees would look.
remade() {
    python3 -I - "$TMP/good.tar.gz" "$TMP/$1.tar.gz" "$1" <<'PY'
import copy, io, sys, tarfile

src, dst, case = sys.argv[1:4]
with tarfile.open(src) as old, tarfile.open(dst, "w:gz") as new:
    for m in old:
        data = old.extractfile(m).read() if m.isreg() else None
        is_main = m.name.endswith("/wealthdb/main.go")
        if case == "linked" and is_main:
            # A link out of the tree, to where the tag's own copy would lie.
            m.type, m.linkname, m.size, data = tarfile.SYMTYPE, "../../ours/wealthdb/main.go", 0, None
        if case == "linked-folder" and m.name.endswith("/wealthdb") and m.isdir():
            m.type, m.linkname = tarfile.SYMTYPE, "../elsewhere"
        if case == "retargeted" and m.issym():
            m.linkname = "wealthdb/main.go"
        if case == "hard-linked" and is_main:
            m.type, m.linkname, m.size, data = tarfile.LNKTYPE, m.name.replace("main.go", ".archive-version"), 0, None
        if case == "executable" and is_main:
            m.mode |= 0o111
        new.addfile(m, io.BytesIO(data) if data is not None else None)
        if is_main and case in ("doubled", "other-case", "dotted"):
            extra = copy.copy(m)
            if case == "other-case":
                extra.name = m.name.replace("main.go", "MAIN.go")
            if case == "dotted":
                extra.name = m.name.replace("wealthdb/main.go", "wealthdb/../../evil.go")
            new.addfile(extra, io.BytesIO(data))
PY
}
while IFS='|' read -r case needle desc; do
    remade "$case" || { bad "$desc: the archive could not be written"; continue; }
    held_back "an archive with $desc" "$needle" 1.2.3 "$TMP/$case.tar.gz"
done <<'CASES'
linked|wealthdb/main.go: a link in the archive, a file in the tag|a link in place of a file
linked-folder|wealthdb: a link in the archive, a dir in the tag|a link in place of a folder
retargeted|CLAUDE.md: the link names another target|a link to another target
hard-linked|wealthdb/main.go: neither a file, a folder nor a link|a hard link in place of a file
executable|wealthdb/main.go: another execute bit|a file made executable
doubled|wealthdb/main.go: in the archive twice|a member in it twice
other-case|wealthdb/MAIN.go: not in the tag|a second member whose name differs in case alone
dotted|not a plain path below the top folder|a member that climbs out with ..
CASES
left_nothing "a refused archive leaves no file behind either"

held_back "a version with no tag" "has no tag v9.9.9" 9.9.9 "$TMP/good.tar.gz"
held_back "an archive that is no file" "is no file" 1.2.3 "$TMP/none.tar.gz"
held_back "no version" "usage: release-formula.sh"
held_back "three arguments" "usage: release-formula.sh" 1.2.3 "$TMP/good.tar.gz" more
held_back "a version with its v" "the tag without its v" v1.2.3
held_back "a version that is no number" "is no version" latest

# The pins reach the formula through three places. Each must be as the
# tag has it.
echo "# edited" >> "$R/shared/wrappers/image-tag.sh"
held_back "a helper that differs from the tag's" "differs from v1.2.3" 1.2.3 "$TMP/good.tar.gz"
git -C "$R" checkout -q -- shared/wrappers/image-tag.sh
echo "# edited" >> "$R/web/Dockerfile"
held_back "pins that differ from the tag's" "differs from v1.2.3" 1.2.3 "$TMP/good.tar.gz"
git -C "$R" checkout -q -- web/Dockerfile

echo "the shape of the Dockerfile's pins:"
F="$R/shared/brew/formula.sh"
# with_pin NAME VALUE: the repository's Dockerfile with that one pin.
with_pin() {
    git -C "$R" checkout -q -- web/Dockerfile
    sed "s|^ARG $1=.*|ARG $1=$2|" "$R/web/Dockerfile" > "$TMP/Dockerfile" && cp "$TMP/Dockerfile" "$R/web/Dockerfile"
}
with_pin MB_SHA256 abc123
refused "a Metabase checksum that is too short" "MB_SHA256" 1.2.3 "$sum"
with_pin DRIVER_SHA256 "$sum # the driver"
refused "a driver checksum with a comment after it" "DRIVER_SHA256" 1.2.3 "$sum"
with_pin MB_VERSION 'v1"; system "x'
refused "a version with a quote in it" "MB_VERSION" 1.2.3 "$sum"
with_pin DRIVER_URL 'https://example.invalid/${DRIVER_VERSION}/d.jar'
refused "an address with a variable in it" "DRIVER_URL" 1.2.3 "$sum"
with_pin DRIVER_URL http://example.invalid/d.jar
refused "an address that is not https" "no https address" 1.2.3 "$sum"
git -C "$R" checkout -q -- web/Dockerfile
"$F" 1.2.3 "$sum" >/dev/null 2>&1 && ok "the tag's own pins pass" || bad "the tag's own pins are refused"

summary "brew formula tests"
