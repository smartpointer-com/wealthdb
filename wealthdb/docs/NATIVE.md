# The native engine

A tree of wealthdb runs one of two kinds of engine:

- **The container engine.** The engine, the dashboards and the MCP
  server each run in a Docker container. This is what a checkout builds
  when nothing else is chosen.
- **The native engine.** The same three run as programs on the
  computer. Docker is then needed for one thing only: the collectors
  that run in a container.

Both read the same config and the same data folder, and give the same
answers. This page is about the native engine. The container engine is
[DESIGN.md](DESIGN.md) §12.

## What runs where

| Part | Container engine | Native engine |
|---|---|---|
| `wealthdb <report>`, `load`, `config`, `init` | container | program |
| MCP server (`wealthdb mcp`) | container | program, kept running |
| Dashboards (`wealthdb web`, Metabase) | container | Java program, kept running |
| Collectors with a Dockerfile | container | container |
| Their host-side verbs (`load`, `prune` and `recompress` of schwab-api and fidelity-web) | program | program |
| Collectors on a Python venv | program | program |

## Choosing it

`./configure` at the root of the tree records the choice. `make all`
then builds what it names.

```sh
./configure --engine=native     # check the tools, write config.mk
make all                        # build the engine, download Metabase, build the collectors
make install                    # put wealthdb on PATH
```

- The native engine needs Go, at least the version in `wealthdb/go.mod`,
  a C compiler, and Python 3.10 or later.
- The dashboards also need Java 21 or later. Without it `./configure`
  says so and goes on: everything else works.
- `./configure` alone, or no `./configure` at all, means the container
  engine.
- `GO=`, `PYTHON=` and `JAVA=` name a tool that is not on `PATH`.

The choice is one line of `config.mk`, a git-ignored file of
`KEY=value` lines. The Makefile includes it, and every wrapper reads it
(`shared/wrappers/engine-mode.sh`). No setting but that file chooses
the kind, so two terminals in one tree run the same kind of engine.

To change the kind, run `./configure` again. It writes `config.mk`
afresh, so every option is given again on a later run. A server of the
other kind may still run after the change: `wealthdb mcp stop` and
`wealthdb web stop` end a server of either kind.

The native program is `wealthdb/wealthdb-engine`, built by
`wealthdb build` with the Go that `./configure` recorded. The launcher
`wealthdb/wealthdb` runs it directly: nothing is mounted, and a path
means what it says.

## One kind of engine per database

On a Mac, Docker runs in a virtual machine. A file lock taken on the
Mac is not seen inside that machine, and one taken inside is not seen
on the Mac. The engine protects the database with file locks. A native
engine and a container engine on one database would therefore each
believe they hold the lock, and write at the same time.

So a database has one kind of engine:

- The first command that writes a database records its kind in a small
  file beside it, `<database>.wealthdb.engine`.
- A write command of the other kind refuses, and says how to move the
  database.
- A report does not refuse. It reads the database as it is, and never
  brings a database of the other kind up to date.

To move a database to the other kind of engine:

1. Stop the servers with `wealthdb mcp stop` and `wealthdb web stop`.
   Each ends a server of either kind.
2. Delete that file.
3. If the config names an AI model on this computer, set its address
   again with `wealthdb config model`: `localhost` for the native
   engine, `host.docker.internal` for the container engine.

The next write command records the new kind. The database itself needs
no change.

## The servers

`wealthdb mcp` and `wealthdb web` keep their subcommands. With the
native engine, a server is a program and not a container:

```sh
wealthdb mcp start      # run the MCP server in the background
wealthdb mcp status     # running or not, its address, its log
wealthdb mcp logs       # follow its log
wealthdb mcp stop       # stop it
wealthdb web start      # the same four for the dashboards
```

On a Mac, `start` registers the server with launchd as a LaunchAgent of
the signed-in account:

- It starts again at login, and again if it dies.
- Its file is `~/Library/LaunchAgents/com.smartpointer.wealthdb.mcp.plist`
  (`…wealthdb.metabase.plist` for the dashboards). The file holds the
  paths of the `start` that wrote it, the server's data folder among
  them, and no secret. The server runs in the directory of that
  `start`.
- Its log is `~/Library/Logs/wealthdb/mcp.log` (`metabase.log`). A
  start moves a log of more than 20 MB aside as `mcp.log.1`.
- `stop` removes the file, so a stopped server stays stopped. A server
  that the config has turned off removes it too, the next time launchd
  starts it.
- After an upgrade of wealthdb, `wealthdb mcp restart` runs the new
  engine. `wealthdb mcp status` says when the running one is outdated.
- A setup of its own has a server of its own name, and with it a file
  and a log of that name: `…wealthdb.mcp-<name>.plist` and
  `mcp-<name>.log` ([DESIGN.md §4.2](DESIGN.md#a-setup-of-its-own)). A
  start refuses a name that the server of another setup holds.

On Linux, where systemd's user manager answers (an account with a
session, on a system that boots with systemd), `start` registers the
server the same way, as a systemd user unit:

- Its file is `~/.config/systemd/user/com.smartpointer.wealthdb.mcp.service`
  (`…wealthdb.metabase.service`), enabled so that it starts at login,
  and restarted half a minute after it dies. It holds what the
  LaunchAgent holds, and no secret.
- Its log is the one in the server's data folder.
- `stop` disables the unit and removes the file; a server that the
  config has turned off does the same from inside.
- A user's units stop at logout unless the account lingers (`loginctl
  enable-linger`); the next section covers a computer where no one is
  signed in.

Elsewhere (a container, or no user session), `start` runs the server as
a plain background process, with a pid file and a log in the server's
data folder. It does not start at login and is not restarted.
`WEALTHDB_NO_LAUNCHD=1` asks for the same on a Mac, and
`WEALTHDB_NO_SYSTEMD=1` on Linux.

`start` returns once the server's process runs. It waits up to a
minute for that. A server that has not come up by then is stopped
again, and `start` shows the end of its log.

`stop` ends a server of either kind, when it serves the same data
folder:

- In a tree of the native engine it also removes the server's
  container, where one is left and Docker answers.
- In a tree of the container engine it also ends a native server whose
  pid file is in that folder.

A server of the other kind that serves another data folder is left
alone, also under the same name.

A native server runs with the rights of the account that starts it: it
can read every file that account can. A container sees only the folders
mounted into it, and has a memory cap. The MCP container's file system
is read-only as well. What protects the data with either kind:

- both servers listen on the computer's loopback address only;
- the MCP server asks for its token, opens the database read-only for
  one call at a time, and has no tool that writes;
- Metabase asks for its login, and reads a snapshot, never the live
  database.

The dashboards are Metabase and its DuckDB driver. The first `wealthdb
web start` downloads the two files into `~/.cache/wealthdb/metabase/`
(`$XDG_CACHE_HOME` moves it, and `WEALTHDB_WEB_LIB_DIR` names another
folder); `wealthdb web build` downloads them ahead of time. An installed
copy holds no Metabase of its own. The versions and checksums are the
ones `web/Dockerfile` pins for the image, a download that does not match
is refused, and a later start downloads again only when a pin has
moved. Metabase runs in UTC, as its container does.

### A computer where no one is signed in

A LaunchAgent runs only while its account is signed in at the screen.
On a server, each server program runs under a service manager instead.
The command to run is the server in the foreground:

```sh
wealthdb mcp serve
wealthdb web serve
```

A service definition needs three things:

- the account that owns the data folder, as the user it runs as;
- the same environment the command line has: `HOME`, and a `PATH` with
  the system's folders (`/usr/bin:/bin`); `WEALTHDB_CONFIG` for a
  config elsewhere, whose folder is then the data root; and
  `WEALTHDB_DATA_ROOT` for the default config's data elsewhere;
- a restart when the command exits.

On a Mac that is a LaunchDaemon in `/Library/LaunchDaemons`, with
`UserName`, `EnvironmentVariables`, `KeepAlive` and
`ProgramArguments` of `/path/to/wealthdb`, `mcp`, `serve`. On Linux it
is a systemd unit with `User=`, `Environment=`, `Restart=always` and
the same command as `ExecStart=`.

`wealthdb web serve` runs Metabase only. The steps around it stay
commands to run after each load, as with any other setup:
`wealthdb web refresh` takes the new snapshot and updates the
dashboards.

## Collectors and the lock

A collector with a Dockerfile runs in a container, and writes its
database there. A native `wealthdb load` reads that database on the
computer. SQLite's own locks do not reach between the two, for the
reason given above.

So the two meet on a lock on the computer:

- The collector's wrapper holds a lock file beside the database for
  each verb that writes it (`load`). It runs the container through the
  engine's `hold-lock`, which keeps the lock for as long as `docker run`
  runs.
- `wealthdb load` holds the same lock, shared, while it reads the
  source.
- A load that finds a collector writing waits ten seconds, then skips
  that source, says so, and loads the others. Its exit status is not 0.
  A reload leaves the rows of a source it skips as they were.
- `wealthdb status` does not read a database that a collector writes at
  that moment: the source shows no "new data" note.
- A collector that finds a load reading waits for it.

Collectors on a Python venv need none of this: they run on the
computer, where SQLite's locks work. The container engine takes no lock
either: it is in the virtual machine with the collectors that run in
containers.

## Days are UTC days

The engine counts in UTC days. DuckDB, left to itself, reads an instant
as a day in the computer's time zone. A container is always in UTC, and
a program on a laptop is not. Every DuckDB session the engine opens is
therefore set to UTC (`internal/duckconn`), and no other code opens a
DuckDB database. The same data gives the same figures in any time zone.

## Installing a copy

`make install` links the checkout's two commands into `~/.local/bin`.
With a prefix it makes a copy that runs without the checkout:

```sh
./configure --engine=native --prefix=/opt/wealthdb
make all
make install        # /opt/wealthdb/libexec/wealthdb, linked from /opt/wealthdb/bin
```

The copy has the checkout's layout and the built engine. It has no
Metabase (the dashboards download theirs on first start), no tests, no
Python environments and no source of the engine. From a git checkout it holds only the files git
tracks, so nothing git ignores is copied. A version built from a source
archive of a release states that release: git writes it into
`wealthdb/.archive-version` when it makes the archive.

The copy runs the engine, both servers and the collectors in plain
Python that need no package. It builds nothing. A collector that runs
in a container uses the image built in the checkout, under the same
tag. A collector that needs Python packages runs from the checkout
only. A second install replaces the copy as a whole.

`make uninstall` removes the copy and the two links. In a tree of the
native engine it first stops both servers, when the command in the bin
directory is this tree's.

For a package, the install is staged: `DESTDIR` puts every file under a
folder of its own, and the links name the place the package puts them.
`LIBEXECDIR` moves the copy, `/usr/lib` for a Debian package.
`shared/deb/package.sh` then makes the `.deb` of such a stage:

```sh
./configure --engine=native --prefix=/usr
make build-wealthdb
make install DESTDIR=/tmp/stage LIBEXECDIR=/usr/lib
WEALTHDB_DEB_MAINTAINER="Name <id+login@users.noreply.github.com>" \
    shared/deb/package.sh 1.2.3 /tmp/stage wealthdb_1.2.3.deb
```

The package's Maintainer is a GitHub noreply address, so a problem goes
to the repository's issues.

`shared/deb/build.sh VERSION OUT [REF]` does all of that for a release:
from git's archive of REF, in an Ubuntu 22.04 container, for x86-64. On
a Mac with Apple silicon the container is arm64 and builds the x86-64
engine with Ubuntu's cross compilers. `shared/deb/test-install.sh DEB`
installs a package with apt on Ubuntu 22.04, 24.04 and 26.04 and checks
that it runs; it needs an x86-64 machine, and the release check
(`release-check.yml`) runs both.

## Testing it

`make test-native` tests the native engine without installing Go or
Java on the computer, and whatever kind of engine the checkout itself
runs:

- It builds an image that holds the tools
  (`shared/images/native-test.Dockerfile`), at the versions the
  engine's, the dashboards' and the Python collectors' Dockerfiles name.
- In a container it copies the tree, configures the copy with
  `--engine=native`, builds it and runs the suites of `make test` that
  need no Docker.
- It installs the copy to a prefix and walks it end to end
  (`shared/native/e2e.sh`): the demo household, the reports, both
  locks, the MCP server, and the dashboards with every card run. Then
  it uninstalls the copy.

The container runs in a time zone far from UTC, so an instant read as a
local day fails a test. `shared/native/e2e.sh` also runs outside the
container, against any tree of the native engine.
