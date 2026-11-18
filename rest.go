// REST v3 handlers and JSON shapes — a faithful port of main.ts.
package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

var token = envOr("MAKEFUN_SHOP_TOKEN", "test-token")

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func strVal(v *JVal, fallback string) string {
	if v != nil && v.Kind == JStr {
		return v.Str
	}
	return fallback
}

func boolVal(v *JVal, fallback bool) bool {
	if v != nil && v.Kind == JBool {
		return v.Bool
	}
	return fallback
}

// parseJSONBody mirrors TS parseJsonBody: empty -> {}; non-object JSON -> {};
// invalid JSON -> not ok.
func parseJSONBody(body []byte) (*JVal, bool) {
	if len(body) == 0 {
		return JObjV(), true
	}
	parsed, err := parseJSON(body)
	if err != nil {
		return nil, false
	}
	if parsed.Kind == JObj {
		return parsed, true
	}
	return JObjV(), true
}

func checkAuth(rq *request) bool {
	return rq.headers["authorization"] == "Bearer "+token
}

func requireAuth(rq *request) *response {
	if checkAuth(rq) {
		return nil
	}
	return errResp(401, "Bad credentials")
}

func intQuery(rq *request, name string, fallback int) int {
	raw, ok := rq.queryGet(name)
	if !ok {
		return fallback
	}
	n := jsParseNumber(raw)
	if n != n || n > 9.007199254740992e15 || n < -9.007199254740992e15 {
		return fallback
	}
	if n != float64(int64(n)) {
		return fallback
	}
	if n <= 0 {
		return fallback
	}
	return int(n)
}

func paginate(items []*JVal, rq *request) ([]*JVal, string, bool) {
	perPage := intQuery(rq, "per_page", 30)
	if perPage > 100 {
		perPage = 100
	}
	page := intQuery(rq, "page", 1)
	start := (page - 1) * perPage
	var slice []*JVal
	if start < len(items) {
		end := start + perPage
		if end > len(items) {
			end = len(items)
		}
		slice = items[start:end]
	}
	var link string
	hasLink := false
	if start+perPage < len(items) {
		host := rq.headers["host"]
		if host == "" {
			host = "127.0.0.1"
		}
		rq.querySet("page", strconv.Itoa(page+1))
		rq.querySet("per_page", strconv.Itoa(perPage))
		link = "<http://" + host + rq.path + "?" + rq.queryString() + ">; rel=\"next\""
		hasLink = true
	}
	return slice, link, hasLink
}

func withLink(extra []header, link string, hasLink bool) []header {
	if hasLink {
		return append(extra, header{"link", link})
	}
	return extra
}

// --- JSON shapes ---

func repoJSON(r *repo) *JVal {
	return JObjV(
		JF("id", JInt(int64(r.id))),
		JF("node_id", JStrV(globalID("Repository", r.id, 0, false))),
		JF("name", JStrV(r.name)),
		JF("full_name", JStrV(r.owner+"/"+r.name)),
		JF("owner", JObjV(JF("login", JStrV(r.owner)), JF("id", JInt(viewerID)), JF("type", JStrV("User")))),
		JF("private", JBoolV(r.private)),
		JF("description", JStrV(r.description)),
		JF("default_branch", JStrV(r.defaultBranch)),
		JF("created_at", JStrV(fixedISO)),
		JF("updated_at", JStrV(fixedISO)),
		JF("html_url", JStrV("https://github.com/"+r.owner+"/"+r.name)),
		JF("url", JStrV("/api/v3/repos/"+r.owner+"/"+r.name)),
		JF("has_issues", JBoolV(true)),
		JF("has_projects", JBoolV(false)),
		JF("has_wiki", JBoolV(false)),
	)
}

func issueJSON(r *repo, iss *issue) *JVal {
	labels := make([]*JVal, len(iss.labels))
	for i, l := range iss.labels {
		labels[i] = JObjV(JF("name", JStrV(l.name)), JF("color", JStrV(l.color)))
	}
	return JObjV(
		JF("id", JInt(int64(iss.id))),
		JF("node_id", JStrV(globalID("Issue", r.id, iss.number, true))),
		JF("number", JInt(int64(iss.number))),
		JF("title", JStrV(iss.title)),
		JF("body", JStrV(iss.body)),
		JF("state", JStrV(iss.state)),
		JF("labels", JArrV(labels...)),
		JF("user", JObjV(JF("login", JStrV(viewerLogin)), JF("id", JInt(viewerID)), JF("type", JStrV("User")))),
		JF("created_at", JStrV(fixedISO)),
		JF("updated_at", JStrV(fixedISO)),
		JF("html_url", JStrV("https://github.com/"+r.owner+"/"+r.name+"/issues/"+strconv.Itoa(iss.number))),
		JF("url", JStrV("/api/v3/repos/"+r.owner+"/"+r.name+"/issues/"+strconv.Itoa(iss.number))),
		JF("comments", JInt(int64(len(iss.comments)))),
	)
}

func pullJSON(r *repo, p *pull) *JVal {
	mergeable := false
	if p.state == "open" {
		mergeable = isMergeable(r, p)
	}
	mergeableState := "dirty"
	if mergeable {
		mergeableState = "clean"
	}
	var mergedAt *JVal
	if p.mergedAt == "" {
		mergedAt = JNullV()
	} else {
		mergedAt = JStrV(p.mergedAt)
	}
	var mergeCommitSha *JVal
	if p.mergeCommitSha == "" {
		mergeCommitSha = JNullV()
	} else {
		mergeCommitSha = JStrV(p.mergeCommitSha)
	}
	return JObjV(
		JF("id", JInt(int64(p.id))),
		JF("node_id", JStrV(globalID("PullRequest", r.id, p.number, true))),
		JF("number", JInt(int64(p.number))),
		JF("title", JStrV(p.title)),
		JF("body", JStrV(p.body)),
		JF("state", JStrV(p.state)),
		JF("merged", JBoolV(p.merged)),
		JF("merged_at", mergedAt),
		JF("merge_commit_sha", mergeCommitSha),
		JF("mergeable", JBoolV(mergeable)),
		JF("mergeable_state", JStrV(mergeableState)),
		JF("head", JObjV(JF("ref", JStrV(p.head)), JF("sha", JStrV(p.headSha)), JF("label", JStrV(r.owner+":"+p.head)))),
		JF("base", JObjV(JF("ref", JStrV(p.base)), JF("sha", JStrV(p.baseSha)), JF("label", JStrV(r.owner+":"+p.base)))),
		JF("user", JObjV(JF("login", JStrV(viewerLogin)), JF("id", JInt(viewerID)), JF("type", JStrV("User")))),
		JF("created_at", JStrV(fixedISO)),
		JF("updated_at", JStrV(fixedISO)),
		JF("html_url", JStrV("https://github.com/"+r.owner+"/"+r.name+"/pull/"+strconv.Itoa(p.number))),
		JF("url", JStrV("/api/v3/repos/"+r.owner+"/"+r.name+"/pulls/"+strconv.Itoa(p.number))),
	)
}

func branchJSON(r *repo, name string) *JVal {
	sha, ok := r.refs["refs/heads/"+name]
	if !ok {
		return nil
	}
	return JObjV(
		JF("name", JStrV(name)),
		JF("commit", JObjV(JF("sha", JStrV(sha)), JF("url", JStrV("/api/v3/repos/"+r.owner+"/"+r.name+"/commits/"+sha)))),
		JF("protected", JBoolV(false)),
	)
}

// parseIdent mirrors the TS ident regex /^(.*) <(.*)> (\d+) [+-]\d{4}$/
// with greedy groups; returns ok=false when it does not match (or the
// timestamp is outside JS Date range, where toISOString would throw).
func parseIdent(raw string) (name, email, date string, ok bool) {
	n := len(raw)
	if n < 7 {
		return raw, "", fixedISO, true
	}
	if raw[n-6] != ' ' || (raw[n-5] != '+' && raw[n-5] != '-') {
		return raw, "", fixedISO, true
	}
	for i := n - 4; i < n; i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return raw, "", fixedISO, true
		}
	}
	dStart := n - 6
	for dStart > 0 && raw[dStart-1] >= '0' && raw[dStart-1] <= '9' {
		dStart--
	}
	if dStart == n-6 || dStart < 2 || raw[dStart-1] != ' ' || raw[dStart-2] != '>' {
		return raw, "", fixedISO, true
	}
	ltPos := -1
	for i := dStart - 2; i >= 1; i-- {
		if raw[i-1] == ' ' && raw[i] == '<' {
			ltPos = i
			break
		}
	}
	if ltPos < 0 {
		return raw, "", fixedISO, true
	}
	tsStr := raw[dStart : n-6]
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil || ts > 8640000000000 || ts < -8640000000000 {
		return "", "", "", false
	}
	return raw[:ltPos-1], raw[ltPos+1 : dStart-2], identDate(ts), true
}

func commitJSON(r *repo, oid string) (*JVal, bool) {
	obj := r.objects[oid]
	if obj == nil || obj.typ != "commit" {
		return nil, false
	}
	view := parseCommit(obj.data)
	authorName, authorEmail, authorDate, ok1 := parseIdent(view.author)
	committerName, committerEmail, committerDate, ok2 := parseIdent(view.committer)
	if !ok1 || !ok2 {
		return nil, false
	}
	parents := make([]*JVal, len(view.parents))
	for i, p := range view.parents {
		parents[i] = JObjV(JF("sha", JStrV(p)))
	}
	return JObjV(
		JF("sha", JStrV(oid)),
		JF("commit", JObjV(
			JF("message", JStrV(view.message)),
			JF("author", JObjV(JF("name", JStrV(authorName)), JF("email", JStrV(authorEmail)), JF("date", JStrV(authorDate)))),
			JF("committer", JObjV(JF("name", JStrV(committerName)), JF("email", JStrV(committerEmail)), JF("date", JStrV(committerDate)))),
			JF("tree", JObjV(JF("sha", JStrV(view.tree)))),
		)),
		JF("parents", JArrV(parents...)),
		JF("author", JObjV(JF("login", JStrV(viewerLogin)), JF("id", JInt(viewerID)))),
		JF("committer", JObjV(JF("login", JStrV(viewerLogin)), JF("id", JInt(viewerID)))),
	), true
}

func findRepoOr404(rq *request, owner, name string) (*repo, *response) {
	if owner != viewerLogin {
		return nil, errResp(404, "Not Found")
	}
	r := getRepo(owner, name)
	if r == nil {
		return nil, errResp(404, "Not Found")
	}
	return r, nil
}

// --- REST handlers ---

func handleRoot(rq *request) *response {
	return jsonResp(200, JObjV(
		JF("current_user_url", JStrV("/api/v3/user")),
		JF("repository_url", JStrV("/api/v3/repos/{owner}/{repo}")),
		JF("user_url", JStrV("/api/v3/users/{user}")),
	), header{"x-oauth-scopes", "repo, read:org"})
}

func handleMeta(rq *request) *response {
	return jsonResp(200, JObjV(
		JF("installed_version", JStrV("1.0.0")),
		JF("verifiable_password_authentication", JBoolV(false)),
	))
}

func handleGetUser(rq *request) *response {
	return jsonResp(200, JObjV(
		JF("login", JStrV(viewerLogin)),
		JF("id", JInt(viewerID)),
		JF("node_id", JStrV(globalID("User", viewerID, 0, false))),
		JF("name", JStrV(viewerName)),
		JF("email", JStrV(viewerEmail)),
		JF("type", JStrV("User")),
		JF("site_admin", JBoolV(false)),
	))
}

func handleListUserRepos(rq *request) *response {
	reps := viewerRepos()
	items := make([]*JVal, len(reps))
	for i, r := range reps {
		items[i] = repoJSON(r)
	}
	page, link, hasLink := paginate(items, rq)
	return jsonResp(200, JArrV(page...), withLink([]header{{"x-oauth-scopes", "repo, read:org"}}, link, hasLink)...)
}

func validRepoName(name string) bool {
	if len(name) < 1 || len(name) > 100 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if i == 0 {
			if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
				return false
			}
		} else if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func handleCreateRepo(rq *request) *response {
	parsed, ok := parseJSONBody(rq.body)
	if !ok {
		return errResp(400, "Problems parsing JSON")
	}
	name := strVal(parsed.ObjGet("name"), "")
	if name == "" || !validRepoName(name) {
		return errResp(422, "Validation Failed: name is invalid")
	}
	if getRepo(viewerLogin, name) != nil {
		return errResp(422, "Repository creation failed: name already exists on this account")
	}
	r := createRepoRecord(viewerLogin, name, strVal(parsed.ObjGet("description"), ""), boolVal(parsed.ObjGet("private"), false))
	return jsonResp(201, repoJSON(r))
}

func handleGetRepo(rq *request, owner, name string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	return jsonResp(200, repoJSON(r))
}

func handlePatchRepo(rq *request, owner, name string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	parsed, ok := parseJSONBody(rq.body)
	if !ok {
		return errResp(400, "Problems parsing JSON")
	}
	if v := parsed.ObjGet("description"); v != nil && v.Kind == JStr {
		r.description = v.Str
	}
	if v := parsed.ObjGet("private"); v != nil && v.Kind == JBool {
		r.private = v.Bool
	}
	if v := parsed.ObjGet("default_branch"); v != nil && v.Kind == JStr {
		if _, ok := r.refs["refs/heads/"+v.Str]; ok {
			r.defaultBranch = v.Str
		}
	}
	return jsonResp(200, repoJSON(r))
}

func handleDeleteRepo(rq *request, owner, name string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	delete(repos, repoKey(r.owner, r.name))
	return emptyResp(204)
}

func handleListBranches(rq *request, owner, name string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	var names []string
	for ref := range r.refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			names = append(names, ref[len("refs/heads/"):])
		}
	}
	sort.Strings(names)
	var items []*JVal
	for _, n := range names {
		if b := branchJSON(r, n); b != nil {
			items = append(items, b)
		}
	}
	page, link, hasLink := paginate(items, rq)
	return jsonResp(200, JArrV(page...), withLink(nil, link, hasLink)...)
}

func handleGetBranch(rq *request, owner, name, branch string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	b := branchJSON(r, branch)
	if b == nil {
		return errResp(404, "Not Found")
	}
	return jsonResp(200, b)
}

func handleListCommits(rq *request, owner, name string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	refName, hasRef := rq.queryGet("sha")
	var tip string
	hasTip := false
	if hasRef {
		if v, ok := r.refs["refs/heads/"+refName]; ok {
			tip, hasTip = v, true
		} else if v, ok := r.refs["refs/tags/"+refName]; ok {
			tip, hasTip = v, true
		} else if isOID(refName) {
			if _, ok := r.objects[refName]; ok {
				tip, hasTip = refName, true
			}
		}
		if !hasTip {
			return errResp(404, "Not Found")
		}
	} else {
		tip, hasTip = r.refs["refs/heads/"+r.defaultBranch]
	}
	if !hasTip {
		return jsonResp(200, JArrV())
	}
	ancestors := ancestorCommits(r, []string{tip})
	commits := make([]string, 0, len(ancestors))
	for oid := range ancestors {
		commits = append(commits, oid)
	}
	sort.Slice(commits, func(i, j int) bool { return r.commitSeq[commits[i]] > r.commitSeq[commits[j]] })
	var items []*JVal
	for _, oid := range commits {
		c, ok := commitJSON(r, oid)
		if !ok {
			return errResp(500, "Internal Server Error")
		}
		items = append(items, c)
	}
	page, link, hasLink := paginate(items, rq)
	return jsonResp(200, JArrV(page...), withLink(nil, link, hasLink)...)
}

func handleCreateRef(rq *request, owner, name string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	parsed, ok := parseJSONBody(rq.body)
	if !ok {
		return errResp(400, "Problems parsing JSON")
	}
	ref := strVal(parsed.ObjGet("ref"), "")
	sha := strVal(parsed.ObjGet("sha"), "")
	if !strings.HasPrefix(ref, "refs/") || !isOID(sha) {
		return errResp(422, "Validation Failed: ref or sha is invalid")
	}
	if _, ok := r.objects[sha]; !ok {
		return errResp(422, "Validation Failed: ref or sha is invalid")
	}
	if _, ok := r.refs[ref]; ok {
		return errResp(422, "Validation Failed: Reference already exists")
	}
	r.refs[ref] = sha
	bumpGit(r)
	return jsonResp(201, JObjV(
		JF("ref", JStrV(ref)),
		JF("object", JObjV(JF("sha", JStrV(sha)), JF("type", JStrV("commit")))),
	))
}

func handleGetRef(rq *request, owner, name, branch string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	ref := "refs/heads/" + branch
	sha, ok := r.refs[ref]
	if !ok {
		return errResp(404, "Not Found")
	}
	return jsonResp(200, JObjV(
		JF("ref", JStrV(ref)),
		JF("object", JObjV(JF("sha", JStrV(sha)), JF("type", JStrV("commit")))),
	))
}

func resolveContentsRef(r *repo, ref string, hasRef bool) (string, bool) {
	if !hasRef {
		tip, ok := r.refs["refs/heads/"+r.defaultBranch]
		return tip, ok
	}
	if v, ok := r.refs["refs/heads/"+ref]; ok {
		return v, true
	}
	if v, ok := r.refs["refs/tags/"+ref]; ok {
		return v, true
	}
	if isOID(ref) {
		if _, ok := r.objects[ref]; ok {
			return ref, true
		}
	}
	return "", false
}

func handleGetContents(rq *request, owner, name, path string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	ref, hasRef := rq.queryGet("ref")
	tip, ok := resolveContentsRef(r, ref, hasRef)
	if !ok {
		return errResp(404, "Not Found")
	}
	tree, hasTree := commitTree(r, tip, true)
	flat := flattenTree(r, tree, hasTree, "")
	if blobOID, ok := flat[path]; ok {
		blob := r.objects[blobOID]
		if blob == nil {
			return errResp(404, "Not Found")
		}
		base := path
		if idx := strings.LastIndexByte(path, '/'); idx >= 0 {
			base = path[idx+1:]
		}
		return jsonResp(200, JObjV(
			JF("type", JStrV("file")),
			JF("encoding", JStrV("base64")),
			JF("size", JInt(int64(len(blob.data)))),
			JF("name", JStrV(base)),
			JF("path", JStrV(path)),
			JF("content", JStrV(base64.StdEncoding.EncodeToString(blob.data))),
			JF("sha", JStrV(blobOID)),
		))
	}
	prefix := ""
	if path != "" {
		prefix = path + "/"
	}
	type dirEnt struct {
		typ  string
		name string
		path string
		sha  string
		size int
	}
	entries := map[string]*dirEnt{}
	var order []string
	for p, oid := range flat {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := p[len(prefix):]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			blob := r.objects[oid]
			size := 0
			if blob != nil {
				size = len(blob.data)
			}
			if _, ok := entries[rest]; !ok {
				order = append(order, rest)
			}
			entries[rest] = &dirEnt{typ: "file", name: rest, path: p, sha: oid, size: size}
		} else {
			dir := rest[:slash]
			if _, ok := entries[dir]; !ok {
				order = append(order, dir)
				entries[dir] = &dirEnt{typ: "dir", name: dir, path: prefix + dir, sha: ""}
			}
		}
	}
	if len(entries) == 0 {
		return errResp(404, "Not Found")
	}
	sort.Slice(order, func(i, j int) bool { return entries[order[i]].name < entries[order[j]].name })
	var list []*JVal
	for _, k := range order {
		e := entries[k]
		if e.typ == "file" {
			list = append(list, JObjV(
				JF("type", JStrV("file")),
				JF("name", JStrV(e.name)),
				JF("path", JStrV(e.path)),
				JF("sha", JStrV(e.sha)),
				JF("size", JInt(int64(e.size))),
			))
		} else {
			list = append(list, JObjV(
				JF("type", JStrV("dir")),
				JF("name", JStrV(e.name)),
				JF("path", JStrV(e.path)),
				JF("sha", JStrV(e.sha)),
			))
		}
	}
	return jsonResp(200, JArrV(list...))
}

func handlePutContents(rq *request, owner, name, path string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	parsed, ok := parseJSONBody(rq.body)
	if !ok {
		return errResp(400, "Problems parsing JSON")
	}
	message := strVal(parsed.ObjGet("message"), "Update file")
	contentB64 := strVal(parsed.ObjGet("content"), "")
	if contentB64 == "" {
		return errResp(422, "Validation Failed: content is required")
	}
	content, ok := lenientB64Decode(contentB64)
	if !ok {
		return errResp(422, "Validation Failed: content is not valid base64")
	}
	branch := strVal(parsed.ObjGet("branch"), r.defaultBranch)
	var shaPtr *string
	if v := parsed.ObjGet("sha"); v != nil && v.Kind == JStr {
		s := v.Str
		shaPtr = &s
	}
	if _, ok := r.refs["refs/heads/"+branch]; !ok {
		if defaultTip, ok := r.refs["refs/heads/"+r.defaultBranch]; ok {
			r.refs["refs/heads/"+branch] = defaultTip
		}
	}
	tip, hasTip := r.refs["refs/heads/"+branch]
	tree, hasTree := commitTree(r, tip, hasTip)
	flat := flattenTree(r, tree, hasTree, "")
	_, exists := flat[path]
	if shaPtr != nil {
		current := flat[path]
		if !exists || current != *shaPtr {
			return errResp(409, "Conflict: sha does not match the current file")
		}
	}
	newCommit := commitFileChange(r, branch, path, content, true, message)
	blobOID := hashObject("blob", content)
	base := path
	if idx := strings.LastIndexByte(path, '/'); idx >= 0 {
		base = path[idx+1:]
	}
	status := 201
	if exists {
		status = 200
	}
	return jsonResp(status, JObjV(
		JF("content", JObjV(
			JF("name", JStrV(base)),
			JF("path", JStrV(path)),
			JF("sha", JStrV(blobOID)),
			JF("size", JInt(int64(len(content)))),
			JF("type", JStrV("file")),
			JF("encoding", JStrV("base64")),
			JF("content", JStrV(contentB64)),
		)),
		JF("commit", JObjV(JF("sha", JStrV(newCommit)), JF("message", JStrV(message)))),
	))
}

func handleDeleteContents(rq *request, owner, name, path string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	parsed, ok := parseJSONBody(rq.body)
	if !ok {
		return errResp(400, "Problems parsing JSON")
	}
	message := strVal(parsed.ObjGet("message"), "Delete file")
	var shaPtr *string
	if v := parsed.ObjGet("sha"); v != nil && v.Kind == JStr {
		s := v.Str
		shaPtr = &s
	}
	branch := strVal(parsed.ObjGet("branch"), r.defaultBranch)
	tip, hasTip := r.refs["refs/heads/"+branch]
	if !hasTip {
		return errResp(404, "Not Found")
	}
	tree, hasTree := commitTree(r, tip, true)
	flat := flattenTree(r, tree, hasTree, "")
	current, ok := flat[path]
	if !ok {
		return errResp(404, "Not Found")
	}
	if shaPtr != nil && current != *shaPtr {
		return errResp(409, "Conflict: sha does not match the current file")
	}
	newCommit := commitFileChange(r, branch, path, nil, false, message)
	base := path
	if idx := strings.LastIndexByte(path, '/'); idx >= 0 {
		base = path[idx+1:]
	}
	return jsonResp(200, JObjV(
		JF("content", JObjV(
			JF("name", JStrV(base)),
			JF("path", JStrV(path)),
			JF("sha", JStrV(current)),
			JF("type", JStrV("file")),
			JF("encoding", JStrV("base64")),
		)),
		JF("commit", JObjV(JF("sha", JStrV(newCommit)), JF("message", JStrV(message)))),
	))
}

func handleCreateLabel(rq *request, owner, name string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	parsed, ok := parseJSONBody(rq.body)
	if !ok {
		return errResp(400, "Problems parsing JSON")
	}
	labelName := strVal(parsed.ObjGet("name"), "")
	if labelName == "" {
		return errResp(422, "Validation Failed: name is required")
	}
	if _, ok := r.labels[labelName]; ok {
		return errResp(422, "Validation Failed: label already exists")
	}
	l := &label{name: labelName, color: strVal(parsed.ObjGet("color"), "ededed")}
	r.labels[labelName] = l
	r.labelOrder = append(r.labelOrder, labelName)
	return jsonResp(201, JObjV(JF("name", JStrV(l.name)), JF("color", JStrV(l.color))))
}

func handleListLabels(rq *request, owner, name string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	var items []*JVal
	for _, n := range r.labelOrder {
		l := r.labels[n]
		items = append(items, JObjV(JF("name", JStrV(l.name)), JF("color", JStrV(l.color))))
	}
	return jsonResp(200, JArrV(items...))
}

func listIssuesFiltered(r *repo, rq *request) []*issue {
	state, _ := rq.queryGet("state")
	if state == "" {
		state = "open"
	}
	sortMode, _ := rq.queryGet("sort")
	if sortMode == "" {
		sortMode = "created"
	}
	direction, _ := rq.queryGet("direction")
	if direction == "" {
		direction = "desc"
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
	if sortMode == "created" {
		if direction == "asc" {
			sort.Slice(items, func(i, j int) bool { return items[i].seq < items[j].seq })
		} else {
			sort.Slice(items, func(i, j int) bool { return items[i].seq > items[j].seq })
		}
	} else {
		if direction == "asc" {
			sort.Slice(items, func(i, j int) bool { return items[i].number < items[j].number })
		} else {
			sort.Slice(items, func(i, j int) bool { return items[i].number > items[j].number })
		}
	}
	return items
}

func handleCreateIssue(rq *request, owner, name string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	parsed, ok := parseJSONBody(rq.body)
	if !ok {
		return errResp(400, "Problems parsing JSON")
	}
	title := strVal(parsed.ObjGet("title"), "")
	if title == "" {
		return errResp(422, "Validation Failed: title is required")
	}
	var labelNames []string
	if v := parsed.ObjGet("labels"); v != nil && v.Kind == JArr {
		for _, l := range v.Arr {
			if l.Kind == JStr {
				labelNames = append(labelNames, l.Str)
			} else {
				labelNames = append(labelNames, strVal(l.ObjGet("name"), ""))
			}
		}
	}
	var labels []*label
	for _, n := range labelNames {
		l, ok := r.labels[n]
		if !ok {
			return errResp(422, "Validation Failed: label \""+n+"\" does not exist")
		}
		labels = append(labels, l)
	}
	number := r.nextNumber
	r.nextNumber++
	iss := &issue{
		id:     nextGlobalID,
		number: number,
		title:  title,
		body:   strVal(parsed.ObjGet("body"), ""),
		state:  "open",
		labels: labels,
		seq:    number,
	}
	nextGlobalID++
	r.issues[number] = iss
	return jsonResp(201, issueJSON(r, iss))
}

func handleListIssues(rq *request, owner, name string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	filtered := listIssuesFiltered(r, rq)
	items := make([]*JVal, len(filtered))
	for i, iss := range filtered {
		items[i] = issueJSON(r, iss)
	}
	page, link, hasLink := paginate(items, rq)
	return jsonResp(200, JArrV(page...), withLink(nil, link, hasLink)...)
}

func issueNumber(s string) int {
	n := jsParseNumber(s)
	if n != n || n != float64(int64(n)) {
		return 0
	}
	return int(n)
}

func handleGetIssue(rq *request, owner, name, number string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	iss, ok := r.issues[issueNumber(number)]
	if !ok {
		return errResp(404, "Not Found")
	}
	return jsonResp(200, issueJSON(r, iss))
}

func handlePatchIssue(rq *request, owner, name, number string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	iss, ok := r.issues[issueNumber(number)]
	if !ok {
		return errResp(404, "Not Found")
	}
	parsed, ok := parseJSONBody(rq.body)
	if !ok {
		return errResp(400, "Problems parsing JSON")
	}
	if v := parsed.ObjGet("state"); v != nil && v.Kind == JStr && (v.Str == "open" || v.Str == "closed") {
		iss.state = v.Str
	}
	if v := parsed.ObjGet("title"); v != nil && v.Kind == JStr {
		iss.title = v.Str
	}
	if v := parsed.ObjGet("body"); v != nil && v.Kind == JStr {
		iss.body = v.Str
	}
	return jsonResp(200, issueJSON(r, iss))
}

func handleCreateComment(rq *request, owner, name, number string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	iss, ok := r.issues[issueNumber(number)]
	if !ok {
		return errResp(404, "Not Found")
	}
	parsed, ok := parseJSONBody(rq.body)
	if !ok {
		return errResp(400, "Problems parsing JSON")
	}
	c := &comment{id: r.nextCommentID, body: strVal(parsed.ObjGet("body"), "")}
	r.nextCommentID++
	iss.comments = append(iss.comments, c)
	return jsonResp(201, JObjV(
		JF("id", JInt(int64(c.id))),
		JF("node_id", JStrV(globalID("IssueComment", r.id, c.id, true))),
		JF("body", JStrV(c.body)),
		JF("user", JObjV(JF("login", JStrV(viewerLogin)), JF("id", JInt(viewerID)), JF("type", JStrV("User")))),
		JF("created_at", JStrV(fixedISO)),
		JF("updated_at", JStrV(fixedISO)),
	))
}

func handleListComments(rq *request, owner, name, number string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	iss, ok := r.issues[issueNumber(number)]
	if !ok {
		return errResp(404, "Not Found")
	}
	var items []*JVal
	for _, c := range iss.comments {
		items = append(items, JObjV(
			JF("id", JInt(int64(c.id))),
			JF("node_id", JStrV(globalID("IssueComment", r.id, c.id, true))),
			JF("body", JStrV(c.body)),
			JF("user", JObjV(JF("login", JStrV(viewerLogin)), JF("id", JInt(viewerID)), JF("type", JStrV("User")))),
			JF("created_at", JStrV(fixedISO)),
			JF("updated_at", JStrV(fixedISO)),
		))
	}
	page, link, hasLink := paginate(items, rq)
	return jsonResp(200, JArrV(page...), withLink(nil, link, hasLink)...)
}

func pullFiles(r *repo, p *pull) []*JVal {
	headTree, hasHeadTree := commitTree(r, p.headSha, true)
	var mbTree string
	var hasMBTree bool
	if p.hasMergeBase {
		mbTree, hasMBTree = commitTree(r, p.mergeBase, true)
	}
	headFlat := flattenTree(r, headTree, hasHeadTree, "")
	baseFlat := flattenTree(r, mbTree, hasMBTree, "")
	paths := map[string]bool{}
	for k := range baseFlat {
		paths[k] = true
	}
	for k := range headFlat {
		paths[k] = true
	}
	var sorted []string
	for k := range paths {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	lineCount := func(oid string, has bool) int {
		if !has {
			return 0
		}
		obj := r.objects[oid]
		if obj == nil {
			return 0
		}
		text := string(obj.data)
		if text == "" {
			return 0
		}
		return strings.Count(text, "\n") + 1
	}
	var out []*JVal
	for _, path := range sorted {
		from, hasFrom := baseFlat[path]
		to, hasTo := headFlat[path]
		if hasFrom && hasTo && from == to {
			continue
		}
		if !hasFrom {
			additions := lineCount(to, hasTo)
			if additions < 1 {
				additions = 1
			}
			out = append(out, JObjV(
				JF("filename", JStrV(path)), JF("status", JStrV("added")),
				JF("additions", JInt(int64(additions))), JF("deletions", JInt(0)), JF("changes", JInt(int64(additions))),
			))
		} else if !hasTo {
			deletions := lineCount(from, hasFrom)
			if deletions < 1 {
				deletions = 1
			}
			out = append(out, JObjV(
				JF("filename", JStrV(path)), JF("status", JStrV("removed")),
				JF("additions", JInt(0)), JF("deletions", JInt(int64(deletions))), JF("changes", JInt(int64(deletions))),
			))
		} else {
			additions := lineCount(to, hasTo)
			if additions < 1 {
				additions = 1
			}
			deletions := lineCount(from, hasFrom)
			out = append(out, JObjV(
				JF("filename", JStrV(path)), JF("status", JStrV("modified")),
				JF("additions", JInt(int64(additions))), JF("deletions", JInt(int64(deletions))), JF("changes", JInt(int64(additions+deletions))),
			))
		}
	}
	return out
}

func handleCreatePull(rq *request, owner, name string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	parsed, ok := parseJSONBody(rq.body)
	if !ok {
		return errResp(400, "Problems parsing JSON")
	}
	head := strVal(parsed.ObjGet("head"), "")
	base := strVal(parsed.ObjGet("base"), r.defaultBranch)
	title := strVal(parsed.ObjGet("title"), "")
	if head == "" || title == "" {
		return errResp(422, "Validation Failed: head and title are required")
	}
	headSha, okH := r.refs["refs/heads/"+head]
	baseSha, okB := r.refs["refs/heads/"+base]
	if !okH || !okB {
		return errResp(422, "Validation Failed: head or base branch does not exist")
	}
	for _, existing := range r.pulls {
		if existing.head == head && existing.base == base && existing.state == "open" {
			return errResp(422, "Validation Failed: a pull request already exists for this head branch")
		}
	}
	number := r.nextNumber
	r.nextNumber++
	mb, hasMB := mergeBase(r, headSha, baseSha)
	p := &pull{
		id:           nextGlobalID,
		number:       number,
		title:        title,
		body:         strVal(parsed.ObjGet("body"), ""),
		state:        "open",
		head:         head,
		base:         base,
		headSha:      headSha,
		baseSha:      baseSha,
		merged:       false,
		mergeBase:    mb,
		hasMergeBase: hasMB,
		seq:          number,
	}
	nextGlobalID++
	r.pulls[number] = p
	return jsonResp(201, pullJSON(r, p))
}

func handleListPulls(rq *request, owner, name string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	state, _ := rq.queryGet("state")
	if state == "" {
		state = "open"
	}
	var items []*pull
	for _, p := range r.pulls {
		items = append(items, p)
	}
	if state != "all" {
		var filtered []*pull
		for _, p := range items {
			if p.state == state {
				filtered = append(filtered, p)
			}
		}
		items = filtered
	}
	sort.Slice(items, func(i, j int) bool { return items[i].seq > items[j].seq })
	var jsonItems []*JVal
	for _, p := range items {
		jsonItems = append(jsonItems, pullJSON(r, p))
	}
	page, link, hasLink := paginate(jsonItems, rq)
	return jsonResp(200, JArrV(page...), withLink(nil, link, hasLink)...)
}

func handleGetPull(rq *request, owner, name, number string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	p, ok := r.pulls[issueNumber(number)]
	if !ok {
		return errResp(404, "Not Found")
	}
	return jsonResp(200, pullJSON(r, p))
}

func handleListPullFiles(rq *request, owner, name, number string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	p, ok := r.pulls[issueNumber(number)]
	if !ok {
		return errResp(404, "Not Found")
	}
	return jsonResp(200, JArrV(pullFiles(r, p)...))
}

func doMerge(r *repo, p *pull) (string, bool, string) {
	headTip, ok := r.refs["refs/heads/"+p.head]
	if !ok {
		headTip = p.headSha
	}
	baseTip, ok := r.refs["refs/heads/"+p.base]
	if !ok {
		baseTip = p.baseSha
	}
	merged, okMerge := threeWayMerge(r, baseTip, true, headTip, true, p.mergeBase, p.hasMergeBase)
	if !okMerge {
		return "", false, "merge conflict"
	}
	newTree := buildTreeFromFlat(r, merged)
	timestamp := nextCommitTimestamp()
	author := viewerName + " <" + viewerEmail + ">"
	data := buildCommit(newTree, []string{baseTip}, p.title, timestamp, author)
	sha := storeCommit(r, data)
	r.refs["refs/heads/"+p.base] = sha
	bumpGit(r)
	p.state = "closed"
	p.merged = true
	p.mergedAt = fixedISO
	p.mergeCommitSha = sha
	p.baseSha = sha
	return sha, true, ""
}

func handleMergePull(rq *request, owner, name, number string) *response {
	r, errR := findRepoOr404(rq, owner, name)
	if errR != nil {
		return errR
	}
	p, ok := r.pulls[issueNumber(number)]
	if !ok {
		return errResp(404, "Not Found")
	}
	_, okBody := parseJSONBody(rq.body)
	if !okBody {
		return errResp(400, "Problems parsing JSON")
	}
	if p.state != "open" {
		return errResp(405, "Pull request is not open")
	}
	sha, okMerge, _ := doMerge(r, p)
	if !okMerge {
		return errResp(405, "Merge conflict")
	}
	return jsonResp(200, JObjV(
		JF("sha", JStrV(sha)),
		JF("merged", JBoolV(true)),
		JF("message", JStrV("Pull request successfully merged")),
	))
}

var _ = fmt.Sprintf
var _ = time.Now
