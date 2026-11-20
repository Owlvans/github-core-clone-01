package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// REST v3 API (ported from the TS forge's handleRestApi)
// ---------------------------------------------------------------------------

var (
	base64Std = base64.StdEncoding
	base64Raw = base64.RawStdEncoding
)

var repoPathRe = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)(/.*)?$`)
var contentsPathRe = regexp.MustCompile(`^/contents(/.*)?$`)

func handleRestAPI(w http.ResponseWriter, r *http.Request, apiPath string) {
	// Locking: plain GETs run under the read lock, except pull paths whose
	// handlers refresh mergeable state; everything else takes the write lock.
	if r.Method == http.MethodGet && !strings.Contains(apiPath, "/pulls") {
		storeMu.RLock()
		defer storeMu.RUnlock()
	} else {
		storeMu.Lock()
		defer storeMu.Unlock()
	}
	w.Header().Set("x-github-request-id", nextRequestID())
	w.Header().Set("access-control-allow-origin", "*")

	method := r.Method
	query := r.URL.Query()

	if apiPath == "/" || apiPath == "" {
		w.Header().Set("x-oauth-scopes", "repo, read:org")
		writeJSON(w, 200, map[string]string{
			"current_user_url":                      "http://localhost/api/v3/user",
			"current_user_authorization_html_url":   "http://localhost/settings/connections/applications{/client_id}",
			"current_authorizations_html_url":       "http://localhost/settings/tokens{/client_id}",
			"authorisations_url":                    "http://localhost/api/v3/authorizations",
			"user_repositories_url":                 "http://localhost/api/v3/users/{user}/repos{?type,page,per_page,sort}",
		})
		return
	}

	if apiPath == "/meta" {
		writeJSON(w, 200, map[string]string{"installed_version": "1.0.0"})
		return
	}

	if apiPath == "/user" {
		writeJSON(w, 200, viewer)
		return
	}

	if apiPath == "/user/repos" {
		if method == http.MethodGet {
			perPage := queryInt(query, "per_page", 30)
			page := queryInt(query, "page", 1)
			all := make([]repoJSON, 0, len(repoOrder))
			for _, name := range repoOrder {
				all = append(all, repoToJSON(repositories[name]))
			}
			start := (page - 1) * perPage
			if start < 0 {
				start = 0
			}
			if start > len(all) {
				start = len(all)
			}
			end := start + perPage
			if end > len(all) {
				end = len(all)
			}
			if start+perPage < len(all) {
				host := r.Host
				if host == "" {
					host = "localhost"
				}
				w.Header().Set("link", fmt.Sprintf("<http://%s/api/v3/user/repos?page=%d&per_page=%d>; rel=\"next\"", host, page+1, perPage))
			}
			writeJSON(w, 200, all[start:end])
			return
		} else if method == http.MethodPost {
			body, err := parseJSONBody(r)
			if err != nil {
				writeJSONMsg(w, 400, "Malformed JSON")
				return
			}
			nameVal, hasName := body["name"]
			name, isStr := nameVal.(string)
			if !hasName || !isStr || name == "" {
				writeJSONMsg(w, 422, "Repository name is required")
				return
			}
			if _, exists := repositories[name]; exists {
				writeJSONMsg(w, 422, "Repository already exists")
				return
			}
			var description *string
			if d, ok := body["description"].(string); ok {
				description = &d
			}
			isPrivate, _ := body["private"].(bool)
			repo := createRepositoryInternal(name, description, isPrivate)
			writeJSON(w, 201, repoToJSON(repo))
			return
		}
		writeJSONMsg(w, 404, "Not Found")
		return
	}

	m := repoPathRe.FindStringSubmatch(apiPath)
	if m == nil {
		writeJSONMsg(w, 404, "Not Found")
		return
	}
	owner, repoName, subPath := m[1], m[2], m[3]
	if subPath == "" {
		subPath = "/"
	}
	if owner != viewer.Login {
		writeJSONMsg(w, 404, "Not Found")
		return
	}
	repo, ok := repositories[repoName]
	if !ok {
		writeJSONMsg(w, 404, "Repository not found")
		return
	}
	handleRepoRoutes(w, r, repo, owner, repoName, subPath, query)
}

func queryInt(q url.Values, key string, def int) int {
	if v := q.Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func handleRepoRoutes(w http.ResponseWriter, r *http.Request, repo *Repository, owner, repoName, subPath string, query url.Values) {
	method := r.Method

	if subPath == "/" {
		switch method {
		case http.MethodGet:
			writeJSON(w, 200, repoToJSON(repo))
			return
		case http.MethodPatch:
			body, err := parseJSONBody(r)
			if err != nil {
				writeJSONMsg(w, 400, "Malformed JSON")
				return
			}
			if d, present := body["description"]; present {
				if s, isStr := d.(string); isStr {
					repo.Description = &s
				} else if d == nil {
					repo.Description = nil
				}
			}
			if p, present := body["private"]; present {
				if b, isBool := p.(bool); isBool {
					repo.Private = b
				}
			}
			repo.UpdatedAt = nextTimestamp()
			writeJSON(w, 200, repoToJSON(repo))
			return
		case http.MethodDelete:
			deleteRepository(repoName)
			w.WriteHeader(204)
			return
		}
		writeJSONMsg(w, 404, "Not Found")
		return
	}

	if subPath == "/branches" {
		if method == http.MethodGet {
			branches := []map[string]any{}
			for _, refname := range repo.RefOrder {
				sha := repo.Refs[refname]
				if strings.HasPrefix(refname, "refs/heads/") {
					name := refname[len("refs/heads/"):]
					branches = append(branches, map[string]any{
						"name":   name,
						"commit": map[string]string{"sha": sha, "url": fmt.Sprintf("http://localhost/api/v3/repos/%s/%s/commits/%s", owner, repoName, sha)},
					})
				}
			}
			writeJSON(w, 200, branches)
			return
		}
		writeJSONMsg(w, 404, "Not Found")
		return
	}

	if strings.HasPrefix(subPath, "/git/refs") {
		if method == http.MethodPost {
			body, err := parseJSONBody(r)
			if err != nil {
				writeJSONMsg(w, 400, "Malformed JSON")
				return
			}
			ref, _ := body["ref"].(string)
			sha, _ := body["sha"].(string)
			if ref == "" || sha == "" {
				writeJSONMsg(w, 422, "ref and sha required")
				return
			}
			repo.setRef(ref, sha)
			writeJSON(w, 201, map[string]any{
				"ref":    ref,
				"object": map[string]string{"sha": sha, "type": "commit", "url": fmt.Sprintf("http://localhost/api/v3/repos/%s/%s/git/commits/%s", owner, repoName, sha)},
			})
			return
		}
		writeJSONMsg(w, 404, "Not Found")
		return
	}

	if subPath == "/commits" {
		if method == http.MethodGet {
			handleListCommits(w, repo, owner, repoName, query)
			return
		}
		writeJSONMsg(w, 404, "Not Found")
		return
	}

	if cm := contentsPathRe.FindStringSubmatch(subPath); cm != nil {
		handleContents(w, r, repo, owner, repoName, cm[1], query)
		return
	}

	if subPath == "/labels" {
		handleLabels(w, r, repo)
		return
	}

	if subPath == "/issues" || strings.HasPrefix(subPath, "/issues/") {
		handleIssues(w, r, repo, owner, repoName, subPath, query)
		return
	}

	if subPath == "/pulls" || strings.HasPrefix(subPath, "/pulls/") {
		handlePulls(w, r, repo, owner, repoName, subPath, query)
		return
	}

	writeJSONMsg(w, 404, "Not Found")
}

func handleListCommits(w http.ResponseWriter, repo *Repository, owner, repoName string, query url.Values) {
	value := query.Get("sha")
	var shaParam string
	switch {
	case value != "" && repo.Refs["refs/heads/"+value] != "":
		shaParam = repo.Refs["refs/heads/"+value]
	case value != "" && repo.Refs["refs/tags/"+value] != "":
		shaParam = repo.Refs["refs/tags/"+value]
	case value != "" && repo.Objects[value] != nil:
		shaParam = value
	default:
		shaParam = repo.Refs["refs/heads/"+repo.DefaultBranch]
		if shaParam == "" {
			shaParam = repo.Refs["HEAD"]
		}
	}
	type commitEntry struct {
		Sha    string `json:"sha"`
		Commit struct {
			Author struct {
				Name  string `json:"name"`
				Email string `json:"email"`
				Date  string `json:"date"`
			} `json:"author"`
			Committer struct {
				Name  string `json:"name"`
				Email string `json:"email"`
				Date  string `json:"date"`
			} `json:"committer"`
			Message string `json:"message"`
		} `json:"commit"`
		Author    User `json:"author"`
		Committer User `json:"committer"`
		Parents   []struct {
			Sha string `json:"sha"`
			URL string `json:"url"`
		} `json:"parents"`
	}
	commits := []commitEntry{}
	curr := shaParam
	visited := map[string]bool{}
	for curr != "" && !visited[curr] {
		obj, ok := repo.Objects[curr]
		if !ok {
			break
		}
		visited[curr] = true
		if obj.Type != "commit" {
			break
		}
		parsed := parseCommit(obj.Raw)
		var e commitEntry
		e.Sha = curr
		e.Commit.Author.Name = "ada iversen"
		e.Commit.Author.Email = "adaiversen@example.invalid"
		e.Commit.Author.Date = nextTimestamp()
		e.Commit.Committer.Name = "ada iversen"
		e.Commit.Committer.Email = "adaiversen@example.invalid"
		e.Commit.Committer.Date = nextTimestamp()
		e.Commit.Message = parsed.Message
		e.Author = viewer
		e.Committer = viewer
		e.Parents = []struct {
			Sha string `json:"sha"`
			URL string `json:"url"`
		}{}
		for _, p := range parsed.Parents {
			e.Parents = append(e.Parents, struct {
				Sha string `json:"sha"`
				URL string `json:"url"`
			}{Sha: p, URL: fmt.Sprintf("http://localhost/api/v3/repos/%s/%s/commits/%s", owner, repoName, p)})
		}
		commits = append(commits, e)
		if len(parsed.Parents) > 0 {
			curr = parsed.Parents[0]
		} else {
			curr = ""
		}
	}
	writeJSON(w, 200, commits)
}

func handleContents(w http.ResponseWriter, r *http.Request, repo *Repository, owner, repoName, rawPathWithSlash string, query url.Values) {
	method := r.Method
	rawPath := strings.TrimPrefix(rawPathWithSlash, "/")
	var pathParts []string
	if rawPath != "" {
		pathParts = strings.Split(rawPath, "/")
	}
	refParam := query.Get("ref")
	if refParam == "" {
		refParam = "refs/heads/" + repo.DefaultBranch
	}
	commitSha := repo.Refs[refParam]
	if commitSha == "" {
		commitSha = repo.Refs["refs/heads/"+refParam]
	}
	if commitSha == "" {
		commitSha = repo.Refs["HEAD"]
	}
	if commitSha == "" {
		commitSha = refParam
	}
	treeSha := ""
	if commitObj, ok := repo.Objects[commitSha]; ok {
		if commitObj.Type == "commit" {
			treeSha = parseCommit(commitObj.Raw).Tree
		} else if commitObj.Type == "tree" {
			treeSha = commitSha
		}
	}

	switch method {
	case http.MethodGet:
		if treeSha == "" {
			writeJSONMsg(w, 404, "Not Found")
			return
		}
		if rawPath == "" {
			writeJSON(w, 200, getDirectoryEntries(repo, treeSha))
			return
		}
		entry := getTreeEntryByPath(repo, treeSha, pathParts)
		if entry == nil {
			writeJSONMsg(w, 404, "Not Found")
			return
		}
		if entry.Type == "tree" {
			writeJSON(w, 200, getDirectoryEntries(repo, entry.Sha))
			return
		}
		blobObj, ok := repo.Objects[entry.Sha]
		if !ok {
			writeJSONMsg(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, map[string]any{
			"name":     pathParts[len(pathParts)-1],
			"path":     rawPath,
			"sha":      entry.Sha,
			"size":     len(blobObj.Raw),
			"type":     "file",
			"encoding": "base64",
			"content":  blobObj.Raw,
			"html_url": fmt.Sprintf("http://localhost/%s/%s/blob/%s/%s", owner, repoName, refParam, rawPath),
		})
		return
	case http.MethodPut:
		body, err := parseJSONBody(r)
		if err != nil {
			writeJSONMsg(w, 400, "Malformed JSON or invalid request")
			return
		}
		message, _ := body["message"].(string)
		contentVal, hasContent := body["content"]
		if message == "" || !hasContent {
			writeJSONMsg(w, 422, "message and content required")
			return
		}
		contentStr, _ := contentVal.(string)
		decoded := base64Decode(contentStr)
		newBlobSha := hashObject("blob", decoded)
		repo.Objects[newBlobSha] = &GitObject{Type: "blob", Raw: decoded}
		branch, _ := body["branch"].(string)
		if branch == "" {
			branch = repo.DefaultBranch
		}
		branchRef := "refs/heads/" + branch
		currentCommitSha := repo.Refs[branchRef]
		currentTreeSha := ""
		var parents []string
		if currentCommitSha != "" {
			parents = append(parents, currentCommitSha)
			if parentObj, ok := repo.Objects[currentCommitSha]; ok && parentObj.Type == "commit" {
				currentTreeSha = parseCommit(parentObj.Raw).Tree
			}
		}
		expectedSha, _ := body["sha"].(string)
		if expectedSha != "" && rawPath != "" {
			if existing := getTreeEntryByPath(repo, currentTreeSha, pathParts); existing != nil && existing.Sha != expectedSha {
				writeJSONMsg(w, 409, "Conflict: sha does not match current file sha")
				return
			}
		}
		newTreeSha := updateTreeAtPath(repo, currentTreeSha, currentTreeSha != "", pathParts, &newBlobSha)
		commitRaw := serializeCommit(CommitObject{
			Tree:      newTreeSha,
			Parents:   parents,
			Author:    fmt.Sprintf("%s <%s> %s +0000", viewer.Name, viewer.Email, nextTimestamp()),
			Committer: fmt.Sprintf("%s <%s> %s +0000", viewer.Name, viewer.Email, nextTimestamp()),
			Message:   message,
		})
		newCommitSha := hashObject("commit", commitRaw)
		repo.Objects[newCommitSha] = &GitObject{Type: "commit", Raw: commitRaw}
		repo.setRef(branchRef, newCommitSha)
		isNew := expectedSha == "" && getTreeEntryByPath(repo, currentTreeSha, pathParts) == nil
		status := 200
		if isNew {
			status = 201
		}
		parentList := []map[string]string{}
		for _, p := range parents {
			parentList = append(parentList, map[string]string{"sha": p})
		}
		writeJSON(w, status, map[string]any{
			"content": map[string]any{
				"name": pathParts[len(pathParts)-1], "path": rawPath, "sha": newBlobSha,
				"size": len(decoded), "type": "file", "encoding": "base64",
			},
			"commit": map[string]any{
				"sha": newCommitSha, "message": message, "tree": newTreeSha, "parents": parentList,
			},
		})
		return
	case http.MethodDelete:
		body, err := parseJSONBody(r)
		if err != nil {
			writeJSONMsg(w, 400, "Malformed JSON")
			return
		}
		message, _ := body["message"].(string)
		if message == "" {
			writeJSONMsg(w, 422, "message required")
			return
		}
		branch, _ := body["branch"].(string)
		if branch == "" {
			branch = repo.DefaultBranch
		}
		branchRef := "refs/heads/" + branch
		currentCommitSha := repo.Refs[branchRef]
		currentTreeSha := ""
		var parents []string
		if currentCommitSha != "" {
			parents = append(parents, currentCommitSha)
			if parentObj, ok := repo.Objects[currentCommitSha]; ok && parentObj.Type == "commit" {
				currentTreeSha = parseCommit(parentObj.Raw).Tree
			}
		}
		newTreeSha := updateTreeAtPath(repo, currentTreeSha, currentTreeSha != "", pathParts, nil)
		commitRaw := serializeCommit(CommitObject{
			Tree:      newTreeSha,
			Parents:   parents,
			Author:    fmt.Sprintf("%s <%s> %s +0000", viewer.Name, viewer.Email, nextTimestamp()),
			Committer: fmt.Sprintf("%s <%s> %s +0000", viewer.Name, viewer.Email, nextTimestamp()),
			Message:   message,
		})
		newCommitSha := hashObject("commit", commitRaw)
		repo.Objects[newCommitSha] = &GitObject{Type: "commit", Raw: commitRaw}
		repo.setRef(branchRef, newCommitSha)
		writeJSON(w, 200, map[string]any{
			"content": nil,
			"commit":  map[string]string{"sha": newCommitSha, "message": message},
		})
		return
	}
	writeJSONMsg(w, 404, "Not Found")
}

func handleLabels(w http.ResponseWriter, r *http.Request, repo *Repository) {
	switch r.Method {
	case http.MethodGet:
		labels := []*Label{}
		for _, name := range repo.LabelOrder {
			labels = append(labels, repo.Labels[name])
		}
		writeJSON(w, 200, labels)
		return
	case http.MethodPost:
		body, err := parseJSONBody(r)
		if err != nil {
			writeJSONMsg(w, 400, "Malformed JSON")
			return
		}
		name, _ := body["name"].(string)
		if name == "" {
			writeJSONMsg(w, 422, "Label name required")
			return
		}
		color, _ := body["color"].(string)
		if _, present := body["color"]; !present {
			color = "ededed"
		}
		label := &Label{Name: name, Color: color}
		repo.setLabel(label)
		writeJSON(w, 201, label)
		return
	}
	writeJSONMsg(w, 404, "Not Found")
}

func handleIssues(w http.ResponseWriter, r *http.Request, repo *Repository, owner, repoName, subPath string, query url.Values) {
	method := r.Method
	parts := strings.Split(strings.TrimPrefix(subPath, "/"), "/")
	if len(parts) == 1 {
		if method == http.MethodGet {
			state := query.Get("state")
			if state == "" {
				state = "open"
			}
			direction := query.Get("direction")
			if direction == "" {
				direction = "desc"
			}
			list := []*Issue{}
			for _, issue := range repo.Issues {
				if state != "all" && issue.State != state {
					continue
				}
				list = append(list, issue)
			}
			sort.Slice(list, func(i, j int) bool {
				if direction == "asc" {
					return list[i].Number < list[j].Number
				}
				return list[i].Number > list[j].Number
			})
			if list == nil {
				list = []*Issue{}
			}
			writeJSON(w, 200, list)
			return
		}
		if method == http.MethodPost {
			body, err := parseJSONBody(r)
			if err != nil {
				writeJSONMsg(w, 400, "Malformed JSON")
				return
			}
			title, _ := body["title"].(string)
			if title == "" {
				writeJSONMsg(w, 422, "Issue title is required")
				return
			}
			var issueBody *string
			if b, present := body["body"]; present {
				if s, isStr := b.(string); isStr {
					issueBody = &s
				}
			}
			formattedLabels := []Label{}
			if labelsRaw, present := body["labels"]; present {
				if labelsArr, isArr := labelsRaw.([]any); isArr {
					for _, l := range labelsArr {
						var lName string
						switch lv := l.(type) {
						case string:
							lName = lv
						case map[string]any:
							lName, _ = lv["name"].(string)
						}
						existing, ok := repo.Labels[lName]
						if !ok {
							writeJSONMsg(w, 422, fmt.Sprintf("Label \"%s\" does not exist", lName))
							return
						}
						formattedLabels = append(formattedLabels, *existing)
					}
				}
			}
			issueNumber := len(repo.Issues) + len(repo.Pulls) + 1
			nodeIDCounter++
			issue := &Issue{
				ID:        nodeIDCounter,
				NodeID:    globalNodeID("Issue", nodeIDCounter),
				Number:    issueNumber,
				Title:     title,
				Body:      issueBody,
				State:     "open",
				Labels:    formattedLabels,
				User:      viewer,
				Comments:  0,
				CreatedAt: nextTimestamp(),
				UpdatedAt: nextTimestamp(),
				HTMLURL:   fmt.Sprintf("http://localhost/%s/%s/issues/%d", owner, repoName, issueNumber),
			}
			repo.Issues[issueNumber] = issue
			writeJSON(w, 201, issue)
			return
		}
		writeJSONMsg(w, 404, "Not Found")
		return
	}

	issueNum, _ := strconv.Atoi(parts[1])
	issue, ok := repo.Issues[issueNum]
	if !ok {
		writeJSONMsg(w, 404, "Issue not found")
		return
	}
	if len(parts) == 2 {
		if method == http.MethodGet {
			writeJSON(w, 200, issue)
			return
		}
		if method == http.MethodPatch {
			body, err := parseJSONBody(r)
			if err != nil {
				writeJSONMsg(w, 400, "Malformed JSON")
				return
			}
			if s, present := body["state"].(string); present && s != "" {
				issue.State = s
			}
			if t, present := body["title"]; present {
				if s, isStr := t.(string); isStr {
					issue.Title = s
				}
			}
			if b, present := body["body"]; present {
				if s, isStr := b.(string); isStr {
					issue.Body = &s
				} else if b == nil {
					issue.Body = nil
				}
			}
			issue.UpdatedAt = nextTimestamp()
			writeJSON(w, 200, issue)
			return
		}
		writeJSONMsg(w, 404, "Not Found")
		return
	}
	if len(parts) == 3 && parts[2] == "comments" {
		if method == http.MethodGet {
			comments := repo.Comments[issueNum]
			if comments == nil {
				comments = []*Comment{}
			}
			writeJSON(w, 200, comments)
			return
		}
		if method == http.MethodPost {
			body, err := parseJSONBody(r)
			if err != nil {
				writeJSONMsg(w, 400, "Malformed JSON")
				return
			}
			commentBody, _ := body["body"].(string)
			if commentBody == "" {
				writeJSONMsg(w, 422, "Comment body required")
				return
			}
			nodeIDCounter++
			comment := &Comment{
				ID:        nodeIDCounter,
				NodeID:    globalNodeID("IssueComment", nodeIDCounter),
				Body:      commentBody,
				User:      viewer,
				CreatedAt: nextTimestamp(),
				UpdatedAt: nextTimestamp(),
				HTMLURL:   fmt.Sprintf("http://localhost/%s/%s/issues/%d#issuecomment-%d", owner, repoName, issueNum, nodeIDCounter),
			}
			repo.Comments[issueNum] = append(repo.Comments[issueNum], comment)
			issue.Comments = len(repo.Comments[issueNum])
			writeJSON(w, 201, comment)
			return
		}
		writeJSONMsg(w, 404, "Not Found")
		return
	}
	writeJSONMsg(w, 404, "Not Found")
}

func handlePulls(w http.ResponseWriter, r *http.Request, repo *Repository, owner, repoName, subPath string, query url.Values) {
	method := r.Method
	parts := strings.Split(strings.TrimPrefix(subPath, "/"), "/")
	if len(parts) == 1 {
		if method == http.MethodGet {
			state := query.Get("state")
			if state == "" {
				state = "open"
			}
			list := []*PullRequest{}
			for _, num := range repo.PullOrder {
				p := repo.Pulls[num]
				refreshMergeable(repo, p)
				if state != "all" && p.State != state {
					continue
				}
				list = append(list, p)
			}
			writeJSON(w, 200, list)
			return
		}
		if method == http.MethodPost {
			body, err := parseJSONBody(r)
			if err != nil {
				writeJSONMsg(w, 400, "Malformed JSON")
				return
			}
			title, _ := body["title"].(string)
			head, _ := body["head"].(string)
			base, _ := body["base"].(string)
			if title == "" || head == "" || base == "" {
				writeJSONMsg(w, 422, "title, head, and base required")
				return
			}
			headRefName := head
			if !strings.HasPrefix(headRefName, "refs/heads/") {
				headRefName = "refs/heads/" + headRefName
			}
			baseRefName := base
			if !strings.HasPrefix(baseRefName, "refs/heads/") {
				baseRefName = "refs/heads/" + baseRefName
			}
			headSha, hasHead := repo.Refs[headRefName]
			baseSha, hasBase := repo.Refs[baseRefName]
			if !hasHead || !hasBase {
				writeJSONMsg(w, 422, "Head or base ref not found")
				return
			}
			hasConflict := checkConflict(repo, headSha, baseSha)
			prNumber := len(repo.Issues) + len(repo.Pulls) + 1
			nodeIDCounter++
			mergeable := !hasConflict
			pr := &PullRequest{
				ID:        nodeIDCounter,
				NodeID:    globalNodeID("PullRequest", nodeIDCounter),
				Number:    prNumber,
				Title:     title,
				State:     "open",
				Head:      PRRef{Ref: strings.TrimPrefix(headRefName, "refs/heads/"), Sha: headSha, Label: owner + ":" + strings.TrimPrefix(headRefName, "refs/heads/")},
				Base:      PRRef{Ref: strings.TrimPrefix(baseRefName, "refs/heads/"), Sha: baseSha, Label: owner + ":" + strings.TrimPrefix(baseRefName, "refs/heads/")},
				Merged:    false,
				Mergeable: &mergeable,
				User:      viewer,
				CreatedAt: nextTimestamp(),
				UpdatedAt: nextTimestamp(),
				HTMLURL:   fmt.Sprintf("http://localhost/%s/%s/pull/%d", owner, repoName, prNumber),
			}
			if b, present := body["body"]; present {
				if s, isStr := b.(string); isStr {
					pr.Body = &s
				}
			}
			repo.Pulls[prNumber] = pr
			repo.PullOrder = append(repo.PullOrder, prNumber)
			writeJSON(w, 201, pr)
			return
		}
		writeJSONMsg(w, 404, "Not Found")
		return
	}

	prNum, _ := strconv.Atoi(parts[1])
	pr, ok := repo.Pulls[prNum]
	if !ok {
		writeJSONMsg(w, 404, "Pull request not found")
		return
	}
	refreshMergeable(repo, pr)

	if len(parts) == 2 && method == http.MethodGet {
		writeJSON(w, 200, pr)
		return
	}
	if len(parts) == 3 && parts[2] == "files" && method == http.MethodGet {
		baseCommitObj, hasBase := repo.Objects[pr.Base.Sha]
		headCommitObj, hasHead := repo.Objects[pr.Head.Sha]
		if !hasBase || !hasHead {
			writeJSONMsg(w, 404, "Commit objects not found")
			return
		}
		baseTreeSha := parseCommit(baseCommitObj.Raw).Tree
		headTreeSha := parseCommit(headCommitObj.Raw).Tree
		writeJSON(w, 200, computeTreeDiff(repo, baseTreeSha, headTreeSha))
		return
	}
	if len(parts) == 3 && parts[2] == "merge" && method == http.MethodPut {
		if pr.Merged {
			writeJSONMsg(w, 405, "Pull request already merged")
			return
		}
		body, err := parseJSONBody(r)
		if err != nil {
			writeJSONMsg(w, 400, "Malformed JSON")
			return
		}
		commitTitle, _ := body["commit_title"].(string)
		if commitTitle == "" {
			commitTitle = pr.Title
		}
		baseRefName := "refs/heads/" + pr.Base.Ref
		baseTipSha, hasBase := repo.Refs[baseRefName]
		headCommitObj, hasHead := repo.Objects[pr.Head.Sha]
		if !hasBase || !hasHead {
			writeJSONMsg(w, 405, "Cannot merge")
			return
		}
		if checkConflict(repo, pr.Head.Sha, baseTipSha) {
			f := false
			pr.Mergeable = &f
			writeJSONMsg(w, 405, "Pull request has merge conflicts")
			return
		}
		headTreeSha := parseCommit(headCommitObj.Raw).Tree
		commitRaw := serializeCommit(CommitObject{
			Tree:      headTreeSha,
			Parents:   []string{baseTipSha},
			Author:    fmt.Sprintf("%s <%s> %s +0000", viewer.Name, viewer.Email, nextTimestamp()),
			Committer: fmt.Sprintf("%s <%s> %s +0000", viewer.Name, viewer.Email, nextTimestamp()),
			Message:   commitTitle,
		})
		newCommitSha := hashObject("commit", commitRaw)
		repo.Objects[newCommitSha] = &GitObject{Type: "commit", Raw: commitRaw}
		repo.setRef(baseRefName, newCommitSha)
		pr.Merged = true
		pr.State = "closed"
		mergedAt := nextTimestamp()
		pr.MergedAt = &mergedAt
		mergedBy := viewer
		pr.MergedBy = &mergedBy
		pr.MergeCommitSha = &newCommitSha
		f := false
		pr.Mergeable = &f
		writeJSON(w, 200, map[string]any{
			"sha":              newCommitSha,
			"merged":           true,
			"message":          "Pull Request successfully merged",
			"merge_commit_sha": newCommitSha,
		})
		return
	}
}

func refreshMergeable(repo *Repository, pr *PullRequest) {
	baseSha := repo.Refs["refs/heads/"+pr.Base.Ref]
	if baseSha == "" {
		baseSha = pr.Base.Sha
	}
	mergeable := !checkConflict(repo, pr.Head.Sha, baseSha)
	pr.Mergeable = &mergeable
}

// base64Decode mirrors Buffer.from(s, 'base64'): whitespace-tolerant and
// forgiving of a missing or partial tail.
func base64Decode(s string) []byte {
	clean := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\n', '\r', '\t':
			return -1
		}
		return r
	}, s)
	if decoded, err := base64Std.DecodeString(clean); err == nil {
		return decoded
	}
	decoded, _ := base64Raw.DecodeString(clean)
	return decoded
}
