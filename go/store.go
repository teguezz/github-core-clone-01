package main

import (
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The forge's in-memory store and scenario identity: a faithful port of the
// reference store.ts. One process, one store, every surface reads and writes
// the same objects. Timestamps come from a logical clock over a fixed epoch,
// ids from counters, so the same request sequence produces the same bytes.

const scenarioSeed uint64 = 0x6a09e667f3bcc909

func fnv1a64(text string) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for _, ch := range text {
		// JS iterates UTF-16 code units; the seed strings are ASCII.
		hash ^= uint64(ch)
		hash *= 0x100000001b3
	}
	return hash
}

func mix64(value uint64) uint64 {
	z := value + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

type scenarioRng struct{ state uint64 }

func newScenarioRng(seed uint64) *scenarioRng {
	if seed == 0 {
		seed = scenarioSeed
	}
	return &scenarioRng{state: seed}
}

func (r *scenarioRng) nextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545f4914f6cdd1d
}

func (r *scenarioRng) nextU32() uint32 {
	return uint32(r.nextU64() >> 32)
}

func (r *scenarioRng) nextInt(maxExclusive int) int {
	if maxExclusive == 1 {
		return 0
	}
	const rangeN = 4294967296
	limit := rangeN - (rangeN % maxExclusive)
	for {
		value := int(r.nextU32())
		if value < limit {
			return value % maxExclusive
		}
	}
}

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}
var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

type ScenarioUser struct {
	ID    int
	Login string
	Name  string
	Email string
}

var loginSanitizer = regexp.MustCompile(`[^a-z0-9-]`)

func scenarioUsers(count int) []ScenarioUser {
	rng := newScenarioRng(mix64(scenarioSeed ^ fnv1a64("users")))
	used := map[string]bool{}
	users := []ScenarioUser{}
	for i := 0; i < count; i++ {
		first := firstNames[rng.nextInt(len(firstNames))]
		last := lastNames[rng.nextInt(len(lastNames))]
		base := loginSanitizer.ReplaceAllString(first+last, "")
		login := base
		suffix := 2
		for used[login] {
			login = fmt.Sprintf("%s-%d", base, suffix)
			suffix++
		}
		used[login] = true
		users = append(users, ScenarioUser{
			ID:    1000 + i,
			Login: login,
			Name:  first + " " + last,
			Email: login + "@example.invalid",
		})
	}
	return users
}

// --- Object model ---

type GitObject struct {
	Type string // blob | tree | commit | tag
	Data []byte
}

type Label struct {
	Name        string
	Color       string
	Description *string
}

type CommentRec struct {
	ID          int
	Body        string
	AuthorLogin string
	AtSec       int64
}

type IssueRec struct {
	Number      int
	ID          int
	Title       string
	Body        string
	State       string // open | closed
	Labels      []string
	Comments    []*CommentRec
	AuthorLogin string
	AtSec       int64
	UpdatedSec  int64
}

type PullRec struct {
	Number         int
	ID             int
	Title          string
	Body           string
	State          string // open | closed
	HeadRef        string
	BaseRef        string
	Merged         bool
	MergeCommitSha *string
	MergedAtSec    *int64
	Comments       []*CommentRec
	AuthorLogin    string
	AtSec          int64
	UpdatedSec     int64
}

// ThreadRec unifies issues and pulls (Kind: "issue" | "pull").
type ThreadRec struct {
	Kind  string
	Issue *IssueRec
	Pull  *PullRec
}

func (t *ThreadRec) Number() int {
	if t.Kind == "issue" {
		return t.Issue.Number
	}
	return t.Pull.Number
}
func (t *ThreadRec) ID() int {
	if t.Kind == "issue" {
		return t.Issue.ID
	}
	return t.Pull.ID
}
func (t *ThreadRec) Title() string {
	if t.Kind == "issue" {
		return t.Issue.Title
	}
	return t.Pull.Title
}
func (t *ThreadRec) SetTitle(v string) {
	if t.Kind == "issue" {
		t.Issue.Title = v
	} else {
		t.Pull.Title = v
	}
}
func (t *ThreadRec) Body() string {
	if t.Kind == "issue" {
		return t.Issue.Body
	}
	return t.Pull.Body
}
func (t *ThreadRec) SetBody(v string) {
	if t.Kind == "issue" {
		t.Issue.Body = v
	} else {
		t.Pull.Body = v
	}
}
func (t *ThreadRec) State() string {
	if t.Kind == "issue" {
		return t.Issue.State
	}
	return t.Pull.State
}
func (t *ThreadRec) SetState(v string) {
	if t.Kind == "issue" {
		t.Issue.State = v
	} else {
		t.Pull.State = v
	}
}
func (t *ThreadRec) Comments() []*CommentRec {
	if t.Kind == "issue" {
		return t.Issue.Comments
	}
	return t.Pull.Comments
}
func (t *ThreadRec) AppendComment(c *CommentRec) {
	if t.Kind == "issue" {
		t.Issue.Comments = append(t.Issue.Comments, c)
	} else {
		t.Pull.Comments = append(t.Pull.Comments, c)
	}
}
func (t *ThreadRec) AuthorLogin() string {
	if t.Kind == "issue" {
		return t.Issue.AuthorLogin
	}
	return t.Pull.AuthorLogin
}
func (t *ThreadRec) AtSec() int64 {
	if t.Kind == "issue" {
		return t.Issue.AtSec
	}
	return t.Pull.AtSec
}
func (t *ThreadRec) UpdatedSec() int64 {
	if t.Kind == "issue" {
		return t.Issue.UpdatedSec
	}
	return t.Pull.UpdatedSec
}
func (t *ThreadRec) SetUpdatedSec(v int64) {
	if t.Kind == "issue" {
		t.Issue.UpdatedSec = v
	} else {
		t.Pull.UpdatedSec = v
	}
}

type Repo struct {
	DatabaseID       int
	Owner            string
	Name             string
	Description      string
	IsPrivate        bool
	DefaultBranch    string
	Objects          map[string]*GitObject
	RefList          []string // insertion order, mirroring JS Map iteration
	Refs             map[string]string
	Labels           map[string]*Label
	LabelOrder       []string
	Threads          map[int]*ThreadRec
	ThreadOrder      []int
	NextThreadNumber int
	CreatedSec       int64
}

func (r *Repo) setRef(name, oid string) {
	if _, ok := r.Refs[name]; !ok {
		r.RefList = append(r.RefList, name)
	}
	r.Refs[name] = oid
}

func (r *Repo) deleteRef(name string) {
	if _, ok := r.Refs[name]; !ok {
		return
	}
	delete(r.Refs, name)
	for i, n := range r.RefList {
		if n == name {
			r.RefList = append(r.RefList[:i], r.RefList[i+1:]...)
			break
		}
	}
}

func (r *Repo) setThread(t *ThreadRec) {
	n := t.Number()
	if _, ok := r.Threads[n]; !ok {
		r.ThreadOrder = append(r.ThreadOrder, n)
	}
	r.Threads[n] = t
}

// threadsInOrder mirrors [...repo.threads.values()] (insertion order).
func (r *Repo) threadsInOrder() []*ThreadRec {
	out := make([]*ThreadRec, 0, len(r.ThreadOrder))
	for _, n := range r.ThreadOrder {
		out = append(out, r.Threads[n])
	}
	return out
}

const clockEpochSec int64 = 1735689600

type Store struct {
	Viewer     ScenarioUser
	Users      []ScenarioUser
	Repos      map[string]*Repo
	RepoOrder  []string
	ReposByID  map[int]*Repo
	repoSeq    int
	issueSeq   int
	commentSeq int
	clockSec   int64
	isoMemo    map[int64]string
}

func NewStore() *Store {
	users := scenarioUsers(12)
	return &Store{
		Viewer:    users[0],
		Users:     users,
		Repos:     map[string]*Repo{},
		ReposByID: map[int]*Repo{},
		clockSec:  clockEpochSec,
		isoMemo:   map[int64]string{},
	}
}

func (s *Store) Tick() int64 {
	s.clockSec++
	return s.clockSec
}

func (s *Store) NowSec() int64 { return s.clockSec }

func (s *Store) IsoOf(sec int64) string {
	if hit, ok := s.isoMemo[sec]; ok {
		return hit
	}
	text := time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05Z")
	if len(s.isoMemo) >= 4096 {
		s.isoMemo = map[int64]string{}
	}
	s.isoMemo[sec] = text
	return text
}

func (s *Store) CreateRepo(name string, description string, isPrivate bool) *Repo {
	s.repoSeq++
	repo := &Repo{
		DatabaseID:       s.repoSeq,
		Owner:            s.Viewer.Login,
		Name:             name,
		Description:      description,
		IsPrivate:        isPrivate,
		DefaultBranch:    "main",
		Objects:          map[string]*GitObject{},
		Refs:             map[string]string{},
		Labels:           map[string]*Label{},
		Threads:          map[int]*ThreadRec{},
		NextThreadNumber: 1,
		CreatedSec:       s.Tick(),
	}
	key := repo.Owner + "/" + repo.Name
	s.Repos[key] = repo
	s.RepoOrder = append(s.RepoOrder, key)
	s.ReposByID[repo.DatabaseID] = repo
	return repo
}

func (s *Store) RepoOf(owner, name string) *Repo {
	if owner != s.Viewer.Login {
		return nil
	}
	return s.Repos[owner+"/"+name]
}

func (s *Store) DeleteRepo(repo *Repo) {
	key := repo.Owner + "/" + repo.Name
	delete(s.Repos, key)
	for i, k := range s.RepoOrder {
		if k == key {
			s.RepoOrder = append(s.RepoOrder[:i], s.RepoOrder[i+1:]...)
			break
		}
	}
	delete(s.ReposByID, repo.DatabaseID)
}

func (s *Store) RenameRepo(repo *Repo, newName string) {
	// JS Map delete + set moves the key to the end of the iteration order.
	oldKey := repo.Owner + "/" + repo.Name
	delete(s.Repos, oldKey)
	for i, k := range s.RepoOrder {
		if k == oldKey {
			s.RepoOrder = append(s.RepoOrder[:i], s.RepoOrder[i+1:]...)
			break
		}
	}
	repo.Name = newName
	newKey := repo.Owner + "/" + repo.Name
	s.Repos[newKey] = repo
	s.RepoOrder = append(s.RepoOrder, newKey)
}

func (s *Store) NextIssueID() int {
	s.issueSeq++
	return s.issueSeq
}

func (s *Store) NextCommentID() int {
	s.commentSeq++
	return s.commentSeq
}

func (s *Store) Identity() string {
	return s.Viewer.Name + " <" + s.Viewer.Email + ">"
}

// reposInOrder mirrors [...store.repos.values()].
func (s *Store) reposInOrder() []*Repo {
	out := make([]*Repo, 0, len(s.RepoOrder))
	for _, k := range s.RepoOrder {
		out = append(out, s.Repos[k])
	}
	return out
}

func (s *Store) userByLogin(login string) ScenarioUser {
	for _, u := range s.Users {
		if u.Login == login {
			return u
		}
	}
	return s.Viewer
}

// --- Relay-style global ids ---

type GlobalID struct {
	Kind       string // repository | issue | pull | comment | user
	DatabaseID int
	Number     int
	CommentID  int
	UserID     int
}

func globalIdOf(id GlobalID) string {
	var raw string
	switch id.Kind {
	case "repository":
		raw = "R" + strconv.Itoa(id.DatabaseID)
	case "issue":
		raw = "I" + strconv.Itoa(id.DatabaseID) + ":" + strconv.Itoa(id.Number)
	case "pull":
		raw = "P" + strconv.Itoa(id.DatabaseID) + ":" + strconv.Itoa(id.Number)
	case "comment":
		raw = "C" + strconv.Itoa(id.DatabaseID) + ":" + strconv.Itoa(id.Number) + ":" + strconv.Itoa(id.CommentID)
	case "user":
		raw = "U" + strconv.Itoa(id.UserID)
	}
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

var (
	reGlobalR = regexp.MustCompile(`^R(\d+)$`)
	reGlobalI = regexp.MustCompile(`^I(\d+):(\d+)$`)
	reGlobalP = regexp.MustCompile(`^P(\d+):(\d+)$`)
	reGlobalC = regexp.MustCompile(`^C(\d+):(\d+):(\d+)$`)
	reGlobalU = regexp.MustCompile(`^U(\d+)$`)
)

// lenientBase64Decode mirrors Buffer.from(text, 'base64'): characters outside
// the alphabet are ignored and missing padding is tolerated.
func lenientBase64Decode(text string) ([]byte, error) {
	var clean strings.Builder
	for _, r := range text {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '+' || r == '/' || r == '=' {
			clean.WriteRune(r)
		}
	}
	s := clean.String()
	s = strings.TrimRight(s, "=")
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	case 1:
		s += "==="
	}
	return base64.StdEncoding.DecodeString(s)
}

func parseGlobalId(text string) *GlobalID {
	rawBytes, err := lenientBase64Decode(text)
	if err != nil {
		return nil
	}
	raw := string(rawBytes)
	atoi := func(s string) int {
		n, _ := strconv.Atoi(s)
		return n
	}
	if m := reGlobalR.FindStringSubmatch(raw); m != nil {
		return &GlobalID{Kind: "repository", DatabaseID: atoi(m[1])}
	}
	if m := reGlobalI.FindStringSubmatch(raw); m != nil {
		return &GlobalID{Kind: "issue", DatabaseID: atoi(m[1]), Number: atoi(m[2])}
	}
	if m := reGlobalP.FindStringSubmatch(raw); m != nil {
		return &GlobalID{Kind: "pull", DatabaseID: atoi(m[1]), Number: atoi(m[2])}
	}
	if m := reGlobalC.FindStringSubmatch(raw); m != nil {
		return &GlobalID{Kind: "comment", DatabaseID: atoi(m[1]), Number: atoi(m[2]), CommentID: atoi(m[3])}
	}
	if m := reGlobalU.FindStringSubmatch(raw); m != nil {
		return &GlobalID{Kind: "user", UserID: atoi(m[1])}
	}
	return nil
}

func hashObject(objType string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(fmt.Sprintf("%s %d\x00", objType, len(data))))
	h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}

const zeroOID = "0000000000000000000000000000000000000000"
