package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// The forge's HTTP shell: a faithful port of the reference app.ts — one
// server routing git smart HTTP, REST v3, GraphQL v4 and MCP over one store,
// with the protocol discipline the suite checks. A single mutex serializes
// requests, matching the reference's single-threaded event-loop semantics.

const jsonType = "application/json; charset=utf-8"
const maxBodyBytes = 256 * 1024 * 1024

type app struct {
	store      *Store
	git        *GitHttp
	token      string
	requestSeq int
	mu         sync.Mutex
}

func newApp(token string) *app {
	store := NewStore()
	return &app{store: store, git: NewGitHttp(store), token: token}
}

func (a *app) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	if header == "" {
		return false
	}
	if a.token == "" {
		return true
	}
	return header == "Bearer "+a.token || header == "token "+a.token
}

func sendBuffer(w http.ResponseWriter, status int, contentType string, body []byte, headers map[string]string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	for k, v := range headers {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	w.Write(body)
}

func sendJson(w http.ResponseWriter, status int, body JVal, headers map[string]string) {
	if status == 204 {
		h := w.Header()
		for k, v := range headers {
			h.Set(k, v)
		}
		w.WriteHeader(204)
		return
	}
	text := stringifyJSON(body) + "\n"
	sendBuffer(w, status, jsonType, []byte(text), headers)
}

func readBody(r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBodyBytes {
		return nil, fmt.Errorf("request body is too large")
	}
	return data, nil
}

var fastIssueRe = regexp.MustCompile(`^/api/v3/repos/([^/]+)/([^/]+)/issues/(\d+)$`)

func splitPath(path string) []string {
	segments := []string{}
	for _, piece := range strings.Split(path, "/") {
		if piece == "" {
			continue
		}
		if decoded, err := url.PathUnescape(piece); err == nil {
			segments = append(segments, decoded)
		} else {
			segments = append(segments, piece)
		}
	}
	return segments
}

func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	defer func() {
		if rec := recover(); rec != nil {
			// A handler failure must never take the forge down.
			func() {
				defer func() { _ = recover() }()
				sendJson(w, 500, obj("message", "Internal Server Error"), nil)
			}()
		}
	}()
	a.handle(w, r)
}

func (a *app) handle(w http.ResponseWriter, r *http.Request) {
	method := r.Method
	rawURI := r.RequestURI
	pathPart := rawURI
	rawQuery := ""
	if idx := strings.IndexByte(rawURI, '?'); idx >= 0 {
		pathPart = rawURI[:idx]
		rawQuery = rawURI[idx+1:]
	}

	// The single-issue read accelerator (see rest.ts's serialized-issue memo).
	if method == "GET" {
		if match := fastIssueRe.FindStringSubmatch(rawURI); match != nil && a.authorized(r) {
			number, _ := strconv.Atoi(match[3])
			if text := fastIssueGetText(a.store, match[1], match[2], number); text != nil {
				a.requestSeq++
				requestId := fmt.Sprintf("F%06x", a.requestSeq)
				sendBuffer(w, 200, jsonType, []byte(*text), map[string]string{"x-github-request-id": requestId})
				return
			}
		}
	}

	segments := splitPath(pathPart)
	path := pathPart

	if method == "GET" && path == "/healthz" {
		sendJson(w, 200, obj("ok", true), nil)
		return
	}

	// Git smart HTTP: /<owner>/<repo>.git/...
	if len(segments) >= 2 && strings.HasSuffix(segments[1], ".git") {
		owner := segments[0]
		repoName := strings.TrimSuffix(segments[1], ".git")
		tail := strings.Join(segments[2:], "/")
		if !a.authorized(r) {
			sendBuffer(w, 401, "text/plain; charset=utf-8", []byte("authentication required\n"), map[string]string{"www-authenticate": "Bearer"})
			return
		}
		query := parseQuery(rawQuery)
		if method == "GET" && tail == "info/refs" {
			protocolV2 := strings.Contains(r.Header.Get("Git-Protocol"), "version=2")
			result := a.git.infoRefs(owner, repoName, query.get("service"), protocolV2)
			sendBuffer(w, result.Status, result.ContentType, result.Body, result.Headers)
			return
		}
		if method == "POST" && tail == "git-upload-pack" {
			body, err := readBody(r)
			if err != nil {
				panic(err)
			}
			result := a.git.uploadPack(owner, repoName, body)
			sendBuffer(w, result.Status, result.ContentType, result.Body, result.Headers)
			return
		}
		if method == "POST" && tail == "git-receive-pack" {
			body, err := readBody(r)
			if err != nil {
				panic(err)
			}
			result := a.git.receivePack(owner, repoName, body)
			sendBuffer(w, result.Status, result.ContentType, result.Body, result.Headers)
			return
		}
		sendJson(w, 404, obj("message", "Not Found"), nil)
		return
	}

	// GraphQL v4.
	if path == "/api/graphql" {
		if method != "POST" {
			sendJson(w, 404, obj("message", "Not Found"), nil)
			return
		}
		if !a.authorized(r) {
			sendJson(w, 401, obj("message", "Bad credentials"), nil)
			return
		}
		raw, err := readBody(r)
		if err != nil {
			panic(err)
		}
		var parsed any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			sendJson(w, 400, obj("errors", arr(obj("message", "Problems parsing JSON"))), nil)
			return
		}
		runOne := func(entry any) JVal {
			item, _ := entry.(map[string]any)
			if item == nil {
				item = map[string]any{}
			}
			queryText, ok := item["query"].(string)
			if !ok {
				return obj("data", nil, "errors", arr(obj("message", "A query is required")))
			}
			variables, _ := item["variables"].(map[string]any)
			if variables == nil {
				variables = map[string]any{}
			}
			var operationName *string
			if s, ok := item["operationName"].(string); ok {
				operationName = &s
			}
			return executeGraphQL(a.store, GqlRequest{Query: queryText, Variables: variables, OperationName: operationName})
		}
		if list, ok := parsed.([]any); ok {
			results := JArr{}
			for _, entry := range list {
				results.Items = append(results.Items, runOne(entry))
			}
			sendJson(w, 200, results, nil)
			return
		}
		sendJson(w, 200, runOne(parsed), nil)
		return
	}

	// MCP.
	if path == "/mcp" {
		if method == "DELETE" {
			w.WriteHeader(200)
			return
		}
		if method != "POST" {
			sendJson(w, 405, obj("message", "Method Not Allowed"), nil)
			return
		}
		if !a.authorized(r) {
			sendJson(w, 401, obj(
				"jsonrpc", "2.0",
				"id", nil,
				"error", obj("code", -32001, "message", "authentication required"),
			), nil)
			return
		}
		raw, err := readBody(r)
		if err != nil {
			panic(err)
		}
		result := handleMcp(a.store, raw)
		if result.Body == nil {
			w.WriteHeader(result.Status)
			return
		}
		sendJson(w, result.Status, result.Body, nil)
		return
	}

	// REST v3.
	if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		a.requestSeq++
		requestId := fmt.Sprintf("F%06x", a.requestSeq)
		restSegments := segments[2:]
		isPublic := method == "GET" && (len(restSegments) == 0 || len(restSegments) == 1 && restSegments[0] == "meta")
		if !isPublic && !a.authorized(r) {
			sendJson(w, 401, obj("message", "Bad credentials"), map[string]string{"x-github-request-id": requestId})
			return
		}
		var body any
		bodyMalformed := false
		if method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE" {
			raw, err := readBody(r)
			if err != nil {
				panic(err)
			}
			if len(raw) > 0 {
				var decoded any
				if err := json.Unmarshal(raw, &decoded); err != nil {
					bodyMalformed = true
				} else {
					body = decoded
				}
			}
		}
		host := r.Host
		ctx := &RestCtx{
			Store:         a.store,
			Method:        method,
			Segments:      restSegments,
			Query:         parseQuery(rawQuery),
			Body:          body,
			BodyMalformed: bodyMalformed,
			Origin:        "http://" + host,
			Path:          path,
		}
		result := handleRest(ctx)
		headers := map[string]string{"x-github-request-id": requestId}
		for k, v := range result.Headers {
			headers[k] = v
		}
		if result.Text != nil {
			sendBuffer(w, result.Status, jsonType, []byte(*result.Text), headers)
			return
		}
		sendJson(w, result.Status, result.Body, headers)
		return
	}

	sendJson(w, 404, obj("message", "Not Found"), nil)
}
