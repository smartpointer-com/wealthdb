# image-tag.sh — the tag of every image a checkout builds, runs and
# removes. POSIX sh; sourced by the engine wrappers, the collector
# wrappers, mcp and web, and read by the Makefile.
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

# wealthdb_export_image_tag ROOT: set WEALTHDB_IMAGE_TAG to the tag for
# the checkout at ROOT and export it, so a wrapper the caller starts
# reads the same tag; exit status 2 as wealthdb_image_tag's.
wealthdb_export_image_tag() {
    WEALTHDB_IMAGE_TAG="$(wealthdb_image_tag "$1")" || return 2
    export WEALTHDB_IMAGE_TAG
}
