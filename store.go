package main

import (
	"encoding/base64"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Data model (mirrors the reference TypeScript forge's shapes exactly)
// ---------------------------------------------------------------------------

type User struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Type  string `json:"type"`
}

var viewer = User{
	Login: "adaiversen",
	ID:    1000,
	Name:  "ada iversen",
	Email: "adaiversen@example.invalid",
	Type:  "User",
}

type Label struct {
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
}

type Issue struct {
	ID        int     `json:"id"`
	NodeID    string  `json:"node_id"`
	Number    int     `json:"number"`
	Title     string  `json:"title"`
	Body      *string `json:"body"`
	State     string  `json:"state"`
	Labels    []Label `json:"labels"`
	User      User    `json:"user"`
	Comments  int     `json:"comments"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	HTMLURL   string  `json:"html_url"`
}

type PRRef struct {
	Ref   string `json:"ref"`
	Sha   string `json:"sha"`
	Label string `json:"label"`
}

type PullRequest struct {
	ID             int     `json:"id"`
	NodeID         string  `json:"node_id"`
	Number         int     `json:"number"`
	Title          string  `json:"title"`
	Body           *string `json:"body"`
	State          string  `json:"state"`
	Head           PRRef   `json:"head"`
	Base           PRRef   `json:"base"`
	Merged         bool    `json:"merged"`
	Mergeable      *bool   `json:"mergeable"`
	User           User    `json:"user"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
	MergedAt       *string `json:"merged_at"`
	HTMLURL        string  `json:"html_url"`
	MergedBy       *User   `json:"merged_by"`
	MergeCommitSha *string `json:"merge_commit_sha"`
}

type Comment struct {
	ID        int    `json:"id"`
	NodeID    string `json:"node_id"`
	Body      string `json:"body"`
	User      User   `json:"user"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	HTMLURL   string `json:"html_url"`
}

type GitObject struct {
	Type string
	Raw  []byte
}

type Repository struct {
	ID            int
	NodeID        string
	Name          string
	FullName      string
	Owner         User
	Private       bool
	Description   *string
	DefaultBranch string
	HTMLURL       string
	CloneURL      string
	CreatedAt     string
	UpdatedAt     string

	Objects map[string]*GitObject
	Refs    map[string]string
	RefOrder []string

	Labels     map[string]*Label
	LabelOrder []string

	Issues   map[int]*Issue
	Pulls    map[int]*PullRequest
	PullOrder []int
	Comments map[int][]*Comment

	packCache map[string][]byte
	packMu    sync.Mutex
}

func (r *Repository) setRef(name, sha string) {
	if _, ok := r.Refs[name]; !ok {
		r.RefOrder = append(r.RefOrder, name)
	}
	r.Refs[name] = sha
}

func (r *Repository) deleteRef(name string) {
	if _, ok := r.Refs[name]; !ok {
		return
	}
	delete(r.Refs, name)
	for i, n := range r.RefOrder {
		if n == name {
			r.RefOrder = append(r.RefOrder[:i], r.RefOrder[i+1:]...)
			break
		}
	}
}

func (r *Repository) setLabel(l *Label) {
	if _, ok := r.Labels[l.Name]; !ok {
		r.LabelOrder = append(r.LabelOrder, l.Name)
	}
	r.Labels[l.Name] = l
}

// ---------------------------------------------------------------------------
// Global store and counters
// ---------------------------------------------------------------------------

var (
	storeMu          sync.RWMutex
	repositories     = map[string]*Repository{}
	repoOrder        []string
	logicalClock     atomic.Int64
	repoIDCounter    = 5000
	nodeIDCounter    = 1
	requestIDCounter atomic.Int64
)

func nextTimestamp() string {
	n := logicalClock.Add(1)
	return time.Unix(1735689600+n, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

func nextRequestID() string {
	return fmt.Sprintf("req_%d", requestIDCounter.Add(1))
}

func globalNodeID(typeName string, dbID int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("0X:%s%d", typeName, dbID)))
}

func createRepositoryInternal(name string, description *string, private bool) *Repository {
	repoIDCounter++
	repo := &Repository{
		ID:            repoIDCounter,
		NodeID:        globalNodeID("Repository", repoIDCounter),
		Name:          name,
		FullName:      viewer.Login + "/" + name,
		Owner:         viewer,
		Private:       private,
		Description:   description,
		DefaultBranch: "main",
		HTMLURL:       "http://localhost/",
		CloneURL:      fmt.Sprintf("http://localhost/%s/%s.git", viewer.Login, name),
		CreatedAt:     nextTimestamp(),
		UpdatedAt:     nextTimestamp(),
		Objects:       map[string]*GitObject{},
		Refs:          map[string]string{},
		Labels:        map[string]*Label{},
		Issues:        map[int]*Issue{},
		Pulls:         map[int]*PullRequest{},
		Comments:      map[int][]*Comment{},
		packCache:     map[string][]byte{},
	}
	repositories[name] = repo
	repoOrder = append(repoOrder, name)
	return repo
}

func deleteRepository(name string) {
	if _, ok := repositories[name]; !ok {
		return
	}
	delete(repositories, name)
	for i, n := range repoOrder {
		if n == name {
			repoOrder = append(repoOrder[:i], repoOrder[i+1:]...)
			break
		}
	}
}

// repoToJson mirrors the TS repoToJson field set and order.
type repoJSON struct {
	ID            int     `json:"id"`
	NodeID        string  `json:"node_id"`
	Name          string  `json:"name"`
	FullName      string  `json:"full_name"`
	Owner         User    `json:"owner"`
	Private       bool    `json:"private"`
	HTMLURL       string  `json:"html_url"`
	Description   *string `json:"description"`
	Fork          bool    `json:"fork"`
	URL           string  `json:"url"`
	CloneURL      string  `json:"clone_url"`
	DefaultBranch string  `json:"default_branch"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

func repoToJSON(r *Repository) repoJSON {
	return repoJSON{
		ID:            r.ID,
		NodeID:        r.NodeID,
		Name:          r.Name,
		FullName:      r.FullName,
		Owner:         r.Owner,
		Private:       r.Private,
		HTMLURL:       r.HTMLURL,
		Description:   r.Description,
		Fork:          false,
		URL:           fmt.Sprintf("http://localhost/api/v3/repos/%s/%s", r.Owner.Login, r.Name),
		CloneURL:      r.CloneURL,
		DefaultBranch: r.DefaultBranch,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
	}
}

func strPtr(s string) *string { return &s }

func strVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
