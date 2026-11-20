package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// zlib writers are expensive to create (flate tables); pool them for pack builds.
var zlibWriterPool = sync.Pool{
	New: func() any {
		zw, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
		return zw
	},
}

// ---------------------------------------------------------------------------
// Pkt-lines
// ---------------------------------------------------------------------------

var (
	flushPkt   = []byte("0000")
	delimPkt   = []byte("0001")
	respEndPkt = []byte("0002")
)

func encodePkt(data []byte) []byte {
	out := make([]byte, 0, len(data)+4)
	out = fmt.Appendf(out, "%04x", len(data)+4)
	out = append(out, data...)
	return out
}

func encodePktStr(s string) []byte { return encodePkt([]byte(s)) }

func encodeFlush() []byte { return []byte("0000") }
func encodeDelim() []byte { return []byte("0001") }

// parsePktLines mirrors the TS parser: special packets are returned as their
// 4-byte marker; other lines as payload slices of the input buffer.
func parsePktLines(buffer []byte) (lines [][]byte, remainder []byte) {
	offset := 0
	for offset+4 <= len(buffer) {
		lenHex := string(buffer[offset : offset+4])
		if lenHex == "0000" || lenHex == "0001" || lenHex == "0002" {
			lines = append(lines, buffer[offset:offset+4])
			offset += 4
			continue
		}
		var length int
		if _, err := fmt.Sscanf(lenHex, "%x", &length); err != nil || length == 0 {
			break
		}
		if offset+length > len(buffer) {
			break
		}
		lines = append(lines, buffer[offset+4:offset+length])
		offset += length
	}
	return lines, buffer[offset:]
}

// ---------------------------------------------------------------------------
// Object hashing
// ---------------------------------------------------------------------------

func hashObject(objType string, raw []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", objType, len(raw))
	h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// Trees
// ---------------------------------------------------------------------------

type TreeEntry struct {
	Mode string
	Name string
	Sha  string // 40-char hex
}

func parseTree(buf []byte) []TreeEntry {
	var entries []TreeEntry
	offset := 0
	for offset < len(buf) {
		spaceIdx := bytes.IndexByte(buf[offset:], ' ')
		if spaceIdx == -1 {
			break
		}
		spaceIdx += offset
		mode := string(buf[offset:spaceIdx])
		nullRel := bytes.IndexByte(buf[spaceIdx+1:], 0)
		if nullRel == -1 {
			break
		}
		nullIdx := spaceIdx + 1 + nullRel
		name := string(buf[spaceIdx+1 : nullIdx])
		if nullIdx+21 > len(buf) {
			break
		}
		sha := hex.EncodeToString(buf[nullIdx+1 : nullIdx+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, Sha: sha})
		offset = nullIdx + 21
	}
	return entries
}

func serializeTree(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		an, bn := sorted[i].Name, sorted[j].Name
		if sorted[i].Mode == "40000" {
			an += "/"
		}
		if sorted[j].Mode == "40000" {
			bn += "/"
		}
		return an < bn
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.Sha)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// Commits
// ---------------------------------------------------------------------------

type CommitObject struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func parseCommit(buf []byte) CommitObject {
	content := string(buf)
	headerText := content
	message := ""
	if idx := strings.Index(content, "\n\n"); idx != -1 {
		headerText = content[:idx]
		message = content[idx+2:]
	}
	c := CommitObject{Message: message}
	for _, line := range strings.Split(headerText, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			c.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			c.Parents = append(c.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			c.Author = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "committer "):
			c.Committer = strings.TrimSpace(line[10:])
		}
	}
	return c
}

func serializeCommit(c CommitObject) []byte {
	var sb strings.Builder
	fmt.Fprintf(&sb, "tree %s\n", c.Tree)
	for _, p := range c.Parents {
		fmt.Fprintf(&sb, "parent %s\n", p)
	}
	fmt.Fprintf(&sb, "author %s\n", c.Author)
	fmt.Fprintf(&sb, "committer %s\n\n", c.Committer)
	sb.WriteString(c.Message)
	return []byte(sb.String())
}

// ---------------------------------------------------------------------------
// Packfiles
// ---------------------------------------------------------------------------

type ParsedObject struct {
	Type string
	Raw  []byte
	Sha  string
}

var packTypeNames = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

// applyPackDelta applies a git pack delta instruction stream to a base object.
func applyPackDelta(base, delta []byte) []byte {
	pos := 0
	readVarint := func() int {
		value := 0
		shift := uint(0)
		for {
			b := delta[pos]
			pos++
			value |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value
	}
	_ = readVarint() // source size
	targetSize := readVarint()
	var out bytes.Buffer
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			offset, size := 0, 0
			if op&0x01 != 0 {
				offset |= int(delta[pos])
				pos++
			}
			if op&0x02 != 0 {
				offset |= int(delta[pos]) << 8
				pos++
			}
			if op&0x04 != 0 {
				offset |= int(delta[pos]) << 16
				pos++
			}
			if op&0x08 != 0 {
				offset |= int(delta[pos]) << 24
				pos++
			}
			if op&0x10 != 0 {
				size |= int(delta[pos])
				pos++
			}
			if op&0x20 != 0 {
				size |= int(delta[pos]) << 8
				pos++
			}
			if op&0x40 != 0 {
				size |= int(delta[pos]) << 16
				pos++
			}
			if size == 0 {
				size = 0x10000
			}
			out.Write(base[offset : offset+size])
		} else if op != 0 {
			out.Write(delta[pos : pos+int(op)])
			pos += int(op)
		} else {
			break
		}
	}
	result := out.Bytes()
	if len(result) == targetSize {
		return result
	}
	return result
}

func parsePackfile(packBuf []byte) []ParsedObject {
	if len(packBuf) < 12 {
		return nil
	}
	if string(packBuf[0:4]) != "PACK" {
		return nil
	}
	numObjects := int(packBuf[8])<<24 | int(packBuf[9])<<16 | int(packBuf[10])<<8 | int(packBuf[11])
	dataEnd := len(packBuf) - 20
	if dataEnd < 12 {
		return nil
	}

	type rawObj struct {
		typeNum int
		data    []byte
		start   int
		baseOfs int
		baseSha string
	}
	var rawObjs []rawObj
	offset := 12
	for i := 0; i < numObjects; i++ {
		if offset >= dataEnd {
			break
		}
		start := offset
		b := packBuf[offset]
		offset++
		typeNum := int(b>>4) & 7
		size := int(b & 15)
		shift := uint(4)
		for b&0x80 != 0 {
			b = packBuf[offset]
			offset++
			size += int(b&0x7f) << shift
			shift += 7
		}
		_ = size
		baseOfs := -1
		baseSha := ""
		if typeNum == 6 {
			if offset+20 > dataEnd {
				return nil
			}
			baseSha = hex.EncodeToString(packBuf[offset : offset+20])
			offset += 20
		} else if typeNum == 7 {
			eb := packBuf[offset]
			offset++
			back := int(eb & 0x7f)
			for eb&0x80 != 0 {
				eb = packBuf[offset]
				offset++
				back = ((back + 1) << 7) | int(eb&0x7f)
			}
			baseOfs = start - back
		}
		br := bytes.NewReader(packBuf[offset:dataEnd])
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil
		}
		raw, err := io.ReadAll(zr)
		_ = zr.Close()
		if err != nil {
			return nil
		}
		offset += (dataEnd - offset) - br.Len()
		rawObjs = append(rawObjs, rawObj{typeNum: typeNum, data: raw, start: start, baseOfs: baseOfs, baseSha: baseSha})
	}

	byOffset := map[int]int{}
	for idx, o := range rawObjs {
		byOffset[o.start] = idx
	}
	resolved := make([]*ParsedObject, len(rawObjs))
	bySha := map[string]*ParsedObject{}
	var resolve func(idx, depth int) *ParsedObject
	resolve = func(idx, depth int) *ParsedObject {
		if resolved[idx] != nil {
			return resolved[idx]
		}
		if depth > 64 {
			return nil
		}
		o := rawObjs[idx]
		var out *ParsedObject
		switch {
		case o.typeNum >= 1 && o.typeNum <= 4:
			t := packTypeNames[o.typeNum]
			out = &ParsedObject{Type: t, Raw: o.data, Sha: hashObject(t, o.data)}
		case o.typeNum == 7 && o.baseOfs >= 0:
			if baseIdx, ok := byOffset[o.baseOfs]; ok {
				if base := resolve(baseIdx, depth+1); base != nil {
					raw := applyPackDelta(base.Raw, o.data)
					out = &ParsedObject{Type: base.Type, Raw: raw, Sha: hashObject(base.Type, raw)}
				}
			}
		case o.typeNum == 6 && o.baseSha != "":
			base := bySha[o.baseSha]
			if base == nil {
				for j := range rawObjs {
					if cand := resolve(j, depth+1); cand != nil && cand.Sha == o.baseSha {
						base = cand
						break
					}
				}
			}
			if base != nil {
				raw := applyPackDelta(base.Raw, o.data)
				out = &ParsedObject{Type: base.Type, Raw: raw, Sha: hashObject(base.Type, raw)}
			}
		}
		resolved[idx] = out
		if out != nil {
			bySha[out.Sha] = out
		}
		return out
	}
	var objects []ParsedObject
	for i := range rawObjs {
		if o := resolve(i, 0); o != nil {
			objects = append(objects, *o)
		}
	}
	return objects
}

func buildPackfile(objects []ParsedObject) []byte {
	var buf bytes.Buffer
	header := make([]byte, 12)
	copy(header, "PACK")
	header[4], header[5], header[6], header[7] = 0, 0, 0, 2
	n := len(objects)
	header[8], header[9], header[10], header[11] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	buf.Write(header)
	for _, obj := range objects {
		typeNum := 3
		switch obj.Type {
		case "commit":
			typeNum = 1
		case "tree":
			typeNum = 2
		case "blob":
			typeNum = 3
		case "tag":
			typeNum = 4
		}
		size := len(obj.Raw)
		first := byte((typeNum&7)<<4) | byte(size&15)
		size >>= 4
		if size > 0 {
			first |= 0x80
		}
		buf.WriteByte(first)
		for size > 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size > 0 {
				b |= 0x80
			}
			buf.WriteByte(b)
		}
		zw := zlibWriterPool.Get().(*zlib.Writer)
		zw.Reset(&buf)
		_, _ = zw.Write(obj.Raw)
		_ = zw.Close()
		zlibWriterPool.Put(zw)
	}
	content := buf.Bytes()
	trailer := sha1.Sum(content)
	out := make([]byte, 0, len(content)+20)
	out = append(out, content...)
	out = append(out, trailer[:]...)
	return out
}
