package main

import (
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// A compact GraphQL engine: a faithful port of the reference graphql.ts —
// parser, validation against the modelled schema, execution, Relay
// connections, node(id:) global ids and __type introspection.

// --- Parser ---

type GqlValue struct {
	Kind   string // string | number | boolean | null | enum | list | object | variable
	Str    string
	Num    float64
	Bool   bool
	Items  []*GqlValue
	Fields []GqlObjField
}

type GqlObjField struct {
	Name  string
	Value *GqlValue
}

func (v *GqlValue) field(name string) *GqlValue {
	for _, f := range v.Fields {
		if f.Name == name {
			return f.Value
		}
	}
	return nil
}

type GqlField struct {
	Alias      *string
	Name       string
	Args       []GqlObjField
	Selections []*GqlSelection
}

type GqlSelection struct {
	Kind     string // field | inline | spread
	Field    *GqlField
	TypeName string
	Name     string
	Sels     []*GqlSelection
}

type GqlOperation struct {
	Type       string // query | mutation
	Name       *string
	Selections []*GqlSelection
}

type GqlFragment struct {
	TypeName   string
	Selections []*GqlSelection
}

type GqlDocument struct {
	Operations []*GqlOperation
	Fragments  map[string]*GqlFragment
}

type gqlSyntaxError struct{ msg string }

func (e *gqlSyntaxError) Error() string { return e.msg }

type gqlParser struct {
	src string
	at  int
}

func (p *gqlParser) fail(format string, args ...any) {
	panic(&gqlSyntaxError{msg: fmt.Sprintf(format, args...)})
}

func (p *gqlParser) skipIgnored() {
	for p.at < len(p.src) {
		ch := p.src[p.at]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' {
			p.at++
		} else if ch == '#' {
			for p.at < len(p.src) && p.src[p.at] != '\n' {
				p.at++
			}
		} else {
			break
		}
	}
}

func (p *gqlParser) peek() byte {
	p.skipIgnored()
	if p.at < len(p.src) {
		return p.src[p.at]
	}
	return 0
}

func (p *gqlParser) expect(ch byte) {
	p.skipIgnored()
	if p.at >= len(p.src) || p.src[p.at] != ch {
		p.fail("expected \"%c\" at offset %d", ch, p.at)
	}
	p.at++
}

func isNameStart(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_'
}

func isNameChar(c byte) bool {
	return isNameStart(c) || c >= '0' && c <= '9'
}

func (p *gqlParser) readName() string {
	p.skipIgnored()
	start := p.at
	if start >= len(p.src) || !isNameStart(p.src[start]) {
		p.fail("expected a name at offset %d", p.at)
	}
	p.at++
	for p.at < len(p.src) && isNameChar(p.src[p.at]) {
		p.at++
	}
	return p.src[start:p.at]
}

func (p *gqlParser) readString() string {
	p.skipIgnored()
	if strings.HasPrefix(p.src[p.at:], "\"\"\"") {
		end := strings.Index(p.src[p.at+3:], "\"\"\"")
		if end < 0 {
			p.fail("unterminated block string")
		}
		raw := p.src[p.at+3 : p.at+3+end]
		p.at += 3 + end + 3
		return raw
	}
	if p.at >= len(p.src) || p.src[p.at] != '"' {
		p.fail("expected a string at offset %d", p.at)
	}
	p.at++
	units := []uint16{}
	push := func(r rune) {
		for _, u := range utf16.Encode([]rune{r}) {
			units = append(units, u)
		}
	}
	for p.at < len(p.src) && p.src[p.at] != '"' {
		ch := p.src[p.at]
		if ch == '\\' {
			if p.at+1 >= len(p.src) {
				p.fail("bad escape at offset %d", p.at)
			}
			esc := p.src[p.at+1]
			switch esc {
			case '"':
				push('"')
				p.at += 2
			case '\\':
				push('\\')
				p.at += 2
			case '/':
				push('/')
				p.at += 2
			case 'b':
				push('\b')
				p.at += 2
			case 'f':
				push('\f')
				p.at += 2
			case 'n':
				push('\n')
				p.at += 2
			case 'r':
				push('\r')
				p.at += 2
			case 't':
				push('\t')
				p.at += 2
			case 'u':
				if p.at+6 > len(p.src) {
					p.fail("bad escape at offset %d", p.at)
				}
				code, err := strconv.ParseUint(p.src[p.at+2:p.at+6], 16, 32)
				if err != nil {
					code = 0
				}
				units = append(units, uint16(code))
				p.at += 6
			default:
				p.fail("bad escape at offset %d", p.at)
			}
		} else {
			// Copy one UTF-8 encoded rune through its code units.
			r, size := decodeRune(p.src[p.at:])
			push(r)
			p.at += size
		}
	}
	if p.at >= len(p.src) {
		p.fail("unterminated string")
	}
	p.at++
	return string(utf16.Decode(units))
}

func decodeRune(s string) (rune, int) {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 {
		return rune(s[0]), 1
	}
	return r, size
}

var gqlNumberRe = buildNumberRe()

func buildNumberRe() func(string) (string, bool) {
	return func(s string) (string, bool) {
		i := 0
		if i < len(s) && s[i] == '-' {
			i++
		}
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return "", false
		}
		if i < len(s) && s[i] == '.' {
			j := i + 1
			fracStart := j
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			if j > fracStart {
				i = j
			}
		}
		if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
			j := i + 1
			if j < len(s) && (s[j] == '+' || s[j] == '-') {
				j++
			}
			expStart := j
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			if j > expStart {
				i = j
			}
		}
		return s[:i], true
	}
}

func (p *gqlParser) readNumber() float64 {
	p.skipIgnored()
	match, ok := gqlNumberRe(p.src[p.at:])
	if !ok {
		p.fail("expected a number at offset %d", p.at)
	}
	p.at += len(match)
	value, _ := strconv.ParseFloat(match, 64)
	return value
}

func (p *gqlParser) readValue() *GqlValue {
	p.skipIgnored()
	if p.at >= len(p.src) {
		p.fail("expected a name at offset %d", p.at)
	}
	ch := p.src[p.at]
	switch {
	case ch == '$':
		p.at++
		return &GqlValue{Kind: "variable", Str: p.readName()}
	case ch == '"':
		return &GqlValue{Kind: "string", Str: p.readString()}
	case ch == '[':
		p.at++
		items := []*GqlValue{}
		for p.peek() != ']' {
			items = append(items, p.readValue())
		}
		p.at++
		return &GqlValue{Kind: "list", Items: items}
	case ch == '{':
		p.at++
		fields := []GqlObjField{}
		for p.peek() != '}' {
			name := p.readName()
			p.expect(':')
			fields = append(fields, GqlObjField{Name: name, Value: p.readValue()})
		}
		p.at++
		return &GqlValue{Kind: "object", Fields: fields}
	case ch == '-' || ch >= '0' && ch <= '9':
		return &GqlValue{Kind: "number", Num: p.readNumber()}
	}
	name := p.readName()
	switch name {
	case "true":
		return &GqlValue{Kind: "boolean", Bool: true}
	case "false":
		return &GqlValue{Kind: "boolean", Bool: false}
	case "null":
		return &GqlValue{Kind: "null"}
	}
	return &GqlValue{Kind: "enum", Str: name}
}

func (p *gqlParser) readType() {
	p.skipIgnored()
	if p.at < len(p.src) && p.src[p.at] == '[' {
		p.at++
		p.readType()
		p.expect(']')
	} else {
		p.readName()
	}
	p.skipIgnored()
	if p.at < len(p.src) && p.src[p.at] == '!' {
		p.at++
	}
}

func (p *gqlParser) readDirectives() {
	for {
		p.skipIgnored()
		if p.at >= len(p.src) || p.src[p.at] != '@' {
			return
		}
		p.at++
		p.readName()
		p.skipIgnored()
		if p.at < len(p.src) && p.src[p.at] == '(' {
			p.at++
			for p.peek() != ')' {
				p.readName()
				p.expect(':')
				p.readValue()
			}
			p.at++
		}
	}
}

func (p *gqlParser) readArgs() []GqlObjField {
	args := []GqlObjField{}
	p.skipIgnored()
	if p.at >= len(p.src) || p.src[p.at] != '(' {
		return args
	}
	p.at++
	for p.peek() != ')' {
		name := p.readName()
		p.expect(':')
		args = append(args, GqlObjField{Name: name, Value: p.readValue()})
	}
	p.at++
	return args
}

func (p *gqlParser) readSelectionSet() []*GqlSelection {
	p.expect('{')
	selections := []*GqlSelection{}
	for p.peek() != '}' {
		selections = append(selections, p.readSelection())
	}
	p.at++
	return selections
}

func (p *gqlParser) readSelection() *GqlSelection {
	p.skipIgnored()
	if strings.HasPrefix(p.src[p.at:], "...") {
		p.at += 3
		p.skipIgnored()
		rest := p.src[p.at:]
		if strings.HasPrefix(rest, "on") && (len(rest) == 2 || !isNameChar(rest[2])) {
			p.at += 2
			typeName := p.readName()
			p.readDirectives()
			return &GqlSelection{Kind: "inline", TypeName: typeName, Sels: p.readSelectionSet()}
		}
		name := p.readName()
		p.readDirectives()
		return &GqlSelection{Kind: "spread", Name: name}
	}
	name := p.readName()
	var alias *string
	p.skipIgnored()
	if p.at < len(p.src) && p.src[p.at] == ':' {
		p.at++
		a := name
		alias = &a
		name = p.readName()
	}
	args := p.readArgs()
	p.readDirectives()
	p.skipIgnored()
	selections := []*GqlSelection{}
	if p.at < len(p.src) && p.src[p.at] == '{' {
		selections = p.readSelectionSet()
	}
	return &GqlSelection{Kind: "field", Field: &GqlField{Alias: alias, Name: name, Args: args, Selections: selections}}
}

func parseGraphQL(source string) (*GqlDocument, error) {
	p := &gqlParser{src: source}
	doc := &GqlDocument{Fragments: map[string]*GqlFragment{}}
	defer func() {
		// converted below via named return handling in parseGraphQLDoc
	}()
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				if se, ok := r.(*gqlSyntaxError); ok {
					err = se
					return
				}
				panic(r)
			}
		}()
		for {
			p.skipIgnored()
			if p.at >= len(p.src) {
				break
			}
			if p.src[p.at] == '{' {
				doc.Operations = append(doc.Operations, &GqlOperation{Type: "query", Selections: p.readSelectionSet()})
				continue
			}
			keyword := p.readName()
			if keyword == "query" || keyword == "mutation" || keyword == "subscription" {
				p.skipIgnored()
				var name *string
				if p.at < len(p.src) && isNameStart(p.src[p.at]) {
					n := p.readName()
					name = &n
				}
				p.skipIgnored()
				if p.at < len(p.src) && p.src[p.at] == '(' {
					p.at++
					for p.peek() != ')' {
						p.expect('$')
						p.readName()
						p.expect(':')
						p.readType()
						p.skipIgnored()
						if p.at < len(p.src) && p.src[p.at] == '=' {
							p.at++
							p.readValue()
						}
						p.readDirectives()
					}
					p.at++
				}
				p.readDirectives()
				opType := "query"
				if keyword == "mutation" {
					opType = "mutation"
				}
				doc.Operations = append(doc.Operations, &GqlOperation{Type: opType, Name: name, Selections: p.readSelectionSet()})
			} else if keyword == "fragment" {
				name := p.readName()
				on := p.readName()
				if on != "on" {
					p.fail("fragment without \"on\"")
				}
				typeName := p.readName()
				p.readDirectives()
				doc.Fragments[name] = &GqlFragment{TypeName: typeName, Selections: p.readSelectionSet()}
			} else {
				p.fail("unexpected keyword \"%s\"", keyword)
			}
		}
		return nil
	}()
	if err != nil {
		return nil, err
	}
	return doc, nil
}

func resolveValue(value *GqlValue, variables map[string]any) any {
	switch value.Kind {
	case "string", "enum":
		return value.Str
	case "number":
		return value.Num
	case "boolean":
		return value.Bool
	case "null":
		return nil
	case "list":
		out := []any{}
		for _, item := range value.Items {
			out = append(out, resolveValue(item, variables))
		}
		return out
	case "object":
		out := map[string]any{}
		for _, f := range value.Fields {
			out[f.Name] = resolveValue(f.Value, variables)
		}
		return out
	case "variable":
		return variables[value.Str]
	}
	return nil
}

// --- Schema model ---

var gqlTypeOrder = []string{
	"Query", "Mutation", "User", "Organization", "Repository", "Issue", "PullRequest",
	"IssueComment", "Ref", "Commit", "GitActor", "IssueConnection", "IssueEdge",
	"PullRequestConnection", "PullRequestEdge", "PullRequestChangedFileConnection",
	"PullRequestChangedFileEdge", "PullRequestChangedFile", "PageInfo",
	"CreateIssuePayload", "CloseIssuePayload", "ReopenIssuePayload",
	"CreatePullRequestPayload", "MergePullRequestPayload", "AddCommentPayload",
	"CommentEdge", "Node", "RateLimit", "__Schema", "__Type", "__Field",
	"__EnumValue", "__InputValue", "__Directive",
}

var gqlTypeFields = map[string][]string{
	"Query":            {"viewer", "repository", "node", "nodes", "rateLimit", "__type", "__schema"},
	"Mutation":         {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment", "reopenIssue"},
	"User":             {"login", "id", "name", "email", "url", "databaseId", "isViewer"},
	"Organization":     {"login", "id", "name", "url"},
	"Repository":       {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "hasProjectsEnabled", "hasWikiEnabled", "isPrivate", "isFork", "isArchived", "description", "url", "createdAt", "updatedAt", "pushedAt", "issue", "issues", "pullRequest", "pullRequests", "stargazerCount", "forkCount", "primaryLanguage"},
	"Issue":            {"id", "databaseId", "number", "title", "body", "state", "author", "repository", "labels", "comments", "createdAt", "updatedAt", "closedAt", "url", "viewerDidAuthor"},
	"PullRequest":      {"id", "databaseId", "number", "title", "body", "state", "merged", "mergedAt", "mergeable", "headRefName", "baseRefName", "headRef", "baseRef", "author", "repository", "files", "comments", "createdAt", "updatedAt", "closedAt", "url", "mergeCommit", "additions", "deletions", "changedFiles", "isDraft"},
	"IssueComment":     {"id", "databaseId", "body", "author", "createdAt", "updatedAt", "url"},
	"Ref":              {"name", "prefix", "target", "repository"},
	"Commit":           {"oid", "id", "message", "messageHeadline", "committedDate", "authoredDate", "author", "committer", "tree"},
	"GitActor":         {"login", "name", "email", "date"},
	"IssueConnection":  {"nodes", "edges", "pageInfo", "totalCount"},
	"IssueEdge":        {"node", "cursor"},
	"PullRequestConnection":            {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestEdge":                  {"node", "cursor"},
	"PullRequestChangedFileConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestChangedFileEdge":       {"node", "cursor"},
	"PullRequestChangedFile":           {"path", "additions", "deletions", "changeType"},
	"PageInfo":                         {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
	"CreateIssuePayload":               {"issue", "clientMutationId"},
	"CloseIssuePayload":                {"issue", "clientMutationId"},
	"ReopenIssuePayload":               {"issue", "clientMutationId"},
	"CreatePullRequestPayload":         {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload":         {"pullRequest", "clientMutationId"},
	"AddCommentPayload":                {"commentEdge", "clientMutationId", "subject"},
	"CommentEdge":                      {"node", "cursor"},
	"Node":                             {"id"},
	"RateLimit":                        {"limit", "cost", "remaining", "used", "resetAt", "nodeCount"},
	"__Schema":                         {"types", "queryType", "mutationType", "subscriptionType", "directives"},
	"__Type":                           {"name", "kind", "description", "fields", "enumValues", "possibleTypes", "ofType", "inputFields", "interfaces", "specifiedByURL"},
	"__Field":                          {"name", "description", "args", "type", "isDeprecated", "deprecationReason"},
	"__EnumValue":                      {"name", "description", "isDeprecated", "deprecationReason"},
	"__InputValue":                     {"name", "description", "type", "defaultValue"},
	"__Directive":                      {"name", "description", "locations", "args"},
}

var gqlEnumValues = map[string][]string{
	"IssueState":              {"OPEN", "CLOSED"},
	"PullRequestState":        {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":          {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"PullRequestMergeMethod":  {"MERGE", "SQUASH", "REBASE"},
	"RepositoryPermission":    {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"SearchType":              {"ISSUE", "USER", "REPOSITORY", "DISCUSSION"},
	"IssueOrderField":         {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"PullRequestOrderField":   {"CREATED_AT", "UPDATED_AT"},
	"OrderDirection":          {"ASC", "DESC"},
	"CommentAuthorAssociation": {"OWNER", "MEMBER", "COLLABORATOR", "NONE"},
	"StatusState":             {"EXPECTED", "ERROR", "FAILURE", "PENDING", "SUCCESS"},
}

// fieldChild maps parent type -> field -> child type ("" = leaf, absent = unmodelled).
var gqlFieldChild = map[string]map[string]*string{
	"Query":    {"viewer": strPtr("User"), "repository": strPtr("Repository"), "node": strPtr("Node"), "__type": strPtr("__Type"), "__schema": strPtr("__Schema")},
	"Mutation": {"createIssue": strPtr("CreateIssuePayload"), "closeIssue": strPtr("CloseIssuePayload"), "reopenIssue": strPtr("ReopenIssuePayload"), "createPullRequest": strPtr("CreatePullRequestPayload"), "mergePullRequest": strPtr("MergePullRequestPayload"), "addComment": strPtr("AddCommentPayload")},
	"Repository": {"owner": strPtr("User"), "defaultBranchRef": strPtr("Ref"), "issue": strPtr("Issue"), "issues": strPtr("IssueConnection"), "pullRequest": strPtr("PullRequest"), "pullRequests": strPtr("PullRequestConnection")},
	"Issue":      {"author": strPtr("User"), "repository": strPtr("Repository")},
	"PullRequest": {"author": strPtr("User"), "repository": strPtr("Repository"), "files": strPtr("PullRequestChangedFileConnection"), "headRef": strPtr("Ref"), "baseRef": strPtr("Ref"), "mergeCommit": strPtr("Commit")},
	"IssueComment": {"author": strPtr("User")},
	"Ref":          {"target": strPtr("Commit"), "repository": strPtr("Repository")},
	"Commit":       {"author": strPtr("GitActor"), "committer": strPtr("GitActor"), "tree": strPtr("Tree")},
	"IssueConnection":                    {"nodes": strPtr("Issue"), "edges": strPtr("IssueEdge"), "pageInfo": strPtr("PageInfo")},
	"IssueEdge":                          {"node": strPtr("Issue")},
	"PullRequestConnection":              {"nodes": strPtr("PullRequest"), "edges": strPtr("PullRequestEdge"), "pageInfo": strPtr("PageInfo")},
	"PullRequestEdge":                    {"node": strPtr("PullRequest")},
	"PullRequestChangedFileConnection":   {"nodes": strPtr("PullRequestChangedFile"), "edges": strPtr("PullRequestChangedFileEdge"), "pageInfo": strPtr("PageInfo")},
	"PullRequestChangedFileEdge":         {"node": strPtr("PullRequestChangedFile")},
	"CreateIssuePayload":                 {"issue": strPtr("Issue")},
	"CloseIssuePayload":                  {"issue": strPtr("Issue")},
	"ReopenIssuePayload":                 {"issue": strPtr("Issue")},
	"CreatePullRequestPayload":           {"pullRequest": strPtr("PullRequest")},
	"MergePullRequestPayload":           {"pullRequest": strPtr("PullRequest")},
	"AddCommentPayload":                  {"commentEdge": strPtr("CommentEdge"), "subject": strPtr("Issue")},
	"CommentEdge":                        {"node": strPtr("IssueComment")},
	"__Type":                             {"fields": strPtr("__Field"), "enumValues": strPtr("__EnumValue"), "possibleTypes": strPtr("__Type"), "ofType": strPtr("__Type"), "inputFields": strPtr("__InputValue"), "interfaces": strPtr("__Type")},
	"__Field":                            {"type": strPtr("__Type"), "args": strPtr("__InputValue")},
	"__Schema":                           {"types": strPtr("__Type"), "queryType": strPtr("__Type"), "mutationType": strPtr("__Type")},
}

func strPtr(s string) *string { return &s }

var gqlScalarTypes = map[string]bool{
	"String": true, "Int": true, "Float": true, "Boolean": true, "ID": true,
	"DateTime": true, "URI": true, "GitObjectID": true, "Base64String": true,
}

// --- Execution ---

type GObj struct {
	Typename string
	Slots    map[string]any
}

func newGObj(typename string, slots map[string]any) *GObj {
	if slots == nil {
		slots = map[string]any{}
	}
	return &GObj{Typename: typename, Slots: slots}
}

type gqlExecCtx struct {
	store     *Store
	variables map[string]any
	fragments map[string]*GqlFragment
	errors    []string
}

type GqlRequest struct {
	Query         string
	Variables     map[string]any
	OperationName *string
}

func executeGraphQL(store *Store, request GqlRequest) JObj {
	document, err := parseGraphQL(request.Query)
	if err != nil {
		return obj("data", nil, "errors", arr(obj("message", err.Error())))
	}
	var operation *GqlOperation
	if request.OperationName != nil && *request.OperationName != "" {
		for _, candidate := range document.Operations {
			if candidate.Name != nil && *candidate.Name == *request.OperationName {
				operation = candidate
				break
			}
		}
		if operation == nil {
			return obj("data", nil, "errors", arr(obj("message", fmt.Sprintf("Unknown operation \"%s\"", *request.OperationName))))
		}
	} else if len(document.Operations) == 1 {
		operation = document.Operations[0]
	} else {
		if len(document.Operations) == 0 {
			return obj("data", nil, "errors", arr(obj("message", "The document holds no operation")))
		}
		operation = document.Operations[0]
	}
	ctx := &gqlExecCtx{store: store, variables: request.Variables, fragments: document.Fragments}
	if ctx.variables == nil {
		ctx.variables = map[string]any{}
	}
	rootType := "Query"
	if operation.Type == "mutation" {
		rootType = "Mutation"
	}
	validationErrors := validateSelections(ctx, rootType, operation.Selections)
	if len(validationErrors) > 0 {
		errs := JArr{}
		for _, message := range validationErrors {
			errs.Items = append(errs.Items, obj("message", message))
		}
		return obj("data", nil, "errors", errs)
	}
	root := newGObj(rootType, nil)
	data := execSelections(ctx, rootType, root, operation.Selections)
	if len(ctx.errors) > 0 {
		errs := JArr{}
		for _, message := range ctx.errors {
			errs.Items = append(errs.Items, obj("message", message))
		}
		return obj("data", data, "errors", errs)
	}
	return obj("data", data)
}

func validateSelections(ctx *gqlExecCtx, typeName string, selections []*GqlSelection) []string {
	errors := []string{}
	fields, hasFields := gqlTypeFields[typeName]
	for _, selection := range selections {
		switch selection.Kind {
		case "inline":
			errors = append(errors, validateSelections(ctx, selection.TypeName, selection.Sels)...)
		case "spread":
			if fragment, ok := ctx.fragments[selection.Name]; ok {
				errors = append(errors, validateSelections(ctx, fragment.TypeName, fragment.Selections)...)
			}
		case "field":
			field := selection.Field
			if field.Name == "__typename" {
				continue
			}
			if !hasFields {
				continue
			}
			found := false
			for _, name := range fields {
				if name == field.Name {
					found = true
					break
				}
			}
			if !found {
				errors = append(errors, fmt.Sprintf("Field '%s' doesn't exist on type '%s'", field.Name, typeName))
				continue
			}
			if childMap, ok := gqlFieldChild[typeName]; ok {
				if child, ok := childMap[field.Name]; ok && child != nil && len(field.Selections) > 0 {
					errors = append(errors, validateSelections(ctx, *child, field.Selections)...)
				}
			}
		}
	}
	return errors
}

func expandSelections(ctx *gqlExecCtx, typeName string, selections []*GqlSelection) []*GqlField {
	out := []*GqlField{}
	for _, selection := range selections {
		switch selection.Kind {
		case "field":
			out = append(out, selection.Field)
		case "inline":
			if selection.TypeName == typeName {
				out = append(out, expandSelections(ctx, typeName, selection.Sels)...)
			}
		case "spread":
			if fragment, ok := ctx.fragments[selection.Name]; ok && fragment.TypeName == typeName {
				out = append(out, expandSelections(ctx, typeName, fragment.Selections)...)
			}
		}
	}
	return out
}

func execSelections(ctx *gqlExecCtx, typeName string, source *GObj, selections []*GqlSelection) JObj {
	out := JObj{}
	for _, field := range expandSelections(ctx, typeName, selections) {
		key := field.Name
		if field.Alias != nil {
			key = *field.Alias
		}
		if field.Name == "__typename" {
			out.Fields = append(out.Fields, JField{key, JStr(typeName)})
			continue
		}
		value, err := resolveField(ctx, typeName, source, field)
		if err != nil {
			ctx.errors = append(ctx.errors, err.Error())
			value = nil
		}
		out.Fields = append(out.Fields, JField{key, completeValue(ctx, field, value)})
	}
	return out
}

func completeValue(ctx *gqlExecCtx, field *GqlField, value any) JVal {
	switch v := value.(type) {
	case nil:
		return JNULL
	case []any:
		items := JArr{}
		for _, item := range v {
			items.Items = append(items.Items, completeValue(ctx, field, item))
		}
		return items
	case *GObj:
		if len(field.Selections) == 0 {
			// A wrapper selected with no subfields: serialize its scalar slots
			// (the reference stringifies the wrapper object itself).
			out := obj("__typename", v.Typename)
			for key, slot := range v.Slots {
				switch slot.(type) {
				case string, int, float64, bool, nil:
					out.Fields = append(out.Fields, JField{key, toJVal(slot)})
				}
			}
			return out
		}
		return execSelections(ctx, v.Typename, v, field.Selections)
	case string:
		return JStr(v)
	case int:
		return JInt(int64(v))
	case float64:
		return JFloat(v)
	case bool:
		return JBool(v)
	}
	return JNULL
}

// --- Object constructors ---

func gqlUserObj(store *Store, login string) *GObj {
	user := store.userByLogin(login)
	return newGObj("User", map[string]any{
		"login":      user.Login,
		"id":         globalIdOf(GlobalID{Kind: "user", UserID: user.ID}),
		"databaseId": user.ID,
		"name":       user.Name,
		"email":      user.Email,
	})
}

func gqlRepoObj(repo *Repo) *GObj {
	return newGObj("Repository", map[string]any{"repo": repo})
}

func gqlIssueObj(repo *Repo, rec *IssueRec) *GObj {
	return newGObj("Issue", map[string]any{"repo": repo, "rec": rec})
}

func gqlPullObj(repo *Repo, rec *PullRec) *GObj {
	return newGObj("PullRequest", map[string]any{"repo": repo, "rec": rec})
}

func gqlCommentObj(repo *Repo, thread *ThreadRec, comment *CommentRec) *GObj {
	return newGObj("IssueComment", map[string]any{"repo": repo, "thread": thread, "comment": comment})
}

func cursorOf(number int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:%d", number)))
}

func numberOfCursor(cursor string) *int {
	rawBytes, err := lenientBase64Decode(cursor)
	if err != nil {
		return nil
	}
	raw := string(rawBytes)
	if !strings.HasPrefix(raw, "cursor:") {
		return nil
	}
	digits := raw[len("cursor:"):]
	if digits == "" {
		return nil
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return nil
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return nil
	}
	return &n
}

func paginateConnection[T any](items []T, first float64, after *string, keyOf func(T) int) ([]T, bool, *string) {
	start := 0
	if after != nil {
		afterNumber := numberOfCursor(*after)
		index := -1
		if afterNumber != nil {
			for i, item := range items {
				if keyOf(item) == *afterNumber {
					index = i
					break
				}
			}
		}
		if index >= 0 {
			start = index + 1
		} else {
			start = 0
		}
	}
	count := int(first)
	if count < 0 {
		count = 0
	}
	page := []T{}
	if start < len(items) {
		end := start + count
		if end > len(items) {
			end = len(items)
		}
		page = items[start:end]
	}
	hasNextPage := start+len(page) < len(items)
	var endCursor *string
	if len(page) > 0 {
		c := cursorOf(keyOf(page[len(page)-1]))
		endCursor = &c
	} else {
		endCursor = after
	}
	return page, hasNextPage, endCursor
}

func connectionObj(typename, edgeTypename string, nodes []*GObj, total int, hasNextPage bool, endCursor *string) *GObj {
	edges := []any{}
	nodeList := []any{}
	for _, node := range nodes {
		nodeList = append(nodeList, node)
		var cursorVal any
		if endCursor != nil {
			cursorVal = *endCursor
		}
		edges = append(edges, newGObj(edgeTypename, map[string]any{"node": node, "cursor": cursorVal}))
	}
	var endCursorVal any
	if endCursor != nil {
		endCursorVal = *endCursor
	}
	pageInfo := newGObj("PageInfo", map[string]any{
		"hasNextPage":     hasNextPage,
		"hasPreviousPage": false,
		"startCursor":     endCursorVal,
		"endCursor":       endCursorVal,
	})
	return newGObj(typename, map[string]any{
		"nodesRaw":   nodeList,
		"edgesRaw":   edges,
		"pageInfo":   pageInfo,
		"totalCount": total,
	})
}

func argsOf(ctx *gqlExecCtx, field *GqlField) map[string]any {
	out := map[string]any{}
	for _, arg := range field.Args {
		out[arg.Name] = resolveValue(arg.Value, ctx.variables)
	}
	return out
}

func inputOf(ctx *gqlExecCtx, field *GqlField) map[string]any {
	args := argsOf(ctx, field)
	if input, ok := args["input"].(map[string]any); ok {
		return input
	}
	return map[string]any{}
}

type gqlError struct{ msg string }

func (e *gqlError) Error() string { return e.msg }

func gqlErr(format string, args ...any) *gqlError {
	return &gqlError{msg: fmt.Sprintf(format, args...)}
}

func repoByGlobalId(store *Store, id any) (*Repo, error) {
	idStr, ok := id.(string)
	if !ok {
		return nil, gqlErr("the repository id is not recognized")
	}
	parsed := parseGlobalId(idStr)
	if parsed == nil || parsed.Kind != "repository" {
		return nil, gqlErr("the repository id is not recognized")
	}
	repo := store.ReposByID[parsed.DatabaseID]
	if repo == nil {
		return nil, gqlErr("the repository does not exist")
	}
	return repo, nil
}

func threadByGlobalId(store *Store, id any, kinds ...string) (*Repo, *ThreadRec, error) {
	idStr, ok := id.(string)
	if !ok {
		return nil, nil, gqlErr("the id is not recognized")
	}
	parsed := parseGlobalId(idStr)
	if parsed == nil || parsed.Kind != "issue" && parsed.Kind != "pull" {
		return nil, nil, gqlErr("the id is not recognized")
	}
	allowed := false
	for _, kind := range kinds {
		if parsed.Kind == kind {
			allowed = true
		}
	}
	if !allowed {
		return nil, nil, gqlErr("the id names the wrong kind of object")
	}
	repo := store.ReposByID[parsed.DatabaseID]
	if repo == nil {
		return nil, nil, gqlErr("the repository does not exist")
	}
	thread := repo.Threads[parsed.Number]
	if thread == nil || thread.Kind != parsed.Kind {
		return nil, nil, gqlErr("the object does not exist")
	}
	return repo, thread, nil
}

// jsToNumber is JS Number(value) for argument coercion.
func jsToNumber(v any) float64 {
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
	return nanValue()
}

func nanValue() float64 {
	return math.NaN()
}

func threadByNumber(repo *Repo, number float64) *ThreadRec {
	if number != number || number != float64(int64(number)) {
		return nil
	}
	return repo.Threads[int(number)]
}

// --- Field resolvers ---

func resolveField(ctx *gqlExecCtx, typeName string, source *GObj, field *GqlField) (any, error) {
	store := ctx.store
	args := argsOf(ctx, field)
	switch typeName {
	case "Query":
		switch field.Name {
		case "viewer":
			return gqlUserObj(store, store.Viewer.Login), nil
		case "repository":
			owner := argStr(args, "owner")
			name := argStr(args, "name")
			repo := store.RepoOf(owner, name)
			if repo == nil {
				return nil, nil
			}
			return gqlRepoObj(repo), nil
		case "node":
			return nodeById(store, args["id"]), nil
		case "__type":
			return typeObj(argStr(args, "name")), nil
		case "__schema":
			return newGObj("__Schema", nil), nil
		case "rateLimit":
			return newGObj("RateLimit", map[string]any{
				"limit": 5000, "cost": 1, "remaining": 5000, "resetAt": store.IsoOf(store.NowSec()),
			}), nil
		}
	case "Mutation":
		return resolveMutation(ctx, field)
	case "__Schema":
		switch field.Name {
		case "queryType":
			return typeObj("Query"), nil
		case "mutationType":
			return typeObj("Mutation"), nil
		case "types":
			out := []any{}
			for _, name := range gqlTypeOrder {
				out = append(out, typeObj(name))
			}
			return out, nil
		}
		return nil, nil
	case "__Type":
		return resolveTypeField(source, field), nil
	case "__Field":
		switch field.Name {
		case "name":
			return source.Slots["name"], nil
		case "description":
			return nil, nil
		case "isDeprecated":
			return false, nil
		case "deprecationReason":
			return nil, nil
		case "type":
			return newGObj("__Type", map[string]any{"name": "String", "kind": "SCALAR"}), nil
		case "args":
			return []any{}, nil
		}
	case "__EnumValue":
		switch field.Name {
		case "name":
			return source.Slots["name"], nil
		case "description":
			return nil, nil
		case "isDeprecated":
			return false, nil
		case "deprecationReason":
			return nil, nil
		}
	case "User", "Organization":
		login, _ := source.Slots["login"].(string)
		switch field.Name {
		case "login":
			return login, nil
		case "id":
			return source.Slots["id"], nil
		case "databaseId":
			return source.Slots["databaseId"], nil
		case "name":
			return source.Slots["name"], nil
		case "email":
			return source.Slots["email"], nil
		case "isViewer":
			return login == store.Viewer.Login, nil
		case "url":
			return "https://forge.invalid/" + login, nil
		}
	case "Repository":
		return resolveRepositoryField(ctx, source.Slots["repo"].(*Repo), field, args)
	case "Issue":
		return resolveIssueField(ctx, source, field), nil
	case "PullRequest":
		return resolvePullField(ctx, source, field, args), nil
	case "IssueComment":
		comment := source.Slots["comment"].(*CommentRec)
		repo := source.Slots["repo"].(*Repo)
		thread := source.Slots["thread"].(*ThreadRec)
		switch field.Name {
		case "id":
			return globalIdOf(GlobalID{Kind: "comment", DatabaseID: repo.DatabaseID, Number: thread.Number(), CommentID: comment.ID}), nil
		case "databaseId":
			return comment.ID, nil
		case "body":
			return comment.Body, nil
		case "author":
			return gqlUserObj(store, comment.AuthorLogin), nil
		case "createdAt", "updatedAt":
			return store.IsoOf(comment.AtSec), nil
		}
	case "Ref":
		switch field.Name {
		case "name":
			return source.Slots["name"], nil
		case "prefix":
			return "refs/heads/", nil
		case "target":
			return newGObj("Commit", map[string]any{"repo": source.Slots["repo"], "oid": source.Slots["oid"]}), nil
		case "repository":
			return gqlRepoObj(source.Slots["repo"].(*Repo)), nil
		}
	case "Commit":
		if field.Name == "oid" || field.Name == "id" {
			return source.Slots["oid"], nil
		}
		return nil, nil
	case "IssueConnection", "PullRequestConnection", "PullRequestChangedFileConnection":
		switch field.Name {
		case "nodes":
			return source.Slots["nodesRaw"], nil
		case "edges":
			return source.Slots["edgesRaw"], nil
		case "pageInfo":
			return source.Slots["pageInfo"], nil
		case "totalCount":
			return source.Slots["totalCount"], nil
		}
	case "IssueEdge", "PullRequestEdge", "PullRequestChangedFileEdge", "CommentEdge":
		switch field.Name {
		case "node":
			return source.Slots["node"], nil
		case "cursor":
			return source.Slots["cursor"], nil
		}
	case "PageInfo":
		if v, ok := source.Slots[field.Name]; ok {
			return v, nil
		}
		return nil, nil
	case "CreateIssuePayload", "CloseIssuePayload", "ReopenIssuePayload":
		switch field.Name {
		case "issue":
			return source.Slots["issue"], nil
		case "clientMutationId":
			return nil, nil
		}
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		switch field.Name {
		case "pullRequest":
			return source.Slots["pullRequest"], nil
		case "clientMutationId":
			return nil, nil
		}
	case "AddCommentPayload":
		switch field.Name {
		case "commentEdge":
			return source.Slots["commentEdge"], nil
		case "subject":
			return source.Slots["subject"], nil
		case "clientMutationId":
			return nil, nil
		}
	case "PullRequestChangedFile":
		if v, ok := source.Slots[field.Name]; ok {
			return v, nil
		}
		return nil, nil
	}
	if v, ok := source.Slots[field.Name]; ok {
		return v, nil
	}
	if fields, ok := gqlTypeFields[typeName]; ok {
		for _, name := range fields {
			if name == field.Name {
				return nil, nil
			}
		}
	}
	return nil, gqlErr("Field '%s' doesn't exist on type '%s'", field.Name, typeName)
}

func argStr(args map[string]any, key string) string {
	if s, ok := args[key].(string); ok {
		return s
	}
	return ""
}

func typeObj(name string) *GObj {
	if _, ok := gqlTypeFields[name]; ok {
		return newGObj("__Type", map[string]any{"name": name, "kind": "OBJECT"})
	}
	if _, ok := gqlEnumValues[name]; ok {
		return newGObj("__Type", map[string]any{"name": name, "kind": "ENUM"})
	}
	if gqlScalarTypes[name] {
		return newGObj("__Type", map[string]any{"name": name, "kind": "SCALAR"})
	}
	if name == "Tree" {
		return newGObj("__Type", map[string]any{"name": name, "kind": "OBJECT"})
	}
	if name == "RateLimit" {
		return newGObj("__Type", map[string]any{"name": name, "kind": "OBJECT"})
	}
	return nil
}

func resolveTypeField(source *GObj, field *GqlField) any {
	name, _ := source.Slots["name"].(string)
	switch field.Name {
	case "name":
		return name
	case "kind":
		return source.Slots["kind"]
	case "description":
		return nil
	case "fields":
		fields, ok := gqlTypeFields[name]
		if !ok {
			return nil
		}
		out := []any{}
		for _, fieldName := range fields {
			out = append(out, newGObj("__Field", map[string]any{"name": fieldName, "parentType": name}))
		}
		return out
	case "enumValues":
		values, ok := gqlEnumValues[name]
		if !ok {
			return nil
		}
		out := []any{}
		for _, value := range values {
			out = append(out, newGObj("__EnumValue", map[string]any{"name": value}))
		}
		return out
	case "possibleTypes", "interfaces":
		return []any{}
	case "ofType", "inputFields":
		return nil
	}
	return nil
}

func nodeById(store *Store, id any) *GObj {
	idStr, ok := id.(string)
	if !ok {
		return nil
	}
	parsed := parseGlobalId(idStr)
	if parsed == nil {
		return nil
	}
	switch parsed.Kind {
	case "repository":
		repo := store.ReposByID[parsed.DatabaseID]
		if repo == nil {
			return nil
		}
		return gqlRepoObj(repo)
	case "issue", "pull":
		repo := store.ReposByID[parsed.DatabaseID]
		if repo == nil {
			return nil
		}
		thread := repo.Threads[parsed.Number]
		if thread == nil || thread.Kind != parsed.Kind {
			return nil
		}
		if thread.Kind == "issue" {
			return gqlIssueObj(repo, thread.Issue)
		}
		return gqlPullObj(repo, thread.Pull)
	case "comment":
		repo := store.ReposByID[parsed.DatabaseID]
		if repo == nil {
			return nil
		}
		thread := repo.Threads[parsed.Number]
		if thread == nil {
			return nil
		}
		for _, comment := range thread.Comments() {
			if comment.ID == parsed.CommentID {
				return gqlCommentObj(repo, thread, comment)
			}
		}
		return nil
	case "user":
		for _, user := range store.Users {
			if user.ID == parsed.UserID {
				return gqlUserObj(store, user.Login)
			}
		}
		return nil
	}
	return nil
}

func resolveRepositoryField(ctx *gqlExecCtx, repo *Repo, field *GqlField, args map[string]any) (any, error) {
	store := ctx.store
	switch field.Name {
	case "id":
		return globalIdOf(GlobalID{Kind: "repository", DatabaseID: repo.DatabaseID}), nil
	case "databaseId":
		return repo.DatabaseID, nil
	case "name":
		return repo.Name, nil
	case "nameWithOwner":
		return repo.Owner + "/" + repo.Name, nil
	case "owner":
		return gqlUserObj(store, repo.Owner), nil
	case "defaultBranchRef":
		oid, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
		if !ok {
			return nil, nil
		}
		return newGObj("Ref", map[string]any{"repo": repo, "name": repo.DefaultBranch, "oid": oid}), nil
	case "viewerPermission":
		return "ADMIN", nil
	case "hasIssuesEnabled", "hasProjectsEnabled", "hasWikiEnabled":
		return true, nil
	case "isPrivate":
		return repo.IsPrivate, nil
	case "isFork", "isArchived":
		return false, nil
	case "description":
		if repo.Description == "" {
			return nil, nil
		}
		return repo.Description, nil
	case "url":
		return "https://forge.invalid/" + repo.Owner + "/" + repo.Name, nil
	case "createdAt", "updatedAt", "pushedAt":
		return store.IsoOf(repo.CreatedSec), nil
	case "stargazerCount", "forkCount":
		return 0, nil
	case "issue":
		thread := threadByNumber(repo, jsToNumber(args["number"]))
		if thread != nil && thread.Kind == "issue" {
			return gqlIssueObj(repo, thread.Issue), nil
		}
		return nil, nil
	case "issues":
		var states []string
		if raw, ok := args["states"].([]any); ok {
			for _, item := range raw {
				if s, ok := item.(string); ok {
					states = append(states, s)
				}
			}
		}
		issues := []*IssueRec{}
		for _, thread := range repo.threadsInOrder() {
			if thread.Kind == "issue" {
				issues = append(issues, thread.Issue)
			}
		}
		if states != nil {
			wanted := map[string]bool{}
			for _, state := range states {
				wanted[strings.ToLower(state)] = true
			}
			filtered := []*IssueRec{}
			for _, issue := range issues {
				if wanted[issue.State] {
					filtered = append(filtered, issue)
				}
			}
			issues = filtered
		}
		descending := false
		if orderBy, ok := args["orderBy"].(map[string]any); ok {
			if direction, ok := orderBy["direction"].(string); ok {
				descending = direction == "DESC"
			}
		}
		sortIssuesStable(issues, descending)
		first := 30.0
		if v, ok := args["first"].(float64); ok {
			first = math.Min(100, v)
		}
		var after *string
		if s, ok := args["after"].(string); ok {
			after = &s
		}
		page, hasNextPage, endCursor := paginateConnection(issues, first, after, func(issue *IssueRec) int { return issue.Number })
		nodes := []*GObj{}
		for _, issue := range page {
			nodes = append(nodes, gqlIssueObj(repo, issue))
		}
		return connectionObj("IssueConnection", "IssueEdge", nodes, len(issues), hasNextPage, endCursor), nil
	case "pullRequest":
		thread := threadByNumber(repo, jsToNumber(args["number"]))
		if thread != nil && thread.Kind == "pull" {
			return gqlPullObj(repo, thread.Pull), nil
		}
		return nil, nil
	case "pullRequests":
		var states []string
		if raw, ok := args["states"].([]any); ok {
			for _, item := range raw {
				if s, ok := item.(string); ok {
					states = append(states, s)
				}
			}
		}
		pulls := []*PullRec{}
		for _, thread := range repo.threadsInOrder() {
			if thread.Kind == "pull" {
				pulls = append(pulls, thread.Pull)
			}
		}
		if states != nil {
			filtered := []*PullRec{}
			for _, pull := range pulls {
				gqlState := strings.ToUpper(pull.State)
				if pull.Merged {
					gqlState = "MERGED"
				}
				for _, state := range states {
					if state == gqlState {
						filtered = append(filtered, pull)
						break
					}
				}
			}
			pulls = filtered
		}
		sortPullsStable(pulls, true)
		first := 30.0
		if v, ok := args["first"].(float64); ok {
			first = math.Min(100, v)
		}
		var after *string
		if s, ok := args["after"].(string); ok {
			after = &s
		}
		page, hasNextPage, endCursor := paginateConnection(pulls, first, after, func(pull *PullRec) int { return pull.Number })
		nodes := []*GObj{}
		for _, pull := range page {
			nodes = append(nodes, gqlPullObj(repo, pull))
		}
		return connectionObj("PullRequestConnection", "PullRequestEdge", nodes, len(pulls), hasNextPage, endCursor), nil
	}
	return nil, nil
}

func sortIssuesStable(issues []*IssueRec, descending bool) {
	for i := 1; i < len(issues); i++ {
		for j := i; j > 0; j-- {
			a, b := issues[j-1].Number, issues[j].Number
			if descending {
				if b <= a {
					break
				}
			} else if b >= a {
				break
			}
			issues[j], issues[j-1] = issues[j-1], issues[j]
		}
	}
}

func sortPullsStable(pulls []*PullRec, descending bool) {
	for i := 1; i < len(pulls); i++ {
		for j := i; j > 0; j-- {
			a, b := pulls[j-1].Number, pulls[j].Number
			if descending {
				if b <= a {
					break
				}
			} else if b >= a {
				break
			}
			pulls[j], pulls[j-1] = pulls[j-1], pulls[j]
		}
	}
}

func resolveIssueField(ctx *gqlExecCtx, source *GObj, field *GqlField) any {
	store := ctx.store
	repo := source.Slots["repo"].(*Repo)
	rec := source.Slots["rec"].(*IssueRec)
	switch field.Name {
	case "id":
		return globalIdOf(GlobalID{Kind: "issue", DatabaseID: repo.DatabaseID, Number: rec.Number})
	case "databaseId":
		return rec.ID
	case "number":
		return rec.Number
	case "title":
		return rec.Title
	case "body":
		return rec.Body
	case "state":
		return strings.ToUpper(rec.State)
	case "author":
		return gqlUserObj(store, rec.AuthorLogin)
	case "repository":
		return gqlRepoObj(repo)
	case "createdAt":
		return store.IsoOf(rec.AtSec)
	case "updatedAt":
		return store.IsoOf(rec.UpdatedSec)
	case "closedAt":
		if rec.State == "closed" {
			return store.IsoOf(rec.UpdatedSec)
		}
		return nil
	case "url":
		return fmt.Sprintf("https://forge.invalid/%s/%s/issues/%d", repo.Owner, repo.Name, rec.Number)
	case "viewerDidAuthor":
		return rec.AuthorLogin == store.Viewer.Login
	}
	return nil
}

func resolvePullField(ctx *gqlExecCtx, source *GObj, field *GqlField, args map[string]any) any {
	store := ctx.store
	repo := source.Slots["repo"].(*Repo)
	rec := source.Slots["rec"].(*PullRec)
	switch field.Name {
	case "id":
		return globalIdOf(GlobalID{Kind: "pull", DatabaseID: repo.DatabaseID, Number: rec.Number})
	case "databaseId":
		return rec.ID
	case "number":
		return rec.Number
	case "title":
		return rec.Title
	case "body":
		return rec.Body
	case "state":
		if rec.Merged {
			return "MERGED"
		}
		return strings.ToUpper(rec.State)
	case "merged":
		return rec.Merged
	case "mergedAt":
		if rec.MergedAtSec == nil {
			return nil
		}
		return store.IsoOf(*rec.MergedAtSec)
	case "mergeable":
		if rec.State != "open" {
			return "UNKNOWN"
		}
		if pullMergeable(repo, rec) {
			return "MERGEABLE"
		}
		return "CONFLICTING"
	case "headRefName":
		return rec.HeadRef
	case "baseRefName":
		return rec.BaseRef
	case "headRef":
		oid, ok := repo.Refs["refs/heads/"+rec.HeadRef]
		if !ok {
			return nil
		}
		return newGObj("Ref", map[string]any{"repo": repo, "name": rec.HeadRef, "oid": oid})
	case "baseRef":
		oid, ok := repo.Refs["refs/heads/"+rec.BaseRef]
		if !ok {
			return nil
		}
		return newGObj("Ref", map[string]any{"repo": repo, "name": rec.BaseRef, "oid": oid})
	case "author":
		return gqlUserObj(store, rec.AuthorLogin)
	case "repository":
		return gqlRepoObj(repo)
	case "createdAt":
		return store.IsoOf(rec.AtSec)
	case "updatedAt":
		return store.IsoOf(rec.UpdatedSec)
	case "closedAt":
		if rec.State == "closed" {
			return store.IsoOf(rec.UpdatedSec)
		}
		return nil
	case "url":
		return fmt.Sprintf("https://forge.invalid/%s/%s/pull/%d", repo.Owner, repo.Name, rec.Number)
	case "isDraft":
		return false
	case "files":
		files := pullFiles(repo, rec)
		type indexedFile struct {
			PullFileInfo
			index int
		}
		indexed := []indexedFile{}
		for i, file := range files {
			indexed = append(indexed, indexedFile{file, i})
		}
		first := 30.0
		if v, ok := args["first"].(float64); ok {
			first = math.Min(100, v)
		}
		var after *string
		if s, ok := args["after"].(string); ok {
			after = &s
		}
		page, hasNextPage, endCursor := paginateConnection(indexed, first, after, func(file indexedFile) int { return file.index })
		nodes := []*GObj{}
		for _, file := range page {
			changeType := "MODIFIED"
			if file.Status == "added" {
				changeType = "ADDED"
			} else if file.Status == "removed" {
				changeType = "DELETED"
			}
			nodes = append(nodes, newGObj("PullRequestChangedFile", map[string]any{
				"path": file.Path, "additions": file.Additions, "deletions": file.Deletions, "changeType": changeType,
			}))
		}
		return connectionObj("PullRequestChangedFileConnection", "PullRequestChangedFileEdge", nodes, len(files), hasNextPage, endCursor)
	case "additions":
		total := 0
		for _, file := range pullFiles(repo, rec) {
			total += file.Additions
		}
		return total
	case "deletions":
		total := 0
		for _, file := range pullFiles(repo, rec) {
			total += file.Deletions
		}
		return total
	case "changedFiles":
		return len(pullFiles(repo, rec))
	case "mergeCommit":
		if rec.MergeCommitSha == nil {
			return nil
		}
		return newGObj("Commit", map[string]any{"repo": repo, "oid": *rec.MergeCommitSha})
	}
	return nil
}

func resolveMutation(ctx *gqlExecCtx, field *GqlField) (any, error) {
	store := ctx.store
	input := inputOf(ctx, field)
	// Domain failures surface as GraphQL errors with the domain message.
	deferConvert := func(err error) error {
		if de, ok := err.(*DomainError); ok {
			return gqlErr("%s", de.Message)
		}
		return err
	}
	_ = deferConvert
	switch field.Name {
	case "createIssue":
		repo, err := repoByGlobalId(store, input["repositoryId"])
		if err != nil {
			return nil, err
		}
		issueInput := map[string]any{"title": input["title"], "body": input["body"]}
		if _, present := input["labelIds"]; present {
			issueInput["labels"] = []any{}
		}
		issue, err := createIssue(store, repo, issueInput)
		if err != nil {
			return nil, deferConvert(err)
		}
		return newGObj("CreateIssuePayload", map[string]any{"issue": gqlIssueObj(repo, issue)}), nil
	case "closeIssue", "reopenIssue":
		repo, thread, err := threadByGlobalId(store, input["issueId"], "issue")
		if err != nil {
			return nil, err
		}
		issue := thread.Issue
		if field.Name == "closeIssue" {
			issue.State = "closed"
		} else {
			issue.State = "open"
		}
		issue.UpdatedSec = store.Tick()
		typename := "CloseIssuePayload"
		if field.Name == "reopenIssue" {
			typename = "ReopenIssuePayload"
		}
		return newGObj(typename, map[string]any{"issue": gqlIssueObj(repo, issue)}), nil
	case "createPullRequest":
		repo, err := repoByGlobalId(store, input["repositoryId"])
		if err != nil {
			return nil, err
		}
		pull, err := createPull(store, repo, map[string]any{
			"title": input["title"], "body": input["body"], "head": input["headRefName"], "base": input["baseRefName"],
		})
		if err != nil {
			return nil, deferConvert(err)
		}
		return newGObj("CreatePullRequestPayload", map[string]any{"pullRequest": gqlPullObj(repo, pull)}), nil
	case "mergePullRequest":
		repo, thread, err := threadByGlobalId(store, input["pullRequestId"], "pull")
		if err != nil {
			return nil, err
		}
		pull := thread.Pull
		raw := "MERGE"
		if s, ok := input["mergeMethod"].(string); ok {
			raw = strings.ToUpper(s)
		}
		method := "merge"
		if raw == "SQUASH" {
			method = "squash"
		} else if raw == "REBASE" {
			method = "rebase"
		}
		if _, err := mergePull(store, repo, pull, method); err != nil {
			return nil, deferConvert(err)
		}
		return newGObj("MergePullRequestPayload", map[string]any{"pullRequest": gqlPullObj(repo, pull)}), nil
	case "addComment":
		repo, thread, err := threadByGlobalId(store, input["subjectId"], "issue", "pull")
		if err != nil {
			return nil, err
		}
		body := ""
		if s, ok := input["body"].(string); ok {
			body = s
		}
		if body == "" {
			return nil, gqlErr("a comment body is required")
		}
		comment := addComment(store, repo, thread, body)
		node := gqlCommentObj(repo, thread, comment)
		var subject *GObj
		if thread.Kind == "issue" {
			subject = gqlIssueObj(repo, thread.Issue)
		} else {
			subject = gqlPullObj(repo, thread.Pull)
		}
		return newGObj("AddCommentPayload", map[string]any{
			"commentEdge": newGObj("CommentEdge", map[string]any{"node": node, "cursor": cursorOf(thread.Number())}),
			"subject":     subject,
		}), nil
	}
	return nil, gqlErr("Field '%s' doesn't exist on type 'Mutation'", field.Name)
}
