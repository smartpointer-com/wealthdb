package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/version"
)

func init() {
	register("mcp-serve", cmdMCPServe)
}

// insecureWarning is printed wherever auth is off: at start, in
// status, and in the server log.
const insecureWarning = "INSECURE: authentication is off; any process on this machine can read this household's finances."

// mcpServer serves the read-only reports as MCP tools. It holds no
// database handle: each call loads the config, opens gold read-only for
// the length of the call and closes it, so a `wealthdb load` waits at
// most one call and answers are as fresh as the last load.
type mcpServer struct {
	configPath string
	// privacy is the stdio session's redaction; over HTTP it is the
	// endpoint's, and each endpoint has its own mcpServer.
	privacy bool
	rows    int // default row limit
	maxRows int // ceiling on a call's limit; 0 means none
	// duckdb is the configuration each read-only open passes DuckDB.
	duckdb map[string]string
	log    *slog.Logger
	now    func() time.Time
}

func cmdMCPServe(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, _, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb mcp-serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stdio := fs.Bool("stdio", false, "serve one session on stdin/stdout")
	httpAddr := fs.String("http", "", "serve streamable HTTP on ADDR (e.g. :3300)")
	privacy := fs.Bool("privacy", false, "with --stdio: redact amounts, identifiers and names")
	auth := fs.String("auth", config.MCPAuthToken, "HTTP auth: token | none")
	tokenFile := fs.String("token-file", "", "file holding the bearer token (HTTP with --auth token)")
	insecure := fs.Bool("insecure", false, "acknowledge --auth none")
	var allowHosts hostList
	fs.Var(&allowHosts, "allow-host", "a Host besides loopback the HTTP server answers to (repeatable, or a comma list)")
	rows := fs.Int("rows", envInt("WEALTHDB_MCP_ROWS", 100), "default rows per result")
	maxRows := fs.Int("max-rows", envInt("WEALTHDB_MCP_MAX_ROWS", 0), "most rows one call may return; 0 for no ceiling")
	memLimit := fs.String("memory-limit", "2GB", "DuckDB memory_limit per open")
	threads := fs.Int("threads", 4, "DuckDB threads per open")
	tempDir := fs.String("temp-dir", filepath.Join(os.TempDir(), "wealthdb-mcp"), "DuckDB spill directory (must be writable)")
	allInterfaces := fs.Bool("all-interfaces", false, "allow --http to listen beyond loopback (inside a container whose publish is loopback-only)")
	verbose := fs.Bool("v", false, "log filter values and other call detail at debug level")
	fs.Usage = func() { fmt.Fprintln(stderr, mcpServeUsage) }
	if err := fs.Parse(subargs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "mcp-serve: bad flags")
	}
	if fs.NArg() != 0 {
		return errs.Newf(2, "mcp-serve: unexpected argument %q", fs.Arg(0))
	}
	if *stdio == (*httpAddr != "") {
		return errs.Newf(2, "mcp-serve: pass exactly one of --stdio and --http ADDR")
	}
	if *privacy && !*stdio {
		return errs.Newf(2, "mcp-serve: --privacy applies to --stdio; over HTTP the /mcp/privacy endpoint redacts")
	}
	if *httpAddr != "" && !*allInterfaces && !loopbackAddr(*httpAddr) {
		return errs.Newf(2, "mcp-serve: --http %s listens beyond loopback; use 127.0.0.1:PORT, or pass --all-interfaces "+
			"where a container's publish keeps it on loopback, as 'wealthdb mcp start' does", *httpAddr)
	}
	if *rows < 1 || *maxRows < 0 {
		return errs.Newf(2, "mcp-serve: --rows must be 1 or more and --max-rows 0 or more")
	}
	if *maxRows > 0 && *rows > *maxRows {
		*rows = *maxRows
	}
	// The config is loaded per call, but a broken one should fail the
	// start, not the first question.
	if _, err := config.Load(g.ConfigPath); err != nil {
		return err
	}
	if err := os.MkdirAll(*tempDir, 0o700); err != nil {
		return errs.Newf(2, "mcp-serve: the DuckDB spill directory %q is not writable: %v", *tempDir, err)
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	s := &mcpServer{
		configPath: g.ConfigPath,
		privacy:    *privacy,
		rows:       *rows,
		maxRows:    *maxRows,
		duckdb: map[string]string{
			"memory_limit":   *memLimit,
			"threads":        strconv.Itoa(*threads),
			"temp_directory": *tempDir,
		},
		// Logs go to stderr in both modes: on stdio, stdout is the
		// protocol.
		log: slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level})),
		now: time.Now,
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *stdio {
		s.log.Info("serving MCP on stdio", "version", version.String(), "privacy", s.privacy)
		err := s.newServer(s.now()).Run(ctx, &mcp.StdioTransport{})
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
			return err
		}
		return nil
	}

	var token string
	switch *auth {
	case config.MCPAuthToken:
		if *insecure {
			return errs.Newf(2, "mcp-serve: --insecure acknowledges --auth none; with a token it means nothing")
		}
		t, err := readToken(*tokenFile, s.log)
		if err != nil {
			return errs.Newf(2, "mcp-serve: %v", err)
		}
		token = t
	case config.MCPAuthNone:
		if !*insecure {
			return errs.Newf(2, "mcp-serve: --auth none serves the data to any local process; pass --insecure as well to mean it")
		}
		s.log.Warn(insecureWarning)
	default:
		return errs.Newf(2, "mcp-serve: --auth %q is not a mode (want token | none)", *auth)
	}
	return serveHTTP(ctx, s, *httpAddr, token, allowHosts)
}

// readToken reads the bearer token from path. A token readable by
// other users is served anyway, with a warning: the file's mode may be
// the bind mount's to decide.
func readToken(path string, log *slog.Logger) (string, error) {
	if path == "" {
		return "", errors.New("--auth token needs --token-file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the token: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if len(token) < 16 {
		return "", fmt.Errorf("the token in %q is shorter than 16 characters", path)
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		log.Warn("the token file is readable by other users; chmod 600 it", "path", path)
	}
	return token, nil
}

// loopbackAddr reports whether a listen address names a loopback host.
// An empty host (":3300") listens on every interface.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// envInt reads an integer environment default for a flag; a value that
// does not parse is ignored, and the flag's own validation reports a
// bad one given on the command line.
func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

// hostList is the repeatable --allow-host flag.
type hostList []string

func (h *hostList) String() string { return strings.Join(*h, ",") }

func (h *hostList) Set(v string) error {
	for _, x := range strings.Split(v, ",") {
		if x = strings.ToLower(strings.TrimSpace(x)); x != "" {
			*h = append(*h, x)
		}
	}
	return nil
}

// newServer builds the MCP server: the instructions dated today, the
// eleven tools, and the guide resources. Each tool is registered with
// its own schema and parses its own arguments, so a small model's
// near-miss gets an answer that names the fix instead of a schema
// validator's message.
func (s *mcpServer) newServer(today time.Time) *mcp.Server {
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "wealthdb", Title: "wealthdb", Version: version.String()},
		&mcp.ServerOptions{
			Instructions: s.instructions(today),
			// Tools and resources only. The tool list never changes
			// while a server runs, and the server sends no log
			// messages to the client.
			Capabilities: &mcp.ServerCapabilities{
				Tools:     &mcp.ToolCapabilities{},
				Resources: &mcp.ResourceCapabilities{},
			},
		})
	no := false
	for _, t := range s.tools() {
		srv.AddTool(&mcp.Tool{
			Name:        t.name,
			Title:       t.title,
			Description: t.description,
			InputSchema: toolSchema(t.params),
			Annotations: &mcp.ToolAnnotations{
				Title:           t.title,
				ReadOnlyHint:    true,
				IdempotentHint:  true,
				DestructiveHint: &no,
				OpenWorldHint:   &no,
			},
		}, s.handler(t))
	}
	s.addResources(srv)
	return srv
}

// handler runs one tool call. Every failure is a tool error the model
// can read and act on, never a protocol error.
func (s *mcpServer) handler(t toolSpec) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (res *mcp.CallToolResult, err error) {
		// The SDK runs a handler in its own goroutine and does not
		// recover a panic, so one bad call would end the server and every
		// call in flight. A panic is a tool error instead, and logged.
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("call panicked", "tool", t.name, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				res, err = &mcp.CallToolResult{IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: t.name + ": internal error; the server log has the detail"}}}, nil
			}
		}()
		start := time.Now()
		var raw []byte
		if req.Params != nil {
			raw = req.Params.Arguments
		}
		var a *toolArgs
		a, err = parseArgs(t.name, t.params, raw)
		var out toolOutput
		if err == nil {
			if t.family != nil {
				out, err = s.runFamily(ctx, t.name, t.family, a)
			} else {
				out, err = t.run(ctx, a)
			}
		}
		s.logCall(t.name, a, out, err, time.Since(start))
		if err != nil {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: s.scrubPaths(err.Error())}},
			}, nil
		}
		res = &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out.text}}}
		if out.structured != nil {
			res.StructuredContent = out.structured
		}
		return res, nil
	}
}

// logCall records a call's shape — tool, view, which window bounds were
// given, format, outcome, duration — and never a cell value. Filter
// values, which can name a merchant or an account, are logged at debug
// level only.
func (s *mcpServer) logCall(tool string, a *toolArgs, out toolOutput, err error, took time.Duration) {
	attrs := []any{"tool", tool, "ms", took.Milliseconds()}
	if a != nil {
		if v := a.str("view"); v != "" {
			attrs = append(attrs, "view", v)
		}
		var window []string
		for _, k := range []string{"from", "to", "as_of"} {
			if a.has(k) {
				window = append(window, k)
			}
		}
		if len(window) > 0 {
			attrs = append(attrs, "window", strings.Join(window, "+"))
		}
		if f := a.str("format"); f != "" {
			attrs = append(attrs, "format", f)
		}
	}
	if err != nil {
		s.log.Info("call failed", append(attrs, "error", s.scrubPaths(err.Error()))...)
		return
	}
	s.log.Info("call", append(attrs, "bytes", len(out.text))...)
	if a != nil && s.log.Enabled(context.Background(), slog.LevelDebug) {
		s.log.Debug("call detail", "tool", tool, "args", a.values)
	}
}

// errGoldMissing and its siblings are the gold-side failures a call
// can meet, each with the action that clears it.
var (
	errGoldMissing = errors.New("there is no database yet: run 'wealthdb init' and 'wealthdb load -a' first")
	errGoldBusy    = errors.New("the database is locked by another wealthdb command (a load, say); try again in a minute")
	errGoldStale   = errors.New("the MCP server is older than the database it reads; run 'wealthdb mcp restart'")
	// errConfigHidden stands in for a config error on the privacy
	// endpoint: validation messages quote the config, and the config
	// names accounts and portfolios.
	errConfigHidden = errors.New("wealthdb.cfg does not load; the server log has the reason")
)

// loadConfig reads the config for a call. On the privacy endpoint a
// failure is reported without its text, which the log keeps.
func (s *mcpServer) loadConfig() (*config.Config, error) {
	cfg, err := config.Load(s.configPath)
	if err != nil && s.privacy {
		s.log.Warn("the config does not load", "error", s.scrubPaths(err.Error()))
		return nil, errConfigHidden
	}
	return cfg, err
}

// withGold runs fn on a read-only gold handle opened for it alone and
// closed when it returns: a `wealthdb load` waits at most that long. An
// open that meets a writer retries on the write side's ladder before
// reporting gold busy.
func (s *mcpServer) withGold(ctx context.Context, cfg *config.Config, fn func(*sql.DB) error) error {
	if _, err := os.Stat(cfg.GoldDB); errors.Is(err, os.ErrNotExist) {
		return errGoldMissing
	}
	db, err := retryGoldLock(ctx, func() (*sql.DB, error) { return gold.OpenReadOnlyWith(cfg.GoldDB, s.duckdb) })
	switch {
	case err == nil:
	case errors.Is(err, gold.ErrStaleBinary):
		return errGoldStale
	case isGoldLockConflict(err):
		return errGoldBusy
	default:
		return fmt.Errorf("could not open the database: %w", err)
	}
	defer db.Close()
	// Open's own staleness check needs a build stamp the image binary
	// does not carry; the schema version does not.
	if err := gold.CheckSchemaKnown(ctx, db); errors.Is(err, gold.ErrStaleBinary) {
		return errGoldStale
	}
	return fn(db)
}

// scrubPaths takes the config file's path and the paths it names out
// of a message bound for the model: a path names the machine's user and
// layout, and is no help to an answer.
func (s *mcpServer) scrubPaths(msg string) string {
	paths := map[string]string{s.configPath: "<config>"}
	if cfg, err := config.Load(s.configPath); err == nil {
		paths[cfg.GoldDB] = "<gold>"
		for _, src := range cfg.SilverSources {
			paths[src.Path] = "<silver " + src.ID + ">"
		}
	}
	for p, label := range paths {
		if p != "" {
			msg = strings.ReplaceAll(msg, p, label)
		}
	}
	return msg
}

const mcpServeUsage = `usage: wealthdb mcp-serve --stdio [--privacy]
       wealthdb mcp-serve --http ADDR (--token-file FILE | --auth none --insecure) [--allow-host HOST]
                          [--all-interfaces]

Serve the read-only reports to AI agents over the Model Context
Protocol. 'wealthdb mcp' runs this in a container and manages it; run it
directly only from a binary built on the host. Every call opens the
database read-only for its own length, so the answers are as fresh as
the last load and a load is never locked out for long.

--stdio serves one session on stdin and stdout, for a client that
starts the server itself. --http serves streamable HTTP with two
endpoints: /mcp with full data and /mcp/privacy with amounts,
identifiers and names redacted. HTTP requires the bearer token in the
file --token-file names; --auth none turns it off and is refused
without --insecure. Requests must come with a loopback Host (or one
--allow-host names) and no foreign Origin.

Flags:
  --stdio               serve one session on stdin/stdout
  --http ADDR           serve streamable HTTP on ADDR, e.g. 127.0.0.1:3300
  --all-interfaces      let ADDR be beyond loopback, e.g. :3300 inside a container
                        whose publish is loopback-only (wealthdb mcp does this)
  --privacy             with --stdio: redact as -p does
  --token-file FILE     the bearer token (at least 16 characters)
  --auth token|none     HTTP authentication (default token)
  --insecure            acknowledge --auth none
  --allow-host HOST     answer to this Host as well as loopback (repeatable)
  --rows N              default rows per result (default 100, or WEALTHDB_MCP_ROWS)
  --max-rows N          most rows one call returns; 0 for no ceiling
                        (default 0, or WEALTHDB_MCP_MAX_ROWS)
  --memory-limit SIZE   DuckDB memory limit per call (default 2GB)
  --threads N           DuckDB threads per call (default 4)
  --temp-dir DIR        DuckDB spill directory (default $TMPDIR/wealthdb-mcp)
  -v                    log call detail, filter values included`
