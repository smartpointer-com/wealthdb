package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "0123456789abcdef0123456789abcdef"

// post sends one JSON-RPC request to the test server and returns the
// status and body.
func post(t *testing.T, srv *httptest.Server, path, auth string, header map[string]string, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	for k, v := range header {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const listTools = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

// TestHTTPGuards pins the front door: the token, the Host and Origin
// checks, and the two endpoints.
func TestHTTPGuards(t *testing.T) {
	t.Parallel()
	cfg := setupCashflowGold(t)
	full := newTestMCP(cfg, false)
	priv := newTestMCP(cfg, true)
	srv := httptest.NewServer(newHTTPFront(full, priv, testToken, []string{"wealthdb-mcp"}))
	defer srv.Close()
	bearer := "Bearer " + testToken

	cases := []struct {
		name   string
		path   string
		auth   string
		header map[string]string
		want   int
	}{
		{"no token", "/mcp", "", nil, http.StatusUnauthorized},
		{"wrong token", "/mcp", "Bearer nope", nil, http.StatusUnauthorized},
		{"basic auth", "/mcp", "Basic " + testToken, nil, http.StatusUnauthorized},
		{"token", "/mcp", bearer, nil, http.StatusOK},
		{"token, lower-case scheme", "/mcp", "bearer " + testToken, nil, http.StatusOK},
		{"privacy endpoint", "/mcp/privacy", bearer, nil, http.StatusOK},
		{"other path", "/", bearer, nil, http.StatusNotFound},
		{"foreign Host", "/mcp", bearer, map[string]string{"Host": "attacker.example:3300"}, http.StatusForbidden},
		{"allowed Host", "/mcp", bearer, map[string]string{"Host": "wealthdb-mcp:3300"}, http.StatusOK},
		{"localhost Host", "/mcp", bearer, map[string]string{"Host": "localhost:3300"}, http.StatusOK},
		{"IPv6 loopback Host", "/mcp", bearer, map[string]string{"Host": "[::1]:3300"}, http.StatusOK},
		{"foreign Origin", "/mcp", bearer, map[string]string{"Origin": "https://attacker.example"}, http.StatusForbidden},
		{"null Origin", "/mcp", bearer, map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"loopback Origin", "/mcp", bearer, map[string]string{"Origin": "http://127.0.0.1:8080"}, http.StatusOK},
	}
	for _, c := range cases {
		code, body := post(t, srv, c.path, c.auth, c.header, listTools)
		if code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.name, code, c.want, strings.TrimSpace(body))
		}
	}
	// The challenge names the scheme, so a client knows what to send.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(listTools))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q", got)
	}
}

// TestHTTPPrivacyEndpointRedacts: the same call on the two endpoints,
// amounts on one and placeholders on the other.
func TestHTTPPrivacyEndpointRedacts(t *testing.T) {
	t.Parallel()
	cfg := setupCashflowGold(t)
	srv := httptest.NewServer(newHTTPFront(newTestMCP(cfg, false), newTestMCP(cfg, true), testToken, nil))
	defer srv.Close()
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"transactions",
		"arguments":{"from":"2026-05-01","to":"2026-06-30"}}}`
	text := func(path string) string {
		code, body := post(t, srv, path, "Bearer "+testToken, nil, call)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", path, code, body)
		}
		var r struct {
			Result struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(body), &r); err != nil || len(r.Result.Content) == 0 {
			t.Fatalf("%s: %v: %s", path, err, body)
		}
		return r.Result.Content[0].Text
	}
	if full := text("/mcp"); !strings.Contains(full, "6000.00") || strings.Contains(full, "*****.**") {
		t.Errorf("/mcp is not full data:\n%s", full)
	}
	if priv := text("/mcp/privacy"); strings.Contains(priv, "6000.00") || !strings.Contains(priv, "*****.**") ||
		!strings.Contains(priv, "redacted") {
		t.Errorf("/mcp/privacy is not redacted:\n%s", priv)
	}
}

// TestHTTPNoAuth: with the token off, the Host guard still stands.
func TestHTTPNoAuth(t *testing.T) {
	t.Parallel()
	cfg := setupCashflowGold(t)
	srv := httptest.NewServer(newHTTPFront(newTestMCP(cfg, false), newTestMCP(cfg, true), "", nil))
	defer srv.Close()
	if code, body := post(t, srv, "/mcp", "", nil, listTools); code != http.StatusOK {
		t.Errorf("no auth: status %d: %s", code, body)
	}
	if code, _ := post(t, srv, "/mcp", "", map[string]string{"Host": "rebound.example"}, listTools); code != http.StatusForbidden {
		t.Errorf("no auth, foreign Host: status %d, want 403", code)
	}
}
