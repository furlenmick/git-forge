// GraphQL: a small parser + executor for the documents the suite sends.
// A faithful port of main.ts.
package main

import (
	"encoding/base64"
	"strconv"
	"strings"
)

type gqlValueKind int

const (
	gqlString gqlValueKind = iota
	gqlInt
	gqlFloat
	gqlBool
	gqlNull
	gqlEnum
	gqlVariable
	gqlList
	gqlObject
)

type gqlValue struct {
	kind   gqlValueKind
	str    string
	num    float64
	isInt  bool
	b      bool
	name   string
	items  []*gqlValue
	fields []gqlField
}

type gqlField struct {
	name  string
	value *gqlValue
}

type gqlSelection struct {
	kind          string // "field" | "inlineFragment"
	name          string
	alias         string
	args          []gqlField
	typeCondition string
	selections    []*gqlSelection
}

type gqlOperation struct {
	operation  string // "query" | "mutation"
	selections []*gqlSelection
}

type gqlParser struct {
	src string
	pos int
}

func (p *gqlParser) skipWs() {
	for {
		for p.pos < len(p.src) {
			c := p.src[p.pos]
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' || c == ',' {
				p.pos++
			} else {
				break
			}
		}
		if p.pos < len(p.src) && p.src[p.pos] == '#' {
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
			continue
		}
		break
	}
}

func (p *gqlParser) peek() byte {
	p.skipWs()
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

func isNameStart(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_'
}

func isNameChar(c byte) bool {
	return isNameStart(c) || c >= '0' && c <= '9'
}

func (p *gqlParser) readName() (string, error) {
	p.skipWs()
	start := p.pos
	if p.pos < len(p.src) && isNameStart(p.src[p.pos]) {
		p.pos++
		for p.pos < len(p.src) && isNameChar(p.src[p.pos]) {
			p.pos++
		}
		return p.src[start:p.pos], nil
	}
	return "", errGql("expected a name at " + strconv.Itoa(p.pos))
}

type gqlError struct{ msg string }

func (e *gqlError) Error() string { return e.msg }
func errGql(msg string) error    { return &gqlError{msg} }

func (p *gqlParser) expect(ch byte) error {
	p.skipWs()
	if p.pos >= len(p.src) || p.src[p.pos] != ch {
		return errGql("expected " + string(ch) + " at " + strconv.Itoa(p.pos))
	}
	p.pos++
	return nil
}

func (p *gqlParser) readString() (string, error) {
	p.skipWs()
	if p.pos >= len(p.src) || p.src[p.pos] != '"' {
		return "", errGql("expected a string at " + strconv.Itoa(p.pos))
	}
	p.pos++
	var sb strings.Builder
	for p.pos < len(p.src) {
		ch := p.src[p.pos]
		if ch == '"' {
			p.pos++
			return sb.String(), nil
		}
		if ch == '\\' {
			p.pos++
			if p.pos >= len(p.src) {
				return "", errGql("unterminated string")
			}
			esc := p.src[p.pos]
			switch esc {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case 'u':
				if p.pos+4 >= len(p.src) {
					return "", errGql("unterminated string")
				}
				v, err := strconv.ParseUint(p.src[p.pos+1:p.pos+5], 16, 32)
				if err != nil {
					return "", errGql("bad unicode escape")
				}
				sb.WriteRune(rune(v))
				p.pos += 4
			default:
				sb.WriteByte(esc)
			}
			p.pos++
			continue
		}
		sb.WriteByte(ch)
		p.pos++
	}
	return "", errGql("unterminated string")
}

func (p *gqlParser) readValue() (*gqlValue, error) {
	p.skipWs()
	if p.pos >= len(p.src) {
		return nil, errGql("unexpected end of input")
	}
	ch := p.src[p.pos]
	switch {
	case ch == '"':
		s, err := p.readString()
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: gqlString, str: s}, nil
	case ch == '$':
		p.pos++
		name, err := p.readName()
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: gqlVariable, name: name}, nil
	case ch == '[':
		p.pos++
		var items []*gqlValue
		for p.peek() != ']' {
			if p.peek() == 0 {
				return nil, errGql("unterminated list")
			}
			v, err := p.readValue()
			if err != nil {
				return nil, err
			}
			items = append(items, v)
		}
		if err := p.expect(']'); err != nil {
			return nil, err
		}
		return &gqlValue{kind: gqlList, items: items}, nil
	case ch == '{':
		p.pos++
		var fields []gqlField
		for p.peek() != '}' {
			if p.peek() == 0 {
				return nil, errGql("unterminated object")
			}
			name, err := p.readName()
			if err != nil {
				return nil, err
			}
			if err := p.expect(':'); err != nil {
				return nil, err
			}
			v, err := p.readValue()
			if err != nil {
				return nil, err
			}
			fields = append(fields, gqlField{name: name, value: v})
		}
		if err := p.expect('}'); err != nil {
			return nil, err
		}
		return &gqlValue{kind: gqlObject, fields: fields}, nil
	case ch == '-' || ch >= '0' && ch <= '9':
		start := p.pos
		for p.pos < len(p.src) {
			c := p.src[p.pos]
			if c >= '0' && c <= '9' || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' {
				p.pos++
			} else {
				break
			}
		}
		lit := p.src[start:p.pos]
		// Validate with the TS regex /^-?\d+(\.\d+)?([eE][+-]?\d+)?/ by parsing.
		f, err := strconv.ParseFloat(lit, 64)
		if err != nil {
			return nil, errGql("bad number at " + strconv.Itoa(start))
		}
		isInt := !strings.ContainsAny(lit, ".eE")
		return &gqlValue{kind: map[bool]gqlValueKind{true: gqlInt, false: gqlFloat}[isInt], num: f, isInt: isInt}, nil
	default:
		name, err := p.readName()
		if err != nil {
			return nil, err
		}
		switch name {
		case "true":
			return &gqlValue{kind: gqlBool, b: true}, nil
		case "false":
			return &gqlValue{kind: gqlBool, b: false}, nil
		case "null":
			return &gqlValue{kind: gqlNull}, nil
		default:
			return &gqlValue{kind: gqlEnum, str: name}, nil
		}
	}
}

func (p *gqlParser) readArguments() ([]gqlField, error) {
	var args []gqlField
	p.skipWs()
	if p.pos >= len(p.src) || p.src[p.pos] != '(' {
		return args, nil
	}
	p.pos++
	for p.peek() != ')' {
		if p.peek() == 0 {
			return nil, errGql("unterminated arguments")
		}
		name, err := p.readName()
		if err != nil {
			return nil, err
		}
		if err := p.expect(':'); err != nil {
			return nil, err
		}
		v, err := p.readValue()
		if err != nil {
			return nil, err
		}
		args = append(args, gqlField{name: name, value: v})
	}
	if err := p.expect(')'); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *gqlParser) readSelectionSet() ([]*gqlSelection, error) {
	if err := p.expect('{'); err != nil {
		return nil, err
	}
	var selections []*gqlSelection
	for p.peek() != '}' {
		if p.peek() == 0 {
			return nil, errGql("unterminated selection set")
		}
		if p.peek() == '.' && p.pos+3 <= len(p.src) && p.src[p.pos:p.pos+3] == "..." {
			p.pos += 3
			p.skipWs()
			if p.pos+2 <= len(p.src) && p.src[p.pos:p.pos+2] == "on" {
				p.pos += 2
				tc, err := p.readName()
				if err != nil {
					return nil, err
				}
				sub, err := p.readSelectionSet()
				if err != nil {
					return nil, err
				}
				selections = append(selections, &gqlSelection{kind: "inlineFragment", typeCondition: tc, selections: sub})
			} else {
				if _, err := p.readName(); err != nil {
					return nil, err
				}
				if p.peek() == '{' {
					if _, err := p.readSelectionSet(); err != nil {
						return nil, err
					}
				}
			}
			continue
		}
		first, err := p.readName()
		if err != nil {
			return nil, err
		}
		alias := ""
		name := first
		p.skipWs()
		if p.pos < len(p.src) && p.src[p.pos] == ':' {
			p.pos++
			alias = first
			name, err = p.readName()
			if err != nil {
				return nil, err
			}
		}
		args, err := p.readArguments()
		if err != nil {
			return nil, err
		}
		var sub []*gqlSelection
		if p.peek() == '{' {
			sub, err = p.readSelectionSet()
			if err != nil {
				return nil, err
			}
		}
		selections = append(selections, &gqlSelection{kind: "field", name: name, alias: alias, args: args, selections: sub})
	}
	if err := p.expect('}'); err != nil {
		return nil, err
	}
	return selections, nil
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	p.skipWs()
	operation := "query"
	if p.pos+5 <= len(p.src) && p.src[p.pos:p.pos+5] == "query" && p.pos+5 < len(p.src)+1 {
		next := byte(0)
		if p.pos+5 < len(p.src) {
			next = p.src[p.pos+5]
		}
		if next == ' ' || next == '\t' || next == '\n' || next == '\r' || next == '(' || next == '{' {
			p.pos += 5
			operation = "query"
		}
	} else if p.pos+8 <= len(p.src) && p.src[p.pos:p.pos+8] == "mutation" {
		next := byte(0)
		if p.pos+8 < len(p.src) {
			next = p.src[p.pos+8]
		}
		if next == ' ' || next == '\t' || next == '\n' || next == '\r' || next == '(' || next == '{' {
			p.pos += 8
			operation = "mutation"
		}
	}
	p.skipWs()
	if p.pos < len(p.src) && isNameStart(p.src[p.pos]) {
		word, err := p.readName()
		if err != nil {
			return nil, err
		}
		if word == "fragment" {
			return nil, errGql("fragment definitions are not supported")
		}
	}
	p.skipWs()
	if p.pos < len(p.src) && p.src[p.pos] == '(' {
		depth := 0
		for p.pos < len(p.src) {
			ch := p.src[p.pos]
			if ch == '(' {
				depth++
			} else if ch == ')' {
				depth--
				p.pos++
				if depth == 0 {
					break
				}
				continue
			} else if ch == '"' {
				if _, err := p.readString(); err != nil {
					return nil, err
				}
				continue
			}
			p.pos++
		}
	}
	sels, err := p.readSelectionSet()
	if err != nil {
		return nil, err
	}
	return &gqlOperation{operation: operation, selections: sels}, nil
}

func gqlValueToJs(v *gqlValue, variables *JVal) *JVal {
	switch v.kind {
	case gqlString, gqlEnum:
		return JStrV(v.str)
	case gqlInt:
		if v.isInt {
			return JInt(int64(v.num))
		}
		return JFloat(v.num)
	case gqlFloat:
		return JFloat(v.num)
	case gqlBool:
		return JBoolV(v.b)
	case gqlNull:
		return JNullV()
	case gqlVariable:
		if variables != nil {
			if val := variables.ObjGet(v.name); val != nil {
				return val
			}
		}
		return JUndefV()
	case gqlList:
		items := make([]*JVal, len(v.items))
		for i, item := range v.items {
			items[i] = gqlValueToJs(item, variables)
		}
		return JArrV(items...)
	case gqlObject:
		obj := JObjV()
		for _, f := range v.fields {
			obj.ObjSet(f.name, gqlValueToJs(f.value, variables))
		}
		return obj
	}
	return JNullV()
}

// --- Executor ---

type gqlObj struct {
	typename    string
	repo        *repo
	issue       *issue
	pull        *pull
	userLogin   string
	refName     string
	typeName    string
	fieldNames  []string
	fieldName   string
	enumName    string
	hasNextPage bool
	endCursor   string
	hasEndCursor bool
	nodes       []*gqlObj
	comment     *comment
	pageInfo    *gqlObj
	fileFields  *JVal
}

var gqlFields = map[string][]string{
	"Query":                   {"viewer", "repository", "node", "__type", "__schema"},
	"Viewer":                  {"login", "id", "name", "email"},
	"Repository":              {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "private", "url"},
	"User":                    {"login", "id", "name", "email"},
	"Ref":                     {"name", "target"},
	"Issue":                   {"id", "number", "title", "state", "body", "url", "createdAt"},
	"IssueConnection":         {"nodes", "pageInfo", "totalCount"},
	"PageInfo":                {"hasNextPage", "endCursor", "hasPreviousPage", "startCursor"},
	"PullRequest":             {"id", "number", "title", "body", "state", "headRefName", "baseRefName", "headRef", "baseRef", "mergeable", "merged", "files", "url", "mergedAt", "mergeCommitSha"},
	"PullRequestConnection":   {"nodes", "pageInfo", "totalCount"},
	"PullRequestFileConnection": {"nodes", "pageInfo"},
	"PullRequestFile":         {"path", "additions", "deletions", "changes"},
	"Comment":                 {"id", "body"},
	"CommentEdge":             {"node"},
	"CreateIssuePayload":      {"issue"},
	"CloseIssuePayload":       {"issue"},
	"CreatePullRequestPayload": {"pullRequest"},
	"MergePullRequestPayload": {"pullRequest"},
	"AddCommentPayload":       {"commentEdge"},
	"Mutation":                {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"__Type":                  {"name", "fields", "enumValues", "kind"},
	"__Field":                 {"name"},
	"__EnumValue":             {"name"},
	"__Schema":                {"types", "queryType", "mutationType"},
	"SearchType":              {},
}

var gqlTypeOrder = []string{
	"Query", "Viewer", "Repository", "User", "Ref", "Issue", "IssueConnection", "PageInfo",
	"PullRequest", "PullRequestConnection", "PullRequestFileConnection", "PullRequestFile",
	"Comment", "CommentEdge", "CreateIssuePayload", "CloseIssuePayload", "CreatePullRequestPayload",
	"MergePullRequestPayload", "AddCommentPayload", "Mutation", "__Type", "__Field", "__EnumValue",
	"__Schema", "SearchType",
}

type gqlExecutor struct {
	errors    []string
	variables *JVal
}

func (e *gqlExecutor) execute(op *gqlOperation) (*JVal, []string) {
	e.errors = nil
	typename := "Query"
	if op.operation == "mutation" {
		typename = "Mutation"
	}
	root := &gqlObj{typename: typename}
	data := e.resolveSelections(root, op.selections)
	if len(e.errors) > 0 {
		return nil, e.errors
	}
	return data, nil
}

func (e *gqlExecutor) argsOf(sel *gqlSelection) *JVal {
	out := JObjV()
	for _, arg := range sel.args {
		out.ObjSet(arg.name, gqlValueToJs(arg.value, e.variables))
	}
	return out
}

func (e *gqlExecutor) resolveSelections(obj *gqlObj, selections []*gqlSelection) *JVal {
	out := JObjV()
	for _, sel := range selections {
		if sel.kind == "inlineFragment" {
			if sel.typeCondition != "" && sel.typeCondition != obj.typename {
				continue
			}
			sub := e.resolveSelections(obj, sel.selections)
			for _, f := range sub.Obj {
				out.ObjSet(f.Key, f.Val)
			}
			continue
		}
		key := sel.alias
		if key == "" {
			key = sel.name
		}
		out.ObjSet(key, e.resolveField(obj, sel))
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (e *gqlExecutor) resolveField(obj *gqlObj, sel *gqlSelection) *JVal {
	typeName := obj.typename
	field := sel.name
	if !containsStr(gqlFields[typeName], field) {
		e.errors = append(e.errors, "Cannot query field \""+field+"\" on type \""+typeName+"\".")
		return JNullV()
	}
	args := e.argsOf(sel)
	value := e.fieldValue(obj, field, args)
	if value == nil {
		return JNullV()
	}
	switch v := value.(type) {
	case *gqlObj:
		if sel.selections != nil {
			return e.resolveSelections(v, sel.selections)
		}
		return JNullV()
	case []*gqlObj:
		items := make([]*JVal, len(v))
		for i, item := range v {
			if sel.selections != nil {
				items[i] = e.resolveSelections(item, sel.selections)
			} else {
				items[i] = JNullV()
			}
		}
		return JArrV(items...)
	case *JVal:
		return v
	}
	return JNullV()
}

func (e *gqlExecutor) fieldValue(obj *gqlObj, field string, args *JVal) any {
	repo := obj.repo
	argStr := func(name string) string {
		v := args.ObjGet(name)
		if v == nil || v.Kind == JNull || v.Kind == JUndef {
			return ""
		}
		return jsStrOf(v)
	}
	switch obj.typename {
	case "Query":
		switch field {
		case "viewer":
			return &gqlObj{typename: "Viewer", userLogin: viewerLogin}
		case "repository":
			owner := argStr("owner")
			name := argStr("name")
			if owner != viewerLogin {
				return nil
			}
			found := getRepo(owner, name)
			if found == nil {
				return nil
			}
			return &gqlObj{typename: "Repository", repo: found}
		case "node":
			parsed, ok := parseGlobalID(argStr("id"))
			if !ok {
				return nil
			}
			found := repoByID(parsed.repoID)
			if found == nil {
				return nil
			}
			switch parsed.kind {
			case "Repository":
				return &gqlObj{typename: "Repository", repo: found}
			case "Issue":
				if parsed.hasNum {
					if iss := found.issues[parsed.number]; iss != nil {
						return &gqlObj{typename: "Issue", repo: found, issue: iss}
					}
				}
				return nil
			case "PullRequest":
				if parsed.hasNum {
					if p := found.pulls[parsed.number]; p != nil {
						return &gqlObj{typename: "PullRequest", repo: found, pull: p}
					}
				}
				return nil
			}
			return nil
		case "__type":
			name := argStr("name")
			fields, ok := gqlFields[name]
			if !ok {
				return nil
			}
			return &gqlObj{typename: "__Type", typeName: name, fieldNames: fields}
		case "__schema":
			return &gqlObj{typename: "__Schema"}
		}
		return nil
	case "Mutation":
		switch field {
		case "createIssue":
			return e.mutationCreateIssue(args)
		case "closeIssue":
			return e.mutationCloseIssue(args)
		case "createPullRequest":
			return e.mutationCreatePull(args)
		case "mergePullRequest":
			return e.mutationMergePull(args)
		case "addComment":
			return e.mutationAddComment(args)
		}
		return nil
	case "Viewer":
		switch field {
		case "login":
			return JStrV(viewerLogin)
		case "id":
			return JStrV(strconv.Itoa(viewerID))
		case "name":
			return JStrV(viewerName)
		case "email":
			return JStrV(viewerEmail)
		}
		return nil
	case "Repository":
		if repo == nil {
			return nil
		}
		switch field {
		case "id":
			return JStrV(globalID("Repository", repo.id, 0, false))
		case "databaseId":
			return JInt(int64(repo.id))
		case "name":
			return JStrV(repo.name)
		case "nameWithOwner":
			return JStrV(repo.owner + "/" + repo.name)
		case "description":
			return JStrV(repo.description)
		case "private":
			return JBoolV(repo.private)
		case "url":
			return JStrV("https://github.com/" + repo.owner + "/" + repo.name)
		case "owner":
			return &gqlObj{typename: "User", userLogin: repo.owner}
		case "defaultBranchRef":
			tip, ok := repo.refs["refs/heads/"+repo.defaultBranch]
			if !ok {
				return nil
			}
			_ = tip
			return &gqlObj{typename: "Ref", refName: repo.defaultBranch}
		case "viewerPermission":
			return JStrV("ADMIN")
		case "hasIssuesEnabled":
			return JBoolV(true)
		case "issue":
			numV := args.ObjGet("number")
			num, ok := jsToInt(numV)
			if !ok {
				return nil
			}
			iss := repo.issues[int(num)]
			if iss == nil {
				return nil
			}
			return &gqlObj{typename: "Issue", repo: repo, issue: iss}
		case "issues":
			return e.issueConnection(repo, args)
		case "pullRequests":
			return e.pullConnection(repo, args)
		case "pullRequest":
			numV := args.ObjGet("number")
			num, ok := jsToInt(numV)
			if !ok {
				return nil
			}
			p := repo.pulls[int(num)]
			if p == nil {
				return nil
			}
			return &gqlObj{typename: "PullRequest", repo: repo, pull: p}
		}
		return nil
	case "User":
		switch field {
		case "login":
			if obj.userLogin != "" {
				return JStrV(obj.userLogin)
			}
			return JStrV(viewerLogin)
		case "id":
			return JStrV(strconv.Itoa(viewerID))
		case "name":
			return JStrV(viewerName)
		case "email":
			return JStrV(viewerEmail)
		}
		return nil
	case "Ref":
		if field == "name" {
			return JStrV(obj.refName)
		}
		return nil
	case "Issue":
		iss := obj.issue
		switch field {
		case "id":
			return JStrV(globalID("Issue", repo.id, iss.number, true))
		case "number":
			return JInt(int64(iss.number))
		case "title":
			return JStrV(iss.title)
		case "state":
			return JStrV(strings.ToUpper(iss.state))
		case "body":
			return JStrV(iss.body)
		case "url":
			return JStrV("https://github.com/" + repo.owner + "/" + repo.name + "/issues/" + strconv.Itoa(iss.number))
		case "createdAt":
			return JStrV(fixedISO)
		}
		return nil
	case "PullRequest":
		p := obj.pull
		switch field {
		case "id":
			return JStrV(globalID("PullRequest", repo.id, p.number, true))
		case "number":
			return JInt(int64(p.number))
		case "title":
			return JStrV(p.title)
		case "body":
			return JStrV(p.body)
		case "state":
			return JStrV(strings.ToUpper(p.state))
		case "headRefName":
			return JStrV(p.head)
		case "baseRefName":
			return JStrV(p.base)
		case "headRef":
			return &gqlObj{typename: "Ref", refName: p.head}
		case "baseRef":
			return &gqlObj{typename: "Ref", refName: p.base}
		case "mergeable":
			if p.state == "open" && isMergeable(repo, p) {
				return JStrV("MERGEABLE")
			}
			return JStrV("CONFLICTING")
		case "merged":
			return JBoolV(p.merged)
		case "mergedAt":
			if p.mergedAt == "" {
				return JNullV()
			}
			return JStrV(p.mergedAt)
		case "mergeCommitSha":
			if p.mergeCommitSha == "" {
				return JNullV()
			}
			return JStrV(p.mergeCommitSha)
		case "url":
			return JStrV("https://github.com/" + repo.owner + "/" + repo.name + "/pull/" + strconv.Itoa(p.number))
		case "files":
			files := pullFiles(repo, p)
			first := 10
			if v := args.ObjGet("first"); v != nil && v.Kind == JNum {
				if n, ok := jsToInt(v); ok {
					first = int(n)
				}
			}
			var fileNodes []*gqlObj
			for i, f := range files {
				if i >= first {
					break
				}
				fileNodes = append(fileNodes, &gqlObj{typename: "PullRequestFile", fileFields: f})
			}
			return &gqlObj{
				typename: "PullRequestFileConnection",
				nodes:    fileNodes,
				pageInfo: &gqlObj{typename: "PageInfo", hasNextPage: len(files) > first},
			}
		}
		return nil
	case "IssueConnection", "PullRequestConnection", "PullRequestFileConnection":
		switch field {
		case "nodes":
			return obj.nodes
		case "totalCount":
			return JInt(int64(len(obj.nodes)))
		case "pageInfo":
			return obj.pageInfo
		}
		return nil
	case "PageInfo":
		switch field {
		case "hasNextPage":
			return JBoolV(obj.hasNextPage)
		case "endCursor":
			if obj.hasEndCursor {
				return JStrV(obj.endCursor)
			}
			return JNullV()
		case "hasPreviousPage":
			return JBoolV(false)
		case "startCursor":
			return JNullV()
		}
		return nil
	case "PullRequestFile":
		if obj.fileFields == nil {
			return nil
		}
		switch field {
		case "path":
			return JStrV(obj.fileFields.ObjGet("filename").Str)
		case "additions":
			return obj.fileFields.ObjGet("additions")
		case "deletions":
			return obj.fileFields.ObjGet("deletions")
		case "changes":
			return obj.fileFields.ObjGet("changes")
		}
		return nil
	case "Comment":
		switch field {
		case "id":
			if obj.comment == nil {
				return JNullV()
			}
			return JStrV(globalID("Comment", repo.id, obj.comment.id, true))
		case "body":
			if obj.comment == nil {
				return JNullV()
			}
			return JStrV(obj.comment.body)
		}
		return nil
	case "CommentEdge":
		if field == "node" {
			if obj.comment == nil {
				return nil
			}
			return &gqlObj{typename: "Comment", repo: repo, comment: obj.comment}
		}
		return nil
	case "CreateIssuePayload", "CloseIssuePayload":
		if field == "issue" {
			if obj.issue == nil {
				return nil
			}
			return &gqlObj{typename: "Issue", repo: repo, issue: obj.issue}
		}
		return nil
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if field == "pullRequest" {
			if obj.pull == nil {
				return nil
			}
			return &gqlObj{typename: "PullRequest", repo: repo, pull: obj.pull}
		}
		return nil
	case "AddCommentPayload":
		if field == "commentEdge" {
			return &gqlObj{typename: "CommentEdge", repo: repo, comment: obj.comment}
		}
		return nil
	case "__Type":
		switch field {
		case "name":
			return JStrV(obj.typeName)
		case "kind":
			if obj.typeName == "SearchType" {
				return JStrV("ENUM")
			}
			return JStrV("OBJECT")
		case "fields":
			var out []*gqlObj
			for _, n := range obj.fieldNames {
				out = append(out, &gqlObj{typename: "__Field", fieldName: n})
			}
			return out
		case "enumValues":
			if obj.typeName == "SearchType" {
				var out []*gqlObj
				for _, n := range []string{"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "COMMIT"} {
					out = append(out, &gqlObj{typename: "__EnumValue", enumName: n})
				}
				return out
			}
			return []*gqlObj{}
		}
		return nil
	case "__Field":
		if field == "name" {
			return JStrV(obj.fieldName)
		}
		return nil
	case "__EnumValue":
		if field == "name" {
			return JStrV(obj.enumName)
		}
		return nil
	case "__Schema":
		switch field {
		case "types":
			var out []*gqlObj
			for _, n := range gqlTypeOrder {
				out = append(out, &gqlObj{typename: "__Type", typeName: n, fieldNames: gqlFields[n]})
			}
			return out
		case "queryType":
			return &gqlObj{typename: "__Type", typeName: "Query", fieldNames: gqlFields["Query"]}
		case "mutationType":
			return &gqlObj{typename: "__Type", typeName: "Mutation", fieldNames: gqlFields["Mutation"]}
		}
		return nil
	}
	return nil
}

func (e *gqlExecutor) issueConnection(repo *repo, args *JVal) *gqlObj {
	var states []string
	var hasStates bool
	if v := args.ObjGet("states"); v != nil && v.Kind == JArr {
		hasStates = true
		for _, item := range v.Arr {
			states = append(states, strings.ToLower(jsStrOf(item)))
		}
	}
	orderBy := args.ObjGet("orderBy")
	direction := "ASC"
	if orderBy != nil && orderBy.Kind == JObj {
		if d := orderBy.ObjGet("direction"); d != nil && d.Kind == JStr {
			direction = d.Str
		}
	}
	var items []*issue
	for _, iss := range repo.issues {
		items = append(items, iss)
	}
	if hasStates {
		var filtered []*issue
		for _, iss := range items {
			for _, s := range states {
				if strings.ToLower(iss.state) == s {
					filtered = append(filtered, iss)
					break
				}
			}
		}
		items = filtered
	} else {
		var filtered []*issue
		for _, iss := range items {
			if iss.state == "open" {
				filtered = append(filtered, iss)
			}
		}
		items = filtered
	}
	desc := strings.ToUpper(direction) == "DESC"
	if desc {
		sortIssuesDesc(items)
	} else {
		sortIssuesAsc(items)
	}
	return e.paginateNodesIssue(repo, items, args)
}

func sortIssuesAsc(items []*issue) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].seq < items[j-1].seq; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

func sortIssuesDesc(items []*issue) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].seq > items[j-1].seq; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

func (e *gqlExecutor) pullConnection(repo *repo, args *JVal) *gqlObj {
	var states []string
	var hasStates bool
	if v := args.ObjGet("states"); v != nil && v.Kind == JArr {
		hasStates = true
		for _, item := range v.Arr {
			states = append(states, strings.ToLower(jsStrOf(item)))
		}
	}
	var items []*pull
	for _, p := range repo.pulls {
		items = append(items, p)
	}
	if hasStates {
		var filtered []*pull
		for _, p := range items {
			for _, s := range states {
				if strings.ToLower(p.state) == s {
					filtered = append(filtered, p)
					break
				}
			}
		}
		items = filtered
	} else {
		var filtered []*pull
		for _, p := range items {
			if p.state == "open" {
				filtered = append(filtered, p)
			}
		}
		items = filtered
	}
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].seq < items[j-1].seq; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
	return e.paginateNodesPull(repo, items, args)
}

func (e *gqlExecutor) paginateNodesIssue(repo *repo, items []*issue, args *JVal) *gqlObj {
	first := 10
	if v := args.ObjGet("first"); v != nil && v.Kind == JNum {
		if n, ok := jsToInt(v); ok {
			first = int(n)
		}
	}
	after := ""
	if v := args.ObjGet("after"); v != nil && v.Kind == JStr {
		after = v.Str
	}
	start := 0
	if after != "" {
		if raw, ok := lenientB64Decode(after); ok {
			parts := strings.Split(string(raw), ":")
			if len(parts) >= 2 {
				if num, err := strconv.Atoi(parts[1]); err == nil {
					for idx, item := range items {
						if item.number == num {
							start = idx + 1
							break
						}
					}
				}
			}
		}
	}
	end := start + first
	if end > len(items) {
		end = len(items)
	}
	if start > len(items) {
		start = len(items)
	}
	page := items[start:end]
	hasNext := start+len(page) < len(items)
	var endCursor string
	hasEndCursor := false
	if len(page) > 0 {
		endCursor = base64Encode("cursor:" + strconv.Itoa(page[len(page)-1].number))
		hasEndCursor = true
	}
	nodes := make([]*gqlObj, len(page))
	for i, iss := range page {
		nodes[i] = &gqlObj{typename: "Issue", repo: repo, issue: iss}
	}
	return e.finishConnection("IssueConnection", nodes, hasNext, endCursor, hasEndCursor)
}

func (e *gqlExecutor) paginateNodesPull(repo *repo, items []*pull, args *JVal) *gqlObj {
	first := 10
	if v := args.ObjGet("first"); v != nil && v.Kind == JNum {
		if n, ok := jsToInt(v); ok {
			first = int(n)
		}
	}
	after := ""
	if v := args.ObjGet("after"); v != nil && v.Kind == JStr {
		after = v.Str
	}
	start := 0
	if after != "" {
		if raw, ok := lenientB64Decode(after); ok {
			parts := strings.Split(string(raw), ":")
			if len(parts) >= 2 {
				if num, err := strconv.Atoi(parts[1]); err == nil {
					for idx, item := range items {
						if item.number == num {
							start = idx + 1
							break
						}
					}
				}
			}
		}
	}
	end := start + first
	if end > len(items) {
		end = len(items)
	}
	if start > len(items) {
		start = len(items)
	}
	page := items[start:end]
	hasNext := start+len(page) < len(items)
	var endCursor string
	hasEndCursor := false
	if len(page) > 0 {
		endCursor = base64Encode("cursor:" + strconv.Itoa(page[len(page)-1].number))
		hasEndCursor = true
	}
	nodes := make([]*gqlObj, len(page))
	for i := range page {
		nodes[i] = &gqlObj{typename: "PullRequest", repo: repo, pull: page[i]}
	}
	return e.finishConnection("PullRequestConnection", nodes, hasNext, endCursor, hasEndCursor)
}

func (e *gqlExecutor) finishConnection(typename string, nodes []*gqlObj, hasNext bool, endCursor string, hasEndCursor bool) *gqlObj {
	return &gqlObj{
		typename: typename,
		nodes:    nodes,
		pageInfo: &gqlObj{typename: "PageInfo", hasNextPage: hasNext, endCursor: endCursor, hasEndCursor: hasEndCursor},
	}
}

func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// --- Mutations ---

func (e *gqlExecutor) mutationCreateIssue(args *JVal) *gqlObj {
	input := args.ObjGet("input")
	if input == nil || input.Kind != JObj {
		input = JObjV()
	}
	parsed, ok := parseGlobalID(jsStrOf(input.ObjGet("repositoryId")))
	if !ok || parsed.kind != "Repository" {
		e.errors = append(e.errors, "Could not resolve repository id.")
		return nil
	}
	repo := repoByID(parsed.repoID)
	if repo == nil {
		e.errors = append(e.errors, "Could not resolve repository id.")
		return nil
	}
	number := repo.nextNumber
	repo.nextNumber++
	iss := &issue{
		id:     nextGlobalID,
		number: number,
		title:  jsStrOf(input.ObjGet("title")),
		body:   jsStrOf(input.ObjGet("body")),
		state:  "open",
		seq:    number,
	}
	nextGlobalID++
	repo.issues[number] = iss
	return &gqlObj{typename: "CreateIssuePayload", repo: repo, issue: iss}
}

func (e *gqlExecutor) mutationCloseIssue(args *JVal) *gqlObj {
	input := args.ObjGet("input")
	if input == nil || input.Kind != JObj {
		input = JObjV()
	}
	parsed, ok := parseGlobalID(jsStrOf(input.ObjGet("issueId")))
	if !ok || parsed.kind != "Issue" || !parsed.hasNum {
		e.errors = append(e.errors, "Could not resolve issue id.")
		return nil
	}
	repo := repoByID(parsed.repoID)
	var iss *issue
	if repo != nil {
		iss = repo.issues[parsed.number]
	}
	if repo == nil || iss == nil {
		e.errors = append(e.errors, "Could not resolve issue id.")
		return nil
	}
	iss.state = "closed"
	return &gqlObj{typename: "CloseIssuePayload", repo: repo, issue: iss}
}

func (e *gqlExecutor) mutationCreatePull(args *JVal) *gqlObj {
	input := args.ObjGet("input")
	if input == nil || input.Kind != JObj {
		input = JObjV()
	}
	parsed, ok := parseGlobalID(jsStrOf(input.ObjGet("repositoryId")))
	if !ok || parsed.kind != "Repository" {
		e.errors = append(e.errors, "Could not resolve repository id.")
		return nil
	}
	repo := repoByID(parsed.repoID)
	if repo == nil {
		e.errors = append(e.errors, "Could not resolve repository id.")
		return nil
	}
	head := jsStrOf(input.ObjGet("headRefName"))
	base := jsStrOf(input.ObjGet("baseRefName"))
	if base == "" {
		base = repo.defaultBranch
	}
	headSha, okH := repo.refs["refs/heads/"+head]
	baseSha, okB := repo.refs["refs/heads/"+base]
	if !okH || !okB {
		e.errors = append(e.errors, "Head or base branch does not exist.")
		return nil
	}
	for _, existing := range repo.pulls {
		if existing.head == head && existing.base == base && existing.state == "open" {
			return &gqlObj{typename: "CreatePullRequestPayload", repo: repo, pull: existing}
		}
	}
	number := repo.nextNumber
	repo.nextNumber++
	mb, hasMB := mergeBase(repo, headSha, baseSha)
	p := &pull{
		id:           nextGlobalID,
		number:       number,
		title:        jsStrOf(input.ObjGet("title")),
		body:         jsStrOf(input.ObjGet("body")),
		state:        "open",
		head:         head,
		base:         base,
		headSha:      headSha,
		baseSha:      baseSha,
		mergeBase:    mb,
		hasMergeBase: hasMB,
		seq:          number,
	}
	nextGlobalID++
	repo.pulls[number] = p
	return &gqlObj{typename: "CreatePullRequestPayload", repo: repo, pull: p}
}

func (e *gqlExecutor) mutationMergePull(args *JVal) *gqlObj {
	input := args.ObjGet("input")
	if input == nil || input.Kind != JObj {
		input = JObjV()
	}
	parsed, ok := parseGlobalID(jsStrOf(input.ObjGet("pullRequestId")))
	if !ok || parsed.kind != "PullRequest" || !parsed.hasNum {
		e.errors = append(e.errors, "Could not resolve pull request id.")
		return nil
	}
	repo := repoByID(parsed.repoID)
	var p *pull
	if repo != nil {
		p = repo.pulls[parsed.number]
	}
	if repo == nil || p == nil {
		e.errors = append(e.errors, "Could not resolve pull request id.")
		return nil
	}
	if p.state == "open" {
		if _, okMerge, _ := doMerge(repo, p); !okMerge {
			e.errors = append(e.errors, "Merge conflict.")
			return nil
		}
	}
	return &gqlObj{typename: "MergePullRequestPayload", repo: repo, pull: p}
}

func (e *gqlExecutor) mutationAddComment(args *JVal) *gqlObj {
	input := args.ObjGet("input")
	if input == nil || input.Kind != JObj {
		input = JObjV()
	}
	parsed, ok := parseGlobalID(jsStrOf(input.ObjGet("subjectId")))
	if !ok || !parsed.hasNum {
		e.errors = append(e.errors, "Could not resolve subject id.")
		return nil
	}
	repo := repoByID(parsed.repoID)
	var iss *issue
	if repo != nil {
		iss = repo.issues[parsed.number]
	}
	if repo == nil || iss == nil {
		e.errors = append(e.errors, "Could not resolve subject id.")
		return nil
	}
	c := &comment{id: repo.nextCommentID, body: jsStrOf(input.ObjGet("body"))}
	repo.nextCommentID++
	iss.comments = append(iss.comments, c)
	return &gqlObj{typename: "AddCommentPayload", repo: repo, comment: c}
}

// --- Handler ---

func handleGraphQL(rq *request) *response {
	parsed, err := parseJSON(rq.body)
	if err != nil {
		return errResp(400, "Problems parsing JSON")
	}
	runOne := func(entry *JVal) *JVal {
		if entry == nil || entry.Kind != JObj {
			return JObjV(
				JF("data", JNullV()),
				JF("errors", JArrV(JObjV(JF("message", JStrV("Expected an operation object."))))),
			)
		}
		query := entry.ObjGet("query")
		if query == nil || query.Kind != JStr {
			return JObjV(
				JF("data", JNullV()),
				JF("errors", JArrV(JObjV(JF("message", JStrV("Expected a query string."))))),
			)
		}
		variables := entry.ObjGet("variables")
		if variables == nil || variables.Kind != JObj {
			variables = JObjV()
		}
		op, err := (&gqlParser{src: query.Str}).parseOperation()
		if err != nil {
			return JObjV(
				JF("data", JNullV()),
				JF("errors", JArrV(JObjV(JF("message", JStrV(err.Error()))))),
			)
		}
		exec := &gqlExecutor{variables: variables}
		data, errs := exec.execute(op)
		if len(errs) > 0 {
			errItems := make([]*JVal, len(errs))
			for i, m := range errs {
				errItems[i] = JObjV(JF("message", JStrV(m)))
			}
			return JObjV(JF("data", JNullV()), JF("errors", JArrV(errItems...)))
		}
		return JObjV(JF("data", data))
	}
	if parsed.Kind == JArr {
		items := make([]*JVal, len(parsed.Arr))
		for i, entry := range parsed.Arr {
			items[i] = runOne(entry)
		}
		return jsonResp(200, JArrV(items...))
	}
	return jsonResp(200, runOne(parsed))
}
