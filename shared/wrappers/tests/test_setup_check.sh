#!/usr/bin/env bash
# Behaviour test of the setup check (shared/wrappers/setup-lib.sh) and
# of every collector's setup declaration (collectors/<name>/setup.txt).
#
# Declarations: each one parses, names only keywords, verbs and README
# sections that exist, and names only variables its collector forwards
# (ENV_VARS) or sources on the host. Every credential-shaped name in a
# collector's code is declared, so the declaration cannot fall behind
# the code.
#
# The check: a fake collector `testcol` runs through wrapper_main (the
# Docker family), wrapper_login_folds and host_begin (the host family),
# with `docker` stubbed. Each case asserts the exit status, what is
# printed, that no value from an env file is ever printed, and that a
# complete setup prints nothing and reaches `docker run` with the argv
# it has with no declaration at all.
#
# A setup of its own: with WEALTHDB_COLLECT and WEALTHDB_PROGRAM set, as
# the engine launcher sets them, every command printed starts with their
# words, and a text that names the default data root names the setup's
# folder. Unset, every text is as the declaration writes it.
#
# Fleet: every collector's `login` with an empty secrets folder prints
# its instructions and starts nothing; plaid, manual, svb and curves
# print the first step their declaration states and exit 0, and the
# last three end with the commands that follow.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"
REPO="$(cd "$HERE/../../.." && pwd -P)"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
TMP="$(cd "$TMP" && pwd -P)"
sandbox_env "$TMP"
unset HAS_ENV_FILE TESTCOL_USERNAME TESTCOL_EMAIL TESTCOL_PASSWORD \
    TESTCOL_NOTE OTHER_ENV_FILE

stub_docker "$TMP/bin"
# Every output of every case lands here too, to look for a leaked value.
ALL="$TMP/all-output"
: > "$ALL"
# The value of a secret in the env files below. It must never be printed.
SECRET_MARK="SYNTHETIC-SECRET"

# is DESC WANT GOT
is() {
    if [[ "$3" == "$2" ]]; then ok "$1"; else bad "$1 (want '$2', got '$3')"; fi
}

# ---- the fake collector ------------------------------------------------

COL="$TMP/col"
mkdir -p "$COL"
cat > "$COL/setup.txt" <<'EOF'
# A comment line.
var TESTCOL_USERNAME required plain <email> The email you sign in with
alias TESTCOL_USERNAME TESTCOL_EMAIL
var TESTCOL_PASSWORD required secret <password> Your password
flag TESTCOL_PASSWORD --pass-from-test
checks login download
skip --sandbox
provider Ask the provider for access first.
docs Setup
signin A code arrives by SMS.
then wealthdb-collect testcol download
then wealthdb-collect testcol load
EOF
COMPLETE="TESTCOL_USERNAME='synthetic-user'
TESTCOL_PASSWORD='$SECRET_MARK'"

# docker_run MODE DECL [args...]: run the fake collector, the Docker
# family when MODE is docker, else the host family. DECL names the
# setup.txt to use, or "none". Prints stdout+stderr, then "rc=N" and,
# for the Docker family, "docker=yes" when `docker run` was reached.
docker_run() {
    local mode="$1" decl="$2"
    shift 2
    rm -f "$TMP/stub.log"
    local dir="$TMP/run-$RANDOM"
    mkdir -p "$dir"
    [[ "$decl" != none ]] && cp "$decl" "$dir/setup.txt"
    (
        # The wrappers' own shell options.
        set -euo pipefail
        PATH="$TMP/bin:$PATH"
        export STUB_LOG="$TMP/stub.log"
        cd "$TMP" || exit 1
        HERE="$dir"
        NAME=testcol; ENV_PREFIX=TESTCOL
        if [[ "$mode" == docker ]]; then
            ENV_VARS=(TESTCOL_USERNAME TESTCOL_EMAIL TESTCOL_PASSWORD)
            # shellcheck disable=SC1091
            source "$REPO/shared/wrappers/wrapper-lib.sh"
            wrapper_init
            if [[ "${FOLD:-0}" == 1 ]] && wrapper_login_folds --help -- "$@"; then
                echo "folded notice"
                exit 0
            fi
            wrapper_main "$@"
        else
            # shellcheck disable=SC1091
            source "$REPO/shared/wrappers/host-lib.sh"
            host_begin login download load -- "$@"
            echo "host verb ran"
        fi
    ) > "$TMP/out" 2>&1
    local rc=$?
    cat "$TMP/out" >> "$ALL"
    cat "$TMP/out"
    echo "rc=$rc"
    if [[ -f "$TMP/stub.log" ]] && grep -qx '  run' "$TMP/stub.log"; then
        echo "docker=yes"
    fi
}

# docker_argv: the `docker run` call of the last run, masked of the
# per-run declaration dir.
docker_argv() {
    awk '/^docker$/ {n++} n' "$TMP/stub.log" 2>/dev/null | sed "s|$TMP/run-[0-9]*|<HERE>|g"
}

DECL="$COL/setup.txt"
SECRETS="$TMP/home/.secrets"
export HOME="$TMP/home"
fresh_secrets() { rm -rf "$SECRETS" "$TMP/alt"; }

echo "the setup check, Docker family:"

fresh_secrets
out="$(docker_run docker "$DECL" login)"
has "no folder: exit 78" "rc=78" "$out"
lacks "no folder: no container" "docker=yes" "$out"
has "no folder: the folder is made" "mkdir -p $SECRETS" "$out"
has "no folder: the folder is 0700" "chmod 700 $SECRETS" "$out"
has "no folder: the file is made" "touch $SECRETS/testcol.env" "$out"
has "no folder: the file is 0600" "chmod 600 $SECRETS/testcol.env" "$out"
has "no folder: the label" "# The email you sign in with." "$out"
has "no folder: the template line" "TESTCOL_USERNAME='<email>'" "$out"
has "no folder: the secret's line" "TESTCOL_PASSWORD='<password>'" "$out"
has "no folder: how to write a single quote" "it's becomes 'it'\\''s'" "$out"
has "no folder: the provider step" "Ask the provider for access first." "$out"
has "no folder: the README section" "section \"Setup\"" "$out"
has "no folder: how the sign-in runs" "A code arrives by SMS." "$out"
has "no folder: run login again" "Then run: wealthdb-collect testcol login" "$out"
has "no folder: the next commands" "  1.  wealthdb-collect testcol download" "$out"
has "no folder: what a download does" "      Fetches the data from the provider and saves the raw files on" "$out"
lacks "no folder: the old error is gone" "secrets dir does not exist" "$out"

mkdir -p "$SECRETS"
out="$(docker_run docker "$DECL" login)"
has "folder, no file: exit 78" "rc=78" "$out"
has "folder, no file: the file is made" "touch $SECRETS/testcol.env" "$out"
has "folder, no file: both lines" "TESTCOL_PASSWORD='<password>'" "$out"

echo "TESTCOL_USERNAME='synthetic-user'" > "$SECRETS/testcol.env"
out="$(docker_run docker "$DECL" login)"
has "a required variable missing: exit 78" "rc=78" "$out"
lacks "a required variable missing: no container" "docker=yes" "$out"
has "a required variable missing: its line" "TESTCOL_PASSWORD='<password>'" "$out"
lacks "a variable that is set: no line" "TESTCOL_USERNAME=" "$out"
lacks "the file exists: not made again" "touch " "$out"
has "the file exists: open it" "open -e $SECRETS/testcol.env" "$out"

out="$(docker_run docker "$DECL" download)"
has "download is checked too" "Then run: wealthdb-collect testcol download" "$out"
lacks "the next commands start after download" ".  wealthdb-collect testcol download" "$out"
has "the next commands: load" "  1.  wealthdb-collect testcol load" "$out"

printf '%s\n' "$COMPLETE" > "$SECRETS/testcol.env"
out="$(docker_run docker none login)"
want_argv="$(docker_argv)"
out="$(docker_run docker "$DECL" login)"
is "complete: prints nothing" "rc=0
docker=yes" "$out"
is "complete: the same docker argv as with no declaration" "$want_argv" "$(docker_argv)"
out="$(docker_run docker none download --dry-run)"
want_argv="$(docker_argv)"
out="$(docker_run docker "$DECL" download --dry-run)"
is "complete download --dry-run: prints nothing" "rc=0
docker=yes" "$out"
is "complete download: the same docker argv" "$want_argv" "$(docker_argv)"

printf "TESTCOL_USERNAME='a b \$c'\nTESTCOL_PASSWORD='%s it'\\\\''s \$HOME'\n" "$SECRET_MARK" \
    > "$SECRETS/testcol.env"
out="$(docker_run docker "$DECL" login)"
is "a value with \$, spaces and a single quote: prints nothing" "rc=0
docker=yes" "$out"

echo "TESTCOL_EMAIL='synthetic-user'
TESTCOL_PASSWORD='$SECRET_MARK'" > "$SECRETS/testcol.env"
out="$(docker_run docker "$DECL" login)"
is "an alias stands for its variable" "rc=0
docker=yes" "$out"

# A value still equal to its placeholder, as copied from the template.
printf "TESTCOL_USERNAME='<email>'\nTESTCOL_PASSWORD='%s'\n" "$SECRET_MARK" \
    > "$SECRETS/testcol.env"
out="$(docker_run docker "$DECL" login)"
has "a placeholder in the file: exit 78" "rc=78" "$out"
lacks "a placeholder in the file: no container" "docker=yes" "$out"
has "a placeholder in the file: said so" "These lines still hold the placeholder." "$out"
has "a placeholder in the file: its line" "TESTCOL_USERNAME='<email>'" "$out"
lacks "a placeholder in the file: no line to add" "Add these lines." "$out"
lacks "a placeholder in the file: a real value is not listed" "TESTCOL_PASSWORD=" "$out"
printf "TESTCOL_EMAIL='<email>'\nTESTCOL_PASSWORD='%s'\n" "$SECRET_MARK" \
    > "$SECRETS/testcol.env"
out="$(docker_run docker "$DECL" login)"
has "a placeholder under an alias: exit 78" "rc=78" "$out"
has "a placeholder under an alias: said so" "still hold the placeholder" "$out"
printf "TESTCOL_USERNAME='<email>x'\nTESTCOL_PASSWORD='%s'\n" "$SECRET_MARK" \
    > "$SECRETS/testcol.env"
out="$(docker_run docker "$DECL" login)"
is "a value that only starts like the placeholder: prints nothing" "rc=0
docker=yes" "$out"
rm -f "$SECRETS/testcol.env"
out="$(TESTCOL_USERNAME=u TESTCOL_PASSWORD='<password>' docker_run docker "$DECL" login)"
has "a placeholder in the environment: exit 78" "rc=78" "$out"
has "a placeholder in the environment: its line" "TESTCOL_PASSWORD='<password>'" "$out"
lacks "a placeholder in the environment: a real value is not listed" "TESTCOL_USERNAME=" "$out"

rm -f "$SECRETS/testcol.env"
out="$(TESTCOL_USERNAME=u TESTCOL_PASSWORD="$SECRET_MARK" docker_run docker "$DECL" login)"
is "set in the environment only: prints nothing" "rc=0
docker=yes" "$out"
out="$(TESTCOL_USERNAME=u TESTCOL_PASSWORD= docker_run docker "$DECL" login)"
has "an empty variable is missing" "TESTCOL_PASSWORD='<password>'" "$out"
out="$(TESTCOL_USERNAME=u docker_run docker "$DECL" login --pass-from-test x)"
is "a flag stands for its variable" "rc=0
docker=yes" "$out"

mkdir -p "$TMP/alt"
out="$(docker_run docker "$DECL" login --secrets-dir "$TMP/alt")"
has "--secrets-dir: the file there" "touch $TMP/alt/testcol.env" "$out"
lacks "--secrets-dir: not the default" "$SECRETS/testcol.env" "$out"
out="$(docker_run docker "$DECL" login --secrets-dir="$TMP/alt/new")"
has "--secrets-dir=DIR: the folder there" "mkdir -p $TMP/alt/new" "$out"
printf '%s\n' "$COMPLETE" > "$TMP/alt/testcol.env"
out="$(docker_run docker "$DECL" login --secrets-dir "$TMP/alt")"
is "--secrets-dir: a complete file there" "rc=0
docker=yes" "$out"
out="$(TESTCOL_SECRETS_DIR="$TMP/alt" docker_run docker "$DECL" login)"
is "TESTCOL_SECRETS_DIR: a complete file there" "rc=0
docker=yes" "$out"

printf '%s\n' "$COMPLETE" > "$TMP/named.env"
out="$(TESTCOL_ENV_FILE="$TMP/named.env" docker_run docker "$DECL" login)"
has "TESTCOL_ENV_FILE without HAS_ENV_FILE: the container's file" \
    "touch $SECRETS/testcol.env" "$out"
out="$(HAS_ENV_FILE=1 TESTCOL_ENV_FILE="$TMP/named.env" docker_run docker "$DECL" login)"
is "TESTCOL_ENV_FILE under HAS_ENV_FILE=1: that file" "rc=0
docker=yes" "$out"
out="$(HAS_ENV_FILE=1 TESTCOL_ENV_FILE="$TMP/sub/none.env" docker_run docker "$DECL" login)"
has "TESTCOL_ENV_FILE that does not exist: made there" "touch $TMP/sub/none.env" "$out"
has "TESTCOL_ENV_FILE in another folder: that folder too" "mkdir -p $TMP/sub" "$out"

printf "TESTCOL_USERNAME=u\nTESTCOL_PASSWORD='%s\n" "$SECRET_MARK" > "$SECRETS/testcol.env"
out="$(docker_run docker "$DECL" login)"
has "a file that fails bash -n: exit 78" "rc=78" "$out"
has "a file that fails bash -n: said so" "testcol.env is not valid." "$out"
lacks "a file that fails bash -n: no container" "docker=yes" "$out"

rm -f "$SECRETS/testcol.env"
for args in "login --help" "login -h" "login --check" "login --env-file /secrets/x.env" \
    "login --env-file=/secrets/x.env" "login --sandbox" "load" "explore"; do
    # shellcheck disable=SC2086
    out="$(docker_run docker "$DECL" $args)"
    is "skipped: $args" "rc=0
docker=yes" "$out"
done

out="$(FOLD=1 docker_run docker "$DECL" login)"
has "a folded login: the instructions" "TESTCOL_PASSWORD='<password>'" "$out"
lacks "a folded login: no notice" "folded notice" "$out"
mkdir -p "$TMP/alt2"
printf '%s\n' "$COMPLETE" > "$TMP/alt2/testcol.env"
out="$(FOLD=1 docker_run docker "$DECL" login --secrets-dir "$TMP/alt2")"
is "a folded login, complete in --secrets-dir: the notice" "folded notice
rc=0" "$out"
out="$(FOLD=1 docker_run docker "$DECL" login --help)"
lacks "a folded login's pass flag: not checked" "rc=78" "$out"

# A declaration with a session, a key file, fallbacks and an optional
# variable.
cat > "$TMP/decl2" <<'EOF'
envfile first.env second.env
var TESTCOL_USERNAME required plain <id> The id
keyfile testcol_key The key
session testcol-profile
checks login
EOF
fresh_secrets; mkdir -p "$SECRETS"
echo "TESTCOL_USERNAME=u" > "$SECRETS/second.env"
out="$(docker_run docker "$TMP/decl2" login)"
has "a missing key file: exit 78" "rc=78" "$out"
has "a missing key file: its path" "$SECRETS/testcol_key  The key" "$out"
lacks "a fallback file that is read: its variable is there" "TESTCOL_USERNAME=" "$out"
: > "$SECRETS/testcol_key"
out="$(docker_run docker "$TMP/decl2" login)"
is "the fallback file and the key: prints nothing" "rc=0
docker=yes" "$out"
: > "$SECRETS/first.env"
out="$(docker_run docker "$TMP/decl2" login)"
has "the first file that exists is the one read" "TESTCOL_USERNAME='<id>'" "$out"
has "the instructions name the first file" "open -e $SECRETS/first.env" "$out"
mkdir -p "$SECRETS/testcol-profile"
out="$(docker_run docker "$TMP/decl2" login)"
is "a stored session: never blocks" "rc=0
docker=yes" "$out"

cat > "$TMP/decl3" <<'EOF'
var TESTCOL_NOTE optional plain <x> A setting that helps
checks login
EOF
fresh_secrets; mkdir -p "$SECRETS"
out="$(docker_run docker "$TMP/decl3" login)"
has "only optional variables, no file: a note" "TESTCOL_NOTE  A setting that helps" "$out"
has "only optional variables, no file: it goes on" "docker=yes" "$out"
echo "TESTCOL_NOTE='<x>'" > "$SECRETS/testcol.env"
out="$(docker_run docker "$TMP/decl3" login)"
has "an optional placeholder: exit 78" "rc=78" "$out"
has "an optional placeholder: said so" "still hold the placeholder" "$out"
echo "# empty" > "$SECRETS/testcol.env"
out="$(docker_run docker "$TMP/decl3" login)"
is "only optional variables, a file: prints nothing" "rc=0
docker=yes" "$out"

echo
echo "relative folders are named absolute in the instructions:"
fresh_secrets
out="$(WEALTHDB_SECRETS_DIR=rel/sec docker_run docker "$DECL" login)"
has "relative secrets: exit 78" "rc=78" "$out"
has "relative secrets: the folder is absolute" "mkdir -p $TMP/rel/sec" "$out"
has "relative secrets: the file is absolute" "touch $TMP/rel/sec/testcol.env" "$out"
lacks "relative secrets: no relative path" "mkdir -p rel/sec" "$out"
mkdir -p "$TMP/rel/sec"
echo "$COMPLETE" > "$TMP/rel/sec/testcol.env"
out="$(WEALTHDB_SECRETS_DIR=rel/sec docker_run docker "$DECL" login)"
has "relative secrets, complete: docker runs" "docker=yes" "$out"
rm -rf "$TMP/rel"
fresh_secrets
out="$(TESTCOL_ENV_FILE=rel/my.env docker_run host "$DECL" login)"
has "relative env file: named absolute" "touch $TMP/rel/my.env" "$out"
rm -rf "$TMP/rel"

echo "a login that ends well names the commands that follow:"
fresh_secrets
mkdir -p "$SECRETS"
echo "$COMPLETE" > "$SECRETS/testcol.env"
# has_text DESC WANT GOT: GOT holds the lines of WANT, as one piece.
has_text() {
    if [[ "$3" == *"$2"* ]]; then ok "$1"; else bad "$1 (no such text in: $3)"; fi
}
NEXT_BLOCK="What to do now

Run these commands in the terminal, in this order:

  1.  wealthdb-collect testcol download

      Fetches the data from the provider and saves the raw files on
      this computer. It changes nothing at the provider.

  2.  wealthdb-collect testcol load

      Reads the saved files into this source's own database.
------------------------------------------------------------------------"
out="$(WEALTHDB_SETUP_NEXT=1 docker_run docker "$DECL" login)"
has "login: docker runs" "docker=yes" "$out"
has_text "login: the block follows, each command with what it does" "$NEXT_BLOCK" "$out"
has "login: exit 0" "rc=0" "$out"
out="$(docker_run docker "$DECL" login)"
lacks "no terminal: no block, a scheduled run's output is as it was" "What to do now" "$out"
out="$(WEALTHDB_SETUP_NEXT=1 docker_run docker "$DECL" login --check)"
lacks "a session probe: no block" "What to do now" "$out"
out="$(WEALTHDB_SETUP_NEXT=1 docker_run docker "$DECL" download)"
lacks "another verb: no block" "What to do now" "$out"
out="$(WEALTHDB_SETUP_NEXT=0 docker_run docker "$DECL" login)"
lacks "WEALTHDB_SETUP_NEXT=0: no block" "What to do now" "$out"
out="$(WEALTHDB_SETUP_NEXT=1 FOLD=1 docker_run docker "$DECL" login)"
has "a folded login: its notice" "folded notice" "$out"
has_text "a folded login: the block after the notice" "$NEXT_BLOCK" "$out"
out="$(WEALTHDB_SETUP_NEXT=1 docker_run host "$DECL" login)"
has "host family: the verb runs" "host verb ran" "$out"
has_text "host family: the block follows" "$NEXT_BLOCK" "$out"
# A collector that links one login at a time: its `link` names what
# follows too, from the commands after its own line.
sed -e 's|^then wealthdb-collect testcol download$|then wealthdb-collect testcol link --item <name>   # once for each bank\
then wealthdb-collect testcol download|' "$DECL" > "$TMP/link.txt"
out="$(WEALTHDB_SETUP_NEXT=1 docker_run docker "$TMP/link.txt" link --item x)"
has "link: the commands after it" "  1.  wealthdb-collect testcol download" "$out"
lacks "link: not its own line again" "testcol link --item <name>" "$out"
out="$(WEALTHDB_SETUP_NEXT=1 docker_run docker "$TMP/link.txt" login)"
has_text "login before link: link comes first, its note below it" "  1.  wealthdb-collect testcol link --item <name>

      Links one bank: it prints the address of a sign-in page to open
      in a browser.
      once for each bank
" "$out"
# An optional part names the login the verb ran with, and goes when it
# ran with none.
sed -e 's|^then wealthdb-collect testcol download$|then wealthdb-collect testcol link --item <name>   # once for each bank\
then wealthdb-collect testcol download [--item <name>] --lookback all|' \
    -e 's|^then wealthdb-collect testcol load$|then wealthdb-collect testcol load [--item <name>]|' "$DECL" > "$TMP/item.txt"
out="$(WEALTHDB_SETUP_NEXT=1 docker_run docker "$TMP/item.txt" link --item schwab)"
has "link --item: download names it" "  1.  wealthdb-collect testcol download --item schwab --lookback all" "$out"
has "link --item: load names it" "  2.  wealthdb-collect testcol load --item schwab" "$out"
out="$(WEALTHDB_SETUP_NEXT=1 docker_run docker "$TMP/item.txt" link --item=chase)"
has "link --item=: the name too" "  2.  wealthdb-collect testcol load --item chase" "$out"
out="$(WEALTHDB_SETUP_NEXT=1 docker_run docker "$TMP/item.txt" login)"
has "no --item: the part goes" "  2.  wealthdb-collect testcol download --lookback all" "$out"
has "no --item: load alone" "  3.  wealthdb-collect testcol load
" "$out"
lacks "no --item: no bracket is left" "[--item" "$out"
# Colours, as wealthdb config shows them: only when asked for here, the
# commands in cyan and what each does dimmed.
out="$(WEALTHDB_SETUP_NEXT=1 WEALTHDB_SETUP_COLOR=1 docker_run docker "$DECL" login)"
has "colours: a command in cyan" $'\033[36mwealthdb-collect testcol download\033[0m' "$out"
has "colours: the title in bold" $'\033[1mWhat to do now\033[0m' "$out"
lacks "no colours without a terminal" $'\033[' "$(WEALTHDB_SETUP_NEXT=1 docker_run docker "$DECL" login)"
lacks "NO_COLOR: no colours" $'\033[' "$(NO_COLOR= WEALTHDB_SETUP_NEXT=1 docker_run docker "$DECL" login)"
out="$(WEALTHDB_SETUP_COLOR=1 docker_run docker "$TMP/decl2" login)"
has "colours: the key files' step in bold, as every numbered step" \
    $'\033[1m3. Put these files in the secrets folder:\033[0m' "$out"
rm -f "$SECRETS/testcol.env"
# A verb that names a login is named back with it.
sed -i.bak -e 's/^checks .*/checks link login/' "$TMP/item.txt"
out="$(docker_run docker "$TMP/item.txt" link --item schwab)"
has "a stopped link names itself with its login" "Then run: wealthdb-collect testcol link --item schwab" "$out"
out="$(docker_run docker "$TMP/item.txt" link)"
has "...and with the placeholder when it named none" "Then run: wealthdb-collect testcol link --item <name>" "$out"
out="$(docker_run docker "$TMP/item.txt" login)"
has "a verb with no then line stays bare" "Then run: wealthdb-collect testcol login" "$out"
printf "TESTCOL_USERNAME=u\nTESTCOL_PASSWORD='%s\n" "$SECRET_MARK" > "$SECRETS/testcol.env"
out="$(docker_run docker "$TMP/item.txt" link --item schwab)"
has "a file that is not valid: exit 78" "rc=78" "$out"
has "a file that is not valid names the stopped link with its login" \
    "Then run: wealthdb-collect testcol link --item schwab" "$out"
rm -f "$SECRETS/testcol.env"
out="$(WEALTHDB_SETUP_NEXT=1 docker_run docker "$DECL" login)"
has "a login the check stops: exit 78" "rc=78" "$out"
lacks "a login the check stops: no block" "What to do now" "$out"

echo "a setup of its own: the commands name its config:"
# The words the engine launcher hands the wrappers of `-c $OWN_CFG`.
OWN_CFG="$TMP/my try/wealthdb.cfg"
OWN="wealthdb -c '$OWN_CFG'"
# Exported in a subshell, as a wrapper finds them: the block a login ends
# with is printed from an exit trap, which a variable set for one call
# does not reach.
own() { (export WEALTHDB_CONFIG="$OWN_CFG" WEALTHDB_PROGRAM="$OWN" WEALTHDB_COLLECT="$OWN collect"; "$@"); }
cat > "$TMP/own.txt" <<'EOF'
var TESTCOL_USERNAME required plain <email> The email you sign in with
checks login link
provider On a new computer, sign in once by hand. Run
provider   wealthdb-collect testcol vnc-login --mode none
provider and wealthdb says the rest.
signin To test the key, run: wealthdb-collect testcol login --check
signin   wealthdb config banks
first Put the files in $XDG_DATA_HOME/wealthdb/testcol by default.
first   wealthdb-collect testcol link --item <name>
then wealthdb-collect testcol link --item <name>   # wealthdb-collect runs it once for each bank.
then wealthdb-collect testcol download [--item <name>] --lookback all
then wealthdb-collect testcol load
then wealthdb config banks   # Adds the bank to the sources wealthdb reads.
then wealthdb load -a
EOF
fresh_secrets
out="$(own docker_run docker "$TMP/own.txt" login)"
has "the stopped verb is named with the config" "Then run: $OWN collect testcol login" "$out"
lacks "...and never as the default setup's command" "Then run: wealthdb-collect" "$out"
has "a step of the collector carries it" "  1.  $OWN collect testcol link --item <name>" "$out"
has "...with its flags" "  2.  $OWN collect testcol download --lookback all" "$out"
has "an engine command carries it" "  4.  $OWN config banks" "$out"
has "...the load into the one database too" "  5.  $OWN load -a" "$out"
has "what a step does is still found by its verb" "Reads the saved files into this source's own database." "$out"
has "a note is text, left as it is" "wealthdb-collect runs it once for each bank." "$out"
has "...the engine command's too" "Adds the bank to the sources wealthdb reads." "$out"
has "a command indented in a text line carries it" "     $OWN collect testcol vnc-login --mode none" "$out"
has "a command inside a sentence carries it" "To test the key, run: $OWN collect testcol login --check" "$out"
has "an engine command indented in a text line carries it" "     $OWN config banks" "$out"
has "a sentence that names wealthdb stays a sentence" "   and wealthdb says the rest." "$out"
out="$(own docker_run docker "$TMP/own.txt" link --item schwab)"
has "a stopped link names itself with its login and the config" \
    "Then run: $OWN collect testcol link --item schwab" "$out"
has "the steps after it name the login" "  1.  $OWN collect testcol download --item schwab --lookback all" "$out"
mkdir -p "$SECRETS"
echo "$COMPLETE" > "$SECRETS/testcol.env"
out="$(WEALTHDB_SETUP_NEXT=1 own docker_run docker "$TMP/own.txt" login)"
has "a login that ends well names the next commands with the config" "  1.  $OWN collect testcol link --item <name>" "$out"
has "...to the last one" "  5.  $OWN load -a" "$out"
# The first step of a collector with nothing to sign in to.
first_lines() {
    (
        set -euo pipefail
        cd "$TMP" || exit 1
        HERE="$TMP/first-own"; NAME=testcol
        mkdir -p "$HERE" && cp "$TMP/own.txt" "$HERE/setup.txt"
        # shellcheck disable=SC1091
        source "$REPO/shared/wrappers/host-lib.sh"
        setup_print_first
    ) 2>&1
}
out="$(own first_lines)"
has "a first line names the setup's folder, not the default data root" \
    "testcol: Put the files in $TMP/my try/testcol by default." "$out"
has "...and its command carries the config" "testcol:   $OWN collect testcol link --item <name>" "$out"
out="$(first_lines)"
has "the default config: a first line as the declaration writes it" \
    'testcol: Put the files in $XDG_DATA_HOME/wealthdb/testcol by default.' "$out"
has "...its command too" "testcol:   wealthdb-collect testcol link --item <name>" "$out"
rm -f "$SECRETS/testcol.env"
out="$(docker_run docker "$TMP/own.txt" login)"
has "the default config: the stopped verb as the declaration writes it" "Then run: wealthdb-collect testcol login" "$out"
has "...an engine command too" "  5.  wealthdb load -a" "$out"
has "...and a text line" "To test the key, run: wealthdb-collect testcol login --check" "$out"
lacks "...and no config anywhere" " -c " "$out"
# A path with a quote and an ampersand reaches the text as it is given.
out="$(WEALTHDB_COLLECT="wealthdb -c '/x/it'\\''s & co/w.cfg' collect" docker_run docker "$TMP/own.txt" login)"
has "words with a quote and an & are printed as they are" \
    "To test the key, run: wealthdb -c '/x/it'\\''s & co/w.cfg' collect testcol login --check" "$out"

echo "the setup check, host family:"
fresh_secrets
out="$(docker_run host "$DECL" login)"
has "host, no folder: exit 78" "rc=78" "$out"
has "host, no folder: the folder is made" "mkdir -p $SECRETS" "$out"
lacks "host, no folder: the verb does not run" "host verb ran" "$out"
out="$(docker_run host "$TMP/decl3" login)"
has "host, no folder, only optional: a note" "A setting that helps" "$out"
has "host, no folder, only optional: it goes on" "host verb ran" "$out"
mkdir -p "$TMP/alt"
printf '%s\n' "$COMPLETE" > "$TMP/named.env"
out="$(TESTCOL_ENV_FILE="$TMP/named.env" docker_run host "$DECL" login)"
is "host: TESTCOL_ENV_FILE names the file" "host verb ran
rc=0" "$out"
out="$(docker_run host "$DECL" load)"
is "host: an unchecked verb" "host verb ran
rc=0" "$out"
out="$(docker_run host "$DECL" login --check)"
is "host: login --check" "host verb ran
rc=0" "$out"

lacks "no output ever holds a value from an env file" "$SECRET_MARK" "$(cat "$ALL")"
lacks "no output ever holds the username" "synthetic-user" "$(cat "$ALL")"

# ---- every collector's declaration ------------------------------------

echo
echo "the setup declarations:"

KEYWORDS=" var alias flag envfile keyfile session checks skip provider signin first then docs "

# validate NAME DECL: print one line per fault of the declaration.
validate() {
    local name="$1" decl="$2" kw rest n need kind shape label a b vars=" " f
    while IFS= read -r line || [[ -n "$line" ]]; do
        [[ -z "$line" || "$line" == \#* ]] && continue
        kw="${line%% *}"
        rest="${line#"$kw" }"
        [[ "$KEYWORDS" == *" $kw "* ]] || { echo "unknown keyword '$kw'"; continue; }
        case "$kw" in
            var)
                read -r n need kind shape label <<< "$rest"
                [[ "$n" =~ ^[A-Z][A-Z0-9_]*$ ]] || echo "bad name '$n'"
                [[ "$need" == required || "$need" == optional ]] || echo "$n: need '$need'"
                [[ "$kind" == secret || "$kind" == plain ]] || echo "$n: kind '$kind'"
                [[ -n "$shape" && "$shape" != *"'"* ]] || echo "$n: bad shape"
                [[ -n "$label" && ${#label} -le 60 ]] || echo "$n: no label, or one over 60 columns"
                vars+="$n "
                ;;
            alias|flag)
                read -r a b <<< "$rest"
                [[ "$vars" == *" $a "* ]] || echo "$kw of an undeclared '$a'"
                [[ -n "$b" ]] || echo "$kw $a: nothing given"
                ;;
            provider|signin|first)
                [[ ${#rest} -le 66 ]] || echo "$kw line over 66 columns"
                ;;
            then)
                [[ "$rest" == "wealthdb-collect $name "* || "$rest" == "wealthdb "* ]] ||
                    echo "then '$rest' is no wealthdb command"
                if [[ "$rest" == *"["* || "$rest" == *"]"* ]]; then
                    [[ "$rest" =~ ^[^][]*(\[[^][]*\<name\>[^][]*\][^][]*)+$ ]] ||
                        echo "then '$rest': a [ ] part holds <name>, once each"
                fi
                ;;
            docs)
                grep -qE "^#+ $rest\$" "$REPO/collectors/$name/README.md" ||
                    echo "docs: no section '$rest' in README.md"
                ;;
            envfile)
                for f in $rest; do [[ "$f" == *.env ]] || echo "envfile '$f'"; done
                ;;
        esac
    done < "$decl"
}

# wrapper_env_vars WRAPPER: the ENV_VARS the wrapper forwards, one per
# line, every assignment of it in the file (schwab-api sets it in a
# branch).
wrapper_env_vars() {
    awk '/ENV_VARS=\(/ {f=1} f {print} f && /\)/ {f=0}' "$1" |
        sed -e 's/.*ENV_VARS=(//' -e 's/[()\\]/ /g' | tr ' ' '\n' | grep -E '^[A-Z]'
}

# The forwarded names that are no sign-in credential, and why.
#   angellist: explore pre-fills its form with them; login and download
#              read none (README.md).
#   BINANCE_API_KEY, COINTRACKING_TIMEZONES: settings of load, from the
#              shell or cointracking.cfg, not cointracking.env.
NOT_CREDENTIALS=" ANGELLIST_USERNAME ANGELLIST_PASSWORD BINANCE_API_KEY COINTRACKING_TIMEZONES "

# A credential-shaped name in a collector's code.
CRED_RE='"[A-Z][A-Z0-9_]*_(USERNAME|EMAIL|LOGIN|LOGIN_ID|PASSWORD|SECRET|CLIENT_ID|CLIENT_SECRET|API_KEY|CONTRACT_NUMBER|REGION)"'

for d in "$REPO"/collectors/*/; do
    name="$(basename "$d")"
    w="$d$name"
    [[ -x "$w" ]] || continue
    decl="${d}setup.txt"
    if [[ ! -f "$decl" ]]; then
        bad "$name has no setup.txt"
        continue
    fi
    faults="$(validate "$name" "$decl")"
    is "$name: setup.txt is valid" "" "$faults"

    declared=" $(awk '$1 == "var" {print $2} $1 == "alias" {print $3}' "$decl" | tr '\n' ' ') "
    forwarded=" $(wrapper_env_vars "$w" | tr '\n' ' ') "
    if [[ "$forwarded" != "  " ]]; then
        for v in $declared; do
            [[ "$forwarded" == *" $v "* ]] || bad "$name: $v is declared but not in ENV_VARS"
        done
        for v in $forwarded; do
            [[ "$declared" == *" $v "* || "$NOT_CREDENTIALS" == *" $v "* ]] ||
                bad "$name: ENV_VARS forwards $v, which setup.txt does not declare"
        done
        ok "$name: declared and forwarded variables agree"
    elif [[ "$declared" != "  " ]]; then
        # A host wrapper: it sources each env file the declaration names.
        for f in $(awk '$1 == "envfile" {print $2}' "$decl"); do
            if [[ "$f" == "$name.env" ]]; then
                grep -qE 'host_source_env_file$' "$w" || bad "$name: $f is never sourced"
            else
                grep -qE "host_source_env_file ${f%.env}( |;|\$)" "$w" || bad "$name: $f is never sourced"
            fi
        done
        [[ -n "$(awk '$1 == "envfile"' "$decl")" ]] ||
            grep -qE 'host_source_env_file$' "$w" || bad "$name: $name.env is never sourced"
        ok "$name: the wrapper sources the env files it declares"
    fi

    code=" $(find "$d" -name '*.py' -not -path '*/tests/*' -not -name 'test_*' \
        -not -name conftest.py -not -path '*/.venv/*' -exec cat {} + |
        grep -oE "$CRED_RE" | tr -d '"' | sort -u | tr '\n' ' ') "
    missing=""
    for v in $code; do
        [[ "$declared" == *" $v "* || "$NOT_CREDENTIALS" == *" $v "* ]] || missing+=" $v"
    done
    is "$name: every credential its code reads is declared" "" "$missing"
done

# ---- every collector's login ------------------------------------------

echo
echo "login on each collector, from an empty secrets folder:"

# fleet_run NAME ROOT ARGS...: the collector's wrapper and setup.txt in a
# tree of their own (no .venv), with stub docker and python3. FLEET_NEXT=1
# asks for the block a login ends with, as a terminal does. FLEET_ENV
# holds more NAME=VALUE pairs for the wrapper's environment.
cat > "$TMP/bin/python3" <<'EOF'
#!/usr/bin/env bash
echo "python3 ran"
EOF
chmod +x "$TMP/bin/python3"
fleet_run() {
    local name="$1" root="$2"
    shift 2
    mkdir -p "$root/tree/collectors/$name" "$root/tree/shared/wrappers" "$root/home"
    cp -p "$REPO/collectors/$name/$name" "$REPO/collectors/$name/setup.txt" \
        "$root/tree/collectors/$name/"
    cp -p "$REPO/shared/wrappers/"*.sh "$root/tree/shared/wrappers/"
    env -i PATH="$TMP/bin:/usr/bin:/bin" HOME="$root/home" LC_ALL=C TMPDIR="$TMP" \
        XDG_DATA_HOME="$root/xd" XDG_CACHE_HOME="$root/xc" XDG_CONFIG_HOME="$root/xcfg" \
        STUB_LOG="$root/stub.log" WEALTHDB_SETUP_NEXT="${FLEET_NEXT:-}" \
        ${FLEET_ENV[@]+"${FLEET_ENV[@]}"} \
        "$root/tree/collectors/$name/$name" "$@" < /dev/null 2>&1
    echo "rc=$?"
}

for d in "$REPO"/collectors/*/; do
    name="$(basename "$d")"
    [[ -x "$d$name" && -f "${d}setup.txt" ]] || continue
    root="$TMP/fleet-$name"
    mkdir -p "$root/home/.secrets"
    out="$(fleet_run "$name" "$root" login)"
    required="$(awk '$1 == "var" && $3 == "required" {print $2}' "${d}setup.txt")"
    keys="$(awk '$1 == "keyfile" {print $2}' "${d}setup.txt")"
    if [[ -n "$required$keys" ]] && grep -qw login <<< "$(awk '$1 == "checks"' "${d}setup.txt")"; then
        has "$name: login stops with 78" "rc=78" "$out"
        for v in $required; do
            has "$name: login names $v" "$v='" "$out"
        done
        has "$name: login says to run it again" "Then run: wealthdb-collect $name login" "$out"
        lacks "$name: no container starts" "  run" "$(cat "$root/stub.log" 2>/dev/null)"
        lacks "$name: no script runs" "python3 ran" "$out"
    else
        has "$name: login goes on" "rc=0" "$out"
    fi
done

for name in plaid manual svb; do
    root="$TMP/first-$name"
    mkdir -p "$root/home/.secrets"
    [[ $name == plaid ]] &&
        printf "PLAID_CLIENT_ID='x'\nPLAID_SECRET='%s'\n" "$SECRET_MARK" > "$root/home/.secrets/plaid.env"
    out="$(fleet_run "$name" "$root" login)"
    has "$name: login exits 0" "rc=0" "$out"
    case $name in
        plaid)  has "plaid: login names link as the first step" \
                    "wealthdb-collect plaid link --item <name>" "$out" ;;
        manual) has "manual: login explains the CSVs" "positions.csv" "$out" ;;
        svb)    has "svb: login explains the statement folder" "bronze/" "$out" ;;
    esac
    lacks "$name: no value printed" "$SECRET_MARK" "$out"
done

# A collector with nothing to sign in to prints the `first` lines of its
# declaration, and its login ends with the declaration's `then` commands.
for name in manual svb curves; do
    root="$TMP/next-$name"
    mkdir -p "$root/home/.secrets"
    first="$(sed -n 's/^first //p' "$REPO/collectors/$name/setup.txt" | sed -n '1p')"
    last="$(sed -n 's/^first //p' "$REPO/collectors/$name/setup.txt" | sed -n '$p')"
    out="$(FLEET_NEXT=1 fleet_run "$name" "$root" login)"
    has "$name: login exits 0" "rc=0" "$out"
    has "$name: login prints the declaration's first step" "$name: $first" "$out"
    has "$name: ...to its last line" "$name: $last" "$out"
    has "$name: login ends with what to do now" "What to do now" "$out"
    has "$name: ...its own load" "wealthdb-collect $name load" "$out"
    has "$name: ...and the load into the one database" "wealthdb load -a" "$out"
    lacks "$name: no script runs" "python3 ran" "$out"
    out="$(fleet_run "$name" "$root" login)"
    lacks "$name: no terminal, no block" "What to do now" "$out"
    out="$(FLEET_NEXT=1 fleet_run "$name" "$root" login --check)"
    lacks "$name: a session probe ends with no block" "What to do now" "$out"
done
for name in manual svb; do
    out="$(FLEET_NEXT=1 fleet_run "$name" "$TMP/next-$name" download)"
    has "$name: download prints the first step too" \
        "$name: $(sed -n 's/^first //p' "$REPO/collectors/$name/setup.txt" | sed -n '1p')" "$out"
    lacks "$name: download ends with no block" "What to do now" "$out"
done

# A setup of its own: a wrapper's own text names the config in the
# command it tells a person to run, and the declaration's text names the
# setup's folder.
echo
echo "the wrappers' own texts, for a setup of its own:"
FLEET_CFG="$TMP/try/wealthdb.cfg"
FLEET_ENV=(WEALTHDB_CONFIG="$FLEET_CFG" WEALTHDB_PROGRAM="wealthdb -c $FLEET_CFG"
           WEALTHDB_COLLECT="wealthdb -c $FLEET_CFG collect")
for name in manual svb curves refdata fred ubs-psn; do
    root="$TMP/own-$name"
    mkdir -p "$root/home/.secrets"
    case $name in
        fred)    printf "FRED_API_KEY='%s'\n" "$SECRET_MARK" > "$root/home/.secrets/fred.env" ;;
        ubs-psn) printf "UBS_PSN_CLIENT_ID='x'\n" > "$root/home/.secrets/ubs-psn.env"
                 : > "$root/home/.secrets/ubs_psn_key" ;;
    esac
    verb=load
    case $name in manual|svb) ;; *) verb="login --check" ;; esac
    out="$(FLEET_NEXT=1 fleet_run "$name" "$root" login)"
    has "$name: login exits 0" "rc=0" "$out"
    has "$name: its text names the command with the config" "wealthdb -c $FLEET_CFG collect $name $verb" "$out"
    lacks "$name: ...and never the default setup's" "wealthdb-collect $name" "$out"
    lacks "$name: no value printed" "$SECRET_MARK" "$out"
done
out="$(FLEET_NEXT=1 fleet_run manual "$TMP/own-manual" login)"
has "manual: the data folder is the setup's" "the data folder, $TMP/try/manual by default." "$out"
has "manual: the last step loads into the setup's database" "wealthdb -c $FLEET_CFG load -a" "$out"
out="$(FLEET_NEXT=1 fleet_run svb "$TMP/own-svb" login)"
has "svb: the statement folder is the setup's" "folder of the data folder, $TMP/try/svb/bronze" "$out"
unset FLEET_ENV

echo
summary "setup check tests"
