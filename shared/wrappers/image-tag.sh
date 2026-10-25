# image-tag.sh — the tag of every image a checkout builds, runs and
# removes, and how a build decides to skip and waits its turn: the
# inputs hash and its label (wealthdb_inputs_hash,
# wealthdb_build_unchanged) and the build lock (wealthdb_build_lock).
# POSIX sh; sourced by the engine wrappers, the collector wrappers, mcp
# and web, and by the Makefile.
#
# WEALTHDB_IMAGE_TAG wins when it is set. Otherwise the first line of
# .image-tag at the repository root names the tag: the file is
# git-ignored, so a second checkout pins its own tag once and no shell
# in it can build or run another checkout's images. Without the file the
# tag is latest. A .image-tag that names no valid tag is an error, never
# a fall back to latest.

# wealthdb_image_tag ROOT: print the tag for the checkout at ROOT; exit
# status 2, with a message on stderr, when ROOT/.image-tag is malformed.
wealthdb_image_tag() {
    if [ -n "${WEALTHDB_IMAGE_TAG:-}" ]; then
        printf '%s\n' "$WEALTHDB_IMAGE_TAG"
        return 0
    fi
    _wit_file="$1/.image-tag"
    if [ ! -e "$_wit_file" ] && [ ! -L "$_wit_file" ]; then
        printf 'latest\n'
        return 0
    fi
    # Present but no readable file (a directory, a dangling link): an
    # error like a malformed tag, never latest.
    if [ ! -f "$_wit_file" ] || [ ! -r "$_wit_file" ]; then
        printf '%s is not a readable file\n' "$_wit_file" >&2
        return 2
    fi
    _wit_tag="$(sed -n '1{s/^[[:space:]]*//;s/[[:space:]]*$//;p;}' "$_wit_file")"
    case "$_wit_tag" in
        '' | [.-]* | *[!A-Za-z0-9_.-]*)
            printf '%s names no image tag (%s); write one tag such as feature-x on its first line\n' \
                "$_wit_file" "'$_wit_tag'" >&2
            return 2
            ;;
    esac
    if [ "${#_wit_tag}" -gt 128 ]; then
        printf '%s names a tag longer than 128 characters\n' "$_wit_file" >&2
        return 2
    fi
    printf '%s\n' "$_wit_tag"
}

# wealthdb_from_ref DOCKERFILE prints the image its first FROM names,
# with ${WEALTHDB_IMAGE_TAG} replaced, or "missing" when the file is
# not there.
wealthdb_from_ref() {
    if [ ! -f "$1" ]; then
        echo missing
        return 0
    fi
    _wfrom="$(sed -n 's/^FROM[[:space:]][[:space:]]*\([^[:space:]]*\).*/\1/p' "$1" | head -1)"
    printf '%s\n' "$_wfrom" | sed "s/\${WEALTHDB_IMAGE_TAG}/${WEALTHDB_IMAGE_TAG}/g"
}

# wealthdb_from_id DOCKERFILE prints the id of that image, or "missing"
# when it is not local.
wealthdb_from_id() {
    _wfrom="$(wealthdb_from_ref "$1")"
    if [ -z "$_wfrom" ] || [ "$_wfrom" = missing ]; then
        echo missing
        return 0
    fi
    docker image inspect -f '{{.Id}}' "$_wfrom" 2>/dev/null || echo missing
}

# wealthdb_export_image_tag ROOT: set WEALTHDB_IMAGE_TAG to the tag for
# the checkout at ROOT and export it, so a wrapper the caller starts
# reads the same tag; exit status 2 as wealthdb_image_tag's.
wealthdb_export_image_tag() {
    WEALTHDB_IMAGE_TAG="$(wealthdb_image_tag "$1")" || return 2
    export WEALTHDB_IMAGE_TAG
}

# wealthdb_inputs_hash PATH... [-- LITERAL...] prints a sha256 of those
# files and directories: each file's name, mode and bytes, and a
# symbolic link's target rather than what it points to. A name is
# relative to the PATH it was found under, kept with that PATH's last
# component only, so moving the checkout does not change the hash.
# Arguments after -- are text, never paths, so a version string is not
# read as a file in the working directory. A path that is not there is
# still part of the hash, so a missing input does not look like an
# empty one. A directory skips a virtualenv, bytecode and egg-info.
# shared/.dockerignore excludes the same names from the base images'
# context; keep the two lists in step.
wealthdb_inputs_hash() {
    python3 - "$@" <<'PY'
import hashlib, os, stat, sys
h = hashlib.sha256()
skip = {".venv", "__pycache__", ".pytest_cache", ".git", ".mypy_cache"}

def add_file(path, name, st):
    h.update(b"%s\0%o\0" % (name.encode(), st.st_mode))
    if stat.S_ISLNK(st.st_mode):
        h.update(os.readlink(path).encode())
        return
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)

def add(path):
    top = os.path.normpath(os.path.abspath(path))
    label = os.path.basename(top)
    if os.path.isfile(path):
        add_file(path, label, os.stat(path))
        return
    if not os.path.isdir(path):
        h.update(label.encode())
        h.update(b"\0missing")
        return
    for root, dirs, files in os.walk(path):
        dirs[:] = sorted(
            d for d in dirs if d not in skip and not d.endswith(".egg-info"))
        # os.walk lists a link to a directory with the directories and
        # does not enter it; it is hashed as a link.
        links = [d for d in dirs if os.path.islink(os.path.join(root, d))]
        for name in sorted(files + links):
            if name.endswith((".pyc", ".egg-link")):
                continue
            full = os.path.join(root, name)
            rel = os.path.relpath(full, path)
            add_file(full, label + "/" + rel, os.lstat(full))

args = sys.argv[1:]
if "--" in args:
    cut = args.index("--")
    paths, literals = args[:cut], args[cut + 1:]
else:
    paths, literals = args, []
for p in paths:
    add(p)
for lit in literals:
    h.update(b"\0lit\0")
    h.update(lit.encode())
print(h.hexdigest())
PY
}

# wealthdb_labeled IMAGE HASH: true when IMAGE was built from HASH. An
# empty HASH matches nothing, so a missing or unlabeled image is never
# taken as built from it.
wealthdb_labeled() {
    _wlab="$(docker image inspect -f '{{index .Config.Labels "wealthdb.inputs"}}' "$1" 2>/dev/null || true)"
    [ -n "$2" ] && [ "$_wlab" = "$2" ]
}

# wealthdb_build_unchanged IMAGE HASH ARGC: true, printing "build: IMAGE
# unchanged", when a build of IMAGE can be skipped: ARGC (the count of
# extra build arguments) is 0, HASH is not empty and IMAGE was built
# from HASH. Any extra argument, such as --no-cache, always builds.
wealthdb_build_unchanged() {
    [ "$3" -eq 0 ] && wealthdb_labeled "$1" "$2" || return 1
    printf 'build: %s unchanged\n' "$1"
}

# wealthdb_build_lock PATH: wait for the lock file PATH, then hold it on
# file descriptor 9 until this shell exits. Two builds of one image
# never run at once. Every command the shell starts inherits the
# descriptor, so the lock is free once the holder and the commands it
# started have exited, however they end: an interrupted or killed build
# leaves no lock behind, and a command that outlives its build (none of
# the builds starts one) would keep it. The file names the PID of the
# shell that holds it, and a wait past three seconds prints the path
# and that PID on stderr. A lock file removed while held (make clean)
# is opened again, so a second build never takes a file the first
# still holds.
wealthdb_build_lock() {
    mkdir -p "$(dirname "$1")" || return 1
    while :; do
        exec 9>>"$1"
        python3 - "$1" <<'PY'
import fcntl, os, sys, time
path = sys.argv[1]
start, said = time.monotonic(), False
while True:
    try:
        fcntl.flock(9, fcntl.LOCK_EX | fcntl.LOCK_NB)
        break
    except BlockingIOError:
        if not said and time.monotonic() - start >= 3:
            try:
                with open(path) as fh:
                    holder = fh.read().strip()
            except OSError:
                holder = ""
            print(f"build: waiting for {path}" + (f", held by pid {holder}" if holder else ""),
                  file=sys.stderr, flush=True)
            said = True
        time.sleep(0.2)
# A file removed while it was held is not the lock any more.
try:
    same = os.path.samestat(os.fstat(9), os.stat(path))
except FileNotFoundError:
    same = False
if not same:
    sys.exit(75)
os.ftruncate(9, 0)
os.write(9, f"{os.getppid()}\n".encode())
PY
        case $? in
            0) return 0 ;;
            75) mkdir -p "$(dirname "$1")" || return 1 ;;
            *) return 1 ;;
        esac
    done
}

# wealthdb_clean_build_locks DIR: remove the lock files under DIR that no
# build holds, and DIR once it is empty. A held lock stays, so a build
# that starts during a clean still waits for the one that holds it.
wealthdb_clean_build_locks() {
    [ -d "$1" ] || return 0
    python3 - "$1" <<'PY'
import fcntl, os, sys
top = sys.argv[1]
for name in os.listdir(top):
    path = os.path.join(top, name)
    if not os.path.isfile(path):
        continue
    with open(path, "a") as fh:
        try:
            fcntl.flock(fh, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            print(f"build: {path} is held, kept", file=sys.stderr)
            continue
        os.unlink(path)
try:
    os.rmdir(top)
except OSError:
    pass
PY
}
