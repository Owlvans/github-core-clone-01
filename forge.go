package main

import (
	"regexp"
)

// ---------------------------------------------------------------------------
// Tree helpers (ported from the TS forge)
// ---------------------------------------------------------------------------

type treeHit struct {
	Sha  string
	Type string // "blob" | "tree"
	Mode string
}

func getTreeEntryByPath(repo *Repository, treeSha string, pathParts []string) *treeHit {
	obj, ok := repo.Objects[treeSha]
	if !ok || obj.Type != "tree" {
		return nil
	}
	entries := parseTree(obj.Raw)
	var entry *TreeEntry
	for i := range entries {
		if entries[i].Name == pathParts[0] {
			entry = &entries[i]
			break
		}
	}
	if entry == nil {
		return nil
	}
	if len(pathParts) == 1 {
		t := "blob"
		if entry.Mode == "40000" {
			t = "tree"
		}
		return &treeHit{Sha: entry.Sha, Type: t, Mode: entry.Mode}
	}
	if entry.Mode != "40000" {
		return nil
	}
	return getTreeEntryByPath(repo, entry.Sha, pathParts[1:])
}

type dirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Sha  string `json:"sha"`
	Type string `json:"type"`
	Size int    `json:"size"`
	Mode string `json:"mode"`
}

func getDirectoryEntries(repo *Repository, treeSha string) []dirEntry {
	result := []dirEntry{}
	obj, ok := repo.Objects[treeSha]
	if !ok || obj.Type != "tree" {
		return result
	}
	for _, e := range parseTree(obj.Raw) {
		isDir := e.Mode == "40000"
		size := 0
		if !isDir {
			if blobObj, ok := repo.Objects[e.Sha]; ok {
				size = len(blobObj.Raw)
			}
		}
		t := "file"
		if isDir {
			t = "dir"
		}
		result = append(result, dirEntry{Name: e.Name, Path: e.Name, Sha: e.Sha, Type: t, Size: size, Mode: e.Mode})
	}
	return result
}

func updateTreeAtPath(repo *Repository, currentTreeSha string, hasCurrent bool, pathParts []string, blobSha *string) string {
	var entries []TreeEntry
	if hasCurrent {
		if obj, ok := repo.Objects[currentTreeSha]; ok {
			entries = parseTree(obj.Raw)
		}
	}
	currentName := pathParts[0]
	isLeaf := len(pathParts) == 1
	if isLeaf {
		filtered := make([]TreeEntry, 0, len(entries)+1)
		for _, e := range entries {
			if e.Name != currentName {
				filtered = append(filtered, e)
			}
		}
		if blobSha != nil {
			filtered = append(filtered, TreeEntry{Mode: "100644", Name: currentName, Sha: *blobSha})
		}
		newRaw := serializeTree(filtered)
		newSha := hashObject("tree", newRaw)
		repo.Objects[newSha] = &GitObject{Type: "tree", Raw: newRaw}
		return newSha
	}
	var subTreeSha string
	hasSub := false
	for _, e := range entries {
		if e.Name == currentName && e.Mode == "40000" {
			subTreeSha = e.Sha
			hasSub = true
			break
		}
	}
	newSubTreeSha := updateTreeAtPath(repo, subTreeSha, hasSub, pathParts[1:], blobSha)
	filtered := make([]TreeEntry, 0, len(entries)+1)
	for _, e := range entries {
		if e.Name != currentName {
			filtered = append(filtered, e)
		}
	}
	filtered = append(filtered, TreeEntry{Mode: "40000", Name: currentName, Sha: newSubTreeSha})
	newRaw := serializeTree(filtered)
	newSha := hashObject("tree", newRaw)
	repo.Objects[newSha] = &GitObject{Type: "tree", Raw: newRaw}
	return newSha
}

func getTreeFiles(repo *Repository, treeSha string, prefix string, out map[string]string) {
	obj, ok := repo.Objects[treeSha]
	if !ok || obj.Type != "tree" {
		return
	}
	for _, e := range parseTree(obj.Raw) {
		fullPath := e.Name
		if prefix != "" {
			fullPath = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			getTreeFiles(repo, e.Sha, fullPath, out)
		} else {
			out[fullPath] = e.Sha
		}
	}
}

type fileDiff struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

func computeTreeDiff(repo *Repository, baseTreeSha, headTreeSha string) []fileDiff {
	baseFiles := map[string]string{}
	headFiles := map[string]string{}
	getTreeFiles(repo, baseTreeSha, "", baseFiles)
	getTreeFiles(repo, headTreeSha, "", headFiles)
	diffs := []fileDiff{}
	seen := map[string]bool{}
	var paths []string
	for p := range baseFiles {
		seen[p] = true
		paths = append(paths, p)
	}
	for p := range headFiles {
		if !seen[p] {
			paths = append(paths, p)
		}
	}
	for _, path := range paths {
		baseSha, hasBase := baseFiles[path]
		headSha, hasHead := headFiles[path]
		switch {
		case !hasBase && hasHead:
			size := 0
			if obj, ok := repo.Objects[headSha]; ok {
				size = len(obj.Raw)
			}
			diffs = append(diffs, fileDiff{Filename: path, Status: "added", Additions: size, Deletions: 0, Changes: size})
		case hasBase && !hasHead:
			size := 0
			if obj, ok := repo.Objects[baseSha]; ok {
				size = len(obj.Raw)
			}
			diffs = append(diffs, fileDiff{Filename: path, Status: "removed", Additions: 0, Deletions: size, Changes: size})
		case hasBase && hasHead && baseSha != headSha:
			diffs = append(diffs, fileDiff{Filename: path, Status: "modified", Additions: 1, Deletions: 1, Changes: 2})
		}
	}
	return diffs
}

// ---------------------------------------------------------------------------
// Commit graph helpers
// ---------------------------------------------------------------------------

func findMergeBase(repo *Repository, headTip, baseTip string) string {
	baseAncestors := map[string]bool{}
	queue := []string{baseTip}
	for len(queue) > 0 {
		sha := queue[0]
		queue = queue[1:]
		if baseAncestors[sha] {
			continue
		}
		baseAncestors[sha] = true
		if obj, ok := repo.Objects[sha]; ok && obj.Type == "commit" {
			queue = append(queue, parseCommit(obj.Raw).Parents...)
		}
	}
	queue = []string{headTip}
	visitedHead := map[string]bool{}
	for len(queue) > 0 {
		sha := queue[0]
		queue = queue[1:]
		if baseAncestors[sha] {
			return sha
		}
		if visitedHead[sha] {
			continue
		}
		visitedHead[sha] = true
		if obj, ok := repo.Objects[sha]; ok && obj.Type == "commit" {
			queue = append(queue, parseCommit(obj.Raw).Parents...)
		}
	}
	return ""
}

func commitTreeSha(repo *Repository, sha string) string {
	if obj, ok := repo.Objects[sha]; ok && obj.Type == "commit" {
		return parseCommit(obj.Raw).Tree
	}
	return ""
}

func checkConflict(repo *Repository, headTip, baseTip string) bool {
	if headTip == baseTip {
		return false
	}
	b := findMergeBase(repo, headTip, baseTip)
	headFiles := map[string]string{}
	baseFilesNow := map[string]string{}
	getTreeFiles(repo, commitTreeSha(repo, headTip), "", headFiles)
	getTreeFiles(repo, commitTreeSha(repo, baseTip), "", baseFilesNow)
	if b == "" {
		for p, h := range headFiles {
			if n, ok := baseFilesNow[p]; ok && n != h {
				return true
			}
		}
		return false
	}
	bFiles := map[string]string{}
	getTreeFiles(repo, commitTreeSha(repo, b), "", bFiles)
	paths := map[string]bool{}
	for p := range bFiles {
		paths[p] = true
	}
	for p := range headFiles {
		paths[p] = true
	}
	for p := range baseFilesNow {
		paths[p] = true
	}
	for p := range paths {
		bv, hasB := bFiles[p]
		hv, hasH := headFiles[p]
		nv, hasN := baseFilesNow[p]
		// TS compares possibly-undefined values with !==; presence matters.
		differsHead := hasH != hasB || hv != bv
		differsBase := hasN != hasB || nv != bv
		differEachOther := hasH != hasN || hv != nv
		if differsHead && differsBase && differEachOther {
			return true
		}
	}
	return false
}

func isDescendant(repo *Repository, commitA, commitB string) bool {
	if commitA == commitB {
		return true
	}
	visited := map[string]bool{}
	queue := []string{commitB}
	for len(queue) > 0 {
		sha := queue[0]
		queue = queue[1:]
		if sha == commitA {
			return true
		}
		if visited[sha] {
			continue
		}
		visited[sha] = true
		if obj, ok := repo.Objects[sha]; ok && obj.Type == "commit" {
			queue = append(queue, parseCommit(obj.Raw).Parents...)
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Fetch object selection (upload-pack)
// ---------------------------------------------------------------------------

var tagObjectRe = regexp.MustCompile(`(?m)^object ([0-9a-f]{40})`)

type fetchSelection struct {
	objectShas []string
	shallow    []string
	unshallow  []string
}

func selectFetchObjects(repo *Repository, wants, haves []string, depth *int, clientShallow []string) fetchSelection {
	shallowSet := map[string]bool{}
	for _, s := range clientShallow {
		shallowSet[s] = true
	}
	objectClosure := func(starts []string) map[string]bool {
		seen := map[string]bool{}
		queue := append([]string(nil), starts...)
		for len(queue) > 0 {
			sha := queue[0]
			queue = queue[1:]
			if seen[sha] {
				continue
			}
			obj, ok := repo.Objects[sha]
			if !ok {
				continue
			}
			seen[sha] = true
			switch obj.Type {
			case "commit":
				parsed := parseCommit(obj.Raw)
				queue = append(queue, parsed.Tree)
				if !shallowSet[sha] {
					queue = append(queue, parsed.Parents...)
				}
			case "tree":
				for _, e := range parseTree(obj.Raw) {
					queue = append(queue, e.Sha)
				}
			case "tag":
				if m := tagObjectRe.FindSubmatch(obj.Raw); m != nil {
					queue = append(queue, string(m[1]))
				}
			}
		}
		return seen
	}

	held := objectClosure(append(append([]string(nil), haves...), clientShallow...))

	objectShas := []string{}
	inObjectShas := map[string]bool{}
	addObject := func(sha string) {
		if !inObjectShas[sha] {
			inObjectShas[sha] = true
			objectShas = append(objectShas, sha)
		}
	}

	var startCommits []string
	for _, w := range wants {
		cur := w
		for hops := 0; hops < 8; hops++ {
			obj, ok := repo.Objects[cur]
			if !ok {
				break
			}
			if obj.Type == "tag" {
				if !held[cur] {
					addObject(cur)
				}
				m := tagObjectRe.FindSubmatch(obj.Raw)
				if m == nil {
					break
				}
				cur = string(m[1])
				continue
			}
			if obj.Type == "commit" {
				startCommits = append(startCommits, cur)
			}
			break
		}
	}

	depthOf := map[string]int{}
	var frontier []string
	for _, c := range startCommits {
		if _, ok := depthOf[c]; !ok {
			depthOf[c] = 1
			frontier = append(frontier, c)
		}
	}
	visited := map[string]bool{}
	var visitedOrder []string
	d := 1
	for len(frontier) > 0 {
		var next []string
		for _, c := range frontier {
			obj, ok := repo.Objects[c]
			if !ok || obj.Type != "commit" {
				continue
			}
			if !visited[c] {
				visited[c] = true
				visitedOrder = append(visitedOrder, c)
			}
			if depth != nil && d >= *depth {
				continue
			}
			for _, p := range parseCommit(obj.Raw).Parents {
				if _, has := depthOf[p]; !has {
					if _, exists := repo.Objects[p]; exists {
						depthOf[p] = d + 1
						next = append(next, p)
					}
				}
			}
		}
		frontier = next
		d++
	}

	for _, c := range visitedOrder {
		if held[c] {
			continue
		}
		addObject(c)
		commitObj, ok := repo.Objects[c]
		if !ok || commitObj.Type != "commit" {
			continue
		}
		treeQueue := []string{parseCommit(commitObj.Raw).Tree}
		treeSeen := map[string]bool{}
		for len(treeQueue) > 0 {
			t := treeQueue[0]
			treeQueue = treeQueue[1:]
			if treeSeen[t] || held[t] {
				continue
			}
			treeSeen[t] = true
			tobj, ok := repo.Objects[t]
			if !ok {
				continue
			}
			addObject(t)
			if tobj.Type == "tree" {
				for _, e := range parseTree(tobj.Raw) {
					treeQueue = append(treeQueue, e.Sha)
				}
			}
		}
	}

	possession := map[string]bool{}
	for sha := range visited {
		possession[sha] = true
	}
	for sha := range held {
		if obj, ok := repo.Objects[sha]; ok && obj.Type == "commit" {
			possession[sha] = true
		}
	}
	var boundary []string
	for _, c := range visitedOrder {
		obj, ok := repo.Objects[c]
		if !ok || obj.Type != "commit" {
			continue
		}
		for _, p := range parseCommit(obj.Raw).Parents {
			if _, exists := repo.Objects[p]; exists && !possession[p] {
				boundary = append(boundary, c)
				break
			}
		}
	}
	boundarySet := map[string]bool{}
	for _, b := range boundary {
		boundarySet[b] = true
	}
	var unshallow []string
	for _, s := range clientShallow {
		if visited[s] && !boundarySet[s] {
			unshallow = append(unshallow, s)
		}
	}
	return fetchSelection{objectShas: objectShas, shallow: boundary, unshallow: unshallow}
}
