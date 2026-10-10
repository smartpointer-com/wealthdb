# mcp — design

How the MCP server is built and why. [README.md](README.md) says how
to use it; [AGENTS.md](AGENTS.md) holds the rules for changing it.

## 1. Shape

| Piece | Where | What |
| --- | --- | --- |
| Server | `wealthdb/cmd/wealthdb/mcp_*.go`, `cmd_mcp_serve.go` | the `mcp-serve` subcommand: protocol, tools, rows, rendering, auth |
| Settings | `cmd_mcp_config.go`, `internal/config` | the `mcp` block, read back by the hidden `mcp-config` |
| Lifecycle | `mcp/mcp` | `docker run` geometry, token, status, the stdio launcher |

The server is a second front-end over the reports the CLI runs. Each
report family has one runner, a `report` (`report.go`): its column
registry, its default columns, and how to fetch its rows. The CLI picks
columns with `-C` and writes a format; the server reads every column to
filter and sort, then renders a page. Both take their cells from
`rowsToTable`, so the same rows print the same strings.
`TestMCPMatchesCLI` holds that for every view of the seven report tools
and for the two dumps, with and without privacy. `status` and
`snapshots` are tables on the server and prose in the CLI, so they have
no CLI twin to compare with.

There is no second image. `wealthdb:latest` already holds DuckDB, the
adapters and the binary; the MCP container is that image with a
different command. The official Go SDK
(`github.com/modelcontextprotocol/go-sdk`) supplies the protocol and
both transports.

## 2. Live gold, not a snapshot

DuckDB allows one read-write handle or any number of read-only ones,
never both. The web server holds a read-only handle open permanently,
so it reads a snapshot copy. The MCP server holds no handle between
calls:

- Each call loads the config, opens gold read-only for the length of
  the call, and closes it. Answers are as fresh as the last load, with
  no second copy of gold and nothing to refresh.
- A call that meets a writer retries the open on a short ladder (0, 1,
  3 and 5 seconds), then answers "the database is locked by another
  wealthdb command; try again in a minute".
- A command that opens gold read-write retries on the same ladder when
  a reader is in the way: a write command, and a read command where the
  file is writable, since it opens read-write to apply a pending
  migration. A reader holds its handle for one report, seconds at most.
  The write mutex between writers stays non-blocking: another writer
  may run for an hour.
- `compact` and `reload -a` rename a new file over the path. The next
  call opens the new file.

Each open passes DuckDB `memory_limit` (2 GB), `threads` (4) and a
`temp_directory` on the container's tmpfs. A long-lived server must not
size itself to the host, and the default spill path beside the
database is on a read-only mount.

After a rebuild of the image, a running server keeps the old binary.
Once a load migrates gold past the newest migration that binary
carries, every call answers "run `wealthdb mcp restart`":
`gold.CheckSchemaKnown` compares gold's schema version with the
binary's own, which needs no build stamp (the image binary carries
none, so the stamp check in `gold.Open` cannot fire there). `wealthdb
mcp status` compares the container's image with `wealthdb:latest`
before any load.

## 3. Transports and endpoints

- **Streamable HTTP**, stateless, JSON responses: the detached
  container. Stateless means no session ids, so a restarted container
  needs no client to initialise again. The server never pushes, so
  there is no event stream.
- **stdio**: `wealthdb mcp stdio` runs one container per client
  session, removed when the client closes stdin.

`/mcp` serves full data and `/mcp/privacy` the same tools with the
CLI's `-p` applied to every result; stdio takes `--privacy`. Privacy is
a property of the URL, never a tool parameter, so a prompt-injected
model cannot turn it off. On the privacy endpoint, filters and sorts
read the redacted cells: a filter cannot probe a value the endpoint
hides, and a redacted column cannot order the rows.

The instructions open with today's date. An HTTP endpoint rebuilds its
server when the UTC date changes, and every result header states its
resolved dates, for a stdio session that outlives the day.

## 4. Life cycle

`wealthdb mcp start|stop|status|restart|logs|stdio|url`, modelled on
`wealthdb web`: the wrapper delegates to `mcp/mcp`, which reads the
settings back through the hidden `mcp-config`, the engine being the one
JSON reader. There is no `build` (it is the engine image) and no
`refresh` (live gold).

```
docker run -d --name wealthdb-mcp --restart unless-stopped \
  -u <uid>:<gid> -e HOME -e XDG_CONFIG_HOME -e WEALTHDB_DATA_ROOT \
  -v "$CFG:$CFG:ro" -v "$DATA_ROOT:$DATA_ROOT:ro" \
  --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges \
  --memory 3g \
  -v "$DATA_DIR/token:/run/wealthdb-mcp/token:ro" \
  -p 127.0.0.1:PORT:PORT -p "[::1]:PORT:PORT" \
  wealthdb:latest -c "$CFG" mcp-serve --http :PORT --token-file /run/wealthdb-mcp/token
```

The mounts are the engine wrapper's, at the same paths, so the config's
paths resolve the same way; the differences are `:ro` and the
hardening. The token travels as a file, because `docker inspect` prints
the environment.

## 5. Security

- Loopback only, both address families, with an IPv4-only retry.
  `mcp-serve` refuses a listen address beyond loopback unless
  `--all-interfaces` says it runs where something else keeps it local;
  `wealthdb mcp start` passes it inside the container, whose publish is
  loopback only.
- A bearer token on every HTTP request, compared in constant time.
  Turning it off takes `"auth": "none"` and `"insecure": true` (or
  their env forms). Config validation refuses `none` alone, `mcp start`
  refuses it, and `mcp-serve` refuses it. Insecure mode warns at start,
  in every `status`, and in the server log.
- The Host header must be a loopback name, or one `--allow-host`
  names; an `Origin` header must be one too. This is the
  DNS-rebinding guard the MCP specification asks of local servers.
- Read-only in depth: no write tool, read-only opens, read-only mounts
  and root file system, no Docker socket, no credential, no outbound
  connection.
- Statement narratives reach the model as data. Read-only tools bound
  a prompt injection to disclosure, and the privacy endpoint bounds the
  disclosure. The log records tool, view, which window bounds were
  given, format, size and duration; filter values only at debug level.
  A path is replaced by a label in every message bound for the model,
  and on the privacy endpoint a config that fails to load is reported
  without its text, which can quote account ids.
- A panic in a call is a tool error, not the end of the server, and an
  integer parameter is bounded so paging arithmetic cannot overflow.

## 6. The tool API

Twelve tools: seven report families, `status`, `snapshots`, the two
enrichment dumps, and `describe`. The design rules:

1. One tool per CLI family; `view` is an enum whose default is the
   coarsest view. `TestMCPCoversEveryCLIView` holds the views against
   the CLI's tables.
2. Nothing required. Every parameter's description states its default.
3. Enums wherever the CLI has a vocabulary, matched in any case; a
   number sent for a string, or a word for a boolean, reads as meant.
   An unknown parameter or value is an error that names the fix.
4. Row filters, `search`, `sort`, `limit` and `offset` on every table,
   applied after the report has run, so shares are still shares of the
   whole.
5. Every result starts with one header line, then a markdown table
   (`csv` and `json` on request), then a paging line when cut.

The descriptions and instructions are tuned against small local models
over the demo household. Each of the following removes a class of wrong
answers or extra round trips such a model otherwise makes:

- **The date in the instructions, resolved dates in every header.**
  Without them a model reads "last year" as two years ago.
- **A filter picks its view.** A model starts at the default view and
  adds a filter (`spending` with a category, `holdings` with a tax
  wrapper). The first view at or after the requested one that carries
  every named filter runs, and the header says so.
- **Money sorts by magnitude.** Spending lines are negative, so a plain
  descending sort puts the smallest purchase first.
- **A category filter implies the detailed vocabulary.** "Groceries" is
  a detailed category.
- **`period` defaults to `total`.** A breakdown by month overflows the
  page, and a model summing monthly rows by hand gets the sum wrong.
- **Column names tolerate a typo,** when one column is that close, and
  `value`, `amount` or the family's own word name the view's main money
  column (on `returns`, the return).
- **An empty filtered result lists the values the column takes,** so a
  guessed section or class is corrected by the next call.
- **The instructions name the data's scope:** property and loans are
  accounts like any other, and mortgage and plan payments are in
  `cashflow`.

Paging: `limit` defaults to 100 and has no fixed maximum; `limit: 0`
returns every row, so a model with a large context pays no round trips.
`WEALTHDB_MCP_MAX_ROWS` sets a ceiling for a deployment serving small
models.

Not exposed, because they write or prompt: `config`, `init`, `load`,
`reset`, `reload`, `compact`, `lots`, `categorize`, `resolve-symbols`,
`categorizations --forget`, `web-*`, `mcp-config`. There is no SQL tool
and no grouping beyond what the reports do.

`describe` and the resources (`wealthdb://guide`,
`wealthdb://guide/<topic>`) carry the long help, generated from the
same parameter specs and column registries the tools use.

## 7. Testing

- `TestMCPMatchesCLI`: every view of the seven report tools and the two
  dumps, CLI csv equals MCP csv, with and without privacy, over seeded
  gold.
- Protocol tests over the SDK's in-memory transport: initialize, the
  tool list and its annotations, call and error shapes, resources.
- HTTP tests: the token, the Host and Origin guards, the two endpoints.
- Unit tests for arguments, filters, sort, column lookup, escalation,
  paging and rendering.
- `TestWriteOpenWaitsOutAReader` holds the read-write retry against a
  reader in another process; `TestCheckSchemaKnown` the staleness check.
- `mcp/test_mcp.sh` (`make test-mcp`): the container geometry, the
  token file, the auth gate and the outdated-image check, against a
  stubbed engine and docker.

A change to a tool description or the instructions is best checked
with a small local model over `make demo-mcp`: tool choice is rarely
the problem, round trips are.
