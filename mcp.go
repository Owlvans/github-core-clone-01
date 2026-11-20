package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// MCP endpoint (ported from the TS forge's mcp.ts)
// ---------------------------------------------------------------------------

type toolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema struct {
		Type       string                       `json:"type"`
		Properties map[string]map[string]string `json:"properties"`
		Required   []string                     `json:"required,omitempty"`
	} `json:"inputSchema"`
}

func makeTool(name, desc string, props map[string]string, required []string) toolDef {
	var t toolDef
	t.Name = name
	t.Description = desc
	t.InputSchema.Type = "object"
	t.InputSchema.Properties = map[string]map[string]string{}
	for k, typ := range props {
		t.InputSchema.Properties[k] = map[string]string{"type": typ}
	}
	t.InputSchema.Required = required
	return t
}

var toolsList = []toolDef{
	makeTool("create_repo", "Create a new repository",
		map[string]string{"name": "string", "description": "string", "private": "boolean"},
		[]string{"name"}),
	makeTool("get_repo", "Get a repository by owner and repo name",
		map[string]string{"owner": "string", "repo": "string", "full_name": "string"},
		[]string{"owner", "repo"}),
	makeTool("create_issue", "Create an issue in a repository",
		map[string]string{"owner": "string", "repo": "string", "full_name": "string", "title": "string", "body": "string"},
		[]string{"owner", "repo", "title"}),
	makeTool("list_issues", "List issues in a repository",
		map[string]string{"owner": "string", "repo": "string", "full_name": "string", "state": "string"},
		[]string{"owner", "repo"}),
	makeTool("create_pull_request", "Create a pull request",
		map[string]string{"owner": "string", "repo": "string", "full_name": "string", "title": "string", "head": "string", "base": "string", "body": "string"},
		[]string{"owner", "repo", "title", "head", "base"}),
	makeTool("merge_pull_request", "Merge a pull request",
		map[string]string{"owner": "string", "repo": "string", "full_name": "string", "pull_number": "integer"},
		[]string{"owner", "repo", "pull_number"}),
}

func wallClockISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func handleMCP(w http.ResponseWriter, r *http.Request) {
	storeMu.Lock()
	defer storeMu.Unlock()

	bodyBytes, _ := io.ReadAll(r.Body)
	var rpc map[string]any
	if err := json.Unmarshal(bodyBytes, &rpc); err != nil {
		writeJSON(w, 400, map[string]any{
			"jsonrpc": "2.0",
			"id":      nil,
			"error":   rpcError{Code: -32700, Message: "Parse error"},
		})
		return
	}

	idRaw, hasID := rpc["id"]
	method, _ := rpc["method"].(string)
	params, _ := rpc["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	if !hasID && strings.HasPrefix(method, "notifications/") {
		w.WriteHeader(202)
		return
	}

	sendResult := func(result any, headers map[string]string) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": result})
	}
	sendError := func(code int, message string) {
		writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": idRaw, "error": rpcError{Code: code, Message: message}})
	}

	switch method {
	case "server/discover":
		sendResult(map[string]any{
			"serverInfo":        map[string]string{"name": "forge-mcp", "version": "1.0.0"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"supportedVersions": []string{"2026-07-28", "2025-11-25"},
			"protocolVersion":   "2026-07-28",
		}, nil)
		return
	case "initialize":
		protoVer := "2025-11-25"
		if pv, ok := params["protocolVersion"].(string); ok && strings.HasPrefix(pv, "2025-") {
			protoVer = pv
		}
		sendResult(map[string]any{
			"protocolVersion": protoVer,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "forge-mcp", "version": "1.0.0"},
		}, map[string]string{"Mcp-Session-Id": "forge-session-1"})
		return
	case "ping":
		sendResult(map[string]any{}, nil)
		return
	case "tools/list":
		sendResult(map[string]any{"tools": toolsList}, nil)
		return
	case "tools/call":
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		if fullName, ok := args["full_name"].(string); ok {
			parts := strings.Split(fullName, "/")
			if len(parts) == 2 {
				if _, has := args["owner"]; !has {
					args["owner"] = parts[0]
				}
				if _, has := args["repo"]; !has {
					args["repo"] = parts[1]
				}
			}
		}
		var tool *toolDef
		for i := range toolsList {
			if toolsList[i].Name == name {
				tool = &toolsList[i]
				break
			}
		}
		if tool == nil {
			sendError(-32602, fmt.Sprintf("Unknown tool: %s", name))
			return
		}
		for _, reqArg := range tool.InputSchema.Required {
			v, present := args[reqArg]
			if !present || v == nil || v == "" {
				sendResult(map[string]any{
					"content": []map[string]string{{"type": "text", "text": fmt.Sprintf("Error: missing required argument %s", reqArg)}},
					"isError": true,
				}, nil)
				return
			}
		}
		result, err := executeToolCall(name, args)
		if err != nil {
			sendResult(map[string]any{
				"content": []map[string]string{{"type": "text", "text": err.Error()}},
				"isError": true,
			}, nil)
			return
		}
		textBytes, _ := json.Marshal(result)
		sendResult(map[string]any{
			"content":           []map[string]string{{"type": "text", "text": string(textBytes)}},
			"structuredContent": result,
			"isError":           false,
		}, nil)
		return
	}

	sendError(-32601, fmt.Sprintf("Method not found: %s", method))
}

func argStr(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

func executeToolCall(name string, args map[string]any) (any, error) {
	switch name {
	case "create_repo":
		repoName := argStr(args, "name")
		if _, exists := repositories[repoName]; exists {
			return nil, fmt.Errorf("Repository already exists")
		}
		var description *string
		if d, present := args["description"]; present {
			if s, isStr := d.(string); isStr {
				description = &s
			}
		}
		privateRepo, _ := args["private"].(bool)
		repo := createRepositoryInternal(repoName, description, privateRepo)
		return formatRepo(repo), nil

	case "get_repo":
		owner, repoName := argStr(args, "owner"), argStr(args, "repo")
		repo, ok := repositories[repoName]
		if owner != viewer.Login || !ok {
			return nil, fmt.Errorf("Repository not found")
		}
		return formatRepo(repo), nil

	case "create_issue":
		owner, repoName := argStr(args, "owner"), argStr(args, "repo")
		repo, ok := repositories[repoName]
		if owner != viewer.Login || !ok {
			return nil, fmt.Errorf("Repository not found")
		}
		title := argStr(args, "title")
		var body *string
		if b, present := args["body"]; present {
			if s, isStr := b.(string); isStr {
				body = &s
			}
		}
		issueNumber := len(repo.Issues) + len(repo.Pulls) + 1
		issue := &Issue{
			ID:        issueNumber,
			NodeID:    "",
			Number:    issueNumber,
			Title:     title,
			Body:      body,
			State:     "open",
			Labels:    []Label{},
			User:      viewer,
			Comments:  0,
			CreatedAt: wallClockISO(),
			UpdatedAt: wallClockISO(),
			HTMLURL:   fmt.Sprintf("http://localhost/%s/%s/issues/%d", owner, repoName, issueNumber),
		}
		repo.Issues[issueNumber] = issue
		return formatIssue(issue), nil

	case "list_issues":
		owner, repoName := argStr(args, "owner"), argStr(args, "repo")
		repo, ok := repositories[repoName]
		if owner != viewer.Login || !ok {
			return nil, fmt.Errorf("Repository not found")
		}
		state := argStr(args, "state")
		if state == "" {
			state = "open"
		}
		issues := []any{}
		nums := sortedIssueNumbers(repo)
		for _, n := range nums {
			issue := repo.Issues[n]
			if state != "all" && issue.State != state {
				continue
			}
			issues = append(issues, formatIssue(issue))
		}
		return map[string]any{"issues": issues}, nil

	case "create_pull_request":
		owner, repoName := argStr(args, "owner"), argStr(args, "repo")
		repo, ok := repositories[repoName]
		if owner != viewer.Login || !ok {
			return nil, fmt.Errorf("Repository not found")
		}
		title := argStr(args, "title")
		head := argStr(args, "head")
		base := argStr(args, "base")
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
			return nil, fmt.Errorf("Head or base ref not found")
		}
		hasConflict := checkConflict(repo, headSha, baseSha)
		prNumber := len(repo.Issues) + len(repo.Pulls) + 1
		var body *string
		if b, present := args["body"]; present {
			if s, isStr := b.(string); isStr {
				body = &s
			}
		}
		mergeable := !hasConflict
		pr := &PullRequest{
			ID:        prNumber,
			NodeID:    "",
			Number:    prNumber,
			Title:     title,
			Body:      body,
			State:     "open",
			Head:      PRRef{Ref: strings.TrimPrefix(headRefName, "refs/heads/"), Sha: headSha, Label: owner + ":" + strings.TrimPrefix(headRefName, "refs/heads/")},
			Base:      PRRef{Ref: strings.TrimPrefix(baseRefName, "refs/heads/"), Sha: baseSha, Label: owner + ":" + strings.TrimPrefix(baseRefName, "refs/heads/")},
			Merged:    false,
			Mergeable: &mergeable,
			User:      viewer,
			CreatedAt: wallClockISO(),
			UpdatedAt: wallClockISO(),
			HTMLURL:   fmt.Sprintf("http://localhost/%s/%s/pull/%d", owner, repoName, prNumber),
		}
		repo.Pulls[prNumber] = pr
		repo.PullOrder = append(repo.PullOrder, prNumber)
		return formatPr(pr), nil

	case "merge_pull_request":
		owner, repoName := argStr(args, "owner"), argStr(args, "repo")
		repo, ok := repositories[repoName]
		if owner != viewer.Login || !ok {
			return nil, fmt.Errorf("Repository not found")
		}
		pullNumber := 0
		if n, isNum := args["pull_number"].(float64); isNum {
			pullNumber = int(n)
		}
		pr, ok := repo.Pulls[pullNumber]
		if !ok {
			return nil, fmt.Errorf("Pull request not found")
		}
		if pr.Merged {
			return nil, fmt.Errorf("Pull request already merged")
		}
		baseRefName := "refs/heads/" + pr.Base.Ref
		baseTipSha, hasBase := repo.Refs[baseRefName]
		headCommitObj, hasHead := repo.Objects[pr.Head.Sha]
		if !hasBase || !hasHead {
			return nil, fmt.Errorf("Cannot merge")
		}
		if checkConflict(repo, pr.Head.Sha, baseTipSha) {
			return nil, fmt.Errorf("Pull request has merge conflicts")
		}
		headTreeSha := parseCommit(headCommitObj.Raw).Tree
		commitRaw := serializeCommit(CommitObject{
			Tree:      headTreeSha,
			Parents:   []string{baseTipSha},
			Author:    fmt.Sprintf("%s <%s> %s +0000", viewer.Name, viewer.Email, wallClockISO()),
			Committer: fmt.Sprintf("%s <%s> %s +0000", viewer.Name, viewer.Email, wallClockISO()),
			Message:   pr.Title,
		})
		newCommitSha := hashObject("commit", commitRaw)
		repo.Objects[newCommitSha] = &GitObject{Type: "commit", Raw: commitRaw}
		repo.setRef(baseRefName, newCommitSha)
		pr.Merged = true
		pr.State = "closed"
		mergedAt := wallClockISO()
		pr.MergedAt = &mergedAt
		mergedBy := viewer
		pr.MergedBy = &mergedBy
		pr.MergeCommitSha = &newCommitSha
		f := false
		pr.Mergeable = &f
		return formatPr(pr), nil
	}
	return nil, fmt.Errorf("Unknown tool: %s", name)
}

func sortedIssueNumbers(repo *Repository) []int {
	nums := make([]int, 0, len(repo.Issues))
	for n := range repo.Issues {
		nums = append(nums, n)
	}
	for i := 0; i < len(nums); i++ {
		for j := i + 1; j < len(nums); j++ {
			if nums[j] < nums[i] {
				nums[i], nums[j] = nums[j], nums[i]
			}
		}
	}
	return nums
}

type briefOwner struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
}

type repoBrief struct {
	ID            int       `json:"id"`
	Name          string    `json:"name"`
	FullName      string    `json:"full_name"`
	Owner         briefOwner `json:"owner"`
	Private       bool      `json:"private"`
	DefaultBranch string    `json:"default_branch"`
	Description   *string   `json:"description"`
}

func formatRepo(repo *Repository) repoBrief {
	return repoBrief{
		ID:            repo.ID,
		Name:          repo.Name,
		FullName:      fmt.Sprintf("%s/%s", viewer.Login, repo.Name),
		Owner:         briefOwner{Login: viewer.Login, ID: viewer.ID},
		Private:       repo.Private,
		DefaultBranch: repo.DefaultBranch,
		Description:   repo.Description,
	}
}

type issueBrief struct {
	ID      int     `json:"id"`
	Number  int     `json:"number"`
	Title   string  `json:"title"`
	Body    *string `json:"body"`
	State   string  `json:"state"`
	Labels  []Label `json:"labels"`
	HTMLURL string  `json:"html_url"`
}

func formatIssue(issue *Issue) issueBrief {
	labels := issue.Labels
	if labels == nil {
		labels = []Label{}
	}
	return issueBrief{
		ID:      issue.ID,
		Number:  issue.Number,
		Title:   issue.Title,
		Body:    issue.Body,
		State:   issue.State,
		Labels:  labels,
		HTMLURL: issue.HTMLURL,
	}
}

type prBrief struct {
	ID      int     `json:"id"`
	Number  int     `json:"number"`
	Title   string  `json:"title"`
	Body    *string `json:"body"`
	State   string  `json:"state"`
	Head    PRRef   `json:"head"`
	Base    PRRef   `json:"base"`
	Merged  bool    `json:"merged"`
	HTMLURL string  `json:"html_url"`
}

func formatPr(pr *PullRequest) prBrief {
	return prBrief{
		ID:      pr.ID,
		Number:  pr.Number,
		Title:   pr.Title,
		Body:    pr.Body,
		State:   pr.State,
		Head:    pr.Head,
		Base:    pr.Base,
		Merged:  pr.Merged,
		HTMLURL: pr.HTMLURL,
	}
}
