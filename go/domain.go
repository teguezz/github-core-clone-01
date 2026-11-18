package main

import (
	"fmt"
	"strings"
)

// Domain operations shared by REST, GraphQL and MCP: a faithful port of the
// reference domain.ts (issue/pull creation, comments, merges).

type DomainError struct {
	Status  int
	Message string
}

func (e *DomainError) Error() string { return e.Message }

func domainError(status int, format string, args ...any) *DomainError {
	return &DomainError{Status: status, Message: fmt.Sprintf(format, args...)}
}

// input access helpers over decoded JSON bodies (JS `unknown` semantics).
func inputStr(input map[string]any, key string) string {
	if v, ok := input[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func createIssue(store *Store, repo *Repo, input map[string]any) (*IssueRec, error) {
	title := inputStr(input, "title")
	if title == "" {
		return nil, domainError(422, "Validation Failed: title is required")
	}
	body := inputStr(input, "body")
	labels := []string{}
	if raw, present := input["labels"]; present && raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return nil, domainError(422, "Validation Failed: labels must be an array")
		}
		for _, item := range list {
			name := ""
			if s, ok := item.(string); ok {
				name = s
			} else if m, ok := item.(map[string]any); ok {
				if s, ok := m["name"].(string); ok {
					name = s
				}
			}
			if _, ok := repo.Labels[name]; !ok {
				return nil, domainError(422, "Validation Failed: label \"%s\" does not exist in %s/%s", name, repo.Owner, repo.Name)
			}
			labels = append(labels, name)
		}
	}
	at := store.Tick()
	issue := &IssueRec{
		Number:      repo.NextThreadNumber,
		ID:          store.NextIssueID(),
		Title:       title,
		Body:        body,
		State:       "open",
		Labels:      labels,
		Comments:    []*CommentRec{},
		AuthorLogin: store.Viewer.Login,
		AtSec:       at,
		UpdatedSec:  at,
	}
	repo.NextThreadNumber++
	repo.setThread(&ThreadRec{Kind: "issue", Issue: issue})
	return issue, nil
}

func createPull(store *Store, repo *Repo, input map[string]any) (*PullRec, error) {
	title := inputStr(input, "title")
	body := inputStr(input, "body")
	stripOwner := func(ref string) string {
		if idx := strings.LastIndex(ref, ":"); idx >= 0 {
			return ref[idx+1:]
		}
		return ref
	}
	head := ""
	if s, ok := input["head"].(string); ok {
		head = stripOwner(s)
	}
	base := ""
	if s, ok := input["base"].(string); ok {
		base = stripOwner(s)
	}
	if head == "" {
		return nil, domainError(422, "Validation Failed: head ref \"refs/heads/%s\" does not exist", head)
	}
	if _, ok := repo.Refs["refs/heads/"+head]; !ok {
		return nil, domainError(422, "Validation Failed: head ref \"refs/heads/%s\" does not exist", head)
	}
	if base == "" {
		return nil, domainError(422, "Validation Failed: base ref \"refs/heads/%s\" does not exist", base)
	}
	if _, ok := repo.Refs["refs/heads/"+base]; !ok {
		return nil, domainError(422, "Validation Failed: base ref \"refs/heads/%s\" does not exist", base)
	}
	for _, thread := range repo.threadsInOrder() {
		if thread.Kind == "pull" && thread.Pull.State == "open" && thread.Pull.HeadRef == head && thread.Pull.BaseRef == base {
			return nil, domainError(422, "Validation Failed: a pull request already exists for %s into %s", head, base)
		}
	}
	at := store.Tick()
	pull := &PullRec{
		Number:      repo.NextThreadNumber,
		ID:          store.NextIssueID(),
		Title:       title,
		Body:        body,
		State:       "open",
		HeadRef:     head,
		BaseRef:     base,
		Merged:      false,
		Comments:    []*CommentRec{},
		AuthorLogin: store.Viewer.Login,
		AtSec:       at,
		UpdatedSec:  at,
	}
	repo.NextThreadNumber++
	repo.setThread(&ThreadRec{Kind: "pull", Pull: pull})
	return pull, nil
}

func addComment(store *Store, repo *Repo, thread *ThreadRec, body string) *CommentRec {
	comment := &CommentRec{
		ID:          store.NextCommentID(),
		Body:        body,
		AuthorLogin: store.Viewer.Login,
		AtSec:       store.Tick(),
	}
	thread.AppendComment(comment)
	thread.SetUpdatedSec(comment.AtSec)
	return comment
}

func pullMergeBase(repo *Repo, pull *PullRec) *string {
	head, hasHead := repo.Refs["refs/heads/"+pull.HeadRef]
	base, hasBase := repo.Refs["refs/heads/"+pull.BaseRef]
	if !hasHead || !hasBase {
		return nil
	}
	return mergeBase(repo, base, head)
}

func pullMergeable(repo *Repo, pull *PullRec) bool {
	if pull.State != "open" {
		return false
	}
	head, hasHead := repo.Refs["refs/heads/"+pull.HeadRef]
	base, hasBase := repo.Refs["refs/heads/"+pull.BaseRef]
	if !hasHead || !hasBase {
		return false
	}
	if isAncestor(repo, head, base) {
		return true
	}
	baseOfMerge := mergeBase(repo, base, head)
	result := mergeCommits(repo, baseOfMerge, base, head)
	return result.Tree != nil
}

type PullFileInfo struct {
	FileChange
	Additions int
	Deletions int
}

func lineDiff(before, after string) (int, int) {
	split := func(s string) []string {
		if s == "" {
			return []string{}
		}
		lines := strings.Split(s, "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		return lines
	}
	a := split(before)
	b := split(after)
	m, n := len(a), len(b)
	dp := make([]int, (m+1)*(n+1))
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i*(n+1)+j] = dp[(i+1)*(n+1)+(j+1)] + 1
			} else if dp[(i+1)*(n+1)+j] > dp[i*(n+1)+(j+1)] {
				dp[i*(n+1)+j] = dp[(i+1)*(n+1)+j]
			} else {
				dp[i*(n+1)+j] = dp[i*(n+1)+(j+1)]
			}
		}
	}
	common := dp[0]
	return n - common, m - common
}

func pullFiles(repo *Repo, pull *PullRec) []PullFileInfo {
	head, ok := repo.Refs["refs/heads/"+pull.HeadRef]
	if !ok {
		return []PullFileInfo{}
	}
	base := pullMergeBase(repo, pull)
	changes := diffCommits(repo, base, &head)
	out := []PullFileInfo{}
	for _, change := range changes {
		beforeText := ""
		if change.Before != nil {
			if object := repo.Objects[change.Before.Oid]; object != nil {
				beforeText = string(object.Data)
			}
		}
		afterText := ""
		if change.After != nil {
			if object := repo.Objects[change.After.Oid]; object != nil {
				afterText = string(object.Data)
			}
		}
		additions, deletions := lineDiff(beforeText, afterText)
		out = append(out, PullFileInfo{FileChange: change, Additions: additions, Deletions: deletions})
	}
	return out
}

func mergePull(store *Store, repo *Repo, pull *PullRec, method string) (string, error) {
	if pull.Merged {
		return "", domainError(405, "Pull Request is not mergeable: it has already been merged")
	}
	if pull.State != "open" {
		return "", domainError(405, "Pull Request is not mergeable: it is closed")
	}
	headOid, hasHead := repo.Refs["refs/heads/"+pull.HeadRef]
	baseOid, hasBase := repo.Refs["refs/heads/"+pull.BaseRef]
	if !hasHead || !hasBase {
		return "", domainError(405, "Pull Request is not mergeable: a ref is missing")
	}
	identity := fmt.Sprintf("%s %d +0000", store.Identity(), store.Tick())
	var mergeOid string

	if isAncestor(repo, headOid, baseOid) {
		mergeOid = baseOid
	} else if method == "rebase" {
		rebased := rebaseOnto(repo, baseOid, headOid, identity)
		if rebased == "" {
			return "", domainError(405, "Pull Request is not mergeable: rebase conflict")
		}
		mergeOid = rebased
	} else {
		baseOfMerge := mergeBase(repo, baseOid, headOid)
		merged := mergeCommits(repo, baseOfMerge, baseOid, headOid)
		if merged.Conflict != nil {
			where := *merged.Conflict
			if where == "" {
				where = "the tree"
			}
			return "", domainError(405, "Pull Request is not mergeable: merge conflict in %s", where)
		}
		if method == "squash" {
			message := pull.Title + "\n\n(squash merge of pull request #" + fmt.Sprint(pull.Number) + ")"
			mergeOid = putObject(repo, "commit", buildCommit(*merged.Tree, []string{baseOid}, identity, identity, message))
		} else if isAncestor(repo, baseOid, headOid) {
			mergeOid = headOid
		} else {
			message := fmt.Sprintf("Merge pull request #%d from %s\n\n%s", pull.Number, pull.HeadRef, pull.Title)
			mergeOid = putObject(repo, "commit", buildCommit(*merged.Tree, []string{baseOid, headOid}, identity, identity, message))
		}
	}

	repo.setRef("refs/heads/"+pull.BaseRef, mergeOid)
	at := store.Tick()
	pull.State = "closed"
	pull.Merged = true
	pull.MergeCommitSha = &mergeOid
	pull.MergedAtSec = &at
	pull.UpdatedSec = at
	return mergeOid, nil
}

func rebaseOnto(repo *Repo, baseOid, headOid, identity string) string {
	baseOfMerge := mergeBase(repo, baseOid, headOid)
	if baseOfMerge == nil {
		return ""
	}
	chain := []string{}
	seen := map[string]bool{}
	cursor := &headOid
	for cursor != nil && *cursor != *baseOfMerge {
		if seen[*cursor] {
			return ""
		}
		seen[*cursor] = true
		chain = append(chain, *cursor)
		object := repo.Objects[*cursor]
		if object == nil || object.Type != "commit" {
			return ""
		}
		parents := parseCommit(object.Data).Parents
		if len(parents) == 0 {
			cursor = nil
		} else {
			p := parents[0]
			cursor = &p
		}
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	newBase := baseOid
	var newBaseTreeBase *string = baseOfMerge
	for _, oid := range chain {
		view := parseCommit(repo.Objects[oid].Data)
		merged := mergeCommits(repo, newBaseTreeBase, newBase, oid)
		if merged.Conflict != nil {
			return ""
		}
		newBase = putObject(repo, "commit", buildCommit(*merged.Tree, []string{newBase}, view.Author, identity, view.Message))
		v := oid
		newBaseTreeBase = &v
	}
	return newBase
}
