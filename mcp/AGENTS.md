# Notes for Claude / coding agents — mcp component

Shared, repo-wide ground rules (no PII in source, git/commit
conventions) live in the repo-root [AGENTS.md](../AGENTS.md). The
MCP-specific surface below applies on top of those.

## 1. Read-only, in depth

The server exists to read. Never:

- add a tool, or a parameter, that writes anything, including
  `categorizations --forget`;
- open gold read-write from the server, or hold a gold handle between
  calls (a held handle locks every `wealthdb load` out);
- drop `:ro` from a mount, `--read-only`, `--cap-drop ALL` or
  `no-new-privileges` from the container, or mount the Docker socket
  or `~/.secrets`;
- add a raw SQL tool.

## 2. Privacy is the endpoint's

- `/mcp/privacy` (and `--stdio --privacy`) applies the CLI's `-p` to
  every result. Never make privacy a tool parameter: a prompt-injected
  model must not be able to turn it off.
- On the privacy endpoint, filters, `search` and `sort` read the
  redacted cells. Keep it that way; a filter over the hidden value is a
  side channel that leaks it a guess at a time.
- Cells come from `rowsToTable`. Never format a cell in the MCP layer:
  that is where the CLI's redaction lives.

## 3. Authentication

- Loopback publish only. Never bind a public interface.
- The token is required on HTTP by default. Turning it off takes two
  settings, auth `none` and `insecure`; keep every one of the three
  refusals (config validation, `mcp start`, `mcp-serve`) and the
  warnings.
- The token reaches the container as a read-only file, never an env
  variable.
- Keep the Host and Origin checks. A client that needs another Host
  name gets it through `--allow-host`, not a wider default.

## 4. One implementation per report

The CLI and the server share each family's `report` runner. Add a view
to the runner and both front-ends have it; `TestMCPCoversEveryCLIView`
fails until the tool's view enum names it, and `TestMCPMatchesCLI`
fails if the two front-ends disagree on a cell. Give the new view its
filters in the family's `carries` and a case in `TestMCPMatchesCLI`.

A new filter needs a parameter, its columns in `filterColumns`, its
place in `filterOrder`, and its views in the family's `carries`.
`TestFilterOrderNamesEveryFilter` and `TestEveryCarriedFilterHasAColumn`
check the last three agree.

## 5. What the model reads

The instructions, tool descriptions and parameter docs were tuned
against small local models, and wording matters more than it looks: a
listing line that names "dividends, interest" under `transactions`
sends a weak model there to sum lines by hand. When changing them:

- keep the date line, the scope paragraph and the pick-a-tool lines;
- say what a tool answers, its views, and one example call;
- state every default;
- put the same questions to a small local model over `make demo-mcp`
  before and after, and keep a change only if the answers hold.

## 6. Logs

Log a call's shape (tool, view, which window bounds, format, size,
duration), never a cell value. Filter values can name a merchant or a
person: debug level only. Every message bound for the model goes
through `scrubPaths`.
