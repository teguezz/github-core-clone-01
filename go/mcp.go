package main

import (
	"encoding/json"
	"fmt"
	"math"
)

// The MCP endpoint (/mcp): a faithful port of the reference mcp.ts —
// stateless streamable-HTTP JSON-RPC speaking both protocol eras.

var mcpServerInfo = obj("name", "github-core-clone", "version", "1.0.0")
var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

func objectSchema(properties JObj, required []string) JObj {
	req := JArr{}
	for _, r := range required {
		req.Items = append(req.Items, JStr(r))
	}
	return obj(
		"type", "object",
		"properties", properties,
		"required", req,
		"additionalProperties", false,
	)
}

func stringProp(description string) JObj {
	return obj("type", "string", "description", description)
}

func intProp(description string) JObj {
	return obj("type", "integer", "description", description)
}

func boolProp(description string) JObj {
	return obj("type", "boolean", "description", description)
}

var mcpTools = []JObj{
	obj("name", "create_repo", "description", "Create a new repository for the authenticated user.", "inputSchema", objectSchema(obj(
		"name", stringProp("The repository name."),
		"description", stringProp("A short description of the repository."),
		"private", boolProp("Whether the repository is private."),
	), []string{"name"})),
	obj("name", "get_repo", "description", "Get a repository by owner and name.", "inputSchema", objectSchema(obj(
		"owner", stringProp("The repository owner login."),
		"repo", stringProp("The repository name."),
	), []string{"owner", "repo"})),
	obj("name", "create_issue", "description", "Create an issue in a repository.", "inputSchema", objectSchema(obj(
		"owner", stringProp("The repository owner login."),
		"repo", stringProp("The repository name."),
		"title", stringProp("The issue title."),
		"body", stringProp("The issue body."),
	), []string{"owner", "repo", "title"})),
	obj("name", "list_issues", "description", "List the issues of a repository, optionally filtered by state (open, closed, all).", "inputSchema", objectSchema(obj(
		"owner", stringProp("The repository owner login."),
		"repo", stringProp("The repository name."),
		"state", stringProp("Issue state filter: open, closed or all."),
	), []string{"owner", "repo"})),
	obj("name", "get_issue", "description", "Get one issue of a repository by its number.", "inputSchema", objectSchema(obj(
		"owner", stringProp("The repository owner login."),
		"repo", stringProp("The repository name."),
		"issue_number", intProp("The issue number."),
	), []string{"owner", "repo", "issue_number"})),
	obj("name", "create_pull_request", "description", "Create a pull request from a head branch into a base branch.", "inputSchema", objectSchema(obj(
		"owner", stringProp("The repository owner login."),
		"repo", stringProp("The repository name."),
		"title", stringProp("The pull request title."),
		"body", stringProp("The pull request body."),
		"head", stringProp("The head branch name."),
		"base", stringProp("The base branch name."),
	), []string{"owner", "repo", "title", "head", "base"})),
	obj("name", "list_pull_requests", "description", "List the pull requests of a repository, optionally filtered by state.", "inputSchema", objectSchema(obj(
		"owner", stringProp("The repository owner login."),
		"repo", stringProp("The repository name."),
		"state", stringProp("Pull request state filter: open, closed or all."),
	), []string{"owner", "repo"})),
	obj("name", "merge_pull_request", "description", "Merge a pull request by number.", "inputSchema", objectSchema(obj(
		"owner", stringProp("The repository owner login."),
		"repo", stringProp("The repository name."),
		"pull_number", intProp("The pull request number."),
	), []string{"owner", "repo", "pull_number"})),
}

type McpResult struct {
	Status int
	Body   JVal // nil: no body
}

type toolOutcome struct {
	value   JVal
	text    *string // when the value is a plain string
	isError bool
}

// jsArgNumber mirrors `typeof v === 'number' ? v : Number(v)` for tool args.
func jsArgNumber(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case string:
		return jsNumberLoose(n)
	case bool:
		if n {
			return 1
		}
		return 0
	case nil:
		return 0
	}
	return math.NaN()
}

// jsNumberLoose is JS Number(string): empty/blank is 0, junk is NaN.
func jsNumberLoose(s string) float64 {
	trimmed := trimJSWhitespace(s)
	if trimmed == "" {
		return 0
	}
	return jsParseNumber(trimmed)
}

func trimJSWhitespace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 {
		c := s[len(s)-1]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			s = s[:len(s)-1]
		} else {
			break
		}
	}
	return s
}

func callTool(store *Store, name string, args map[string]any) (*toolOutcome, bool) {
	repoOf := func() (*Repo, error) {
		owner := inputStr(args, "owner")
		repoName := inputStr(args, "repo")
		repo := store.RepoOf(owner, repoName)
		if repo == nil {
			return nil, domainError(404, "repository %s/%s not found", owner, repoName)
		}
		return repo, nil
	}
	need := func(key string) (string, error) {
		v, ok := args[key].(string)
		if !ok || v == "" {
			return "", domainError(422, "missing required argument: %s", key)
		}
		return v, nil
	}
	fail := func(err error) (*toolOutcome, bool) {
		if de, ok := err.(*DomainError); ok {
			return &toolOutcome{value: JStr(de.Message), isError: true}, true
		}
		panic(err)
	}

	switch name {
	case "create_repo":
		repoName, err := need("name")
		if err != nil {
			return fail(err)
		}
		if _, exists := store.Repos[store.Viewer.Login+"/"+repoName]; exists {
			return &toolOutcome{value: JStr(fmt.Sprintf("repository %s already exists", repoName)), isError: true}, true
		}
		repo := store.CreateRepo(repoName, inputStr(args, "description"), args["private"] == true)
		return &toolOutcome{value: repoJson(store, repo)}, true
	case "get_repo":
		if _, err := need("owner"); err != nil {
			return fail(err)
		}
		if _, err := need("repo"); err != nil {
			return fail(err)
		}
		repo, err := repoOf()
		if err != nil {
			return fail(err)
		}
		return &toolOutcome{value: repoJson(store, repo)}, true
	case "create_issue":
		if _, err := need("owner"); err != nil {
			return fail(err)
		}
		if _, err := need("repo"); err != nil {
			return fail(err)
		}
		title, err := need("title")
		if err != nil {
			return fail(err)
		}
		repo, err := repoOf()
		if err != nil {
			return fail(err)
		}
		issue, err := createIssue(store, repo, map[string]any{"title": title, "body": inputStr(args, "body")})
		if err != nil {
			return fail(err)
		}
		return &toolOutcome{value: issueJson(store, repo, issue)}, true
	case "list_issues":
		if _, err := need("owner"); err != nil {
			return fail(err)
		}
		if _, err := need("repo"); err != nil {
			return fail(err)
		}
		repo, err := repoOf()
		if err != nil {
			return fail(err)
		}
		state := "open"
		if s, ok := args["state"].(string); ok {
			state = s
		}
		issues := []*IssueRec{}
		for _, thread := range repo.threadsInOrder() {
			if thread.Kind == "issue" && (state == "all" || thread.Issue.State == state) {
				issues = append(issues, thread.Issue)
			}
		}
		sortIssuesByNumber(issues)
		items := JArr{}
		for _, issue := range issues {
			items.Items = append(items.Items, issueJson(store, repo, issue))
		}
		return &toolOutcome{value: items}, true
	case "get_issue":
		if _, err := need("owner"); err != nil {
			return fail(err)
		}
		if _, err := need("repo"); err != nil {
			return fail(err)
		}
		repo, err := repoOf()
		if err != nil {
			return fail(err)
		}
		number := jsArgNumber(args["issue_number"])
		thread := lookupThread(repo, number)
		if thread == nil || thread.Kind != "issue" {
			return fail(domainError(404, "issue %s not found", jsFormatNumber(number)))
		}
		return &toolOutcome{value: issueJson(store, repo, thread.Issue)}, true
	case "create_pull_request":
		if _, err := need("owner"); err != nil {
			return fail(err)
		}
		if _, err := need("repo"); err != nil {
			return fail(err)
		}
		repo, err := repoOf()
		if err != nil {
			return fail(err)
		}
		title, err := need("title")
		if err != nil {
			return fail(err)
		}
		head, err := need("head")
		if err != nil {
			return fail(err)
		}
		base, err := need("base")
		if err != nil {
			return fail(err)
		}
		pull, err := createPull(store, repo, map[string]any{
			"title": title, "body": inputStr(args, "body"), "head": head, "base": base,
		})
		if err != nil {
			return fail(err)
		}
		v := pullMergeable(repo, pull)
		return &toolOutcome{value: pullJson(store, repo, pull, &v)}, true
	case "list_pull_requests":
		if _, err := need("owner"); err != nil {
			return fail(err)
		}
		if _, err := need("repo"); err != nil {
			return fail(err)
		}
		repo, err := repoOf()
		if err != nil {
			return fail(err)
		}
		state := "open"
		if s, ok := args["state"].(string); ok {
			state = s
		}
		pulls := []*PullRec{}
		for _, thread := range repo.threadsInOrder() {
			if thread.Kind == "pull" && (state == "all" || thread.Pull.State == state) {
				pulls = append(pulls, thread.Pull)
			}
		}
		sortPullsByNumber(pulls)
		items := JArr{}
		for _, pull := range pulls {
			items.Items = append(items.Items, pullJson(store, repo, pull, nil))
		}
		return &toolOutcome{value: items}, true
	case "merge_pull_request":
		if _, err := need("owner"); err != nil {
			return fail(err)
		}
		if _, err := need("repo"); err != nil {
			return fail(err)
		}
		repo, err := repoOf()
		if err != nil {
			return fail(err)
		}
		number := jsArgNumber(args["pull_number"])
		thread := lookupThread(repo, number)
		if thread == nil || thread.Kind != "pull" {
			return fail(domainError(404, "pull request %s not found", jsFormatNumber(number)))
		}
		sha, err := mergePull(store, repo, thread.Pull, "merge")
		if err != nil {
			return fail(err)
		}
		return &toolOutcome{value: obj("merged", true, "sha", sha, "message", "Pull Request successfully merged")}, true
	}
	return nil, false
}

func lookupThread(repo *Repo, number float64) *ThreadRec {
	if math.IsNaN(number) || number != math.Trunc(number) || number > 1e15 || number < -1e15 {
		return nil
	}
	return repo.Threads[int(number)]
}

func sortIssuesByNumber(issues []*IssueRec) {
	for i := 1; i < len(issues); i++ {
		for j := i; j > 0 && issues[j].Number < issues[j-1].Number; j-- {
			issues[j], issues[j-1] = issues[j-1], issues[j]
		}
	}
}

func sortPullsByNumber(pulls []*PullRec) {
	for i := 1; i < len(pulls); i++ {
		for j := i; j > 0 && pulls[j].Number < pulls[j-1].Number; j-- {
			pulls[j], pulls[j-1] = pulls[j-1], pulls[j]
		}
	}
}

func handleMcp(store *Store, rawBody []byte) McpResult {
	var parsed any
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		return McpResult{Status: 400, Body: obj(
			"jsonrpc", "2.0",
			"id", nil,
			"error", obj("code", -32700, "message", "Parse error"),
		)}
	}
	request, ok := parsed.(map[string]any)
	if !ok {
		return McpResult{Status: 400, Body: obj(
			"jsonrpc", "2.0",
			"id", nil,
			"error", obj("code", -32600, "message", "Invalid Request"),
		)}
	}
	idValue, hasID := request["id"]
	if !hasID {
		return McpResult{Status: 202}
	}
	var id JVal = JNULL
	switch v := idValue.(type) {
	case string:
		id = JStr(v)
	case float64:
		id = JFloat(v)
	case bool:
		id = JBool(v)
	}
	method := inputStr(request, "method")
	params, _ := request["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	okResult := func(result JVal) McpResult {
		return McpResult{Status: 200, Body: obj("jsonrpc", "2.0", "id", id, "result", result)}
	}
	failResult := func(code int, message string) McpResult {
		return McpResult{Status: 200, Body: obj("jsonrpc", "2.0", "id", id, "error", obj("code", code, "message", message))}
	}

	switch method {
	case "server/discover":
		versions := JArr{}
		for _, v := range mcpSupportedVersions {
			versions.Items = append(versions.Items, JStr(v))
		}
		return okResult(obj(
			"resultType", "complete",
			"supportedVersions", versions,
			"capabilities", obj("tools", obj()),
			"serverInfo", mcpServerInfo,
		))
	case "initialize":
		requested := inputStr(params, "protocolVersion")
		protocolVersion := "2025-11-25"
		for _, v := range mcpSupportedVersions {
			if v == requested {
				protocolVersion = requested
				break
			}
		}
		return okResult(obj(
			"protocolVersion", protocolVersion,
			"capabilities", obj("tools", obj()),
			"serverInfo", mcpServerInfo,
		))
	case "ping":
		return okResult(obj())
	case "tools/list":
		tools := JArr{}
		for _, tool := range mcpTools {
			tools.Items = append(tools.Items, tool)
		}
		return okResult(obj("tools", tools))
	case "tools/call":
		toolName := inputStr(params, "name")
		toolArgs, _ := params["arguments"].(map[string]any)
		if toolArgs == nil {
			toolArgs = map[string]any{}
		}
		outcome, known := callTool(store, toolName, toolArgs)
		if !known {
			return failResult(-32602, fmt.Sprintf("Invalid params: unknown tool \"%s\"", toolName))
		}
		var text string
		if s, isStr := outcome.value.(JStr); isStr {
			text = string(s)
		} else {
			text = stringifyJSONPretty(outcome.value)
		}
		var structured JVal
		if arrValue, isArr := outcome.value.(JArr); isArr {
			structured = obj("items", arrValue)
		} else {
			structured = outcome.value
		}
		fields := []JField{
			{Key: "content", Val: arr(obj("type", "text", "text", text))},
			{Key: "structuredContent", Val: structured},
		}
		if outcome.isError {
			fields = append(fields, JField{Key: "isError", Val: JBool(true)})
		}
		return okResult(JObj{Fields: fields})
	default:
		return failResult(-32601, "Method not found: "+method)
	}
}
