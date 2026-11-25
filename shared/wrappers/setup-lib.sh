# shellcheck shell=bash
# The setup check: before a verb that signs in, a wrapper reads the
# collector's setup declaration (collectors/<name>/setup.txt) and tests
# that every credential it needs is in place. A missing one prints
# step-by-step instructions and stops the verb with SETUP_EXIT. A
# complete setup prints nothing. Sourced by wrapper-lib.sh and
# host-lib.sh, after image-tag.sh (wealthdb_abs_path) and paths-lib.sh
# (collector_env_prefix). The declaration format is in
# collectors/README.md, "The setup declaration".
#
# Caller must SET (before calling):
#   NAME         short collector name, e.g. "viac".
#   HERE         the wrapper's own directory, which holds setup.txt.
#
# The check never blocks when in doubt. It is skipped when the collector
# has no setup.txt, for a verb the declaration does not check, and when
# an argument asks for help, a session probe (--check), an explicit
# --env-file, or a flag of the declaration's `skip` line. It does not
# block when a stored session of a `session` line exists.
#
# An env file is read only the way AGENTS.md §3 allows: `bash -n`, then
# sourced in a subshell, which reports the names it set and never a
# value.
#
# A declaration writes each command for the default setup:
# `wealthdb-collect <name> <verb>` and `wealthdb <command>`. A command is
# printed with the words the engine launcher hands a setup of its own in
# place of its first word: WEALTHDB_COLLECT (`wealthdb -c CFG collect`)
# and WEALTHDB_PROGRAM (`wealthdb -c CFG`). With neither set, it is
# printed as it is written. A text line that names the data root as
# $XDG_DATA_HOME/wealthdb names the folder of a setup of its own instead
# (_setup_text).
#
# bash 3.2 compatibility: no namerefs or associative arrays.

# The exit status of a verb the setup check stops: EX_CONFIG of
# sysexits.h. No collector uses it for anything else.
SETUP_EXIT=78

# The flags that turn the check off for one run, on every collector.
_SETUP_FLEET_SKIP="--help -h --check --env-file"

# setup_read FILE — parse the declaration FILE into the _S_* globals.
# A line is a keyword, one space and its value. Blank lines and lines
# whose first character is `#` are skipped, and so is a keyword this
# version does not know.
setup_read() {
    _S_VARS=() _S_NEED=() _S_SHAPE=() _S_LABEL=()
    _S_ALIAS=() _S_FLAG=() _S_FILES=() _S_KEYS=() _S_KEYLABEL=()
    _S_SESSIONS=() _S_PROVIDER=() _S_SIGNIN=() _S_FIRST=() _S_THEN=()
    _S_CHECKS="" _S_SKIP="" _S_DOCS=""
    local line kw rest a b c d e
    while IFS= read -r line || [[ -n "$line" ]]; do
        line="${line#"${line%%[![:space:]]*}"}"
        [[ -z "$line" || "$line" == \#* ]] && continue
        kw="${line%%[[:space:]]*}"
        # One space parts the keyword from its value. More spaces stay,
        # so a text line can indent a command.
        rest="${line#"$kw"}"
        rest="${rest# }"
        case "$kw" in
            var)
                # The third field, the variable's kind, is for a reader of
                # the declaration alone.
                read -r a b c d e <<< "$rest"
                _S_VARS+=("$a") _S_NEED+=("$b") _S_SHAPE+=("$d") _S_LABEL+=("$e")
                ;;
            alias)    _S_ALIAS+=("$rest") ;;
            flag)     _S_FLAG+=("$rest") ;;
            envfile)  _S_FILES+=("$rest") ;;
            keyfile)
                read -r a b <<< "$rest"
                _S_KEYS+=("$a") _S_KEYLABEL+=("$b")
                ;;
            session)  _S_SESSIONS+=("$rest") ;;
            checks)   _S_CHECKS="$rest" ;;
            skip)     _S_SKIP="$rest" ;;
            provider) _S_PROVIDER+=("$rest") ;;
            signin)   _S_SIGNIN+=("$rest") ;;
            first)    _S_FIRST+=("$rest") ;;
            then)     _S_THEN+=("$rest") ;;
            docs)     _S_DOCS="$rest" ;;
        esac
    done < "$1"
    # A declaration with variables and no envfile line reads <name>.env.
    if [[ ${#_S_VARS[@]} -gt 0 && ${#_S_FILES[@]} -eq 0 ]]; then
        _S_FILES=("$NAME.env")
    fi
}

# _setup_has WORD LIST... — is WORD one of LIST?
_setup_has() {
    local w="$1" x
    shift
    for x; do
        [[ "$x" == "$w" ]] && return 0
    done
    return 1
}

# _setup_skipped ARGS... — does an argument turn the check off? A flag
# matches in both its `--flag VALUE` and `--flag=VALUE` forms.
_setup_skipped() {
    local a f
    for a; do
        for f in $_SETUP_FLEET_SKIP $_S_SKIP; do
            [[ "$a" == "$f" || "$a" == "$f="* ]] && return 0
        done
    done
    return 1
}

# _setup_q PATH — PATH as one shell word: as it is when it holds only
# safe characters, else in single quotes.
_setup_q() {
    if [[ "$1" =~ ^[A-Za-z0-9_./+@%=:,-]+$ ]]; then
        printf '%s' "$1"
    else
        printf "'%s'" "${1//\'/\'\\\'\'}"
    fi
}

# _setup_resolve_files SECRETS OVERRIDE — set, for each envfile line i,
# _S_PATH[i] (the file the collector reads, or "" when none exists) and
# _S_TARGET[i] (where to create it). A line names the file and its
# fallbacks: the first that exists is read. With OVERRIDE=1, a set
# <FILE>_ENV_FILE (FILE in upper snake case, e.g. VIAC_ENV_FILE) names
# the file in place of the line, as host_source_env_file reads it.
_setup_resolve_files() {
    local secrets="$1" override="$2" i n first ovar
    _S_PATH=() _S_TARGET=()
    for ((i = 0; i < ${#_S_FILES[@]}; i++)); do
        # shellcheck disable=SC2086
        set -- ${_S_FILES[$i]}
        first="$1"
        ovar="$(collector_env_prefix "${first%.env}")_ENV_FILE"
        if [[ "$override" == 1 && -n "${!ovar:-}" ]]; then
            _S_TARGET[i]="$(wealthdb_abs_path "${!ovar}")"
            _S_PATH[i]=""
            [[ -f "${!ovar}" ]] && _S_PATH[i]="${!ovar}"
            continue
        fi
        _S_TARGET[i]="$secrets/$first"
        _S_PATH[i]=""
        for n; do
            if [[ -f "$secrets/$n" ]]; then
                _S_PATH[i]="$secrets/$n"
                break
            fi
        done
    done
}

# _setup_names_in FILE NAME SHAPE... — once FILE is sourced, print for
# each NAME that is set and not empty "set NAME", or "placeholder NAME"
# when its value is still exactly SHAPE, the declared placeholder. The
# file is sourced in a subshell with its output dropped, and the value
# is compared there, so only names leave it, never a value. Returns 1,
# printing nothing, when `bash -n` rejects the file.
_setup_names_in() {
    local f="$1"
    shift
    bash -n "$f" 2>/dev/null || return 1
    (
        set +eu +o pipefail
        # shellcheck disable=SC1090
        . "$f" >/dev/null 2>&1 < /dev/null
        while [[ $# -ge 2 ]]; do
            if [[ -n "${!1:-}" ]]; then
                if [[ "${!1}" == "$2" ]]; then
                    printf 'placeholder %s\n' "$1"
                else
                    printf 'set %s\n' "$1"
                fi
            fi
            shift 2
        done
    )
    return 0
}

# _setup_all_names — every declared variable and alias, each followed by
# the shape of its variable, one word per line.
_setup_all_names() {
    local i l n other
    for ((i = 0; i < ${#_S_VARS[@]}; i++)); do
        printf '%s\n%s\n' "${_S_VARS[$i]}" "${_S_SHAPE[$i]}"
    done
    for l in ${_S_ALIAS[@]+"${_S_ALIAS[@]}"}; do
        read -r n other <<< "$l"
        printf '%s\n%s\n' "$other" "$(_setup_shape_of "$n")"
    done
}

# _setup_shape_of VAR — the declared shape of the variable VAR.
_setup_shape_of() {
    local i
    for ((i = 0; i < ${#_S_VARS[@]}; i++)); do
        [[ "${_S_VARS[$i]}" == "$1" ]] && { printf '%s' "${_S_SHAPE[$i]}"; return 0; }
    done
}

# _setup_present VAR SHAPE ARGS... — is VAR given: set in the
# environment or a file read (_S_FOUND), under its name or an alias, or
# by its flag among ARGS? A value that is still exactly SHAPE, the
# placeholder of the template, does not count: then _SETUP_PLACEHOLDER
# is 1. The value is compared, never printed.
_setup_present() {
    local v="$1" shape="$2" l n other a
    shift 2
    _SETUP_PLACEHOLDER=0
    local -a names=("$v")
    for l in ${_S_ALIAS[@]+"${_S_ALIAS[@]}"}; do
        read -r n other <<< "$l"
        [[ "$n" == "$v" ]] && names+=("$other")
    done
    for n in "${names[@]}"; do
        if [[ -n "${!n:-}" ]]; then
            [[ "${!n}" != "$shape" ]] && return 0
            _SETUP_PLACEHOLDER=1
        fi
        _setup_has "set:$n" ${_S_FOUND[@]+"${_S_FOUND[@]}"} && return 0
        _setup_has "placeholder:$n" ${_S_FOUND[@]+"${_S_FOUND[@]}"} &&
            _SETUP_PLACEHOLDER=1
    done
    for l in ${_S_FLAG[@]+"${_S_FLAG[@]}"}; do
        read -r n other <<< "$l"
        [[ "$n" == "$v" ]] || continue
        for a; do
            [[ "$a" == "$other" || "$a" == "$other="* ]] && return 0
        done
    done
    return 1
}

# setup_check MODE SECRETS ARGS... — the check before the verb ARGS[0].
# MODE is `docker` (the secrets folder is a mount, so a missing one
# stops the verb, and <FILE>_ENV_FILE counts only under HAS_ENV_FILE=1)
# or `host`. SECRETS is the resolved secrets folder, made absolute here
# so the instructions name it from any folder. Returns 0 to go on;
# exits SETUP_EXIT after printing what to do.
setup_check() {
    local mode="$1" secrets
    secrets="$(wealthdb_abs_path "$2")"
    shift 2
    local decl="$HERE/setup.txt" verb="${1:-}"
    [[ -f "$decl" ]] || return 0
    setup_read "$decl"
    _setup_item_of "$@"
    # shellcheck disable=SC2086
    _setup_has "$verb" $_S_CHECKS || return 0
    _setup_skipped "$@" && return 0
    local s
    for s in ${_S_SESSIONS[@]+"${_S_SESSIONS[@]}"}; do
        [[ -e "$secrets/$s" ]] && return 0
    done

    local override=1
    [[ "$mode" == docker && "${HAS_ENV_FILE:-0}" != 1 ]] && override=0
    _setup_resolve_files "$secrets" "$override"
    local i found any_file=0
    _S_FOUND=()
    for ((i = 0; i < ${#_S_FILES[@]}; i++)); do
        [[ -n "${_S_PATH[$i]}" ]] || continue
        any_file=1
        # shellcheck disable=SC2046
        if ! found="$(_setup_names_in "${_S_PATH[$i]}" $(_setup_all_names))"; then
            _setup_print_invalid "${_S_PATH[$i]}" "$verb"
            exit "$SETUP_EXIT"
        fi
        # "set NAME" lines become set:NAME words.
        # shellcheck disable=SC2206
        [[ -n "$found" ]] && _S_FOUND+=(${found// /:})
    done

    local block=0 missing_opt=0 v k
    _S_MISSING=() _S_PLACEHOLDERS=()
    for ((i = 0; i < ${#_S_VARS[@]}; i++)); do
        v="${_S_VARS[$i]}"
        _setup_present "$v" "${_S_SHAPE[$i]}" "$@" && continue
        # A placeholder copied from the template would reach the
        # provider's sign-in, so it stops the verb even when optional.
        if [[ $_SETUP_PLACEHOLDER == 1 ]]; then
            _S_PLACEHOLDERS+=("$i")
            block=1
            continue
        fi
        _S_MISSING+=("$i")
        if [[ "${_S_NEED[$i]}" == required ]]; then
            block=1
        else
            missing_opt=1
        fi
    done
    _S_MISSING_KEYS=()
    for ((i = 0; i < ${#_S_KEYS[@]}; i++)); do
        k="${_S_KEYS[$i]}"
        [[ -f "$secrets/$k" ]] && continue
        _S_MISSING_KEYS+=("$i")
        block=1
    done
    [[ "$mode" == docker && ! -d "$secrets" ]] && block=1

    if [[ $block == 1 ]]; then
        setup_print_instructions "$secrets" "$verb"
        exit "$SETUP_EXIT"
    fi
    if [[ $any_file == 0 && $missing_opt == 1 ]]; then
        _setup_print_note
    fi
    return 0
}

# _setup_print_invalid FILE VERB — the env file is not valid shell.
# bash's own message is never shown: it quotes the line, which may
# hold a secret.
_setup_print_invalid() {
    cat >&2 <<EOF
$NAME: the file $(_setup_q "$1") is not valid.
A line in it does not follow the shell's rules. Often a quote is not
closed, or a value with a space or a \$ is not in single quotes.
Open the file in a text editor and check each line:

  NAME='value'

Keep each value in single quotes. Write a single quote inside a value
as '\''.

Then run: $(_setup_verb_command "$2")
EOF
}

# _setup_print_note — the env file is missing and every variable it
# would hold is optional: say what it would add. The verb goes on.
_setup_print_note() {
    local i
    {
        echo "$NAME: $(_setup_q "${_S_TARGET[0]}") does not exist. It can hold:"
        for i in "${_S_MISSING[@]}"; do
            printf '  %s  %s\n' "${_S_VARS[$i]}" "${_S_LABEL[$i]}"
        done
        echo "Going on without it."
    } >&2
}

# setup_print_instructions SECRETS VERB — what to put where, for a
# person with no software experience: what to do at the provider first,
# the folder and the file with the commands that make them, the lines to
# put in the file, the key files, how the sign-in runs, and the commands
# to run next. Reads the _S_* globals of setup_check.
setup_print_instructions() {
    local secrets="$1" verb="$2" step=0 i l f qs qf
    qs="$(_setup_q "$secrets")"
    f="${_S_TARGET[0]:-}"
    qf="$(_setup_q "$f")"
    _setup_colors
    {
        echo "$NAME: some setup is needed first."
        if [[ ${#_S_PROVIDER[@]} -gt 0 ]]; then
            step=$((step + 1))
            echo
            echo "$(_setup_paint 1 "$step. Do this first:")"
            for l in "${_S_PROVIDER[@]}"; do
                _setup_text "$l"
                echo "   $_S_LINE"
            done
            if [[ -n "$_S_DOCS" ]]; then
                echo "   More: $(_setup_q "$HERE/README.md"), section \"$_S_DOCS\"."
            fi
        fi
        if [[ ! -d "$secrets" || ( -n "$f" && ! -f "$f" ) ]]; then
            step=$((step + 1))
            echo
            if [[ -n "$f" ]]; then
                echo "$(_setup_paint 1 "$step. Make the secrets folder and the file, readable by you alone:")"
            else
                echo "$(_setup_paint 1 "$step. Make the secrets folder, readable by you alone:")"
            fi
            echo
            echo "     $(_setup_paint 36 "mkdir -p $qs")"
            echo "     $(_setup_paint 36 "chmod 700 $qs")"
            if [[ -n "$f" ]]; then
                [[ "$(dirname "$f")" != "$secrets" ]] &&
                    echo "     $(_setup_paint 36 "mkdir -p $(_setup_q "$(dirname "$f")")")"
                echo "     $(_setup_paint 36 "touch $qf")"
                echo "     $(_setup_paint 36 "chmod 600 $qf")"
            fi
        fi
        if [[ ${#_S_MISSING[@]} -gt 0 || ${#_S_PLACEHOLDERS[@]} -gt 0 ]]; then
            step=$((step + 1))
            echo
            echo "$(_setup_paint 1 "$step. Open the file in a text editor. On a Mac:")"
            echo
            echo "     $(_setup_paint 36 "open -e $qf")"
            if [[ ${#_S_MISSING[@]} -gt 0 ]]; then
                echo
                echo "   Add these lines. Put your own value in place of each"
                echo "   placeholder, between the single quotes:"
                echo
                _setup_print_lines "${_S_MISSING[@]}"
            fi
            if [[ ${#_S_PLACEHOLDERS[@]} -gt 0 ]]; then
                echo
                echo "   These lines still hold the placeholder. Put your own"
                echo "   value in its place, between the single quotes:"
                echo
                _setup_print_lines "${_S_PLACEHOLDERS[@]}"
            fi
            echo
            echo "   Keep the single quotes. Inside them, a \$ or a space is"
            echo "   fine. Write a single quote inside a value as '\\'' :"
            echo "   it's becomes 'it'\\''s'."
        fi
        if [[ ${#_S_MISSING_KEYS[@]} -gt 0 ]]; then
            step=$((step + 1))
            echo
            echo "$(_setup_paint 1 "$step. Put these files in the secrets folder:")"
            echo
            for i in "${_S_MISSING_KEYS[@]}"; do
                echo "     $(_setup_q "$secrets/${_S_KEYS[$i]}")  ${_S_KEYLABEL[$i]}"
            done
        fi
        if [[ ${#_S_SIGNIN[@]} -gt 0 ]]; then
            echo
            echo "How the sign-in runs:"
            for l in "${_S_SIGNIN[@]}"; do
                _setup_text "$l"
                echo "   $_S_LINE"
            done
        fi
        echo
        echo "Then run: $(_setup_paint 36 "$(_setup_verb_command "$verb")")"
        _setup_print_then "$verb"
    } >&2
}

# _setup_colors — set _S_COLOR to 1 when what goes to stderr is
# coloured, as `wealthdb config` colours: a terminal, with no NO_COLOR
# and a TERM that is not dumb. WEALTHDB_SETUP_COLOR=1 or 0 decides it.
_setup_colors() {
    _S_COLOR=0
    if [[ -t 2 && -z "${NO_COLOR+x}" && "${TERM:-dumb}" != dumb ]]; then
        _S_COLOR=1
    fi
    case "${WEALTHDB_SETUP_COLOR:-}" in 0|1) _S_COLOR="$WEALTHDB_SETUP_COLOR" ;; esac
    return 0
}

# _setup_paint CODE TEXT — print TEXT, in the SGR colour CODE when
# _S_COLOR is 1: 1 a title, 2 help, 36 a command to run.
_setup_paint() {
    if [[ "${_S_COLOR:-0}" == 1 ]]; then
        printf '\033[%sm%s\033[0m' "$1" "$2"
    else
        printf '%s' "$2"
    fi
}

# _setup_print_lines INDEX... — the template line of each variable
# INDEX, under its label.
_setup_print_lines() {
    local i l
    for i; do
        l="${_S_LABEL[$i]}."
        [[ "${_S_NEED[$i]}" == optional ]] && l="$l Optional."
        echo "     $(_setup_paint 2 "# $l")"
        echo "     $(_setup_paint 36 "${_S_VARS[$i]}='${_S_SHAPE[$i]}'")"
    done
}

# _setup_then_after VERB — set _S_REST to the `then` commands, in order.
# When VERB's own command is among them, only the ones after it.
_setup_then_after() {
    _S_REST=()
    local l
    for l in ${_S_THEN[@]+"${_S_THEN[@]}"}; do
        if [[ "$l" == "wealthdb-collect $NAME $1" || "$l" == "wealthdb-collect $NAME $1 "* ]]; then
            _S_REST=()
            continue
        fi
        _S_REST+=("$l")
    done
}

# _setup_item_of ARGS... — set _SETUP_ITEM to the value of an --item
# flag among ARGS: the login a `then` line's <name> stands for.
_setup_item_of() {
    _SETUP_ITEM=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --item)   _SETUP_ITEM="${2:-}"; shift ;;
            --item=*) _SETUP_ITEM="${1#*=}" ;;
        esac
        shift
    done
    return 0
}

# _setup_verb_command VERB — print the command that runs VERB: as its
# `then` line writes it, with the login's name in place of <name> when
# the run named one, else the bare verb.
_setup_verb_command() {
    local l cmd="wealthdb-collect $NAME $1"
    for l in ${_S_THEN[@]+"${_S_THEN[@]}"}; do
        if [[ "$l" == "wealthdb-collect $NAME $1 "* ]]; then
            _setup_fill "${l%%   # *}"
            cmd="$_S_LINE"
            [[ -n "${_SETUP_ITEM:-}" ]] && cmd="${cmd//<name>/$_SETUP_ITEM}"
            break
        fi
    done
    _setup_command "$cmd"
    printf '%s' "$_S_LINE"
}

# _setup_command CMD — set _S_LINE to the command CMD of a declaration
# as it is printed. Its first word gives way to the words of this setup:
# `wealthdb-collect` to WEALTHDB_COLLECT, and `wealthdb` to
# WEALTHDB_PROGRAM. Unset, the word stays.
_setup_command() {
    case "$1" in
        "wealthdb-collect "*) _S_LINE="${WEALTHDB_COLLECT:-wealthdb-collect} ${1#wealthdb-collect }" ;;
        "wealthdb "*)         _S_LINE="${WEALTHDB_PROGRAM:-wealthdb} ${1#wealthdb }" ;;
        *)                    _S_LINE="$1" ;;
    esac
}

# _setup_text LINE — set _S_LINE to the text LINE of a declaration as it
# is printed. An indented line that is one command is printed as a
# command (_setup_command). In any other line, each `wealthdb-collect`
# before a source gives way to WEALTHDB_COLLECT, when that is set. For a
# setup of its own, `$XDG_DATA_HOME/wealthdb`, the default data root as
# a text names it, gives way to the setup's folder. The pieces are
# joined by hand: bash versions differ in how ${line//a/b} reads a
# quote or an & in b.
_setup_text() {
    local line="$1" lead out=""
    lead="${line%%[! ]*}"
    if [[ -n "$lead" && ( "${line#"$lead"}" == "wealthdb "* || "${line#"$lead"}" == "wealthdb-collect "* ) ]]; then
        _setup_command "${line#"$lead"}"
        _S_LINE="$lead$_S_LINE"
        return 0
    fi
    if [[ -n "${WEALTHDB_COLLECT:-}" ]]; then
        while [[ "$line" == *"wealthdb-collect "* ]]; do
            out="$out${line%%wealthdb-collect *}$WEALTHDB_COLLECT "
            line="${line#*wealthdb-collect }"
        done
        line="$out$line"
    fi
    if [[ "$line" == *'$XDG_DATA_HOME/wealthdb'* ]]; then
        _setup_own_root
        if [[ -n "$_S_OWN_ROOT" ]]; then
            line="${line%%\$XDG_DATA_HOME/wealthdb*}${_S_OWN_ROOT%/}${line#*\$XDG_DATA_HOME/wealthdb}"
        fi
    fi
    _S_LINE="$line"
}

# _setup_own_root — set _S_OWN_ROOT to the data root of a setup of its
# own, the config's folder, or to nothing for the default config
# (wealthdb_engine_paths). Resolved once, in a subshell, which keeps the
# rule's variables out of the wrapper.
_setup_own_root() {
    [[ -z "${_S_OWN_ROOT+x}" ]] || return 0
    _S_OWN_ROOT="$(
        wealthdb_engine_paths
        if [[ "$OWN_SETUP" == 1 ]]; then printf '%s' "$DATA_ROOT"; fi
    )"
}

# _setup_fill LINE — set _S_LINE to LINE with each optional part, the
# text between [ and ], filled in or left out: with the <name> it holds
# known (_SETUP_ITEM), the part is kept with the name in place of
# <name>; else it goes, with the space before it.
_setup_fill() {
    local line="$1" pre rest part out=""
    while [[ "$line" == *"["*"]"* ]]; do
        pre="${line%%\[*}"
        rest="${line#*\[}"
        part="${rest%%\]*}"
        line="${rest#*\]}"
        if [[ -n "${_SETUP_ITEM:-}" && "$part" == *"<name>"* ]]; then
            out="$out$pre${part//<name>/$_SETUP_ITEM}"
        else
            out="$out${pre% }"
        fi
    done
    _S_LINE="$out$line"
}

# _setup_step LINE — split a `then` line into _S_CMD, the command, and
# _S_DOES, the lines that say what it does, so a reader learns what each
# step is for. The lines come from the command's verb. A note after
# "   # " on the line is one more line.
_setup_step() {
    _setup_fill "$1"
    local line="$_S_LINE" note=""
    if [[ "$line" == *"   # "* ]]; then
        note="${line#*   # }"
        line="${line%%   # *}"
    fi
    while [[ "$line" == *" " ]]; do line="${line% }"; done
    _setup_command "$line"
    _S_CMD="$_S_LINE"
    _S_DOES=()
    case "$line" in
        "wealthdb load"*)
            _S_DOES=("Brings the database of every source into the one database that"
                     "the reports read.") ;;
        "wealthdb-collect $NAME download"*)
            _S_DOES=("Fetches the data from the provider and saves the raw files on"
                     "this computer. It changes nothing at the provider.") ;;
        "wealthdb-collect $NAME load"*)
            _S_DOES=("Reads the saved files into this source's own database.") ;;
        "wealthdb-collect $NAME link"*)
            _S_DOES=("Links one bank: it prints the address of a sign-in page to open"
                     "in a browser.") ;;
        "wealthdb-collect $NAME login"*)
            _S_DOES=("Signs in and keeps the session for the downloads.") ;;
    esac
    if [[ "$line" == *"--lookback all"* ]]; then
        _S_DOES+=("--lookback all asks for the whole history. A later download"
                  "needs no flag: it fetches what is new.")
    fi
    [[ -n "$note" ]] && _S_DOES+=("$note")
    return 0
}

# _setup_print_steps — the commands of _S_REST, numbered, each on a line
# of its own with what it does below it.
_setup_print_steps() {
    local i l
    for ((i = 0; i < ${#_S_REST[@]}; i++)); do
        _setup_step "${_S_REST[$i]}"
        echo
        echo "  $((i + 1)).  $(_setup_paint 36 "$_S_CMD")"
        [[ ${#_S_DOES[@]} -gt 0 ]] && echo
        for l in ${_S_DOES[@]+"${_S_DOES[@]}"}; do
            echo "      $(_setup_paint 2 "$l")"
        done
    done
}

# _setup_print_then VERB — the commands that follow VERB.
_setup_print_then() {
    _setup_then_after "$1"
    [[ ${#_S_REST[@]} -gt 0 ]] || return 0
    echo
    echo "After that, in this order:"
    _setup_print_steps
}

# setup_arm_next VERB ARGS... — after a `login` that ends well, or the
# `link` of a collector that links one login at a time, print the
# commands that follow it (setup_print_next), so the first command a
# setup names always says what comes next. Only for a person: when
# stderr is a terminal, so a scheduled run's output stays as it is.
# WEALTHDB_SETUP_NEXT=1 prints it without a terminal, 0 never. Not for
# a help flag or a session probe (--check).
setup_arm_next() {
    [[ ( "${1:-}" == login || "${1:-}" == link ) && -f "$HERE/setup.txt" ]] || return 0
    local a
    for a; do
        case "$a" in --help|-h|--check) return 0 ;; esac
    done
    case "${WEALTHDB_SETUP_NEXT:-}" in
        0) return 0 ;;
        1) ;;
        *) [[ -t 2 ]] || return 0 ;;
    esac
    _SETUP_NEXT=1 _SETUP_NEXT_VERB="$1"
    _setup_item_of "$@"
    # Decided now: the block prints from the exit trap, where the run's
    # own environment may be gone.
    _setup_colors
    trap 'setup_print_next $?' EXIT
}

# setup_exec CMD... — run the verb's command: in place of the shell,
# or as a child when the exit trap of setup_arm_next has to run after
# it.
setup_exec() {
    if [[ "${_SETUP_NEXT:-0}" == 1 ]]; then
        "$@"
        exit $?
    fi
    exec "$@"
}

# setup_print_next STATUS — the block a `login` that ended with STATUS 0
# closes with: the commands that follow it, numbered, between two rules.
setup_print_next() {
    [[ "$1" == 0 ]] || return 0
    setup_read "$HERE/setup.txt"
    _setup_then_after "${_SETUP_NEXT_VERB:-login}"
    [[ ${#_S_REST[@]} -gt 0 ]] || return 0
    local rule="------------------------------------------------------------------------"
    rule="$(_setup_paint 2 "$rule")"
    {
        echo
        echo "$rule"
        echo "$(_setup_paint 1 "What to do now")"
        echo
        if [[ ${#_S_REST[@]} -eq 1 ]]; then
            echo "Run this command in the terminal:"
        else
            echo "Run these commands in the terminal, in this order:"
        fi
        _setup_print_steps
        echo "$rule"
    } >&2
}

# setup_print_first — print the declaration's `first` lines, the first
# step of a collector whose `login` is no sign-in, to stderr.
setup_print_first() {
    local decl="$HERE/setup.txt" l
    [[ -f "$decl" ]] || return 0
    setup_read "$decl"
    for l in ${_S_FIRST[@]+"${_S_FIRST[@]}"}; do
        _setup_text "$l"
        echo "$NAME: $_S_LINE" >&2
    done
}

# setup_secrets_dir DEFAULT ARGS... — print the secrets folder the
# --secrets-dir flag among ARGS names, else DEFAULT. For a verb that
# stops before the directory flags are parsed (a folded `login`).
setup_secrets_dir() {
    local dir="$1"
    shift
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --secrets-dir)   dir="${2:-}"; shift ;;
            --secrets-dir=*) dir="${1#*=}" ;;
        esac
        shift
    done
    printf '%s\n' "$dir"
}
