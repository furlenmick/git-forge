// MCP: JSON-RPC 2.0 Streamable HTTP at /mcp (revision 2026-07-28 + legacy
// handshake). Plain JSON responses; no SSE needed by the suite's client.
// A faithful port of main.ts.
package main

import (
	"strconv"
)

var mcpTools = []*JVal{
	JObjV(
		JF("name", JStrV("create_repo")),
		JF("description", JStrV("Create a repository owned by the authenticated user.")),
		JF("inputSchema", JObjV(
			JF("type", JStrV("object")),
			JF("properties", JObjV(
				JF("name", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Repository name")))),
				JF("description", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Repository description")))),
				JF("private", JObjV(JF("type", JStrV("boolean")), JF("description", JStrV("Whether the repository is private")))),
				JF("owner", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Owner login (defaults to the authenticated user)")))),
			)),
			JF("required", JArrV(JStrV("name"))),
		)),
	),
	JObjV(
		JF("name", JStrV("get_repo")),
		JF("description", JStrV("Get a repository by owner and name.")),
		JF("inputSchema", JObjV(
			JF("type", JStrV("object")),
			JF("properties", JObjV(
				JF("owner", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Repository owner login")))),
				JF("repo", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Repository name")))),
			)),
			JF("required", JArrV(JStrV("owner"), JStrV("repo"))),
		)),
	),
	JObjV(
		JF("name", JStrV("create_issue")),
		JF("description", JStrV("Create an issue in a repository.")),
		JF("inputSchema", JObjV(
			JF("type", JStrV("object")),
			JF("properties", JObjV(
				JF("owner", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Repository owner login")))),
				JF("repo", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Repository name")))),
				JF("title", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Issue title")))),
				JF("body", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Issue body")))),
			)),
			JF("required", JArrV(JStrV("owner"), JStrV("repo"), JStrV("title"))),
		)),
	),
	JObjV(
		JF("name", JStrV("list_issues")),
		JF("description", JStrV("List issues in a repository.")),
		JF("inputSchema", JObjV(
			JF("type", JStrV("object")),
			JF("properties", JObjV(
				JF("owner", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Repository owner login")))),
				JF("repo", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Repository name")))),
				JF("state", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Issue state: open, closed or all")))),
			)),
			JF("required", JArrV(JStrV("owner"), JStrV("repo"))),
		)),
	),
	JObjV(
		JF("name", JStrV("create_pull_request")),
		JF("description", JStrV("Create a pull request from a head branch into a base branch.")),
		JF("inputSchema", JObjV(
			JF("type", JStrV("object")),
			JF("properties", JObjV(
				JF("owner", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Repository owner login")))),
				JF("repo", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Repository name")))),
				JF("title", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Pull request title")))),
				JF("body", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Pull request body")))),
				JF("head", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Head branch name")))),
				JF("base", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Base branch name")))),
			)),
			JF("required", JArrV(JStrV("owner"), JStrV("repo"), JStrV("title"), JStrV("head"), JStrV("base"))),
		)),
	),
	JObjV(
		JF("name", JStrV("merge_pull_request")),
		JF("description", JStrV("Merge a pull request with a squash commit.")),
		JF("inputSchema", JObjV(
			JF("type", JStrV("object")),
			JF("properties", JObjV(
				JF("owner", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Repository owner login")))),
				JF("repo", JObjV(JF("type", JStrV("string")), JF("description", JStrV("Repository name")))),
				JF("pull_number", JObjV(JF("type", JStrV("integer")), JF("description", JStrV("Pull request number")))),
			)),
			JF("required", JArrV(JStrV("owner"), JStrV("repo"), JStrV("pull_number"))),
		)),
	),
}

var mcpServerInfo = JObjV(JF("name", JStrV("git-forge")), JF("version", JStrV("1.0.0")))

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

func mcpToolResult(payload *JVal, isError bool) *JVal {
	var text string
	if payload != nil && payload.Kind == JStr {
		text = payload.Str
	} else {
		text = string(encodeJSON(payload))
	}
	out := JObjV(
		JF("content", JArrV(JObjV(JF("type", JStrV("text")), JF("text", JStrV(text))))),
		JF("isError", JBoolV(isError)),
	)
	if payload == nil || payload.Kind != JStr {
		out.ObjSet("structuredContent", payload)
	}
	return out
}

type rpcError struct {
	code    int
	message string
}

// mcpCallTool returns either a tool result JVal or *rpcError for unknown tools.
func mcpCallTool(name string, args *JVal) (*JVal, *rpcError) {
	var tool *JVal
	for _, t := range mcpTools {
		if t.ObjGet("name").Str == name {
			tool = t
			break
		}
	}
	if tool == nil {
		return nil, &rpcError{-32602, "Unknown tool: " + name}
	}
	schema := tool.ObjGet("inputSchema")
	var required []string
	if schema != nil {
		if req := schema.ObjGet("required"); req != nil && req.Kind == JArr {
			for _, item := range req.Arr {
				required = append(required, item.Str)
			}
		}
	}
	var missing []string
	for _, key := range required {
		v := args.ObjGet(key)
		if v == nil || v.Kind == JNull || v.Kind == JUndef || (v.Kind == JStr && v.Str == "") {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return mcpToolResult(JObjV(JF("error", JStrV("Missing required argument(s): "+joinStr(missing, ", ")))), true), nil
	}
	argStr := func(key string) string {
		v := args.ObjGet(key)
		if v == nil || v.Kind == JNull || v.Kind == JUndef {
			return ""
		}
		return jsStrOf(v)
	}
	owner := viewerLogin
	if v := args.ObjGet("owner"); v != nil && v.Kind != JNull && v.Kind != JUndef {
		owner = jsStrOf(v)
	}
	switch name {
	case "create_repo":
		repoName := argStr("name")
		if getRepo(viewerLogin, repoName) != nil {
			return mcpToolResult(JObjV(JF("error", JStrV("Repository "+repoName+" already exists"))), true), nil
		}
		priv := false
		if v := args.ObjGet("private"); v != nil && v.Kind == JBool {
			priv = v.Bool
		}
		r := createRepoRecord(viewerLogin, repoName, argStr("description"), priv)
		return mcpToolResult(repoJSON(r), false), nil
	case "get_repo":
		var r *repo
		if owner == viewerLogin {
			r = getRepo(owner, argStr("repo"))
		}
		if r == nil {
			return mcpToolResult(JObjV(JF("error", JStrV("Repository "+owner+"/"+argStr("repo")+" not found"))), true), nil
		}
		return mcpToolResult(repoJSON(r), false), nil
	case "create_issue":
		var r *repo
		if owner == viewerLogin {
			r = getRepo(owner, argStr("repo"))
		}
		if r == nil {
			return mcpToolResult(JObjV(JF("error", JStrV("Repository "+owner+"/"+argStr("repo")+" not found"))), true), nil
		}
		number := r.nextNumber
		r.nextNumber++
		iss := &issue{id: nextGlobalID, number: number, title: argStr("title"), body: argStr("body"), state: "open", seq: number}
		nextGlobalID++
		r.issues[number] = iss
		return mcpToolResult(issueJSON(r, iss), false), nil
	case "list_issues":
		var r *repo
		if owner == viewerLogin {
			r = getRepo(owner, argStr("repo"))
		}
		if r == nil {
			return mcpToolResult(JObjV(JF("error", JStrV("Repository "+owner+"/"+argStr("repo")+" not found"))), true), nil
		}
		state := argStr("state")
		if state == "" {
			state = "open"
		}
		var items []*issue
		for _, iss := range r.issues {
			items = append(items, iss)
		}
		if state != "all" {
			var filtered []*issue
			for _, iss := range items {
				if iss.state == state {
					filtered = append(filtered, iss)
				}
			}
			items = filtered
		}
		// sort desc by seq
		for i := 1; i < len(items); i++ {
			for j := i; j > 0 && items[j].seq > items[j-1].seq; j-- {
				items[j], items[j-1] = items[j-1], items[j]
			}
		}
		itemsJ := make([]*JVal, len(items))
		for i, iss := range items {
			itemsJ[i] = issueJSON(r, iss)
		}
		return mcpToolResult(JObjV(JF("issues", JArrV(itemsJ...))), false), nil
	case "create_pull_request":
		var r *repo
		if owner == viewerLogin {
			r = getRepo(owner, argStr("repo"))
		}
		if r == nil {
			return mcpToolResult(JObjV(JF("error", JStrV("Repository "+owner+"/"+argStr("repo")+" not found"))), true), nil
		}
		head := argStr("head")
		base := argStr("base")
		if base == "" {
			base = r.defaultBranch
		}
		headSha, okH := r.refs["refs/heads/"+head]
		baseSha, okB := r.refs["refs/heads/"+base]
		if !okH || !okB {
			return mcpToolResult(JObjV(JF("error", JStrV("Head or base branch does not exist"))), true), nil
		}
		for _, existing := range r.pulls {
			if existing.head == head && existing.base == base && existing.state == "open" {
				return mcpToolResult(pullJSON(r, existing), false), nil
			}
		}
		number := r.nextNumber
		r.nextNumber++
		mb, hasMB := mergeBase(r, headSha, baseSha)
		p := &pull{
			id:           nextGlobalID,
			number:       number,
			title:        argStr("title"),
			body:         argStr("body"),
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
		r.pulls[number] = p
		return mcpToolResult(pullJSON(r, p), false), nil
	case "merge_pull_request":
		var r *repo
		if owner == viewerLogin {
			r = getRepo(owner, argStr("repo"))
		}
		if r == nil {
			return mcpToolResult(JObjV(JF("error", JStrV("Repository "+owner+"/"+argStr("repo")+" not found"))), true), nil
		}
		pullNumber, _ := jsToInt(args.ObjGet("pull_number"))
		p := r.pulls[int(pullNumber)]
		if p == nil {
			return mcpToolResult(JObjV(JF("error", JStrV("Pull request #"+strconv.FormatInt(pullNumber, 10)+" not found"))), true), nil
		}
		if p.state != "open" {
			return mcpToolResult(JObjV(JF("error", JStrV("Pull request is not open"))), true), nil
		}
		if _, okMerge, _ := doMerge(r, p); !okMerge {
			return mcpToolResult(JObjV(JF("error", JStrV("Merge conflict"))), true), nil
		}
		return mcpToolResult(pullJSON(r, p), false), nil
	default:
		return mcpToolResult(JObjV(JF("error", JStrV("Tool "+name+" is not implemented"))), true), nil
	}
}

func joinStr(items []string, sep string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

func handleMcp(rq *request) *response {
	if rq.method == "DELETE" {
		return emptyResp(204)
	}
	if rq.method != "POST" {
		return errResp(405, "Method Not Allowed")
	}
	parsed, err := parseJSON(rq.body)
	if err != nil {
		return jsonResp(400, JObjV(
			JF("jsonrpc", JStrV("2.0")),
			JF("id", JNullV()),
			JF("error", JObjV(JF("code", JInt(-32700)), JF("message", JStrV("Parse error")))),
		))
	}
	if parsed == nil || parsed.Kind != JObj {
		return jsonResp(400, JObjV(
			JF("jsonrpc", JStrV("2.0")),
			JF("id", JNullV()),
			JF("error", JObjV(JF("code", JInt(-32600)), JF("message", JStrV("Invalid Request")))),
		))
	}
	id := parsed.ObjGet("id")
	hasID := id != nil && id.Kind != JUndef
	var idVal *JVal
	if hasID {
		idVal = id
	} else {
		idVal = JNullV()
	}
	method := ""
	if v := parsed.ObjGet("method"); v != nil && v.Kind == JStr {
		method = v.Str
	}
	params := JObjV()
	if v := parsed.ObjGet("params"); v != nil && v.Kind == JObj {
		params = v
	}
	result := func(value *JVal) *response {
		return jsonResp(200, JObjV(
			JF("jsonrpc", JStrV("2.0")),
			JF("id", idVal),
			JF("result", value),
		))
	}
	errResp2 := func(code int, message string) *response {
		return jsonResp(200, JObjV(
			JF("jsonrpc", JStrV("2.0")),
			JF("id", idVal),
			JF("error", JObjV(JF("code", JInt(int64(code))), JF("message", JStrV(message)))),
		))
	}
	// Notifications (no id) are answered 202 with no body.
	if !hasID {
		return emptyResp(202)
	}
	if method == "server/discover" {
		versions := make([]*JVal, len(mcpSupportedVersions))
		for i, v := range mcpSupportedVersions {
			versions[i] = JStrV(v)
		}
		return result(JObjV(
			JF("resultType", JStrV("discover")),
			JF("supportedVersions", JArrV(versions...)),
			JF("capabilities", JObjV(JF("tools", JObjV()))),
			JF("instructions", JStrV("A GitHub-core clone: repositories, issues, pull requests, git smart HTTP, GraphQL.")),
			JF("serverInfo", mcpServerInfo),
			JF("_meta", JObjV(JF("io.modelcontextprotocol/serverInfo", mcpServerInfo))),
		))
	}
	if method == "initialize" {
		offered := ""
		if v := params.ObjGet("protocolVersion"); v != nil && v.Kind == JStr {
			offered = v.Str
		}
		negotiated := "2025-11-25"
		for _, v := range mcpSupportedVersions {
			if v == offered {
				negotiated = offered
				break
			}
		}
		return result(JObjV(
			JF("protocolVersion", JStrV(negotiated)),
			JF("capabilities", JObjV(JF("tools", JObjV()))),
			JF("serverInfo", mcpServerInfo),
		))
	}
	if method == "tools/list" {
		tools := make([]*JVal, len(mcpTools))
		copy(tools, mcpTools)
		return result(JObjV(JF("tools", JArrV(tools...))))
	}
	if method == "tools/call" {
		toolName := ""
		if v := params.ObjGet("name"); v != nil && v.Kind == JStr {
			toolName = v.Str
		}
		toolArgs := JObjV()
		if v := params.ObjGet("arguments"); v != nil && v.Kind == JObj {
			toolArgs = v
		}
		outcome, rpcErr := mcpCallTool(toolName, toolArgs)
		if rpcErr != nil {
			return errResp2(rpcErr.code, rpcErr.message)
		}
		return result(outcome)
	}
	if method == "ping" {
		return result(JObjV())
	}
	return errResp2(-32601, "Method not found: "+method)
}
