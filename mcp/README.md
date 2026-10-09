# mcp — the reports for AI agents (optional)

An [MCP](https://modelcontextprotocol.io/) server that puts the
read-only wealthdb reports in front of any MCP client: Claude Desktop
and Claude Code, Cursor, LM Studio, open-webui, or a custom agent. A model asks "what did we spend on groceries last year" and gets
the same table `wealthdb spending` prints.

The server is the engine itself (`wealthdb mcp-serve`), run in the
`wealthdb:latest` image. It can only read:

- it has no tool that writes;
- it opens the database read-only, for one call at a time;
- the data and the config are mounted read-only;
- the container has a read-only file system and no capabilities.

Every call reads the live database, so answers are as fresh as the last
`wealthdb load`. There is nothing to refresh. A load that starts during
a call waits a few seconds for it to finish, and so does a call that
starts during a load.

Part of the **wealthdb** suite: see [the architecture
overview](../DESIGN.md). Run it via the main wrapper:

```sh
wealthdb mcp start | stop | status | restart | logs | stdio | url
```

## Enable it

Add an `mcp` block to `wealthdb.cfg`:

```json
"mcp": { "enabled": true, "port": 3300 }
```

`port` defaults to 3300. The server needs no image of its own: build
the engine as usual (`wealthdb build`).

## Start it and connect a client

```sh
wealthdb mcp start     # runs the server; generates a token on first start
wealthdb mcp url       # prints the URL and the header to paste
```

A client that speaks HTTP takes the URL and the header:

```json
{
  "mcpServers": {
    "wealthdb": {
      "type": "http",
      "url": "http://127.0.0.1:3300/mcp",
      "headers": { "Authorization": "Bearer <token from wealthdb mcp url>" }
    }
  }
}
```

A client that starts its own server process runs `wealthdb mcp stdio`
instead. It needs no token, because it runs as the same user:

```json
{
  "mcpServers": {
    "wealthdb": { "command": "wealthdb", "args": ["mcp", "stdio"] }
  }
}
```

Give the full path to `wealthdb` if the client does not see the shell's
`PATH`.

To try it without real data, `make demo` builds a synthetic household
and `make demo-mcp` serves it on port 3400 (container
`wealthdb-mcp-demo`).

## Claude Desktop

Claude Desktop runs the server itself, over stdio. Its *Add custom
connector* dialog does not work here: Claude calls a custom connector
[from Anthropic's servers](https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp),
and those cannot reach a loopback address or an SSH tunnel.

Open **Settings → Developer → Edit Config**. It edits
`claude_desktop_config.json`. Add the server, then quit and reopen the
app:

```json
{
  "mcpServers": {
    "wealthdb": {
      "command": "/path/to/wealthdb",
      "args": ["mcp", "stdio"],
      "env": {
        "PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin",
        "WEALTHDB_DATA_ROOT": "/path/to/data-root"
      }
    }
  }
}
```

A desktop app does not get the shell's environment, so the config
passes it:

- `command` is the full path to the `wealthdb` wrapper.
- `PATH` must hold the directory `docker` is in.
- `WEALTHDB_DATA_ROOT` is needed only when the data root is not the
  default. The same holds for `XDG_CONFIG_HOME` and the config file.

When wealthdb runs on another host, the command is `ssh`. It needs no
tunnel, no port and no token:

```json
{
  "mcpServers": {
    "wealthdb": {
      "command": "/usr/bin/ssh",
      "args": ["-T", "-o", "BatchMode=yes", "<host>",
               "env", "PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin",
               "WEALTHDB_DATA_ROOT=/path/to/data-root",
               "/path/to/wealthdb", "mcp", "stdio"]
    }
  }
}
```

SSH must log in without a prompt, with a key the agent holds. Run
`ssh <host> true` once in a terminal to accept the host key.

Claude Desktop sends each result to Anthropic's hosted model. Put
`"--privacy"` after `"stdio"` to send redacted results instead.

## What a model can ask

Twelve tools, every parameter optional:

| Tool | Answers |
| --- | --- |
| `holdings` | what is held, where, and what it is worth, on a date |
| `returns` | how investments performed, as time- and money-weighted returns |
| `gains` | what was gained or lost, realized and unrealized |
| `transactions` | the individual booked lines over a window |
| `spending` | what was spent, on what |
| `income` | what was received, from whom |
| `cashflow` | where the cash came from and went |
| `status`, `snapshots` | whether the data is current, and which dates it covers |
| `resolutions`, `categorizations` | the stored ticker and category answers |
| `describe` | help on any tool, column or term |

Each tool also filters, sorts and pages its rows, because a model has no
`jq` to pipe a result into. The tools are written for small local models
as well as large ones: one call usually answers a question. The server's
instructions and `describe` explain the rest to the model.

## Two endpoints: full and redacted

`/mcp` serves full data. `/mcp/privacy` serves the same tools with the
CLI's `-p` applied. Amounts, quantities and account numbers are hidden,
and so are the names taken off statements: merchants, payers and
narratives. Account names, holdings, categories, dates and shares stay
legible. A model that runs elsewhere, such as a hosted one, gets the
privacy URL. No tool parameter can turn the redaction off.
`wealthdb mcp stdio --privacy` is the redacted stdio form.

## Security

- Published on loopback only (`127.0.0.1` and `[::1]`). From another
  machine, use an SSH port-forward: `ssh -L 3300:127.0.0.1:3300
  <this-host>`.
- Every HTTP request needs the bearer token. It is generated on first
  start into `$XDG_DATA_HOME/wealthdb/mcp/token` (chmod 600), or taken
  from `WEALTHDB_MCP_TOKEN` (for example in
  `~/.secrets/wealthdb-mcp.env`).
- Turning the token off takes two settings, `"auth": "none"` and
  `"insecure": true`. `"auth": "none"` alone is refused, and every start
  and status prints a warning while it is off.
- Requests with a Host other than loopback, or from a browser page on
  another origin, are refused. A client in another container that
  reaches the server by name needs that name in
  `WEALTHDB_MCP_ALLOW_HOSTS`.
- The server holds no credential and makes no outbound connection. Its
  log records which tool ran and how long it took, never a value.

## Knobs

Environment variables, not config: `WEALTHDB_CONFIG`,
`WEALTHDB_MCP_CONTAINER`, `WEALTHDB_MCP_DATA_DIR`, `WEALTHDB_MCP_BIND`
(`both`, `v4` or `v6`), `WEALTHDB_MCP_TOKEN`, `WEALTHDB_MCP_AUTH` and
`WEALTHDB_MCP_INSECURE`, `WEALTHDB_MCP_ROWS` (rows per result, default
100), `WEALTHDB_MCP_MAX_ROWS` (a ceiling on what one call returns; none
by default), `WEALTHDB_MCP_ALLOW_HOSTS`, `WEALTHDB_MCP_ENV_FILE`.
`wealthdb mcp help` describes each.

After rebuilding the image, run `wealthdb mcp restart`: a running server
keeps the old binary, and `wealthdb mcp status` says so.

## Layout

```
mcp/
├── mcp           lifecycle script (`wealthdb mcp …` delegates here)
├── test_mcp.sh   unit tests for it (make test-mcp)
├── README.md     this file
├── DESIGN.md     how it works and why
└── AGENTS.md     rules for changing it

$XDG_DATA_HOME/wealthdb/mcp/
└── token         the bearer token (chmod 600)
```

The server's code is in the engine: `wealthdb/cmd/wealthdb/mcp_*.go`
and `cmd_mcp_serve.go`.
