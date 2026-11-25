# shellcheck shell=bash
# service-lib.sh — the lifecycle helpers web/web and mcp/mcp share: the
# Docker check, the engine's settings, the loopback address, the
# server's state, its stop and its log, and the optional env file. Bash;
# sourced by both, which keep their own subcommands and their own
# `docker run` argv.
#
# A server is a container in a tree of the container engine. In a tree
# of the native engine (engine-mode.sh) it is a program on this
# computer, which `<service> serve` runs in the foreground. `start`
# hands that command to launchd as a LaunchAgent of the signed-in user,
# which starts it at login and restarts it when it dies. Where there is
# no launchd, `start` runs it as a plain background process.
#
# Caller must SET (before sourcing):
#   SERVICE        the service's name, `web` or `mcp`. It heads every
#                  message, names the engine's `config-env` section and
#                  the default env file, and gives WEALTHDB_<SERVICE>_*
#                  the variables below are read from.
#   WEALTHDB       the engine wrapper (`config-env` runs through it).
#   ROOT           the tree's root.
# Caller MAY SET:
#   SERVICE_CPORT  the port the server listens on inside the container;
#                  unset, the published port.
#   SERVICE_PROC   a string the native server's command line holds, by
#                  which a pid file's process is known to be the server.
#   SERVICE_ENV_KEEP
#                  the environment variables, beside the common ones
#                  (_write_plist), a LaunchAgent hands to `serve`. Never
#                  one that holds a secret: `serve` reads the env file
#                  itself.
#   SERVICE_CONFIG the config file the server's container mounts, when
#                  it mounts one (mcp). A start takes a container that
#                  mounts it for this setup's server (_start_guard).
# The helpers read CONTAINER and DATA_DIR at call time, and PROGRAM, the
# words a command to run starts with (wealthdb_engine_paths).
#
# The caller sources image-tag.sh first. Its wealthdb_engine_paths tells
# the default config from a setup of its own, which keeps its servers
# apart: their names, their data folders and their env files' names
# (_service_knobs, _source_env_file).

# shellcheck disable=SC1091
. "$(dirname "${BASH_SOURCE[0]}")/envfile-lib.sh"
# shellcheck disable=SC1091
. "$(dirname "${BASH_SOURCE[0]}")/engine-mode.sh"

SERVICE_ENV="WEALTHDB_$(printf '%s' "$SERVICE" | tr '[:lower:]' '[:upper:]')"
# The default config's values, until the paths are resolved
# (_service_knobs).
PROGRAM=wealthdb OWN_SETUP=0
SERVICE_MODE="$(wealthdb_engine_mode "$ROOT")" || exit 2

_require_docker() {
    [ "$SERVICE_MODE" = native ] && return 0
    command -v docker >/dev/null 2>&1 || { echo "$SERVICE: docker not found on PATH" >&2; return 1; }
}

# _config_env evaluates the engine's hidden `config-env SERVICE`: the
# service's resolved settings and the gold path, as shell assignments.
# The engine is the one component that parses and validates
# wealthdb.cfg; WEALTHDB_CONFIG names a non-default file.
_config_env() {
    _require_docker || return 1
    local out
    if [ -n "${WEALTHDB_CONFIG:-}" ]; then
        out="$("$WEALTHDB" -c "$WEALTHDB_CONFIG" config-env "$SERVICE")" || out=""
    else
        out="$("$WEALTHDB" config-env "$SERVICE")" || out=""
    fi
    if [ -z "$out" ]; then
        local cfg=wealthdb.cfg
        if [ "$OWN_SETUP" = 1 ]; then cfg="$CFG"; fi
        echo "$SERVICE: could not read settings via '$PROGRAM config-env $SERVICE' (is $cfg valid?)" >&2
        return 1
    fi
    eval "$out"
}

# _publish_args PORT BIND → prints `docker run` -p flags (one token
# per line) for loopback IPv4 and/or IPv6, onto SERVICE_CPORT in the
# container.
_publish_args() {
    local port="$1" bind="$2" cport="${SERVICE_CPORT:-$1}"
    case "$bind" in
        v4)   printf '%s\n' -p "127.0.0.1:${port}:${cport}" ;;
        v6)   printf '%s\n' -p "[::1]:${port}:${cport}" ;;
        both) printf '%s\n' -p "127.0.0.1:${port}:${cport}" -p "[::1]:${port}:${cport}" ;;
        *)    _bad_bind "$bind"; return 1 ;;
    esac
}

# _loopback_host BIND prints the loopback address a native server
# listens on. It has one address: IPv6's for v6, IPv4's for v4 and for
# both.
_loopback_host() {
    case "$1" in
        both | v4) echo 127.0.0.1 ;;
        v6)        echo ::1 ;;
        *)         _bad_bind "$1"; return 1 ;;
    esac
}

_bad_bind() { echo "$SERVICE: invalid ${SERVICE_ENV}_BIND '$1' (want both|v4|v6)" >&2; }

# _service_knobs BASE sets CONTAINER and DATA_DIR, and with them the
# engine's paths (wealthdb_engine_paths). ${SERVICE_ENV}_CONTAINER and
# ${SERVICE_ENV}_DATA_DIR win, each on its own. For the default config
# the container is wealthdb-BASE and the data folder is
# $XDG_DATA_HOME/wealthdb/SERVICE, else ~/.local/share/wealthdb/SERVICE.
# A setup of its own has the container wealthdb-BASE-NAME, NAME from its
# folder (wealthdb_setup_name), and the data folder SERVICE in that
# folder, so its server never takes the name or the files of another
# setup's.
_service_knobs() {
    local container="${SERVICE_ENV}_CONTAINER" data="${SERVICE_ENV}_DATA_DIR"
    wealthdb_engine_paths
    if [ "$OWN_SETUP" = 1 ]; then
        CONTAINER="${!container:-wealthdb-$1-$SETUP_NAME}"
        DATA_DIR="${!data:-${SETUP_DIR%/}/$SERVICE}"
    else
        CONTAINER="${!container:-wealthdb-$1}"
        DATA_DIR="${!data:-${XDG_DATA_HOME:-$HOME/.local/share}/wealthdb/$SERVICE}"
    fi
}

# _is_running: the server runs. _exists: there is a server to stop or
# remove, running or not: a container, a LaunchAgent or a pid file.
_is_running() {
    if [ "$SERVICE_MODE" = native ]; then _native_pid >/dev/null; return; fi
    [ -n "$(docker ps  -q -f "name=^${CONTAINER}\$" -f status=running 2>/dev/null)" ]
}
_exists() {
    if [ "$SERVICE_MODE" = native ]; then _native_exists; return; fi
    [ -n "$(docker ps -aq -f "name=^${CONTAINER}\$" 2>/dev/null)" ]
}
_native_exists() {
    [ -f "$(_service_pidfile)" ] || { _uses_launchd && [ -f "$(_service_plist)" ]; }
}

# _where names the server in a message: its container, or its program.
_where() {
    if [ "$SERVICE_MODE" = native ]; then echo "the server"; else echo "container $CONTAINER"; fi
}

# ---- stop and logs -----------------------------------------------------

# _service_stop KEPT stops the server and says so, with KEPT: what a
# stop leaves in place. It ends a server of either kind that serves this
# data folder, so a server that ran when ./configure changed the kind is
# stopped too.
_service_stop() {
    local other=0
    if _stop_other_kind "$1"; then other=1; fi
    if [ "$SERVICE_MODE" = native ]; then
        if _exists; then
            _native_stop
            echo "$SERVICE: stopped the server ($1)"
        elif [ "$other" = 0 ]; then
            echo "$SERVICE: no server to stop"
        fi
        return 0
    fi
    if _exists; then
        _container_stop
        echo "$SERVICE: stopped $CONTAINER ($1)"
    elif [ "$other" = 0 ]; then
        echo "$SERVICE: no container '$CONTAINER' to stop"
    fi
}

# _stop_other_kind KEPT stops a server of the kind this tree does not
# run, says so, and fails when there is none. Only a server of this
# data folder (DATA_DIR) counts: another installation's server, under
# the same name, is not this stop's to end. In a tree of the container
# engine that is a native server whose pid file is in the folder. In a
# tree of the native engine it is the container that mounts the folder,
# or a file in it, where docker is on PATH. Docker gets
# SERVICE_DOCKER_WAIT seconds (default 5) to say whether the container
# exists: a daemon that does not answer has no container to stop, and
# holds no stop up for longer.
_stop_other_kind() {
    if [ "$SERVICE_MODE" = native ]; then
        command -v docker >/dev/null 2>&1 || return 1
        _within "${SERVICE_DOCKER_WAIT:-5}" docker container inspect "$CONTAINER" || return 1
        _container_serves_here || return 1
        _container_stop
        echo "$SERVICE: stopped container $CONTAINER, the container engine's server ($1)"
    else
        [ -f "$(_service_pidfile)" ] || return 1
        _native_stop
        echo "$SERVICE: stopped the native engine's server ($1)"
    fi
}

# _container_serves_here succeeds when the container mounts DATA_DIR, or
# a file or folder in it: the token, Metabase's own database. Docker
# reports a mount's source as a clean path, so DATA_DIR is compared
# clean.
_container_serves_here() {
    local src here
    here="$(wealthdb_clean_path "$DATA_DIR")"
    while IFS= read -r src; do
        case "$src" in "$here" | "${here%/}"/*) return 0 ;; esac
    done < <(_container_mounts)
    return 1
}

# _container_mounts prints the host path of each of the container's
# mounts, one per line.
_container_mounts() {
    docker container inspect -f '{{range .Mounts}}{{println .Source}}{{end}}' "$CONTAINER" 2>/dev/null
}

# _container_reads_config succeeds when the container mounts
# SERVICE_CONFIG, the config of this setup. It tells whose server an MCP
# container with no token is: that one mounts nothing of its data
# folder.
_container_reads_config() {
    [ -n "${SERVICE_CONFIG:-}" ] || return 1
    _container_mounts | grep -qxF -- "$(wealthdb_clean_path "$SERVICE_CONFIG")"
}

# _start_guard fails, with a message, when the name this start would
# take is held by the server of another setup: a container of that name
# that serves another data folder and reads another config, or, for a
# native server under launchd, a LaunchAgent of that label that serves
# another data folder. A start replaces only its own server, so `start`
# and `restart` call this before they stop or remove anything. A native
# server that is a plain background process has no name to share: its
# pid file is in its own data folder.
_start_guard() {
    local name="$CONTAINER"
    if [ "$SERVICE_MODE" = native ]; then
        _uses_launchd && [ -f "$(_service_plist)" ] || return 0
        _agent_serves_here && return 0
        name="$(_service_label)"
    else
        _exists || return 0
        _container_serves_here && return 0
        _container_reads_config && return 0
    fi
    echo "$SERVICE: $name is the server of another setup (its files are not in $DATA_DIR). Stop that one first, or name this one with ${SERVICE_ENV}_CONTAINER." >&2
    return 1
}

_container_stop() {
    docker stop "$CONTAINER" >/dev/null 2>&1 || true
    docker rm   "$CONTAINER" >/dev/null 2>&1 || true
}

# _within SECONDS CMD... runs CMD with its output dropped and returns
# its status. A CMD that still runs after SECONDS is killed, and the
# status is 124.
_within() {
    local tries=$(($1 * 10)) pid
    shift
    "$@" >/dev/null 2>&1 &
    pid=$!
    while kill -0 "$pid" 2>/dev/null; do
        if [ "$tries" -le 0 ]; then
            kill "$pid" 2>/dev/null || true
            wait "$pid" 2>/dev/null || true
            return 124
        fi
        sleep 0.1
        tries=$((tries - 1))
    done
    wait "$pid"
}

# _service_logs follows the server's log: the native server's file, or
# the container's output.
_service_logs() {
    if [ "$SERVICE_MODE" = native ]; then exec tail -n 100 -f "$(_service_logfile)"; fi
    _require_docker || return 1
    exec docker logs -f "$CONTAINER"
}

# ---- the native server -------------------------------------------------

# _service_name is the server's short name, the container's without its
# prefix: mcp, metabase, or a second instance's (mcp-demo).
_service_name()    { printf '%s\n' "${CONTAINER#wealthdb-}"; }
_service_label()   { printf 'com.smartpointer.wealthdb.%s\n' "$(_service_name)"; }
_service_plist()   { printf '%s/Library/LaunchAgents/%s.plist\n' "$HOME" "$(_service_label)"; }
_service_pidfile() { printf '%s/%s.pid\n' "$DATA_DIR" "$SERVICE"; }
_service_logfile() {
    if _uses_launchd; then
        printf '%s/Library/Logs/wealthdb/%s.log\n' "$HOME" "$(_service_name)"
    else
        printf '%s/%s.log\n' "$DATA_DIR" "$SERVICE"
    fi
}
# WEALTHDB_NO_LAUNCHD=1 keeps a Mac's servers out of launchd: plain
# background processes, as on any other system.
_uses_launchd() {
    [ "${WEALTHDB_NO_LAUNCHD:-0}" != 1 ] && [ "$(uname -s)" = Darwin ] && command -v launchctl >/dev/null 2>&1
}
_launchd_domain() { printf 'gui/%s\n' "$(id -u)"; }

# _native_pid prints the pid of the running native server: the pid
# file's, when that process lives and its command line holds
# SERVICE_PROC. A pid file left by a server that died names no process,
# or another one.
_native_pid() {
    local pid file
    file="$(_service_pidfile)"
    [ -f "$file" ] || return 1
    pid="$(tr -d '[:space:]' < "$file")" || return 1
    case "$pid" in '' | *[!0-9]*) return 1 ;; esac
    kill -0 "$pid" 2>/dev/null || return 1
    _pid_command "$pid" | grep -qF -- "${SERVICE_PROC:-wealthdb}" || return 1
    printf '%s\n' "$pid"
}

# _pid_command PID prints the command line of a process. -ww gives the
# whole line: without it ps cuts the line at COLUMNS, where that is set.
# Where ps gives nothing, /proc has the line on Linux.
_pid_command() {
    local cmd
    cmd="$(ps -ww -p "$1" -o command= 2>/dev/null)" || cmd=
    if [ -z "$cmd" ] && [ -r "/proc/$1/cmdline" ]; then
        cmd="$(tr '\0' ' ' < "/proc/$1/cmdline" 2>/dev/null)" || cmd=
    fi
    printf '%s\n' "$cmd"
}

# _plist_value KEY prints the string the LaunchAgent's file gives KEY
# among its environment variables, as _write_plist wrote it: the line
# after the key's, unescaped. It prints nothing for a key that is not
# there.
_plist_value() {
    awk -v key="<key>$1</key>" '
        found { sub(/^[[:space:]]*<string>/, ""); sub(/<\/string>[[:space:]]*$/, ""); print; exit }
        index($0, key) { found = 1 }' "$(_service_plist)" 2>/dev/null |
        sed -e 's/&lt;/</g' -e 's/&gt;/>/g' -e 's/&amp;/\&/g'
}

# _agent_serves_here succeeds when the LaunchAgent of this label serves
# DATA_DIR: the data folder its file names. A file that names none was
# written for the default data folder, under the XDG_DATA_HOME it names.
_agent_serves_here() {
    local dir
    dir="$(_plist_value "${SERVICE_ENV}_DATA_DIR")"
    if [ -z "$dir" ]; then
        dir="$(_plist_value XDG_DATA_HOME)"
        dir="${dir:-$HOME/.local/share}/wealthdb/$SERVICE"
    fi
    [ "$(wealthdb_clean_path "$dir")" = "$(wealthdb_clean_path "$DATA_DIR")" ]
}

# _serve_here records this process as the server. `serve` calls it just
# before it execs the server, which keeps the pid. It refuses while
# another process is the server: a second one would take the pid file
# and then end on the port that is taken.
_serve_here() {
    local pid
    if pid="$(_native_pid)" && [ "$pid" != "$$" ]; then
        echo "$SERVICE: already running (pid $pid). Use '$PROGRAM $SERVICE restart'." >&2
        return 1
    fi
    mkdir -p "$DATA_DIR" && printf '%s\n' "$$" > "$(_service_pidfile)"
}

# _retire_agent ends a LaunchAgent from inside: `serve` calls it when
# the config has turned the server off since the `start` that wrote the
# agent. Without it launchd would start a server that exits at once,
# every half minute, until a `stop`. It acts only in a process launchd
# started under this server's label, and unloading the agent ends that
# process.
_retire_agent() {
    _uses_launchd && [ "${XPC_SERVICE_NAME:-}" = "$(_service_label)" ] || return 0
    echo "$SERVICE: turned off in the config; removing the server from launchd" >&2
    rm -f "$(_service_plist)" "$(_service_pidfile)"
    launchctl bootout "$(_launchd_domain)/$(_service_label)" 2>/dev/null || true
}

_xml_escape() { sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'; }

# _plist_string VALUE prints VALUE as a property list string element.
_plist_string() { printf '<string>%s</string>\n' "$(printf '%s' "$1" | _xml_escape)"; }

# _write_plist writes the LaunchAgent that runs `<entry> SERVICE serve`:
# at login, and again whenever it exits. The entry is the command the
# caller started wealthdb by (WEALTHDB_ENTRY), which stays in place when
# a newer version is installed. The agent hands `serve` the paths and
# knobs of this start, and no secret. The data folder is always among
# them: it tells whose server the agent is (_agent_serves_here). A
# relative path among them is written absolute: launchd runs `serve` in
# another directory, and the one of this start may be gone by the next
# login.
_write_plist() {
    local plist log var val
    plist="$(_service_plist)" log="$(_service_logfile)"
    mkdir -p "$(dirname "$plist")" "$(dirname "$log")" || return 1
    {
        printf '%s\n' '<?xml version="1.0" encoding="UTF-8"?>' \
            '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' \
            '<plist version="1.0">' '<dict>' \
            '    <key>Label</key>' "    $(_plist_string "$(_service_label)")" \
            '    <key>ProgramArguments</key>' '    <array>' \
            "        $(_plist_string "${WEALTHDB_ENTRY:-$WEALTHDB}")" \
            "        $(_plist_string "$SERVICE")" '        <string>serve</string>' '    </array>' \
            '    <key>EnvironmentVariables</key>' '    <dict>'
        # shellcheck disable=SC2086  # SERVICE_ENV_KEEP is a list of names
        for var in HOME PATH XDG_CONFIG_HOME XDG_DATA_HOME XDG_CACHE_HOME \
            WEALTHDB_CONFIG WEALTHDB_DATA_ROOT WEALTHDB_SECRETS_DIR \
            "${SERVICE_ENV}_CONTAINER" "${SERVICE_ENV}_DATA_DIR" "${SERVICE_ENV}_BIND" \
            "${SERVICE_ENV}_ENV_FILE" ${SERVICE_ENV_KEEP:-}; do
            val="${!var:-}"
            if [ "$var" = "${SERVICE_ENV}_DATA_DIR" ]; then val="$DATA_DIR"; fi
            [ -n "$val" ] || continue
            case "$var" in
                XDG_*_HOME | WEALTHDB_CONFIG | WEALTHDB_DATA_ROOT | WEALTHDB_SECRETS_DIR | *_DATA_DIR | *_ENV_FILE)
                    val="$(wealthdb_abs_path "$val")" ;;
            esac
            printf '        <key>%s</key>\n        %s\n' "$var" "$(_plist_string "$val")"
        done
        printf '%s\n' '    </dict>' \
            '    <key>RunAtLoad</key>' '    <true/>' \
            '    <key>KeepAlive</key>' '    <true/>' \
            '    <key>ThrottleInterval</key>' '    <integer>30</integer>' \
            '    <key>StandardOutPath</key>' "    $(_plist_string "$log")" \
            '    <key>StandardErrorPath</key>' "    $(_plist_string "$log")" \
            '</dict>' '</plist>'
    } > "$plist"
}

# _native_start starts the native server and returns once its process
# lives. A server that does not come up within SERVICE_START_WAIT
# seconds (default 60) is stopped again, so launchd does not keep
# restarting one that cannot start, and the end of its log is shown. A
# background process that ends before it is the server fails the start
# at once.
_native_start() {
    local log tries child=
    log="$(_service_logfile)"
    mkdir -p "$DATA_DIR" "$(dirname "$log")" || return 1
    _rotate_log "$log"
    rm -f "$(_service_pidfile)"
    if _uses_launchd; then
        _write_plist || { echo "$SERVICE: could not write $(_service_plist)" >&2; return 1; }
        launchctl bootout "$(_launchd_domain)/$(_service_label)" 2>/dev/null || true
        if ! launchctl bootstrap "$(_launchd_domain)" "$(_service_plist)"; then
            rm -f "$(_service_plist)"
            echo "$SERVICE: launchd did not take the server. It runs servers for an account that is signed in" >&2
            echo "     at the screen. On a server, run '$PROGRAM $SERVICE serve' under a system service instead." >&2
            return 1
        fi
    else
        nohup "${WEALTHDB_ENTRY:-$WEALTHDB}" "$SERVICE" serve >> "$log" 2>&1 < /dev/null &
        child=$!
        # The shell reports nothing about a process it no longer owns.
        disown "$child" 2>/dev/null || true
    fi
    tries=$(( ${SERVICE_START_WAIT:-60} * 5 ))
    while [ "$tries" -gt 0 ]; do
        if _native_pid >/dev/null; then return 0; fi
        if [ -n "$child" ] && ! kill -0 "$child" 2>/dev/null; then break; fi
        sleep 0.2
        tries=$((tries - 1))
    done
    echo "$SERVICE: the server did not start. The end of $log:" >&2
    tail -n 15 "$log" 2>/dev/null | sed 's/^/     /' >&2
    _native_stop
    # The background process may live on with no pid file that names it.
    if [ -n "$child" ]; then _end_process "$child"; fi
    return 1
}

# _rotate_log FILE moves a log of more than SERVICE_LOG_MAX bytes
# (default 20 MB) aside as FILE.1, in place of the one there. A start
# calls it, so a server's log and the one before it stay bounded by how
# much it writes between two starts.
_rotate_log() {
    local size
    [ -f "$1" ] || return 0
    size="$(wc -c < "$1" | tr -d '[:space:]')"
    if [ "${size:-0}" -gt "${SERVICE_LOG_MAX:-20971520}" ]; then mv -f "$1" "$1.1"; fi
}

# _native_stop stops the native server and removes its LaunchAgent, so
# it stays stopped after the next login. It waits for the process to go.
_native_stop() {
    local pid
    pid="$(_native_pid)" || pid=
    if _uses_launchd; then
        launchctl bootout "$(_launchd_domain)/$(_service_label)" 2>/dev/null || true
        rm -f "$(_service_plist)"
    fi
    if [ -n "$pid" ]; then _end_process "$pid"; fi
    rm -f "$(_service_pidfile)"
}

# _end_process PID asks a process to end, gives it ten seconds, then
# kills it.
_end_process() {
    local tries=50
    kill "$1" 2>/dev/null || true
    while kill -0 "$1" 2>/dev/null && [ "$tries" -gt 0 ]; do
        sleep 0.2
        tries=$((tries - 1))
    done
    kill -9 "$1" 2>/dev/null || true
}

# _file_id prints a file's modification time and size, which tell a
# program that was rebuilt or replaced from the one a server started.
_file_id() {
    # GNU's form first: BSD's -f means something else to GNU stat.
    stat -c '%Y %s' "$1" 2>/dev/null || stat -f '%m %z' "$1" 2>/dev/null
}

# _source_env_file sources the optional env file for the secrets and
# the knobs: ${SERVICE_ENV}_ENV_FILE, else wealthdb-SERVICE.env in the
# secrets folder, $WEALTHDB_SECRETS_DIR or ~/.secrets, as the collectors
# find theirs (repo convention: secrets never live in the config file).
# A setup of its own reads wealthdb-SERVICE-NAME.env there instead,
# NAME from its folder as in its container's name (wealthdb_setup_name),
# so it never reads the secrets of another setup's server.
# It is a bash script: syntax-checked, then sourced with every
# assignment exported. A file that does not parse returns 1, and bash's
# message is not shown, since it quotes the line.
_source_env_file() {
    local var="${SERVICE_ENV}_ENV_FILE" envf name="wealthdb-$SERVICE"
    wealthdb_engine_paths
    if [ "$OWN_SETUP" = 1 ]; then name="$name-$SETUP_NAME"; fi
    envf="${!var:-${WEALTHDB_SECRETS_DIR:-$HOME/.secrets}/$name.env}"
    if [ -f "$envf" ] && ! wealthdb_source_bash "$envf"; then
        echo "$SERVICE: $envf has a bash syntax error" >&2
        return 1
    fi
}
