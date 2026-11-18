package main

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The JSON shapes every surface agrees on: a faithful port of shapes.ts.
// Builders produce ordered JObj values whose JSON.stringify bytes match the
// reference exactly (same keys, same order, same values).

// userJson is memoized per store+login, as in the reference (users never change).
var userJsonMemo = map[*Store]map[string]JObj{}

func userJson(store *Store, login string) JObj {
	byLogin, ok := userJsonMemo[store]
	if !ok {
		byLogin = map[string]JObj{}
		userJsonMemo[store] = byLogin
	}
	if hit, ok := byLogin[login]; ok {
		return hit
	}
	user := store.userByLogin(login)
	json := obj(
		"login", user.Login,
		"id", user.ID,
		"node_id", globalIdOf(GlobalID{Kind: "user", UserID: user.ID}),
		"type", "User",
		"site_admin", false,
	)
	byLogin[login] = json
	return json
}

func repoJson(store *Store, repo *Repo) JObj {
	openIssues := 0
	for _, thread := range repo.threadsInOrder() {
		if thread.Kind == "issue" && thread.Issue.State == "open" {
			openIssues++
		}
	}
	visibility := "public"
	if repo.IsPrivate {
		visibility = "private"
	}
	return obj(
		"id", repo.DatabaseID,
		"node_id", globalIdOf(GlobalID{Kind: "repository", DatabaseID: repo.DatabaseID}),
		"name", repo.Name,
		"full_name", repo.Owner+"/"+repo.Name,
		"private", repo.IsPrivate,
		"owner", userJson(store, repo.Owner),
		"description", repo.Description,
		"fork", false,
		"html_url", "https://forge.invalid/"+repo.Owner+"/"+repo.Name,
		"url", "/api/v3/repos/"+repo.Owner+"/"+repo.Name,
		"git_url", "git://forge.invalid/"+repo.Owner+"/"+repo.Name+".git",
		"ssh_url", "git@forge.invalid:"+repo.Owner+"/"+repo.Name+".git",
		"clone_url", "https://forge.invalid/"+repo.Owner+"/"+repo.Name+".git",
		"default_branch", repo.DefaultBranch,
		"has_issues", true,
		"has_projects", true,
		"has_wiki", true,
		"has_pages", false,
		"has_downloads", true,
		"archived", false,
		"disabled", false,
		"visibility", visibility,
		"created_at", store.IsoOf(repo.CreatedSec),
		"updated_at", store.IsoOf(repo.CreatedSec),
		"pushed_at", store.IsoOf(repo.CreatedSec),
		"size", 0,
		"stargazers_count", 0,
		"watchers_count", 0,
		"forks_count", 0,
		"open_issues_count", openIssues,
		"language", nil,
		"license", nil,
		"topics", arr(),
	)
}

func labelID(repo *Repo, name string) int {
	hash := repo.DatabaseID * 131
	for _, ch := range name {
		// JS iterates UTF-16 code units; label names in play are ASCII.
		hash = (hash*31 + int(ch)) % 1000000007
	}
	return hash + 1
}

func labelJson(repo *Repo, name string) JObj {
	label := repo.Labels[name]
	color := "ededed"
	var description JVal = JNULL
	if label != nil {
		color = label.Color
		if label.Description != nil {
			description = JStr(*label.Description)
		}
	}
	id := labelID(repo, name)
	return obj(
		"id", id,
		"node_id", "LA_"+strconv.Itoa(repo.DatabaseID)+"_"+strconv.Itoa(id),
		"name", name,
		"color", color,
		"description", description,
		"default", false,
	)
}

func commentJson(store *Store, repo *Repo, thread *ThreadRec, comment *CommentRec) JObj {
	return obj(
		"id", comment.ID,
		"node_id", globalIdOf(GlobalID{Kind: "comment", DatabaseID: repo.DatabaseID, Number: thread.Number(), CommentID: comment.ID}),
		"body", comment.Body,
		"user", userJson(store, comment.AuthorLogin),
		"created_at", store.IsoOf(comment.AtSec),
		"updated_at", store.IsoOf(comment.AtSec),
		"author_association", "OWNER",
	)
}

func issueJson(store *Store, repo *Repo, issue *IssueRec) JObj {
	labels := JArr{}
	for _, name := range issue.Labels {
		labels.Items = append(labels.Items, labelJson(repo, name))
	}
	var closedAt JVal = JNULL
	if issue.State == "closed" {
		closedAt = JStr(store.IsoOf(issue.UpdatedSec))
	}
	return obj(
		"id", issue.ID,
		"node_id", globalIdOf(GlobalID{Kind: "issue", DatabaseID: repo.DatabaseID, Number: issue.Number}),
		"number", issue.Number,
		"title", issue.Title,
		"body", issue.Body,
		"state", issue.State,
		"locked", false,
		"labels", labels,
		"user", userJson(store, issue.AuthorLogin),
		"assignee", nil,
		"assignees", arr(),
		"comments", len(issue.Comments),
		"created_at", store.IsoOf(issue.AtSec),
		"updated_at", store.IsoOf(issue.UpdatedSec),
		"closed_at", closedAt,
		"author_association", "OWNER",
	)
}

func pullJson(store *Store, repo *Repo, pull *PullRec, mergeable *bool) JObj {
	headSha, hasHead := repo.Refs["refs/heads/"+pull.HeadRef]
	baseSha, hasBase := repo.Refs["refs/heads/"+pull.BaseRef]
	side := func(ref string, sha *string) JObj {
		var shaVal JVal = JNULL
		if sha != nil {
			shaVal = JStr(*sha)
		}
		return obj(
			"label", repo.Owner+":"+ref,
			"ref", ref,
			"sha", shaVal,
			"user", userJson(store, repo.Owner),
			"repo", repoJson(store, repo),
		)
	}
	var headPtr, basePtr *string
	if hasHead {
		headPtr = &headSha
	}
	if hasBase {
		basePtr = &baseSha
	}
	var mergedAt JVal = JNULL
	if pull.MergedAtSec != nil {
		mergedAt = JStr(store.IsoOf(*pull.MergedAtSec))
	}
	var mergeCommitSha JVal = JNULL
	if pull.MergeCommitSha != nil {
		mergeCommitSha = JStr(*pull.MergeCommitSha)
	}
	var mergeableVal JVal = JNULL
	if mergeable != nil {
		mergeableVal = JBool(*mergeable)
	}
	mergeableState := "unknown"
	if mergeable != nil {
		if *mergeable {
			mergeableState = "clean"
		} else {
			mergeableState = "dirty"
		}
	}
	var closedAt JVal = JNULL
	if pull.State == "closed" {
		closedAt = JStr(store.IsoOf(pull.UpdatedSec))
	}
	return obj(
		"id", pull.ID,
		"node_id", globalIdOf(GlobalID{Kind: "pull", DatabaseID: repo.DatabaseID, Number: pull.Number}),
		"number", pull.Number,
		"state", pull.State,
		"locked", false,
		"title", pull.Title,
		"body", pull.Body,
		"user", userJson(store, pull.AuthorLogin),
		"labels", arr(),
		"head", side(pull.HeadRef, headPtr),
		"base", side(pull.BaseRef, basePtr),
		"merged", pull.Merged,
		"merged_at", mergedAt,
		"merge_commit_sha", mergeCommitSha,
		"mergeable", mergeableVal,
		"mergeable_state", mergeableState,
		"comments", len(pull.Comments),
		"commits", 0,
		"additions", 0,
		"deletions", 0,
		"changed_files", 0,
		"draft", false,
		"created_at", store.IsoOf(pull.AtSec),
		"updated_at", store.IsoOf(pull.UpdatedSec),
		"closed_at", closedAt,
		"author_association", "OWNER",
	)
}

func branchJson(repo *Repo, name string) JObj {
	sha, ok := repo.Refs["refs/heads/"+name]
	var shaVal JVal = JNULL
	if ok {
		shaVal = JStr(sha)
	}
	return obj(
		"name", name,
		"commit", obj(
			"sha", shaVal,
			"url", "/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/commits/"+sha,
		),
		"protected", false,
	)
}

var personRe = regexp.MustCompile(`^(.*)<([^>]*)>\s+(\d+)\s+([+-]\d{4})$`)

func commitJson(store *Store, repo *Repo, oid string) JVal {
	object := repo.Objects[oid]
	if object == nil || object.Type != "commit" {
		return nil
	}
	view := parseCommit(object.Data)
	person := func(line string) JObj {
		m := personRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			return obj(
				"name", line,
				"email", "",
				"date", store.IsoOf(store.NowSec()),
			)
		}
		sec, _ := strconv.ParseInt(m[3], 10, 64)
		date := time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05Z")
		return obj(
			"name", strings.TrimSpace(m[1]),
			"email", m[2],
			"date", date,
		)
	}
	parents := JArr{}
	for _, parent := range view.Parents {
		parents.Items = append(parents.Items, obj("sha", parent, "url", ""))
	}
	return obj(
		"sha", oid,
		"node_id", "C_"+oid,
		"commit", obj(
			"author", person(view.Author),
			"committer", person(view.Committer),
			"message", view.Message,
			"tree", obj("sha", view.Tree, "url", ""),
			"comment_count", 0,
		),
		"author", nil,
		"committer", nil,
		"parents", parents,
	)
}
