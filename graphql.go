package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// GraphQL endpoint (ported from the TS forge's graphql.ts)
// ---------------------------------------------------------------------------

func encodeGlobalID(typeName string, parts ...any) string {
	strs := make([]string, 0, len(parts)+1)
	strs = append(strs, typeName)
	for _, p := range parts {
		strs = append(strs, fmt.Sprint(p))
	}
	return base64.StdEncoding.EncodeToString([]byte(strings.Join(strs, ":")))
}

func decodeGlobalID(idStr string) (string, []string, bool) {
	raw, err := base64.StdEncoding.DecodeString(idStr)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(idStr)
		if err != nil {
			return "", nil, false
		}
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) == 0 {
		return "", nil, false
	}
	return parts[0], parts[1:], true
}

func partNum(parts []string, i int) (int, bool) {
	if i >= len(parts) {
		return 0, false
	}
	n, err := strconv.Atoi(parts[i])
	if err != nil {
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------------------
// Lexer
// ---------------------------------------------------------------------------

func gqlIsNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func gqlIsNameChar(c byte) bool {
	return gqlIsNameStart(c) || (c >= '0' && c <= '9')
}

func gqlTokenize(query string) []string {
	var tokens []string
	i := 0
	for i < len(query) {
		c := query[i]
		switch {
		case c == '#':
			for i < len(query) && query[i] != '\n' {
				i++
			}
		case c == '"':
			j := i + 1
			for j < len(query) {
				if query[j] == '\\' {
					j += 2
					continue
				}
				if query[j] == '"' {
					j++
					break
				}
				j++
			}
			if j > len(query) {
				j = len(query)
			}
			tokens = append(tokens, query[i:j])
			i = j
		case c == '$' && i+1 < len(query) && gqlIsNameStart(query[i+1]):
			j := i + 1
			for j < len(query) && gqlIsNameChar(query[j]) {
				j++
			}
			tokens = append(tokens, query[i:j])
			i = j
		case gqlIsNameStart(c):
			j := i
			for j < len(query) && gqlIsNameChar(query[j]) {
				j++
			}
			tokens = append(tokens, query[i:j])
			i = j
		case c >= '0' && c <= '9':
			j := i
			for j < len(query) && query[j] >= '0' && query[j] <= '9' {
				j++
			}
			if j < len(query) && query[j] == '.' {
				k := j + 1
				for k < len(query) && query[k] >= '0' && query[k] <= '9' {
					k++
				}
				if k > j+1 {
					j = k
				}
			}
			tokens = append(tokens, query[i:j])
			i = j
		case c == '.' && i+2 < len(query) && query[i+1] == '.' && query[i+2] == '.':
			tokens = append(tokens, "...")
			i += 3
		case strings.ContainsRune("{}():,!=$[]", rune(c)):
			tokens = append(tokens, string(c))
			i++
		default:
			i++
		}
	}
	return tokens
}

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

type varRef struct{ name string }

type selNode struct {
	isFragment    bool
	typeCondition string
	hasCondition  bool
	alias         string
	name          string
	args          map[string]any
	sel           []selNode
}

type operation struct {
	kind string // "query" | "mutation"
	name string
	sel  []selNode
}

type gqlParser struct {
	tokens []string
	pos    int
}

func (p *gqlParser) peek() string {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return ""
}

func (p *gqlParser) consume(expected ...string) string {
	if p.pos >= len(p.tokens) {
		panic(fmt.Sprintf("Expected '%s', got 'undefined'", firstOrEmpty(expected)))
	}
	tok := p.tokens[p.pos]
	p.pos++
	if len(expected) > 0 && expected[0] != "" && tok != expected[0] {
		panic(fmt.Sprintf("Expected '%s', got '%s'", expected[0], tok))
	}
	return tok
}

func firstOrEmpty(s []string) string {
	if len(s) > 0 {
		return s[0]
	}
	return ""
}

func (p *gqlParser) parseDocument() []operation {
	var ops []operation
	for p.pos < len(p.tokens) {
		ops = append(ops, p.parseOperation())
	}
	return ops
}

func (p *gqlParser) parseOperation() operation {
	op := operation{kind: "query"}
	tok := p.peek()
	if tok == "query" || tok == "mutation" {
		op.kind = p.consume()
		if next := p.peek(); next != "" && gqlIsNameStart(next[0]) {
			op.name = p.consume()
		}
	}
	if p.peek() == "(" {
		p.consume("(")
		depth := 1
		for depth > 0 && p.pos < len(p.tokens) {
			t := p.consume()
			if t == "(" {
				depth++
			}
			if t == ")" {
				depth--
			}
		}
	}
	op.sel = p.parseSelectionSet()
	return op
}

func (p *gqlParser) parseSelectionSet() []selNode {
	p.consume("{")
	var selections []selNode
	for p.pos < len(p.tokens) && p.peek() != "}" {
		selections = append(selections, p.parseSelection())
	}
	p.consume("}")
	return selections
}

func (p *gqlParser) parseSelection() selNode {
	if p.peek() == "..." {
		p.consume("...")
		node := selNode{isFragment: true}
		if p.peek() == "on" {
			p.consume("on")
			node.typeCondition = p.consume()
			node.hasCondition = true
		}
		node.sel = p.parseSelectionSet()
		return node
	}
	name := p.consume()
	node := selNode{}
	if p.peek() == ":" {
		p.consume(":")
		node.alias = name
		name = p.consume()
	}
	node.name = name
	if p.peek() == "(" {
		node.args = p.parseArguments()
	}
	if p.peek() == "{" {
		node.sel = p.parseSelectionSet()
	}
	return node
}

func (p *gqlParser) parseArguments() map[string]any {
	p.consume("(")
	args := map[string]any{}
	for p.pos < len(p.tokens) && p.peek() != ")" {
		key := p.consume()
		p.consume(":")
		args[key] = p.parseValue()
		if p.peek() == "," {
			p.consume(",")
		}
	}
	p.consume(")")
	return args
}

func (p *gqlParser) parseValue() any {
	t := p.consume()
	if strings.HasPrefix(t, "\"") {
		var s string
		if err := json.Unmarshal([]byte(t), &s); err == nil {
			return s
		}
		return t
	}
	switch t {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	case "[":
		var arr []any
		for p.peek() != "]" {
			arr = append(arr, p.parseValue())
			if p.peek() == "," {
				p.consume(",")
			}
		}
		p.consume("]")
		return arr
	case "{":
		obj := map[string]any{}
		for p.peek() != "}" {
			k := p.consume()
			p.consume(":")
			obj[k] = p.parseValue()
			if p.peek() == "," {
				p.consume(",")
			}
		}
		p.consume("}")
		return obj
	}
	if strings.HasPrefix(t, "$") {
		return varRef{name: t[1:]}
	}
	if n, err := strconv.ParseFloat(t, 64); err == nil {
		return n
	}
	return t
}

// ---------------------------------------------------------------------------
// Variable resolution
// ---------------------------------------------------------------------------

var gqlUndefined = &struct{ undef bool }{true}

func resolveVariables(val any, variables map[string]any) any {
	switch v := val.(type) {
	case varRef:
		if resolved, ok := variables[v.name]; ok {
			return resolved
		}
		return gqlUndefined
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = resolveVariables(e, variables)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, e := range v {
			out[k] = resolveVariables(e, variables)
		}
		return out
	}
	return val
}

func resolveArgsDeep(selections []selNode, variables map[string]any) {
	for i := range selections {
		sel := &selections[i]
		if sel.isFragment {
			resolveArgsDeep(sel.sel, variables)
			continue
		}
		if sel.args != nil {
			sel.args = resolveVariables(sel.args, variables).(map[string]any)
		}
		if sel.sel != nil {
			resolveArgsDeep(sel.sel, variables)
		}
	}
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

func handleGraphQL(w http.ResponseWriter, r *http.Request) {
	storeMu.Lock()
	defer storeMu.Unlock()

	bodyBytes, _ := io.ReadAll(r.Body)
	var payload any
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		writeJSONMsg(w, 400, "Malformed JSON")
		return
	}
	isBatch := false
	var operations []any
	if arr, ok := payload.([]any); ok {
		isBatch = true
		operations = arr
	} else {
		operations = []any{payload}
	}

	results := make([]any, 0, len(operations))
	for _, opItem := range operations {
		results = append(results, executeGraphQLOperation(opItem))
	}
	if isBatch {
		writeJSON(w, 200, results)
	} else {
		writeJSON(w, 200, results[0])
	}
}

func executeGraphQLOperation(opItem any) (result any) {
	defer func() {
		if rec := recover(); rec != nil {
			result = map[string]any{
				"data":   nil,
				"errors": []map[string]string{{"message": fmt.Sprintf("%v", rec)}},
			}
		}
	}()
	item, _ := opItem.(map[string]any)
	queryStr, _ := item["query"].(string)
	variables, _ := item["variables"].(map[string]any)
	if variables == nil {
		variables = map[string]any{}
	}
	if queryStr == "" {
		return map[string]any{"errors": []map[string]string{{"message": "Missing query"}}}
	}
	tokens := gqlTokenize(queryStr)
	parser := &gqlParser{tokens: tokens}
	parsedOps := parser.parseDocument()
	if len(parsedOps) == 0 {
		panic("Execution error")
	}
	op := parsedOps[0]
	var errs []map[string]string
	data := executeOperation(op, variables, &errs)
	resObj := map[string]any{"data": data}
	if len(errs) > 0 {
		resObj["errors"] = errs
	}
	return resObj
}

func executeOperation(op operation, variables map[string]any, errs *[]map[string]string) map[string]any {
	resolveArgsDeep(op.sel, variables)
	data := map[string]any{}
	for _, sel := range op.sel {
		if sel.isFragment {
			continue
		}
		fieldName := sel.name
		key := sel.alias
		if key == "" {
			key = fieldName
		}
		args := map[string]any{}
		if sel.args != nil {
			if resolved, ok := resolveVariables(sel.args, variables).(map[string]any); ok {
				args = resolved
			}
		}
		if op.kind == "mutation" {
			data[key] = executeMutation(fieldName, args, sel.sel, errs)
		} else {
			data[key] = executeQuery(fieldName, args, sel.sel, errs)
		}
	}
	return data
}

func argString(args map[string]any, key string) string {
	if s, ok := args[key].(string); ok {
		return s
	}
	return ""
}

func findRepoByID(id int) *Repository {
	for _, name := range repoOrder {
		if repositories[name].ID == id {
			return repositories[name]
		}
	}
	return nil
}
