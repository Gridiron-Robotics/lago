package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// This file adds the uniform "Gateway HTTP Contract v1" (aka Contract A) surface
// on top of the same read-only tools the stdio server exposes. It reuses the
// Server's tool registry and handlers verbatim, so the read-only invariant
// (enforced by repo-gates/mcp-gate.sh) still holds: every tool reaches Lago only
// through LagoClient's GET-only choke point.
//
// Routes:
//
//	GET  /tools?server=<name>  -> {"tools":[{name,description,input_schema,annotations}]}
//	POST /invoke               -> {"tool":<name>,"result":...} (+ "replayed":true on replay)
//	HEAD /                     -> 200 (health)
//
// A Bearer token is required at the boundary on /tools and /invoke. LAGO_API_KEY
// remains the downstream credential; the boundary token is the caller's identity
// (a Keycloak JWT in the fleet) and is not validated here beyond presence.

// contractCatalog returns tool entries in the Contract A shape: input_schema
// (snake_case, unlike the JSON-RPC inputSchema) plus annotations. Every Lago tool
// is read-only, so destructiveHint is always false.
func (s *Server) contractCatalog() []map[string]any {
	out := make([]map[string]any, 0, len(s.order))
	for _, name := range s.order {
		t := s.tools[name]
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"name":         t.Name,
			"description":  t.Description,
			"input_schema": schema,
			"annotations":  map[string]any{"destructiveHint": false},
		})
	}
	return out
}

// invokeTool dispatches one tool by name. ok=false means the tool is unknown.
func (s *Server) invokeTool(ctx context.Context, name string, args map[string]any) (text string, ok bool, err error) {
	t, found := s.tools[name]
	if !found {
		return "", false, nil
	}
	if args == nil {
		args = map[string]any{}
	}
	out, herr := t.Handler(ctx, args)
	return out, true, herr
}

// httpAdapter carries the per-server idempotency cache for the HTTP transport.
type httpAdapter struct {
	srv    *Server
	mu     sync.Mutex
	replay map[[2]string]map[string]any // (tenant, idempotency-key) -> payload
}

// HTTPHandler builds a Contract A http.Handler backed by this server's tools.
func (s *Server) HTTPHandler() http.Handler {
	a := &httpAdapter{srv: s, replay: make(map[[2]string]map[string]any)}
	mux := http.NewServeMux()
	mux.HandleFunc("/tools", a.handleTools)
	mux.HandleFunc("/invoke", a.handleInvoke)
	mux.HandleFunc("/", a.handleRoot)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// hasBearer reports whether the request carries an Authorization: Bearer header.
func hasBearer(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	return len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") && strings.TrimSpace(h[7:]) != ""
}

func (a *httpAdapter) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead || (r.Method == http.MethodGet && r.URL.Path == "/") {
		w.WriteHeader(http.StatusOK)
		return
	}
	writeErr(w, http.StatusNotFound, "not found: "+r.URL.Path)
}

func (a *httpAdapter) handleTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !hasBearer(r) {
		writeErr(w, http.StatusUnauthorized, "missing bearer token")
		return
	}
	server := r.URL.Query().Get("server")
	if server != "" && server != a.srv.name {
		writeJSON(w, http.StatusOK, map[string]any{"tools": []any{}}) // unknown server -> empty
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": a.srv.contractCatalog()})
}

func (a *httpAdapter) handleInvoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !hasBearer(r) {
		writeErr(w, http.StatusUnauthorized, "missing bearer token")
		return
	}
	var body struct {
		Server    string         `json:"server"`
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
		Args      map[string]any `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Server != "" && body.Server != a.srv.name {
		writeErr(w, http.StatusNotFound, "unknown server: "+body.Server)
		return
	}
	if body.Tool == "" {
		writeErr(w, http.StatusBadRequest, "tool is required")
		return
	}
	args := body.Arguments
	if args == nil {
		args = body.Args
	}

	tenant := r.Header.Get("X-Tenant-Id")
	if tenant == "" {
		tenant = "default"
	}
	idem := r.Header.Get("Idempotency-Key")
	key := [2]string{tenant, idem}
	if idem != "" {
		a.mu.Lock()
		if cached, ok := a.replay[key]; ok {
			a.mu.Unlock()
			replayed := make(map[string]any, len(cached)+1)
			for k, v := range cached {
				replayed[k] = v
			}
			replayed["replayed"] = true
			writeJSON(w, http.StatusOK, replayed)
			return
		}
		a.mu.Unlock()
	}

	text, ok, err := a.srv.invokeTool(r.Context(), body.Tool, args)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown tool: "+body.Tool)
		return
	}
	if err != nil {
		// Structured error back to the agent — never a 2xx with a failure inside.
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Handlers return raw JSON from Lago; embed it verbatim when valid so the
	// result is a real object, not a JSON-encoded string.
	var result any
	if json.Valid([]byte(text)) {
		result = json.RawMessage(text)
	} else {
		result = text
	}
	payload := map[string]any{"tool": body.Tool, "result": result}
	if idem != "" {
		a.mu.Lock()
		a.replay[key] = payload
		a.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, payload)
}
