package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Git smart HTTP (ported from the TS forge)
// ---------------------------------------------------------------------------

func sortedRefNames(repo *Repository) []string {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func cachedFetchPack(repo *Repository, wants, haves []string, depth *int, clientShallow []string, selection fetchSelection) []byte {
	depthKey := "nil"
	if depth != nil {
		depthKey = strconv.Itoa(*depth)
	}
	keyParts := []string{
		strconv.Itoa(len(repo.Objects)),
		depthKey,
		strings.Join(sortedCopy(wants), ","),
		strings.Join(sortedCopy(haves), ","),
		strings.Join(sortedCopy(clientShallow), ","),
	}
	key := strings.Join(keyParts, "|")
	repo.packMu.Lock()
	defer repo.packMu.Unlock()
	if pack, ok := repo.packCache[key]; ok {
		return pack
	}
	objects := make([]ParsedObject, 0, len(selection.objectShas))
	for _, sha := range selection.objectShas {
		if obj, ok := repo.Objects[sha]; ok {
			objects = append(objects, ParsedObject{Type: obj.Type, Raw: obj.Raw})
		}
	}
	pack := buildPackfile(objects)
	if len(repo.packCache) >= 32 {
		repo.packCache = map[string][]byte{}
	}
	repo.packCache[key] = pack
	return pack
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func handleGitSmartHTTP(w http.ResponseWriter, r *http.Request, repo *Repository, gitPath string) {
	serviceQuery := r.URL.Query().Get("service")
	protocolHeader := r.Header.Get("git-protocol")

	if gitPath == "/info/refs" {
		if serviceQuery == "git-upload-pack" {
			storeMu.RLock()
			defer storeMu.RUnlock()
			w.Header().Set("content-type", "application/x-git-upload-pack-advertisement")
			w.WriteHeader(200)
			var bufs [][]byte
			if strings.Contains(protocolHeader, "version=2") {
				bufs = append(bufs,
					encodePktStr("version 2\n"),
					encodePktStr("agent=forge/1.0\n"),
					encodePktStr("ls-refs=unborn\n"),
					encodePktStr("fetch=shallow wait-for-done\n"),
					encodePktStr("object-format=sha1\n"),
					encodeFlush(),
				)
			} else {
				bufs = append(bufs, encodePktStr("# service=git-upload-pack\n"), encodeFlush())
				headSha, ok := repo.Refs["HEAD"]
				if !ok {
					headSha, ok = repo.Refs["refs/heads/"+repo.DefaultBranch]
				}
				if !ok {
					headSha = "0000000000000000000000000000000000000000"
				}
				caps := "side-band-64k ofs-delta shallow multi_ack_detailed agent=forge/1.0"
				firstLine := fmt.Sprintf("%s HEAD\x00%s\n", headSha, caps)
				if len(repo.Refs) == 0 {
					firstLine = fmt.Sprintf("0000000000000000000000000000000000000000 HEAD\x00%s\n", caps)
				}
				bufs = append(bufs, encodePktStr(firstLine))
				for _, refname := range sortedRefNames(repo) {
					if refname == "HEAD" {
						continue
					}
					bufs = append(bufs, encodePktStr(fmt.Sprintf("%s %s\n", repo.Refs[refname], refname)))
				}
				bufs = append(bufs, encodeFlush())
			}
			_, _ = w.Write(bytes.Join(bufs, nil))
			return
		} else if serviceQuery == "git-receive-pack" {
			storeMu.RLock()
			defer storeMu.RUnlock()
			w.Header().Set("content-type", "application/x-git-receive-pack-advertisement")
			w.WriteHeader(200)
			var bufs [][]byte
			bufs = append(bufs, encodePktStr("# service=git-receive-pack\n"), encodeFlush())
			caps := "report-status side-band-64k delete-refs ofs-delta"
			names := sortedRefNames(repo)
			if len(names) == 0 {
				bufs = append(bufs, encodePktStr(fmt.Sprintf("0000000000000000000000000000000000000000 refs/heads/main\x00%s\n", caps)))
			} else {
				for i, refname := range names {
					if i == 0 {
						bufs = append(bufs, encodePktStr(fmt.Sprintf("%s %s\x00%s\n", repo.Refs[refname], refname, caps)))
					} else {
						bufs = append(bufs, encodePktStr(fmt.Sprintf("%s %s\n", repo.Refs[refname], refname)))
					}
				}
			}
			bufs = append(bufs, encodeFlush())
			_, _ = w.Write(bytes.Join(bufs, nil))
			return
		}
	}

	bodyBuf, _ := io.ReadAll(r.Body)

	if gitPath == "/git-upload-pack" {
		storeMu.RLock()
		defer storeMu.RUnlock()
		w.Header().Set("content-type", "application/x-git-upload-pack-result")
		w.WriteHeader(200)
		if strings.Contains(protocolHeader, "version=2") {
			handleUploadPackV2(w, repo, bodyBuf)
		} else {
			handleUploadPackV0(w, repo, bodyBuf)
		}
		return
	}

	if gitPath == "/git-receive-pack" {
		storeMu.Lock()
		defer storeMu.Unlock()
		w.Header().Set("content-type", "application/x-git-receive-pack-result")
		w.WriteHeader(200)
		handleReceivePack(w, repo, bodyBuf)
		return
	}

	writeJSONMsg(w, 404, "Not Found")
}

func handleUploadPackV2(w http.ResponseWriter, repo *Repository, bodyBuf []byte) {
	lines, _ := parsePktLines(bodyBuf)
	command := ""
	var args []string
	mode := "cmd"
	for _, line := range lines {
		text := strings.TrimSpace(string(line))
		if text == "0001" || bytes.Equal(line, delimPkt) {
			mode = "args"
			continue
		}
		if text == "0000" || bytes.Equal(line, flushPkt) {
			continue
		}
		if mode == "cmd" && strings.HasPrefix(text, "command=") {
			command = text[len("command="):]
		} else if mode == "args" {
			args = append(args, text)
		}
	}

	if command == "ls-refs" {
		wantPeel := containsStr(args, "peel")
		wantSymrefs := containsStr(args, "symrefs")
		wantUnborn := containsStr(args, "unborn")
		var prefixes []string
		for _, a := range args {
			if strings.HasPrefix(a, "ref-prefix ") {
				prefixes = append(prefixes, a[len("ref-prefix "):])
			}
		}
		matchesPrefix := func(name string) bool {
			if len(prefixes) == 0 {
				return true
			}
			for _, p := range prefixes {
				if strings.HasPrefix(name, p) {
					return true
				}
			}
			return false
		}
		var bufs [][]byte
		headSha, hasHead := repo.Refs["refs/heads/"+repo.DefaultBranch]
		if matchesPrefix("HEAD") {
			symref := ""
			if wantSymrefs {
				symref = " symref-target:refs/heads/" + repo.DefaultBranch
			}
			switch {
			case hasHead:
				bufs = append(bufs, encodePktStr(fmt.Sprintf("%s HEAD%s\n", headSha, symref)))
			case wantUnborn:
				bufs = append(bufs, encodePktStr(fmt.Sprintf("unborn HEAD%s\n", symref)))
			default:
				bufs = append(bufs, encodePktStr(fmt.Sprintf("0000000000000000000000000000000000000000 HEAD%s\n", symref)))
			}
		}
		for _, refname := range sortedRefNames(repo) {
			if refname == "HEAD" {
				continue
			}
			if !matchesPrefix(refname) {
				continue
			}
			sha := repo.Refs[refname]
			line := fmt.Sprintf("%s %s", sha, refname)
			if wantPeel {
				if direct, ok := repo.Objects[sha]; ok && direct.Type == "tag" {
					cur := sha
					for hops := 0; hops < 8; hops++ {
						obj, ok := repo.Objects[cur]
						if !ok || obj.Type != "tag" {
							break
						}
						m := tagObjectRe.FindSubmatch(obj.Raw)
						if m == nil {
							cur = ""
							break
						}
						cur = string(m[1])
					}
					if target, ok := repo.Objects[cur]; ok && target.Type != "tag" {
						line += " peeled:" + cur
					}
				}
			}
			bufs = append(bufs, encodePktStr(line+"\n"))
		}
		bufs = append(bufs, encodeFlush())
		_, _ = w.Write(bytes.Join(bufs, nil))
		return
	}

	if command == "fetch" {
		var wants, haves, clientShallow []string
		var depth *int
		requestDone := false
		for _, a := range args {
			switch {
			case strings.HasPrefix(a, "want "):
				wants = append(wants, strings.TrimSpace(a[len("want "):]))
			case strings.HasPrefix(a, "have "):
				haves = append(haves, strings.TrimSpace(a[len("have "):]))
			case strings.HasPrefix(a, "shallow "):
				clientShallow = append(clientShallow, strings.TrimSpace(a[len("shallow "):]))
			case strings.HasPrefix(a, "deepen "):
				if n, err := strconv.Atoi(strings.TrimSpace(a[len("deepen "):])); err == nil {
					depth = &n
				}
			case a == "done":
				requestDone = true
			}
		}
		selection := selectFetchObjects(repo, wants, haves, depth, clientShallow)
		packBuf := cachedFetchPack(repo, wants, haves, depth, clientShallow, selection)

		var bufs [][]byte
		bufs = append(bufs, encodePktStr("acknowledgments\n"))
		var common []string
		for _, h := range haves {
			if _, ok := repo.Objects[h]; ok {
				common = append(common, h)
			}
		}
		if len(common) == 0 {
			bufs = append(bufs, encodePktStr("NAK\n"))
		} else {
			for _, h := range common {
				bufs = append(bufs, encodePktStr(fmt.Sprintf("ACK %s\n", h)))
			}
			if requestDone {
				bufs = append(bufs, encodePktStr(fmt.Sprintf("ACK %s ready\n", common[len(common)-1])))
			}
		}
		bufs = append(bufs, encodeDelim())
		if len(selection.shallow) > 0 || len(selection.unshallow) > 0 {
			bufs = append(bufs, encodePktStr("shallow-info\n"))
			for _, s := range selection.shallow {
				bufs = append(bufs, encodePktStr(fmt.Sprintf("shallow %s\n", s)))
			}
			for _, u := range selection.unshallow {
				bufs = append(bufs, encodePktStr(fmt.Sprintf("unshallow %s\n", u)))
			}
			bufs = append(bufs, encodeDelim())
		}
		bufs = append(bufs, encodePktStr("packfile\n"))
		bufs = append(bufs, sidebandChunks(packBuf)...)
		bufs = append(bufs, encodeFlush())
		_, _ = w.Write(bytes.Join(bufs, nil))
		return
	}
}

func handleUploadPackV0(w http.ResponseWriter, repo *Repository, bodyBuf []byte) {
	lines, _ := parsePktLines(bodyBuf)
	var wants, haves, clientShallow []string
	var depth *int
	for _, line := range lines {
		text := strings.TrimSpace(string(line))
		switch {
		case strings.HasPrefix(text, "want "):
			rest := text[len("want "):]
			if idx := strings.IndexByte(rest, ' '); idx != -1 {
				rest = rest[:idx]
			}
			wants = append(wants, rest)
		case strings.HasPrefix(text, "have "):
			haves = append(haves, strings.TrimSpace(text[len("have "):]))
		case strings.HasPrefix(text, "shallow "):
			clientShallow = append(clientShallow, strings.TrimSpace(text[len("shallow "):]))
		case strings.HasPrefix(text, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(text[len("deepen "):])); err == nil {
				depth = &n
			}
		}
	}
	selection := selectFetchObjects(repo, wants, haves, depth, clientShallow)
	packBuf := cachedFetchPack(repo, wants, haves, depth, clientShallow, selection)

	var bufs [][]byte
	for _, s := range selection.shallow {
		bufs = append(bufs, encodePktStr(fmt.Sprintf("shallow %s\n", s)))
	}
	for _, u := range selection.unshallow {
		bufs = append(bufs, encodePktStr(fmt.Sprintf("unshallow %s\n", u)))
	}
	var common []string
	for _, h := range haves {
		if _, ok := repo.Objects[h]; ok {
			common = append(common, h)
		}
	}
	if len(common) == 0 {
		bufs = append(bufs, encodePktStr("NAK\n"))
	} else {
		for _, h := range common {
			bufs = append(bufs, encodePktStr(fmt.Sprintf("ACK %s\n", h)))
		}
	}
	bufs = append(bufs, sidebandChunks(packBuf)...)
	bufs = append(bufs, encodeFlush())
	_, _ = w.Write(bytes.Join(bufs, nil))
}

func sidebandChunks(packBuf []byte) [][]byte {
	var bufs [][]byte
	offset := 0
	for offset < len(packBuf) {
		end := offset + 65515
		if end > len(packBuf) {
			end = len(packBuf)
		}
		chunk := make([]byte, 0, end-offset+1)
		chunk = append(chunk, 1)
		chunk = append(chunk, packBuf[offset:end]...)
		bufs = append(bufs, encodePkt(chunk))
		offset = end
	}
	return bufs
}

func handleReceivePack(w http.ResponseWriter, repo *Repository, bodyBuf []byte) {
	lines, _ := parsePktLines(bodyBuf)
	type refCmd struct{ oldOid, newOid, refname string }
	var commands []refCmd
	hasSideband := false
	if len(lines) > 0 && !bytes.Equal(lines[0], flushPkt) {
		if strings.Contains(string(lines[0]), "side-band-64k") {
			hasSideband = true
		}
	}
	for _, line := range lines {
		if bytes.Equal(line, flushPkt) {
			break
		}
		text := strings.TrimSpace(string(line))
		parts := strings.Fields(text)
		if len(parts) >= 3 {
			refname := parts[2]
			if idx := strings.IndexByte(refname, 0); idx != -1 {
				refname = refname[:idx]
			}
			commands = append(commands, refCmd{oldOid: parts[0], newOid: parts[1], refname: refname})
		}
	}

	if packOffset := bytes.Index(bodyBuf, []byte("PACK")); packOffset != -1 {
		for _, obj := range parsePackfile(bodyBuf[packOffset:]) {
			repo.Objects[obj.Sha] = &GitObject{Type: obj.Type, Raw: obj.Raw}
		}
	}

	results := []string{"unpack ok\n"}
	zeroOid := "0000000000000000000000000000000000000000"
	for _, cmd := range commands {
		switch {
		case cmd.newOid == zeroOid:
			repo.deleteRef(cmd.refname)
			results = append(results, fmt.Sprintf("ok %s\n", cmd.refname))
		case cmd.oldOid == zeroOid:
			repo.setRef(cmd.refname, cmd.newOid)
			results = append(results, fmt.Sprintf("ok %s\n", cmd.refname))
		default:
			current, exists := repo.Refs[cmd.refname]
			if exists && current != cmd.oldOid {
				results = append(results, fmt.Sprintf("ng %s non-fast-forward\n", cmd.refname))
			} else if isDescendant(repo, cmd.oldOid, cmd.newOid) {
				repo.setRef(cmd.refname, cmd.newOid)
				results = append(results, fmt.Sprintf("ok %s\n", cmd.refname))
			} else {
				results = append(results, fmt.Sprintf("ng %s non-fast-forward\n", cmd.refname))
			}
		}
	}

	if hasSideband {
		var reportBufs [][]byte
		for _, r := range results {
			reportBufs = append(reportBufs, encodePktStr(r))
		}
		reportBufs = append(reportBufs, encodeFlush())
		reportStream := bytes.Join(reportBufs, nil)
		var bufs [][]byte
		bufs = append(bufs, sidebandChunks(reportStream)...)
		bufs = append(bufs, encodeFlush())
		_, _ = w.Write(bytes.Join(bufs, nil))
	} else {
		var bufs [][]byte
		for _, r := range results {
			bufs = append(bufs, encodePktStr(r))
		}
		bufs = append(bufs, encodeFlush())
		_, _ = w.Write(bytes.Join(bufs, nil))
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
