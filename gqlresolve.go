package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// GraphQL field resolution (ported from the TS forge)
// ---------------------------------------------------------------------------

type userView struct {
	login, id, name, email string
}

type issueView struct {
	issue        *Issue
	repoID       int
	commentsList []*Comment
}

type prView struct {
	pr     *PullRequest
	repoID int
}

type commentView struct {
	id          string
	body        string
	authorLogin string
	createdAt   string
}

type labelView struct{ name string }

func repoIDOrDefault(id int) int {
	if id == 0 {
		return 5001
	}
	return id
}

func resolveSelections(obj any, selections []selNode, typeName string, errs *[]map[string]string) any {
	if obj == nil {
		return nil
	}
	result := map[string]any{}
	for _, sel := range selections {
		if sel.isFragment {
			if sel.hasCondition && sel.typeCondition != typeName {
				continue
			}
			if sub, ok := resolveSelections(obj, sel.sel, typeName, errs).(map[string]any); ok {
				for k, v := range sub {
					result[k] = v
				}
			}
			continue
		}
		fieldName := sel.name
		key := sel.alias
		if key == "" {
			key = fieldName
		}
		args := sel.args
		if args == nil {
			args = map[string]any{}
		}
		val, found := getFieldValue(obj, fieldName, args, typeName, errs, sel.sel)
		if found {
			result[key] = val
		} else {
			result[key] = nil
			*errs = append(*errs, map[string]string{"message": fmt.Sprintf("Field \"%s\" not found on type \"%s\"", fieldName, typeName)})
		}
	}
	return result
}

func getFieldValue(obj any, fieldName string, args map[string]any, typeName string, errs *[]map[string]string, subSel []selNode) (any, bool) {
	switch typeName {
	case "User":
		if uv, ok := obj.(userView); ok {
			switch fieldName {
			case "login":
				return uv.login, true
			case "id":
				return uv.id, true
			case "name":
				return uv.name, true
			case "email":
				return uv.email, true
			}
		}
	case "Repository":
		if repo, ok := obj.(*Repository); ok {
			return repoFieldValue(repo, fieldName, args, errs, subSel)
		}
	case "Issue":
		if iv, ok := obj.(issueView); ok {
			return issueFieldValue(iv, fieldName, errs, subSel)
		}
	case "PullRequest":
		if pv, ok := obj.(prView); ok {
			return prFieldValue(pv, fieldName, errs, subSel)
		}
	case "Comment":
		if cv, ok := obj.(commentView); ok {
			switch fieldName {
			case "id":
				return cv.id, true
			case "body":
				return cv.body, true
			case "createdAt":
				return cv.createdAt, true
			case "author":
				return map[string]any{"login": cv.authorLogin}, true
			}
		}
	case "Label":
		if lv, ok := obj.(labelView); ok && fieldName == "name" {
			return lv.name, true
		}
	}
	if m, ok := obj.(map[string]any); ok {
		if v, present := m[fieldName]; present {
			return v, true
		}
	}
	return nil, false
}

func repoFieldValue(repo *Repository, fieldName string, args map[string]any, errs *[]map[string]string, subSel []selNode) (any, bool) {
	switch fieldName {
	case "id":
		return encodeGlobalID("Repository", repo.ID), true
	case "databaseId":
		return repo.ID, true
	case "name":
		return repo.Name, true
	case "nameWithOwner":
		return viewer.Login + "/" + repo.Name, true
	case "owner":
		return map[string]any{"login": viewer.Login, "id": encodeGlobalID("User", 1000)}, true
	case "description":
		return repo.Description, true
	case "isPrivate":
		return repo.Private, true
	case "viewerPermission":
		return "ADMIN", true
	case "hasIssuesEnabled":
		return true, true
	case "createdAt":
		return repo.CreatedAt, true
	case "updatedAt":
		return repo.UpdatedAt, true
	case "defaultBranchRef":
		defaultBranch := repo.DefaultBranch
		if defaultBranch == "" {
			defaultBranch = "main"
		}
		oid := repo.Refs["refs/heads/"+defaultBranch]
		if oid == "" {
			oid = repo.Refs["HEAD"]
		}
		if oid == "" {
			return nil, true
		}
		return map[string]any{"name": defaultBranch, "target": map[string]any{"oid": oid}}, true
	case "issue":
		num, ok := argNumber(args, "number")
		if !ok {
			return nil, true
		}
		issue, exists := repo.Issues[num]
		if !exists {
			return nil, true
		}
		return resolveSelections(issueView{issue: issue, repoID: repo.ID, commentsList: commentsOrEmpty(repo, num)}, subSel, "Issue", errs), true
	case "issues":
		return resolveIssuesConnection(repo, args, subSel, errs), true
	case "pullRequest":
		num, ok := argNumber(args, "number")
		if !ok {
			return nil, true
		}
		pr, exists := repo.Pulls[num]
		if !exists {
			return nil, true
		}
		return resolveSelections(prView{pr: pr, repoID: repo.ID}, subSel, "PullRequest", errs), true
	case "pullRequests":
		return resolvePullRequestsConnection(repo, args, subSel, errs), true
	}
	return nil, false
}

func commentsOrEmpty(repo *Repository, num int) []*Comment {
	if list, ok := repo.Comments[num]; ok && list != nil {
		return list
	}
	return []*Comment{}
}

func argNumber(args map[string]any, key string) (int, bool) {
	if f, ok := args[key].(float64); ok {
		return int(f), true
	}
	return 0, false
}

func issueFieldValue(iv issueView, fieldName string, errs *[]map[string]string, subSel []selNode) (any, bool) {
	issue := iv.issue
	switch fieldName {
	case "id":
		return encodeGlobalID("Issue", repoIDOrDefault(iv.repoID), issue.Number), true
	case "databaseId":
		return repoIDOrDefault(iv.repoID)*100000 + issue.Number, true
	case "number":
		return issue.Number, true
	case "title":
		return issue.Title, true
	case "body":
		return issue.Body, true
	case "state":
		if issue.State == "" {
			return "OPEN", true
		}
		return strings.ToUpper(issue.State), true
	case "url":
		return issue.HTMLURL, true
	case "createdAt":
		return issue.CreatedAt, true
	case "updatedAt":
		return issue.UpdatedAt, true
	case "author":
		login := viewer.Login
		if issue.User.Login != "" {
			login = issue.User.Login
		}
		return map[string]any{"login": login}, true
	case "labels":
		nodeSel := findFieldSelections(subSel, "nodes")
		nodes := []any{}
		for _, l := range issue.Labels {
			nodes = append(nodes, resolveSelections(labelView{name: l.Name}, nodeSel, "Label", errs))
		}
		return map[string]any{"nodes": nodes}, true
	case "comments":
		nodeSel := findFieldSelections(subSel, "nodes")
		nodes := []any{}
		for _, c := range iv.commentsList {
			login := viewer.Login
			if c.User.Login != "" {
				login = c.User.Login
			}
			cv := commentView{
				id:          encodeGlobalID("Comment", repoIDOrDefault(iv.repoID), issue.Number, c.ID),
				body:        c.Body,
				authorLogin: login,
				createdAt:   c.CreatedAt,
			}
			nodes = append(nodes, resolveSelections(cv, nodeSel, "Comment", errs))
		}
		return map[string]any{"nodes": nodes}, true
	}
	return nil, false
}

func prFieldValue(pv prView, fieldName string, errs *[]map[string]string, subSel []selNode) (any, bool) {
	pr := pv.pr
	switch fieldName {
	case "id":
		return encodeGlobalID("PullRequest", repoIDOrDefault(pv.repoID), pr.Number), true
	case "databaseId":
		return repoIDOrDefault(pv.repoID)*100000 + pr.Number, true
	case "number":
		return pr.Number, true
	case "title":
		return pr.Title, true
	case "body":
		return pr.Body, true
	case "state":
		if pr.Merged {
			return "MERGED", true
		}
		if pr.State == "" {
			return "OPEN", true
		}
		return strings.ToUpper(pr.State), true
	case "merged":
		return pr.Merged, true
	case "mergeable":
		if pr.Mergeable != nil && !*pr.Mergeable {
			return "CONFLICTING", true
		}
		return "MERGEABLE", true
	case "headRefName":
		return pr.Head.Ref, true
	case "baseRefName":
		return pr.Base.Ref, true
	case "headRefOid":
		return pr.Head.Sha, true
	case "baseRefOid":
		return pr.Base.Sha, true
	case "createdAt":
		return pr.CreatedAt, true
	case "updatedAt":
		return pr.UpdatedAt, true
	case "author":
		login := viewer.Login
		if pr.User.Login != "" {
			login = pr.User.Login
		}
		return map[string]any{"login": login}, true
	case "mergedAt":
		return pr.MergedAt, true
	case "files":
		nodeSel := findFieldSelections(subSel, "nodes")
		repo := findRepoByID(repoIDOrDefault(pv.repoID))
		if repo == nil {
			return map[string]any{"nodes": []any{}}, true
		}
		headObj, hasHead := repo.Objects[pr.Head.Sha]
		baseObj, hasBase := repo.Objects[pr.Base.Sha]
		if !hasHead || !hasBase {
			return map[string]any{"nodes": []any{}}, true
		}
		headTree := parseCommit(headObj.Raw).Tree
		baseTree := parseCommit(baseObj.Raw).Tree
		diffs := computeTreeDiff(repo, baseTree, headTree)
		nodes := []any{}
		for _, d := range diffs {
			nodes = append(nodes, resolveSelections(map[string]any{"path": d.Filename}, nodeSel, "PullRequestChangedFile", errs))
		}
		return map[string]any{"nodes": nodes}, true
	}
	return nil, false
}

func findFieldSelections(selections []selNode, name string) []selNode {
	for _, sel := range selections {
		if !sel.isFragment && sel.name == name {
			return sel.sel
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Connections
// ---------------------------------------------------------------------------

type gqlConnection struct {
	nodes      []any
	nodeType   string
	pageInfo   map[string]any
	totalCount int
	rawNodes   []any
}

func projectConnection(conn gqlConnection, subSel []selNode, errs *[]map[string]string) any {
	out := map[string]any{}
	for _, sel := range subSel {
		if sel.isFragment {
			continue
		}
		key := sel.alias
		if key == "" {
			key = sel.name
		}
		switch sel.name {
		case "nodes":
			nodes := make([]any, 0, len(conn.nodes))
			for _, n := range conn.nodes {
				nodes = append(nodes, resolveSelections(n, sel.sel, conn.nodeType, errs))
			}
			out[key] = nodes
		case "pageInfo":
			out[key] = resolveSelections(conn.pageInfo, sel.sel, "PageInfo", errs)
		case "totalCount":
			out[key] = conn.totalCount
		}
	}
	if len(out) == 0 {
		return map[string]any{"nodes": conn.rawNodes, "pageInfo": conn.pageInfo, "totalCount": conn.totalCount}
	}
	return out
}

func resolveIssuesConnection(repo *Repository, args map[string]any, subSel []selNode, errs *[]map[string]string) any {
	nums := sortedIssueNumbers(repo)
	var views []issueView
	for _, n := range nums {
		views = append(views, issueView{issue: repo.Issues[n], repoID: repo.ID, commentsList: commentsOrEmpty(repo, n)})
	}
	if statesRaw, ok := args["states"].([]any); ok {
		states := map[string]bool{}
		for _, s := range statesRaw {
			if str, isStr := s.(string); isStr {
				states[str] = true
			}
		}
		filtered := views[:0]
		for _, v := range views {
			if states[strings.ToUpper(v.issue.State)] {
				filtered = append(filtered, v)
			}
		}
		views = filtered
	}
	direction := -1
	if orderBy, ok := args["orderBy"].(map[string]any); ok && orderBy["direction"] == "ASC" {
		direction = 1
	}
	sort.SliceStable(views, func(i, j int) bool {
		if direction == 1 {
			return views[i].issue.Number < views[j].issue.Number
		}
		return views[i].issue.Number > views[j].issue.Number
	})
	if after, ok := args["after"].(string); ok && after != "" {
		if typeName, parts, decoded := decodeGlobalID(after); decoded && typeName == "cursor" {
			if afterNum, ok2 := partNum(parts, 0); ok2 {
				filtered := views[:0]
				for _, v := range views {
					if direction == -1 && v.issue.Number < afterNum {
						filtered = append(filtered, v)
					} else if direction == 1 && v.issue.Number > afterNum {
						filtered = append(filtered, v)
					}
				}
				views = filtered
			}
		}
	}
	first := len(views)
	if f, ok := args["first"].(float64); ok && int(f) != 0 {
		first = int(f)
	}
	sliced := views
	if first < len(views) {
		sliced = views[:first]
	}
	hasNextPage := len(views) > first
	var endCursor any
	if len(sliced) > 0 {
		endCursor = encodeGlobalID("cursor", sliced[len(sliced)-1].issue.Number)
	}
	nodes := make([]any, 0, len(sliced))
	rawNodes := make([]any, 0, len(sliced))
	for _, v := range sliced {
		nodes = append(nodes, v)
		rawNodes = append(rawNodes, rawIssueMap(v))
	}
	return projectConnection(gqlConnection{
		nodes:      nodes,
		nodeType:   "Issue",
		pageInfo:   map[string]any{"hasNextPage": hasNextPage, "endCursor": endCursor},
		totalCount: len(repo.Issues),
		rawNodes:   rawNodes,
	}, subSel, errs)
}

func resolvePullRequestsConnection(repo *Repository, args map[string]any, subSel []selNode, errs *[]map[string]string) any {
	var views []prView
	for _, n := range repo.PullOrder {
		views = append(views, prView{pr: repo.Pulls[n], repoID: repo.ID})
	}
	if statesRaw, ok := args["states"].([]any); ok {
		states := map[string]bool{}
		for _, s := range statesRaw {
			if str, isStr := s.(string); isStr {
				states[str] = true
			}
		}
		filtered := views[:0]
		for _, v := range views {
			state := strings.ToUpper(v.pr.State)
			if v.pr.Merged {
				state = "MERGED"
			}
			if states[state] {
				filtered = append(filtered, v)
			}
		}
		views = filtered
	}
	first := len(views)
	if f, ok := args["first"].(float64); ok && int(f) != 0 {
		first = int(f)
	}
	sliced := views
	if first < len(views) {
		sliced = views[:first]
	}
	nodes := make([]any, 0, len(sliced))
	rawNodes := make([]any, 0, len(sliced))
	for _, v := range sliced {
		nodes = append(nodes, v)
		rawNodes = append(rawNodes, rawPRMap(v))
	}
	return projectConnection(gqlConnection{
		nodes:      nodes,
		nodeType:   "PullRequest",
		pageInfo:   map[string]any{"hasNextPage": len(views) > first, "endCursor": nil},
		totalCount: len(repo.Pulls),
		rawNodes:   rawNodes,
	}, subSel, errs)
}

func rawIssueMap(v issueView) map[string]any {
	m := map[string]any{
		"id": v.issue.ID, "node_id": v.issue.NodeID, "number": v.issue.Number,
		"title": v.issue.Title, "body": v.issue.Body, "state": v.issue.State,
		"labels": v.issue.Labels, "user": v.issue.User, "comments": v.issue.Comments,
		"created_at": v.issue.CreatedAt, "updated_at": v.issue.UpdatedAt, "html_url": v.issue.HTMLURL,
		"repoId": v.repoID, "commentsList": v.commentsList,
	}
	return m
}

func rawPRMap(v prView) map[string]any {
	return map[string]any{
		"id": v.pr.ID, "node_id": v.pr.NodeID, "number": v.pr.Number,
		"title": v.pr.Title, "body": v.pr.Body, "state": v.pr.State,
		"head": v.pr.Head, "base": v.pr.Base, "merged": v.pr.Merged,
		"mergeable": v.pr.Mergeable, "user": v.pr.User,
		"created_at": v.pr.CreatedAt, "updated_at": v.pr.UpdatedAt, "merged_at": v.pr.MergedAt,
		"html_url": v.pr.HTMLURL, "merged_by": v.pr.MergedBy, "merge_commit_sha": v.pr.MergeCommitSha,
		"repoId": v.repoID,
	}
}

// ---------------------------------------------------------------------------
// Queries
// ---------------------------------------------------------------------------

func executeQuery(fieldName string, args map[string]any, selections []selNode, errs *[]map[string]string) any {
	switch fieldName {
	case "viewer":
		uv := userView{login: viewer.Login, id: encodeGlobalID("User", 1000), name: viewer.Name, email: viewer.Email}
		return resolveSelections(uv, selections, "User", errs)
	case "repository":
		owner := argString(args, "owner")
		name := argString(args, "name")
		repo, ok := repositories[name]
		if owner != viewer.Login || !ok {
			return nil
		}
		return resolveSelections(repo, selections, "Repository", errs)
	case "node":
		globalID := argString(args, "id")
		typeName, parts, ok := decodeGlobalID(globalID)
		if !ok {
			return nil
		}
		switch typeName {
		case "Repository":
			if id, ok2 := partNum(parts, 0); ok2 {
				if repo := findRepoByID(id); repo != nil {
					return resolveSelections(repo, selections, "Repository", errs)
				}
			}
		case "Issue":
			repoID, ok1 := partNum(parts, 0)
			issueNum, ok2 := partNum(parts, 1)
			if ok1 && ok2 {
				if repo := findRepoByID(repoID); repo != nil {
					if issue, exists := repo.Issues[issueNum]; exists {
						return resolveSelections(issueView{issue: issue, repoID: repo.ID, commentsList: commentsOrEmpty(repo, issueNum)}, selections, "Issue", errs)
					}
				}
			}
		case "PullRequest":
			repoID, ok1 := partNum(parts, 0)
			prNum, ok2 := partNum(parts, 1)
			if ok1 && ok2 {
				if repo := findRepoByID(repoID); repo != nil {
					if pr, exists := repo.Pulls[prNum]; exists {
						return resolveSelections(prView{pr: pr, repoID: repo.ID}, selections, "PullRequest", errs)
					}
				}
			}
		}
		return map[string]any{"id": globalID}
	case "__type":
		return resolveIntrospectionType(argString(args, "name"))
	}
	*errs = append(*errs, map[string]string{"message": fmt.Sprintf("Field \"%s\" not found", fieldName)})
	return nil
}

func resolveIntrospectionType(typeName string) any {
	if typeName == "SearchType" {
		return map[string]any{
			"name": "SearchType",
			"kind": "ENUM",
			"enumValues": []map[string]string{
				{"name": "ISSUE"}, {"name": "REPOSITORY"}, {"name": "USER"},
			},
		}
	}
	var fieldNames []string
	switch typeName {
	case "Issue":
		fieldNames = []string{"id", "databaseId", "number", "title", "body", "state", "url", "createdAt", "updatedAt", "author", "labels", "comments"}
	case "PullRequest":
		fieldNames = []string{"id", "databaseId", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "files", "createdAt", "updatedAt", "author"}
	case "Repository":
		fieldNames = []string{"id", "databaseId", "name", "nameWithOwner", "owner", "description", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequest", "pullRequests"}
	}
	fields := make([]map[string]string, 0, len(fieldNames))
	for _, n := range fieldNames {
		fields = append(fields, map[string]string{"name": n})
	}
	return map[string]any{"name": typeName, "kind": "OBJECT", "fields": fields}
}

// ---------------------------------------------------------------------------
// Mutations
// ---------------------------------------------------------------------------

func mutationInput(args map[string]any) map[string]any {
	if input, ok := args["input"].(map[string]any); ok {
		return input
	}
	return map[string]any{}
}

func repoFromGlobalID(input map[string]any, key string) *Repository {
	idStr, _ := input[key].(string)
	if idStr == "" {
		return nil
	}
	_, parts, ok := decodeGlobalID(idStr)
	if !ok {
		return nil
	}
	id, ok := partNum(parts, 0)
	if !ok {
		return nil
	}
	return findRepoByID(id)
}

func executeMutation(fieldName string, args map[string]any, selections []selNode, errs *[]map[string]string) any {
	input := mutationInput(args)
	switch fieldName {
	case "createIssue":
		repo := repoFromGlobalID(input, "repositoryId")
		if repo == nil {
			*errs = append(*errs, map[string]string{"message": "Repository not found"})
			return nil
		}
		title := argString(input, "title")
		var body *string
		if b, present := input["body"]; present {
			if s, isStr := b.(string); isStr && s != "" {
				body = &s
			}
		}
		formattedLabels := []Label{}
		if labelIDs, ok := input["labelIds"].([]any); ok {
			for _, l := range labelIDs {
				if lName, isStr := l.(string); isStr {
					if existing, exists := repo.Labels[lName]; exists {
						formattedLabels = append(formattedLabels, *existing)
					}
				}
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
			Labels:    formattedLabels,
			User:      viewer,
			Comments:  0,
			CreatedAt: wallClockISO(),
			UpdatedAt: wallClockISO(),
			HTMLURL:   fmt.Sprintf("http://localhost/%s/%s/issues/%d", viewer.Login, repo.Name, issueNumber),
		}
		repo.Issues[issueNumber] = issue
		issueObj := resolveSelections(issueView{issue: issue, repoID: repo.ID, commentsList: []*Comment{}}, findFieldSelections(selections, "issue"), "Issue", errs)
		return map[string]any{"issue": issueObj}

	case "closeIssue":
		idStr, _ := input["issueId"].(string)
		typeName, parts, ok := decodeGlobalID(idStr)
		if !ok || typeName != "Issue" {
			*errs = append(*errs, map[string]string{"message": "Invalid issue id"})
			return nil
		}
		repoID, _ := partNum(parts, 0)
		issueNum, _ := partNum(parts, 1)
		repo := findRepoByID(repoID)
		if repo == nil {
			*errs = append(*errs, map[string]string{"message": "Repository not found"})
			return nil
		}
		issue, exists := repo.Issues[issueNum]
		if !exists {
			*errs = append(*errs, map[string]string{"message": "Issue not found"})
			return nil
		}
		issue.State = "closed"
		issue.UpdatedAt = wallClockISO()
		issueObj := resolveSelections(issueView{issue: issue, repoID: repo.ID, commentsList: commentsOrEmpty(repo, issueNum)}, findFieldSelections(selections, "issue"), "Issue", errs)
		return map[string]any{"issue": issueObj}

	case "createPullRequest":
		repo := repoFromGlobalID(input, "repositoryId")
		if repo == nil {
			*errs = append(*errs, map[string]string{"message": "Repository not found"})
			return nil
		}
		title := argString(input, "title")
		var body *string
		if b, present := input["body"]; present {
			if s, isStr := b.(string); isStr {
				body = &s
			}
		}
		headRaw, hasHeadRaw := input["headRefName"]
		if !hasHeadRaw || headRaw == nil {
			headRaw = input["head"]
		}
		baseRaw, hasBaseRaw := input["baseRefName"]
		if !hasBaseRaw || baseRaw == nil {
			baseRaw = input["base"]
		}
		headStr, _ := headRaw.(string)
		baseStr, _ := baseRaw.(string)
		headRefName := headStr
		if !strings.HasPrefix(headRefName, "refs/heads/") {
			headRefName = "refs/heads/" + headRefName
		}
		baseRefName := baseStr
		if !strings.HasPrefix(baseRefName, "refs/heads/") {
			baseRefName = "refs/heads/" + baseRefName
		}
		headSha, hasHead := repo.Refs[headRefName]
		baseSha, hasBase := repo.Refs[baseRefName]
		hasConflict := true
		if hasHead && hasBase {
			hasConflict = checkConflict(repo, headSha, baseSha)
		}
		prNumber := len(repo.Issues) + len(repo.Pulls) + 1
		mergeable := !hasConflict
		pr := &PullRequest{
			ID:        prNumber,
			NodeID:    "",
			Number:    prNumber,
			Title:     title,
			Body:      body,
			State:     "open",
			Head:      PRRef{Ref: strings.TrimPrefix(headRefName, "refs/heads/"), Sha: headSha, Label: viewer.Login + ":" + strings.TrimPrefix(headRefName, "refs/heads/")},
			Base:      PRRef{Ref: strings.TrimPrefix(baseRefName, "refs/heads/"), Sha: baseSha, Label: viewer.Login + ":" + strings.TrimPrefix(baseRefName, "refs/heads/")},
			Merged:    false,
			Mergeable: &mergeable,
			User:      viewer,
			CreatedAt: wallClockISO(),
			UpdatedAt: wallClockISO(),
			HTMLURL:   fmt.Sprintf("http://localhost/%s/%s/pull/%d", viewer.Login, repo.Name, prNumber),
		}
		repo.Pulls[prNumber] = pr
		repo.PullOrder = append(repo.PullOrder, prNumber)
		prObj := resolveSelections(prView{pr: pr, repoID: repo.ID}, findFieldSelections(selections, "pullRequest"), "PullRequest", errs)
		return map[string]any{"pullRequest": prObj}

	case "mergePullRequest":
		idStr, _ := input["pullRequestId"].(string)
		typeName, parts, ok := decodeGlobalID(idStr)
		if !ok || typeName != "PullRequest" {
			*errs = append(*errs, map[string]string{"message": "Invalid pull request id"})
			return nil
		}
		repoID, _ := partNum(parts, 0)
		prNum, _ := partNum(parts, 1)
		repo := findRepoByID(repoID)
		if repo == nil {
			*errs = append(*errs, map[string]string{"message": "Repository not found"})
			return nil
		}
		pr, exists := repo.Pulls[prNum]
		if !exists {
			*errs = append(*errs, map[string]string{"message": "Pull request not found"})
			return nil
		}
		baseRefName := "refs/heads/" + pr.Base.Ref
		baseTipSha, hasBase := repo.Refs[baseRefName]
		headCommitObj, hasHead := repo.Objects[pr.Head.Sha]
		if hasBase && hasHead {
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
		}
		prObj := resolveSelections(prView{pr: pr, repoID: repo.ID}, findFieldSelections(selections, "pullRequest"), "PullRequest", errs)
		return map[string]any{"pullRequest": prObj}

	case "addComment":
		idStr, _ := input["subjectId"].(string)
		typeName, parts, ok := decodeGlobalID(idStr)
		if !ok || typeName != "Issue" {
			*errs = append(*errs, map[string]string{"message": "Invalid subject id"})
			return nil
		}
		repoID, _ := partNum(parts, 0)
		issueNum, _ := partNum(parts, 1)
		repo := findRepoByID(repoID)
		if repo == nil {
			*errs = append(*errs, map[string]string{"message": "Repository not found"})
			return nil
		}
		issue, exists := repo.Issues[issueNum]
		if !exists {
			*errs = append(*errs, map[string]string{"message": "Issue not found"})
			return nil
		}
		commentID := time.Now().UnixMilli()
		commentBody := argString(input, "body")
		comment := &Comment{
			ID:        int(commentID),
			Body:      commentBody,
			User:      viewer,
			CreatedAt: wallClockISO(),
		}
		repo.Comments[issueNum] = append(repo.Comments[issueNum], comment)
		issue.Comments = len(repo.Comments[issueNum])
		edgeSel := findFieldSelections(selections, "commentEdge")
		nodeSel := findFieldSelections(edgeSel, "node")
		cv := commentView{
			id:          encodeGlobalID("Comment", repo.ID, issueNum, commentID),
			body:        commentBody,
			authorLogin: viewer.Login,
			createdAt:   comment.CreatedAt,
		}
		return map[string]any{
			"commentEdge": map[string]any{
				"node": resolveSelections(cv, nodeSel, "Comment", errs),
			},
		}
	}

	*errs = append(*errs, map[string]string{"message": fmt.Sprintf("Mutation \"%s\" not found", fieldName)})
	return nil
}
