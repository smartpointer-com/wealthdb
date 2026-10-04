package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP endpoints. One server answers both: privacy is a property of the
// URL a client is given, never a tool parameter, so a prompt-injected
// model cannot turn the redaction off.
const (
	endpointFull    = "/mcp"
	endpointPrivacy = "/mcp/privacy"
)

// serveHTTP serves the two endpoints on addr until ctx ends, then
// drains in-flight calls.
func serveHTTP(ctx context.Context, s *mcpServer, addr, token string, allowHosts []string) error {
	priv := *s
	priv.privacy = true
	front := newHTTPFront(s, &priv, token, allowHosts)
	srv := &http.Server{
		Addr:              addr,
		Handler:           front,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	auth := "token"
	if token == "" {
		auth = "none"
	}
	s.log.Info("serving MCP over HTTP", "addr", addr, "endpoints", endpointFull+" "+endpointPrivacy, "auth", auth)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// httpFront guards the endpoints: a loopback (or allowed) Host, no
// foreign Origin, and the bearer token. The Host and Origin checks are
// the DNS-rebinding guard the MCP specification asks of local servers;
// they matter most with the token off.
type httpFront struct {
	tokenSum   [sha256.Size]byte
	auth       bool
	allowHosts map[string]bool
	full, priv *dailyServer
}

func newHTTPFront(full, priv *mcpServer, token string, allowHosts []string) *httpFront {
	f := &httpFront{
		auth:       token != "",
		tokenSum:   sha256.Sum256([]byte(token)),
		allowHosts: map[string]bool{},
		full:       newDailyServer(full),
		priv:       newDailyServer(priv),
	}
	for _, h := range allowHosts {
		f.allowHosts[strings.ToLower(h)] = true
	}
	return f
}

func (f *httpFront) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var target *dailyServer
	switch r.URL.Path {
	case endpointFull:
		target = f.full
	case endpointPrivacy:
		target = f.priv
	default:
		http.Error(w, "not found: the MCP endpoints are "+endpointFull+" and "+endpointPrivacy, http.StatusNotFound)
		return
	}
	if host := hostOnly(r.Host); !f.hostAllowed(host) {
		http.Error(w, fmt.Sprintf("forbidden: Host %q is not a loopback name (see --allow-host)", r.Host), http.StatusForbidden)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !f.originAllowed(origin) {
		http.Error(w, fmt.Sprintf("forbidden: requests from Origin %q are refused", origin), http.StatusForbidden)
		return
	}
	if f.auth && !f.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="wealthdb"`)
		http.Error(w, "unauthorized: send 'Authorization: Bearer <token>'; 'wealthdb mcp url' prints it", http.StatusUnauthorized)
		return
	}
	target.handler.ServeHTTP(w, r)
}

// authorized compares the bearer token in constant time; hashing both
// sides first keeps the comparison's length fixed too.
func (f *httpFront) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(h[len(prefix):])))
	return subtle.ConstantTimeCompare(sum[:], f.tokenSum[:]) == 1
}

func (f *httpFront) hostAllowed(host string) bool {
	if host == "localhost" || f.allowHosts[host] {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (f *httpFront) originAllowed(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return f.hostAllowed(strings.ToLower(u.Hostname()))
}

// hostOnly is a Host header without its port or IPv6 brackets.
func hostOnly(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return strings.ToLower(strings.Trim(host, "[]"))
}

// dailyServer serves one endpoint, rebuilding its MCP server when the
// UTC date changes: the instructions open with today's date, and a
// server that runs for weeks must not tell a model it is still the day
// it started.
type dailyServer struct {
	s       *mcpServer
	handler *mcp.StreamableHTTPHandler

	mu  sync.Mutex
	day string
	srv *mcp.Server
}

func newDailyServer(s *mcpServer) *dailyServer {
	d := &dailyServer{s: s}
	// Stateless with JSON responses: no session ids, so a restarted
	// container needs no client to initialise again, and no event
	// stream, because the server never pushes.
	//
	// The SDK's own Host check is off because httpFront makes the same
	// check with the --allow-host names added; the SDK's would refuse
	// those whenever the server listens on loopback.
	d.handler = mcp.NewStreamableHTTPHandler(d.server, &mcp.StreamableHTTPOptions{
		Stateless:                  true,
		JSONResponse:               true,
		DisableLocalhostProtection: true,
	})
	return d
}

func (d *dailyServer) server(*http.Request) *mcp.Server {
	now := d.s.now().UTC()
	day := now.Format("2006-01-02")
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.srv == nil || d.day != day {
		d.srv, d.day = d.s.newServer(now), day
	}
	return d.srv
}
