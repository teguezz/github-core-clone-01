package main

import (
	"encoding/base64"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The REST v3 surface: a faithful port of the reference rest.ts, including
// GitHub-style pagination with Link headers and the serialized-issue memo
// behind the read hot path.

// --- Ordered query params with JS URLSearchParams semantics ---

type queryParams struct {
	pairs [][2]string
}

func parseQuery(raw string) *queryParams {
	q := &queryParams{}
	if raw == "" {
		return q
	}
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		key, value := part, ""
		if idx := strings.IndexByte(part, '='); idx >= 0 {
			key, value = part[:idx], part[idx+1:]
		}
		q.pairs = append(q.pairs, [2]string{formDecode(key), formDecode(value)})
	}
	return q
}

func formDecode(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '+':
			sb.WriteByte(' ')
		case '%':
			if i+2 < len(s) {
				if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
					sb.WriteByte(byte(v))
					i += 2
					continue
				}
			}
			sb.WriteByte(s[i])
		default:
			sb.WriteByte(s[i])
		}
	}
	return sb.String()
}

func formEncode(s string) string {
	var sb strings.Builder
	for _, b := range []byte(s) {
		switch {
		case b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '*' || b == '-' || b == '.' || b == '_':
			sb.WriteByte(b)
		case b == ' ':
			sb.WriteByte('+')
		default:
			fmt.Fprintf(&sb, "%%%02X", b)
		}
	}
	return sb.String()
}

func (q *queryParams) get(key string) *string {
	for _, pair := range q.pairs {
		if pair[0] == key {
			v := pair[1]
			return &v
		}
	}
	return nil
}

func (q *queryParams) set(key, value string) {
	found := false
	out := q.pairs[:0]
	for _, pair := range q.pairs {
		if pair[0] == key {
			if !found {
				out = append(out, [2]string{key, value})
				found = true
			}
			continue
		}
		out = append(out, pair)
	}
	if !found {
		out = append(out, [2]string{key, value})
	}
	q.pairs = out
}

func (q *queryParams) clone() *queryParams {
	out := &queryParams{pairs: append([][2]string{}, q.pairs...)}
	return out
}

func (q *queryParams) String() string {
	parts := make([]string, len(q.pairs))
	for i, pair := range q.pairs {
		parts[i] = formEncode(pair[0]) + "=" + formEncode(pair[1])
	}
	return strings.Join(parts, "&")
}

// --- REST plumbing ---

type RestCtx struct {
	Store         *Store
	Method        string
	Segments      []string
	Query         *queryParams
	Body          any
	BodyMalformed bool
	Origin        string
	Path          string
}

type RestResult struct {
	Status  int
	Body    JVal
	Text    *string
	Headers map[string]string
}

type RestError struct {
	Status  int
	Message string
}

func (e *RestError) Error() string { return e.Message }

func restError(status int, message string) *RestError {
	return &RestError{Status: status, Message: message}
}

func notFoundErr() *RestError { return restError(404, "Not Found") }

func bodyObject(ctx *RestCtx) (map[string]any, error) {
	if ctx.BodyMalformed {
		return nil, restError(400, "Problems parsing JSON")
	}
	m, ok := ctx.Body.(map[string]any)
	if !ok {
		return nil, restError(400, "Body should be a JSON object")
	}
	return m, nil
}

func optionalBody(ctx *RestCtx) (map[string]any, error) {
	if ctx.BodyMalformed {
		return nil, restError(400, "Problems parsing JSON")
	}
	m, ok := ctx.Body.(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}
	return m, nil
}

func repoOr404(ctx *RestCtx, owner, name string) (*Repo, error) {
	repo := ctx.Store.RepoOf(owner, name)
	if repo == nil {
		return nil, notFoundErr()
	}
	return repo, nil
}

func threadOr404(repo *Repo, number int) (*ThreadRec, error) {
	thread := repo.Threads[number]
	if thread == nil {
		return nil, notFoundErr()
	}
	return thread, nil
}

func jsParseNumber(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

func paginate[T any](ctx *RestCtx, items []T) ([]T, map[string]string) {
	perPage := 30
	if raw := ctx.Query.get("per_page"); raw != nil {
		v := jsParseNumber(*raw)
		if !math.IsNaN(v) && v > 0 {
			perPage = int(math.Min(100, math.Floor(v)))
		}
	}
	page := 1
	if raw := ctx.Query.get("page"); raw != nil {
		v := jsParseNumber(*raw)
		if !math.IsNaN(v) && v > 0 {
			page = int(math.Floor(v))
		}
	}
	totalPages := int(math.Max(1, math.Ceil(float64(len(items))/float64(perPage))))
	start := (page - 1) * perPage
	var slice []T
	if start < len(items) {
		end := start + perPage
		if end > len(items) {
			end = len(items)
		}
		slice = items[start:end]
	} else {
		slice = []T{}
	}
	urlFor := func(target int) string {
		params := ctx.Query.clone()
		params.set("page", strconv.Itoa(target))
		params.set("per_page", strconv.Itoa(perPage))
		return ctx.Origin + ctx.Path + "?" + params.String()
	}
	links := []string{}
	if page < totalPages {
		links = append(links, "<"+urlFor(page+1)+">; rel=\"next\"")
		links = append(links, "<"+urlFor(totalPages)+">; rel=\"last\"")
	}
	if page > 1 {
		links = append(links, "<"+urlFor(page-1)+">; rel=\"prev\"")
		links = append(links, "<"+urlFor(1)+">; rel=\"first\"")
	}
	headers := map[string]string{}
	if len(links) > 0 {
		headers["link"] = strings.Join(links, ", ")
	}
	return slice, headers
}

// --- The serialized-issue memo ---

type issueMemoEntry struct {
	fingerprint string
	text        string
}

var issueTextMemo = map[*Store]map[string]issueMemoEntry{}

func issueFingerprint(repo *Repo, issue *IssueRec) string {
	parts := []string{
		strconv.Itoa(issue.ID),
		strconv.Itoa(issue.Number),
		issue.State,
		strconv.FormatInt(issue.AtSec, 10),
		strconv.FormatInt(issue.UpdatedSec, 10),
		strconv.Itoa(len(issue.Comments)),
		issue.AuthorLogin,
		issue.Title,
		issue.Body,
	}
	fingerprint := strings.Join(parts, "|")
	for _, name := range issue.Labels {
		label := repo.Labels[name]
		color := ""
		description := ""
		if label != nil {
			color = label.Color
			if label.Description != nil {
				description = *label.Description
			}
		}
		fingerprint += "|" + name + "=" + color + ":" + description
	}
	return fingerprint
}

func issueGetText(store *Store, repo *Repo, issue *IssueRec) string {
	memo, ok := issueTextMemo[store]
	if !ok {
		memo = map[string]issueMemoEntry{}
		issueTextMemo[store] = memo
	}
	key := strconv.Itoa(repo.DatabaseID) + ":" + strconv.Itoa(issue.Number)
	fingerprint := issueFingerprint(repo, issue)
	if hit, ok := memo[key]; ok && hit.fingerprint == fingerprint {
		return hit.text
	}
	text := stringifyJSON(issueJson(store, repo, issue)) + "\n"
	if len(memo) >= 256 {
		for k := range memo {
			delete(memo, k)
		}
	}
	memo[key] = issueMemoEntry{fingerprint: fingerprint, text: text}
	return text
}

func fastIssueGetText(store *Store, owner, name string, number int) *string {
	repo := store.RepoOf(owner, name)
	if repo == nil {
		return nil
	}
	thread := repo.Threads[number]
	if thread == nil || thread.Kind != "issue" {
		return nil
	}
	text := issueGetText(store, repo, thread.Issue)
	return &text
}

// --- Contents helpers ---

func lookupPath(repo *Repo, treeOid *string, path string) *FileEntry {
	if treeOid == nil {
		return nil
	}
	flat := flattenTree(repo, *treeOid, "", nil)
	if entry, ok := flat[path]; ok {
		v := entry
		return &v
	}
	return nil
}

var shaLikeRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

func resolveRef(repo *Repo, ref *string) *string {
	if ref == nil || *ref == "" {
		if oid, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
			return &oid
		}
		return nil
	}
	if oid, ok := repo.Refs["refs/heads/"+*ref]; ok {
		return peelToCommit(repo, oid)
	}
	if oid, ok := repo.Refs["refs/tags/"+*ref]; ok {
		return peelToCommit(repo, oid)
	}
	if shaLikeRe.MatchString(*ref) && repo.Objects[*ref] != nil {
		return peelToCommit(repo, *ref)
	}
	return nil
}

type contentsCommit struct {
	commitSha string
	blobSha   *string
	created   bool
}

func commitFile(ctx *RestCtx, repo *Repo, path string, content []byte, message string, branch *string, sha *string) (*contentsCommit, error) {
	store := ctx.Store
	branchName := repo.DefaultBranch
	if branch != nil {
		branchName = *branch
	}
	ref := "refs/heads/" + branchName
	tip, hasTip := repo.Refs[ref]
	var tipPtr *string
	var baseTree *string
	if hasTip {
		tipPtr = &tip
		baseTree = treeOfCommit(repo, tip)
	}
	current := lookupPath(repo, baseTree, path)
	var blobSha *string
	created := false
	if content != nil {
		if current != nil {
			if sha == nil {
				return nil, restError(422, fmt.Sprintf("Validation Failed: \"sha\" is required when updating %s", path))
			}
			if *sha != current.Oid {
				return nil, restError(409, fmt.Sprintf("%s does not match the given sha", path))
			}
		} else if sha != nil {
			return nil, restError(409, fmt.Sprintf("%s does not exist, so no sha can match", path))
		}
		created = current == nil
		oid := putObject(repo, "blob", content)
		blobSha = &oid
	} else {
		if current == nil {
			return nil, notFoundErr()
		}
		if sha != nil && *sha != current.Oid {
			return nil, restError(409, fmt.Sprintf("%s does not match the given sha", path))
		}
	}
	segments := strings.Split(path, "/")
	var blobArg *FileEntry
	if content != nil {
		mode := "100644"
		if current != nil {
			mode = current.Mode
		}
		blobArg = &FileEntry{Oid: *blobSha, Mode: mode}
	}
	newTree := treeWithPath(repo, baseTree, segments, blobArg)
	at := store.Tick()
	identity := fmt.Sprintf("%s %d +0000", store.Identity(), at)
	parents := []string{}
	if tipPtr != nil {
		parents = []string{*tipPtr}
	}
	commitOid := putObject(repo, "commit", buildCommit(newTree, parents, identity, identity, message))
	repo.setRef(ref, commitOid)
	return &contentsCommit{commitSha: commitOid, blobSha: blobSha, created: created}, nil
}

// --- The router ---

func handleRest(ctx *RestCtx) (result RestResult) {
	defer func() {
		if r := recover(); r != nil {
			switch e := r.(type) {
			case *RestError:
				result = RestResult{Status: e.Status, Body: obj("message", e.Message)}
			case *DomainError:
				result = RestResult{Status: e.Status, Body: obj("message", e.Message)}
			default:
				panic(r)
			}
		}
	}()
	return route(ctx)
}

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

func route(ctx *RestCtx) RestResult {
	store, method, segments := ctx.Store, ctx.Method, ctx.Segments

	if len(segments) == 0 {
		if method != "GET" {
			panic(notFoundErr())
		}
		return RestResult{
			Status: 200,
			Body: obj(
				"current_user_url", "/api/v3/user",
				"repository_url", "/api/v3/repos/{owner}/{repo}",
				"issue_url", "/api/v3/repos/{owner}/{repo}/issues",
				"rate_limit_url", "/api/v3/rate_limit",
			),
			Headers: map[string]string{"x-oauth-scopes": "repo, read:org"},
		}
	}

	if segments[0] == "meta" && len(segments) == 1 {
		if method != "GET" {
			panic(notFoundErr())
		}
		return RestResult{
			Status: 200,
			Body: obj(
				"installed_version", "1.0.0",
				"services", obj("git", true, "rest", true, "graphql", true, "mcp", true),
			),
		}
	}

	if segments[0] == "user" && len(segments) == 1 {
		if method != "GET" {
			panic(notFoundErr())
		}
		viewer := store.Viewer
		base := userJson(store, viewer.Login)
		fields := append([]JField{}, base.Fields...)
		fields = append(fields,
			JField{"name", JStr(viewer.Name)},
			JField{"email", JStr(viewer.Email)},
			JField{"created_at", JStr(store.IsoOf(store.NowSec()))},
			JField{"updated_at", JStr(store.IsoOf(store.NowSec()))},
		)
		return RestResult{Status: 200, Body: JObj{Fields: fields}}
	}

	if segments[0] == "user" && len(segments) == 2 && segments[1] == "repos" {
		if method == "GET" {
			all := []*Repo{}
			for _, repo := range store.reposInOrder() {
				if repo.Owner == store.Viewer.Login {
					all = append(all, repo)
				}
			}
			sort.SliceStable(all, func(i, j int) bool { return all[i].DatabaseID < all[j].DatabaseID })
			page, headers := paginate(ctx, all)
			items := JArr{}
			for _, repo := range page {
				items.Items = append(items.Items, repoJson(store, repo))
			}
			return RestResult{Status: 200, Body: items, Headers: headers}
		}
		if method == "POST" {
			body := must(bodyObject(ctx))
			name := inputStr(body, "name")
			if !repoNameRe.MatchString(name) || name == "." || name == ".." {
				panic(restError(422, "Repository creation failed: the name is invalid"))
			}
			if _, exists := store.Repos[store.Viewer.Login+"/"+name]; exists {
				return RestResult{
					Status: 422,
					Body: obj(
						"message", "Repository creation failed.",
						"errors", arr(obj(
							"resource", "Repository",
							"code", "custom",
							"field", "name",
							"message", "name already exists on this account",
						)),
					),
				}
			}
			repo := store.CreateRepo(name, inputStr(body, "description"), body["private"] == true)
			if body["auto_init"] == true {
				must(commitFile(ctx, repo, "README.md", []byte("# "+name+"\n"), "Initial commit", nil, nil))
			}
			return RestResult{Status: 201, Body: repoJson(store, repo)}
		}
		panic(notFoundErr())
	}

	if segments[0] == "repos" && len(segments) >= 3 {
		owner, name := segments[1], segments[2]
		rest := segments[3:]
		if len(rest) == 0 {
			return repoRoot(ctx, owner, name)
		}
		repo := must(repoOr404(ctx, owner, name))
		return repoRoute(ctx, repo, rest)
	}

	panic(notFoundErr())
}

func repoRoot(ctx *RestCtx, owner, name string) RestResult {
	store, method := ctx.Store, ctx.Method
	if method == "GET" {
		repo := must(repoOr404(ctx, owner, name))
		return RestResult{Status: 200, Body: repoJson(store, repo)}
	}
	if method == "PATCH" {
		repo := must(repoOr404(ctx, owner, name))
		body := must(bodyObject(ctx))
		if v, ok := body["description"].(string); ok {
			repo.Description = v
		}
		if v, ok := body["private"].(bool); ok {
			repo.IsPrivate = v
		}
		if v, ok := body["name"].(string); ok && v != repo.Name && repoNameRe.MatchString(v) {
			store.RenameRepo(repo, v)
		}
		if v, ok := body["default_branch"].(string); ok && v != "" {
			repo.DefaultBranch = v
		}
		return RestResult{Status: 200, Body: repoJson(store, repo)}
	}
	if method == "DELETE" {
		repo := must(repoOr404(ctx, owner, name))
		store.DeleteRepo(repo)
		return RestResult{Status: 204}
	}
	panic(notFoundErr())
}

func repoRoute(ctx *RestCtx, repo *Repo, rest []string) RestResult {
	store, method := ctx.Store, ctx.Method
	head := rest[0]

	if head == "branches" {
		if len(rest) == 1 {
			if method != "GET" {
				panic(notFoundErr())
			}
			names := []string{}
			for _, ref := range repo.RefList {
				if strings.HasPrefix(ref, "refs/heads/") {
					names = append(names, ref[len("refs/heads/"):])
				}
			}
			sort.Strings(names)
			page, headers := paginate(ctx, names)
			items := JArr{}
			for _, branch := range page {
				items.Items = append(items.Items, branchJson(repo, branch))
			}
			return RestResult{Status: 200, Body: items, Headers: headers}
		}
		if method != "GET" {
			panic(notFoundErr())
		}
		branch := strings.Join(rest[1:], "/")
		if _, ok := repo.Refs["refs/heads/"+branch]; !ok {
			panic(notFoundErr())
		}
		return RestResult{Status: 200, Body: branchJson(repo, branch)}
	}

	if head == "commits" {
		if len(rest) == 1 {
			if method != "GET" {
				panic(notFoundErr())
			}
			tip := resolveRef(repo, ctx.Query.get("sha"))
			if tip == nil {
				return RestResult{Status: 200, Body: arr()}
			}
			history := historyOf(repo, *tip, 10000)
			page, headers := paginate(ctx, history)
			items := JArr{}
			for _, oid := range page {
				if json := commitJson(store, repo, oid); json != nil {
					items.Items = append(items.Items, json)
				}
			}
			return RestResult{Status: 200, Body: items, Headers: headers}
		}
		if len(rest) == 2 && method == "GET" {
			json := commitJson(store, repo, rest[1])
			if json == nil {
				panic(notFoundErr())
			}
			return RestResult{Status: 200, Body: json}
		}
		panic(notFoundErr())
	}

	if head == "contents" {
		return contentsRoute(ctx, repo, strings.Join(rest[1:], "/"))
	}
	if head == "labels" {
		return labelsRoute(ctx, repo, rest)
	}
	if head == "issues" {
		return issuesRoute(ctx, repo, rest)
	}
	if head == "pulls" {
		return pullsRoute(ctx, repo, rest)
	}
	if head == "git" && len(rest) >= 2 && rest[1] == "refs" {
		return gitRefsRoute(ctx, repo, rest[2:])
	}

	panic(notFoundErr())
}

func contentsRoute(ctx *RestCtx, repo *Repo, path string) RestResult {
	method := ctx.Method
	if method == "GET" {
		commit := resolveRef(repo, ctx.Query.get("ref"))
		if commit == nil {
			panic(notFoundErr())
		}
		tree := treeOfCommit(repo, *commit)
		if tree == nil {
			panic(notFoundErr())
		}
		if path == "" {
			return RestResult{Status: 200, Body: dirListing(repo, *tree, "")}
		}
		segments := strings.Split(path, "/")
		treeOid := tree
		for i := 0; i < len(segments); i++ {
			if treeOid == nil {
				panic(notFoundErr())
			}
			entries := mustParseTree(repo.Objects[*treeOid].Data)
			var entry *TreeEntry
			for j := range entries {
				if entries[j].Name == segments[i] {
					v := entries[j]
					entry = &v
					break
				}
			}
			if entry == nil {
				panic(notFoundErr())
			}
			if i == len(segments)-1 {
				if entry.Mode == "40000" {
					return RestResult{Status: 200, Body: dirListing(repo, entry.Oid, path)}
				}
				blob := repo.Objects[entry.Oid]
				if blob == nil {
					panic(notFoundErr())
				}
				return RestResult{
					Status: 200,
					Body: obj(
						"type", "file",
						"encoding", "base64",
						"content", base64.StdEncoding.EncodeToString(blob.Data),
						"sha", entry.Oid,
						"size", len(blob.Data),
						"name", entry.Name,
						"path", path,
					),
				}
			}
			if entry.Mode != "40000" {
				panic(notFoundErr())
			}
			v := entry.Oid
			treeOid = &v
		}
		panic(notFoundErr())
	}
	if method == "PUT" {
		if path == "" {
			panic(restError(422, "Validation Failed: a file path is required"))
		}
		body := must(bodyObject(ctx))
		message := inputStr(body, "message")
		content := []byte{}
		if s, ok := body["content"].(string); ok {
			decoded, _ := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
			content = decoded
		}
		var branch, sha *string
		if s, ok := body["branch"].(string); ok {
			branch = &s
		}
		if s, ok := body["sha"].(string); ok {
			sha = &s
		}
		result := must(commitFile(ctx, repo, path, content, message, branch, sha))
		size := 0
		if result.blobSha != nil {
			if blob := repo.Objects[*result.blobSha]; blob != nil {
				size = len(blob.Data)
			}
		}
		status := 200
		if result.created {
			status = 201
		}
		var blobShaVal JVal = JNULL
		if result.blobSha != nil {
			blobShaVal = JStr(*result.blobSha)
		}
		nameParts := strings.Split(path, "/")
		return RestResult{
			Status: status,
			Body: obj(
				"content", obj(
					"type", "file",
					"encoding", "base64",
					"sha", blobShaVal,
					"size", size,
					"name", nameParts[len(nameParts)-1],
					"path", path,
				),
				"commit", obj("sha", result.commitSha),
			),
		}
	}
	if method == "DELETE" {
		if path == "" {
			panic(notFoundErr())
		}
		body := must(bodyObject(ctx))
		message := inputStr(body, "message")
		var branch, sha *string
		if s, ok := body["branch"].(string); ok {
			branch = &s
		}
		if s, ok := body["sha"].(string); ok {
			sha = &s
		}
		result := must(commitFile(ctx, repo, path, nil, message, branch, sha))
		return RestResult{
			Status: 200,
			Body: obj(
				"content", nil,
				"commit", obj("sha", result.commitSha),
			),
		}
	}
	panic(notFoundErr())
}

func dirListing(repo *Repo, treeOid, prefix string) JArr {
	entries := mustParseTree(repo.Objects[treeOid].Data)
	items := JArr{}
	for _, entry := range entries {
		isDir := entry.Mode == "40000"
		size := 0
		if !isDir {
			if object := repo.Objects[entry.Oid]; object != nil {
				size = len(object.Data)
			}
		}
		typ := "file"
		if isDir {
			typ = "dir"
		}
		path := entry.Name
		if prefix != "" {
			path = prefix + "/" + entry.Name
		}
		items.Items = append(items.Items, obj(
			"type", typ,
			"sha", entry.Oid,
			"size", size,
			"name", entry.Name,
			"path", path,
		))
	}
	return items
}

func labelsRoute(ctx *RestCtx, repo *Repo, rest []string) RestResult {
	method := ctx.Method
	if len(rest) != 1 {
		panic(notFoundErr())
	}
	if method == "GET" {
		names := append([]string{}, repo.LabelOrder...)
		sort.Strings(names)
		page, headers := paginate(ctx, names)
		items := JArr{}
		for _, name := range page {
			items.Items = append(items.Items, labelJson(repo, name))
		}
		return RestResult{Status: 200, Body: items, Headers: headers}
	}
	if method == "POST" {
		body := must(bodyObject(ctx))
		name := inputStr(body, "name")
		if name == "" {
			panic(restError(422, "Validation Failed: a label name is required"))
		}
		if _, exists := repo.Labels[name]; exists {
			panic(restError(422, "Validation Failed: the label already exists"))
		}
		color := "ededed"
		if s, ok := body["color"].(string); ok {
			color = strings.TrimPrefix(s, "#")
		}
		var description *string
		if s, ok := body["description"].(string); ok {
			description = &s
		}
		repo.Labels[name] = &Label{Name: name, Color: color, Description: description}
		repo.LabelOrder = append(repo.LabelOrder, name)
		return RestResult{Status: 201, Body: labelJson(repo, name)}
	}
	panic(notFoundErr())
}

func issuesRoute(ctx *RestCtx, repo *Repo, rest []string) RestResult {
	store, method := ctx.Store, ctx.Method
	if len(rest) == 1 {
		if method == "POST" {
			body := must(bodyObject(ctx))
			issue := must(createIssue(store, repo, body))
			return RestResult{Status: 201, Body: issueJson(store, repo, issue)}
		}
		if method == "GET" {
			state := "open"
			if v := ctx.Query.get("state"); v != nil {
				state = *v
			}
			direction := "desc"
			if v := ctx.Query.get("direction"); v != nil {
				direction = *v
			}
			issues := []*IssueRec{}
			for _, thread := range repo.threadsInOrder() {
				if thread.Kind == "issue" {
					issues = append(issues, thread.Issue)
				}
			}
			if state == "open" || state == "closed" {
				filtered := []*IssueRec{}
				for _, issue := range issues {
					if issue.State == state {
						filtered = append(filtered, issue)
					}
				}
				issues = filtered
			} else if state != "all" {
				panic(restError(422, fmt.Sprintf("Validation Failed: state \"%s\" is not valid", state)))
			}
			if labelFilter := ctx.Query.get("labels"); labelFilter != nil && *labelFilter != "" {
				wanted := strings.Split(*labelFilter, ",")
				filtered := []*IssueRec{}
				for _, issue := range issues {
					ok := true
					for _, name := range wanted {
						found := false
						for _, have := range issue.Labels {
							if have == name {
								found = true
								break
							}
						}
						if !found {
							ok = false
							break
						}
					}
					if ok {
						filtered = append(filtered, issue)
					}
				}
				issues = filtered
			}
			sort.SliceStable(issues, func(i, j int) bool {
				if direction == "asc" {
					return issues[i].Number < issues[j].Number
				}
				return issues[i].Number > issues[j].Number
			})
			page, headers := paginate(ctx, issues)
			items := JArr{}
			for _, issue := range page {
				items.Items = append(items.Items, issueJson(store, repo, issue))
			}
			return RestResult{Status: 200, Body: items, Headers: headers}
		}
		panic(notFoundErr())
	}
	number, err := strconv.Atoi(rest[1])
	if err != nil || number <= 0 {
		panic(notFoundErr())
	}
	if len(rest) == 2 {
		thread := must(threadOr404(repo, number))
		if method == "GET" {
			if thread.Kind == "issue" {
				text := issueGetText(store, repo, thread.Issue)
				return RestResult{Status: 200, Text: &text}
			}
			var mergeable *bool
			if thread.Pull.State == "open" {
				v := pullMergeable(repo, thread.Pull)
				mergeable = &v
			} else if thread.Pull.Merged {
				v := true
				mergeable = &v
			}
			return RestResult{Status: 200, Body: pullJson(store, repo, thread.Pull, mergeable)}
		}
		if method == "PATCH" {
			body := must(bodyObject(ctx))
			if v, ok := body["title"].(string); ok {
				thread.SetTitle(v)
			}
			if v, ok := body["body"].(string); ok {
				thread.SetBody(v)
			}
			if v, ok := body["state"].(string); ok && (v == "open" || v == "closed") {
				thread.SetState(v)
			}
			if thread.Kind == "issue" {
				if rawLabels, present := body["labels"]; present {
					if list, ok := rawLabels.([]any); ok {
						names := []string{}
						for _, raw := range list {
							name, _ := raw.(string)
							if _, exists := repo.Labels[name]; !exists {
								panic(restError(422, fmt.Sprintf("Validation Failed: label \"%s\" does not exist", name)))
							}
							names = append(names, name)
						}
						thread.Issue.Labels = names
					}
				}
			}
			thread.SetUpdatedSec(store.Tick())
			if thread.Kind == "issue" {
				return RestResult{Status: 200, Body: issueJson(store, repo, thread.Issue)}
			}
			return RestResult{Status: 200, Body: pullJson(store, repo, thread.Pull, nil)}
		}
		panic(notFoundErr())
	}
	if len(rest) == 3 && rest[2] == "comments" {
		thread := must(threadOr404(repo, number))
		if method == "GET" {
			page, headers := paginate(ctx, thread.Comments())
			items := JArr{}
			for _, comment := range page {
				items.Items = append(items.Items, commentJson(store, repo, thread, comment))
			}
			return RestResult{Status: 200, Body: items, Headers: headers}
		}
		if method == "POST" {
			body := must(bodyObject(ctx))
			text := inputStr(body, "body")
			if text == "" {
				panic(restError(422, "Validation Failed: a comment body is required"))
			}
			comment := addComment(store, repo, thread, text)
			return RestResult{Status: 201, Body: commentJson(store, repo, thread, comment)}
		}
		panic(notFoundErr())
	}
	panic(notFoundErr())
}

func pullsRoute(ctx *RestCtx, repo *Repo, rest []string) RestResult {
	store, method := ctx.Store, ctx.Method
	if len(rest) == 1 {
		if method == "POST" {
			body := must(bodyObject(ctx))
			pull := must(createPull(store, repo, body))
			v := pullMergeable(repo, pull)
			return RestResult{Status: 201, Body: pullJson(store, repo, pull, &v)}
		}
		if method == "GET" {
			state := "open"
			if v := ctx.Query.get("state"); v != nil {
				state = *v
			}
			pulls := []*PullRec{}
			for _, thread := range repo.threadsInOrder() {
				if thread.Kind == "pull" {
					pulls = append(pulls, thread.Pull)
				}
			}
			if state == "open" {
				filtered := []*PullRec{}
				for _, pull := range pulls {
					if pull.State == "open" {
						filtered = append(filtered, pull)
					}
				}
				pulls = filtered
			} else if state == "closed" {
				filtered := []*PullRec{}
				for _, pull := range pulls {
					if pull.State == "closed" {
						filtered = append(filtered, pull)
					}
				}
				pulls = filtered
			} else if state != "all" {
				panic(restError(422, fmt.Sprintf("Validation Failed: state \"%s\" is not valid", state)))
			}
			direction := "desc"
			if v := ctx.Query.get("direction"); v != nil {
				direction = *v
			}
			sort.SliceStable(pulls, func(i, j int) bool {
				if direction == "asc" {
					return pulls[i].Number < pulls[j].Number
				}
				return pulls[i].Number > pulls[j].Number
			})
			page, headers := paginate(ctx, pulls)
			items := JArr{}
			for _, pull := range page {
				var mergeable *bool
				if pull.State == "open" {
					v := pullMergeable(repo, pull)
					mergeable = &v
				}
				items.Items = append(items.Items, pullJson(store, repo, pull, mergeable))
			}
			return RestResult{Status: 200, Body: items, Headers: headers}
		}
		panic(notFoundErr())
	}
	number, err := strconv.Atoi(rest[1])
	if err != nil || number <= 0 {
		panic(notFoundErr())
	}
	thread := repo.Threads[number]
	if thread == nil || thread.Kind != "pull" {
		panic(notFoundErr())
	}
	pull := thread.Pull
	if len(rest) == 2 {
		if method == "GET" {
			var mergeable *bool
			if pull.State == "open" {
				v := pullMergeable(repo, pull)
				mergeable = &v
			}
			return RestResult{Status: 200, Body: pullJson(store, repo, pull, mergeable)}
		}
		if method == "PATCH" {
			body := must(bodyObject(ctx))
			if v, ok := body["title"].(string); ok {
				pull.Title = v
			}
			if v, ok := body["body"].(string); ok {
				pull.Body = v
			}
			if v, ok := body["state"].(string); ok && (v == "open" || v == "closed") {
				pull.State = v
			}
			pull.UpdatedSec = store.Tick()
			return RestResult{Status: 200, Body: pullJson(store, repo, pull, nil)}
		}
		panic(notFoundErr())
	}
	if len(rest) == 3 && rest[2] == "files" && method == "GET" {
		files := pullFiles(repo, pull)
		page, headers := paginate(ctx, files)
		items := JArr{}
		for _, file := range page {
			sha := zeroOID
			if file.After != nil {
				sha = file.After.Oid
			} else if file.Before != nil {
				sha = file.Before.Oid
			}
			items.Items = append(items.Items, obj(
				"sha", sha,
				"filename", file.Path,
				"status", file.Status,
				"additions", file.Additions,
				"deletions", file.Deletions,
				"changes", file.Additions+file.Deletions,
			))
		}
		return RestResult{Status: 200, Body: items, Headers: headers}
	}
	if len(rest) == 3 && rest[2] == "merge" && method == "PUT" {
		body := must(optionalBody(ctx))
		raw := "merge"
		if s, ok := body["merge_method"].(string); ok {
			raw = strings.ToLower(s)
		}
		method2 := "merge"
		if raw == "squash" {
			method2 = "squash"
		} else if raw == "rebase" {
			method2 = "rebase"
		}
		sha := must(mergePull(store, repo, pull, method2))
		return RestResult{
			Status: 200,
			Body: obj(
				"sha", sha,
				"merged", true,
				"message", "Pull Request successfully merged",
			),
		}
	}
	panic(notFoundErr())
}

func gitRefsRoute(ctx *RestCtx, repo *Repo, rest []string) RestResult {
	method := ctx.Method
	if len(rest) == 0 {
		if method == "GET" {
			type refRow struct {
				ref string
				oid string
			}
			rows := []refRow{}
			names := append([]string{}, repo.RefList...)
			sort.Strings(names)
			for _, ref := range names {
				rows = append(rows, refRow{ref, repo.Refs[ref]})
			}
			page, headers := paginate(ctx, rows)
			items := JArr{}
			for _, row := range page {
				typ := "commit"
				if object := repo.Objects[row.oid]; object != nil {
					typ = object.Type
				}
				items.Items = append(items.Items, obj(
					"ref", row.ref,
					"node_id", "REF_"+row.ref,
					"url", "/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/git/refs/"+strings.TrimPrefix(row.ref, "refs/"),
					"object", obj("type", typ, "sha", row.oid),
				))
			}
			return RestResult{Status: 200, Body: items, Headers: headers}
		}
		if method == "POST" {
			body := must(bodyObject(ctx))
			ref := inputStr(body, "ref")
			sha := inputStr(body, "sha")
			if !strings.HasPrefix(ref, "refs/") || !shaLikeRe.MatchString(sha) || repo.Objects[sha] == nil {
				panic(restError(422, "Validation Failed: the ref or the object is invalid"))
			}
			if strings.HasPrefix(ref, "refs/heads/") && repo.Objects[sha].Type != "commit" {
				panic(restError(422, "Validation Failed: a branch must point at a commit"))
			}
			if _, exists := repo.Refs[ref]; exists {
				panic(restError(422, "Reference already exists"))
			}
			repo.setRef(ref, sha)
			return RestResult{
				Status: 201,
				Body: obj(
					"ref", ref,
					"node_id", "REF_"+ref,
					"url", "",
					"object", obj("type", repo.Objects[sha].Type, "sha", sha),
				),
			}
		}
		panic(notFoundErr())
	}
	joined := strings.Join(rest, "/")
	full := joined
	if !strings.HasPrefix(joined, "refs/") {
		full = "refs/" + joined
	}
	oid, exists := repo.Refs[full]
	if method == "GET" {
		if !exists {
			panic(notFoundErr())
		}
		typ := "commit"
		if object := repo.Objects[oid]; object != nil {
			typ = object.Type
		}
		return RestResult{
			Status: 200,
			Body: obj(
				"ref", full,
				"node_id", "REF_"+full,
				"url", "",
				"object", obj("type", typ, "sha", oid),
			),
		}
	}
	if method == "DELETE" {
		if !exists {
			panic(notFoundErr())
		}
		repo.deleteRef(full)
		return RestResult{Status: 204}
	}
	panic(notFoundErr())
}
