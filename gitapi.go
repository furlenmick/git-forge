// Git smart HTTP handlers — a faithful port of main.ts.
package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func gitRepoOr404(rq *request, owner, repoName string) (*repo, *response) {
	if owner != viewerLogin {
		return nil, jsonResp(404, JObjV(JF("message", JStrV("Not Found"))))
	}
	r := getRepo(owner, repoName)
	if r == nil {
		return nil, jsonResp(404, JObjV(JF("message", JStrV("Not Found"))))
	}
	return r, nil
}

func gitCacheGet(r *repo, key string) ([]byte, bool) {
	if r.gitCache == nil {
		return nil, false
	}
	b, ok := r.gitCache[key]
	return b, ok
}

func gitCachePut(r *repo, key string, body []byte) {
	if r.gitCache == nil {
		r.gitCache = map[string][]byte{}
	}
	if len(r.gitCache) > 256 {
		r.gitCache = map[string][]byte{}
	}
	r.gitCache[key] = body
}

func sendGitResp(status int, contentType string, body []byte) *response {
	return gitResp(status, contentType, body)
}

type refLine struct {
	oid    string
	name   string
	peeled string
	hasPeeled bool
}

func refLines(r *repo) []refLine {
	var heads, tags []string
	for name := range r.refs {
		if strings.HasPrefix(name, "refs/heads/") {
			heads = append(heads, name)
		} else if strings.HasPrefix(name, "refs/tags/") {
			tags = append(tags, name)
		}
	}
	sort.Strings(heads)
	sort.Strings(tags)
	var lines []refLine
	for _, name := range heads {
		lines = append(lines, refLine{oid: r.refs[name], name: name})
	}
	for _, name := range tags {
		oid := r.refs[name]
		rl := refLine{oid: oid, name: name}
		if obj := r.objects[oid]; obj != nil && obj.typ == "tag" {
			target, _ := parseTag(obj.data)
			current := target
			for i := 0; i < 8; i++ {
				cur := r.objects[current]
				if cur == nil || cur.typ != "tag" {
					break
				}
				t, _ := parseTag(cur.data)
				current = t
			}
			rl.peeled = current
			rl.hasPeeled = true
		}
		lines = append(lines, rl)
	}
	return lines
}

func handleInfoRefs(rq *request, owner, repoName string) *response {
	service, _ := rq.queryGet("service")
	r, errR := gitRepoOr404(rq, owner, repoName)
	if errR != nil {
		return errR
	}
	v2 := service == "git-upload-pack" && rq.headers["git-protocol"] == "version=2"
	if service == "git-upload-pack" && v2 {
		body := encodePktLines([]any{
			"version 2\n",
			"agent=git-forge/1.0\n",
			"ls-refs=unborn\n",
			"fetch=shallow filter\n",
			"object-format=sha1\n",
			"flush",
		})
		return sendGitResp(200, "application/x-git-upload-pack-advertisement", body)
	}
	if service != "git-upload-pack" && service != "git-receive-pack" {
		return errResp(400, "unknown service")
	}
	isUpload := service == "git-upload-pack"
	var caps string
	if isUpload {
		caps = "side-band-64k symref=HEAD:refs/heads/main thin-pack ofs-delta shallow multi_ack agent=git-forge/1.0"
	} else {
		caps = "report-status side-band-64k delete-refs ofs-delta agent=git-forge/1.0"
	}
	// The v0 advertisement is a pure function of (repo version, service):
	// all 50 bench clones ask for the same bytes.
	advKey := "adv0:" + service + ":" + strconv.FormatInt(r.gitVersion, 10)
	if cached, ok := gitCacheGet(r, advKey); ok {
		return sendGitResp(200, "application/x-"+service+"-advertisement", cached)
	}
	var lines []any
	lines = append(lines, "# service="+service+"\n", "flush")
	refs := refLines(r)
	defaultTip, hasDefault := r.refs["refs/heads/main"]
	if len(refs) == 0 {
		lines = append(lines, zeroOID+" capabilities^{}\x00"+caps+"\n")
	} else {
		headOID := defaultTip
		if !hasDefault {
			headOID = refs[0].oid
		}
		lines = append(lines, headOID+" HEAD\x00"+caps+"\n")
		for _, ref := range refs {
			lines = append(lines, ref.oid+" "+ref.name+"\n")
			if ref.hasPeeled {
				lines = append(lines, ref.peeled+" "+ref.name+"^{}\n")
			}
		}
	}
	lines = append(lines, "flush")
	body := encodePktLines(lines)
	gitCachePut(r, advKey, body)
	return sendGitResp(200, "application/x-"+service+"-advertisement", body)
}

func handleUploadPack(rq *request, owner, repoName string) *response {
	r, errR := gitRepoOr404(rq, owner, repoName)
	if errR != nil {
		return errR
	}
	packets, err := decodePktLines(rq.body)
	if err != nil {
		return errResp(400, "malformed pkt-line stream")
	}
	var dataLines []string
	for _, p := range packets {
		if p.kind == 'd' {
			dataLines = append(dataLines, string(p.payload))
		}
	}
	first := ""
	if len(dataLines) > 0 {
		first = dataLines[0]
	}
	if !strings.HasPrefix(first, "command=") {
		return errResp(400, "expected a command= line")
	}
	command := strings.TrimSpace(first[len("command="):])
	if command == "ls-refs" {
		peel, symrefs, unborn := false, false, false
		var prefixes []string
		pastDelim := false
		for _, packet := range packets {
			if packet.kind == 'l' {
				pastDelim = true
				continue
			}
			if !pastDelim || packet.kind != 'd' {
				continue
			}
			line := strings.TrimSpace(string(packet.payload))
			if line == "peel" {
				peel = true
			} else if line == "symrefs" {
				symrefs = true
			} else if line == "unborn" {
				unborn = true
			} else if strings.HasPrefix(line, "ref-prefix ") {
				prefixes = append(prefixes, line[len("ref-prefix "):])
			}
		}
		var out []string
		// ls-refs is a pure function of (repo version, peel, symrefs, unborn,
		// prefixes): all 50 bench clones ask for the same bytes.
		sortedPrefixes := append([]string{}, prefixes...)
		sort.Strings(sortedPrefixes)
		lsKey := "ls:" + strconv.FormatInt(r.gitVersion, 10) + ":" +
			strconv.FormatBool(peel) + ":" + strconv.FormatBool(symrefs) + ":" +
			strconv.FormatBool(unborn) + ":" + strings.Join(sortedPrefixes, ",")
		if cached, ok := gitCacheGet(r, lsKey); ok {
			return sendGitResp(200, "application/x-git-upload-pack-result", cached)
		}
		keep := func(name string) bool {
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
		defaultTip, hasDefault := r.refs["refs/heads/"+r.defaultBranch]
		if hasDefault {
			if keep("HEAD") {
				s := defaultTip + " HEAD"
				if symrefs {
					s += " symref-target:refs/heads/" + r.defaultBranch
				}
				out = append(out, s+"\n")
			}
			for _, ref := range refLines(r) {
				if !keep(ref.name) {
					continue
				}
				line := ref.oid + " " + ref.name
				if ref.hasPeeled && peel {
					line += " peeled:" + ref.peeled
				}
				out = append(out, line+"\n")
			}
		} else if unborn && keep("HEAD") {
			s := "unborn HEAD"
			if symrefs {
				s += " symref-target:refs/heads/" + r.defaultBranch
			}
			out = append(out, s+"\n")
		}
		var lines []any
		for _, o := range out {
			lines = append(lines, o)
		}
		lines = append(lines, "flush")
		lsBody := encodePktLines(lines)
		gitCachePut(r, lsKey, lsBody)
		return sendGitResp(200, "application/x-git-upload-pack-result", lsBody)
	}
	if command == "fetch" {
		var wants, haves, shallow []string
		var deepen *int
		pastDelim := false
		for _, packet := range packets {
			if packet.kind == 'l' {
				pastDelim = true
				continue
			}
			if !pastDelim || packet.kind != 'd' {
				continue
			}
			line := strings.TrimSpace(string(packet.payload))
			if strings.HasPrefix(line, "want ") {
				wants = append(wants, line[5:])
			} else if strings.HasPrefix(line, "have ") {
				haves = append(haves, line[5:])
			} else if strings.HasPrefix(line, "shallow ") {
				shallow = append(shallow, line[8:])
			} else if strings.HasPrefix(line, "deepen ") {
				d, _ := strconv.Atoi(line[7:])
				deepen = &d
			}
		}
		// The whole fetch response is a pure function of (repo version,
		// wants, haves, shallow, deepen): all 50 bench clones ask for the
		// same bytes (same tip, deepen 1, no haves).
		sortedWants := append([]string{}, wants...)
		sort.Strings(sortedWants)
		sortedHaves := append([]string{}, haves...)
		sort.Strings(sortedHaves)
		sortedShallow := append([]string{}, shallow...)
		sort.Strings(sortedShallow)
		deepenStr := "none"
		if deepen != nil {
			deepenStr = strconv.Itoa(*deepen)
		}
		fetchKey := "fetch:" + strconv.FormatInt(r.gitVersion, 10) + ":" +
			strings.Join(sortedWants, ",") + "|d=" + deepenStr +
			"|h=" + strings.Join(sortedHaves, ",") +
			"|s=" + strings.Join(sortedShallow, ",")
		if cached, ok := gitCacheGet(r, fetchKey); ok {
			return sendGitResp(200, "application/x-git-upload-pack-result", cached)
		}
		for _, want := range wants {
			if _, ok := r.objects[want]; !ok {
				payload := append([]byte{3}, []byte("want "+want+" not found\n")...)
				errBody := encodePktLines([]any{"packfile\n", payload, "flush"})
				gitCachePut(r, fetchKey, errBody)
				return sendGitResp(200, "application/x-git-upload-pack-result", errBody)
			}
		}
		var wantTags, wantCommits []string
		for _, want := range wants {
			obj := r.objects[want]
			if obj.typ == "tag" {
				wantTags = append(wantTags, want)
				target, _ := parseTag(obj.data)
				current := target
				for i := 0; i < 8; i++ {
					cur := r.objects[current]
					if cur == nil || cur.typ != "tag" {
						break
					}
					t, _ := parseTag(cur.data)
					current = t
				}
				wantCommits = append(wantCommits, current)
			} else {
				wantCommits = append(wantCommits, want)
			}
		}
		havesReachable := clientHasCommits(r, haves, shallow)
		var sent map[string]bool
		var newShallow []string
		if deepen != nil {
			gen := map[string]int{}
			type qItem struct {
				oid string
				g   int
			}
			var queue []qItem
			for _, w := range wantCommits {
				if _, ok := r.objects[w]; ok {
					queue = append(queue, qItem{w, 1})
				}
			}
			for len(queue) > 0 {
				item := queue[0]
				queue = queue[1:]
				if _, ok := gen[item.oid]; ok {
					continue
				}
				obj := r.objects[item.oid]
				if obj == nil || obj.typ != "commit" {
					continue
				}
				gen[item.oid] = item.g
				if item.g < *deepen {
					for _, parent := range parseCommit(obj.data).parents {
						if _, ok := gen[parent]; !ok {
							queue = append(queue, qItem{parent, item.g + 1})
						}
					}
				}
			}
			sent = map[string]bool{}
			for oid, g := range gen {
				if g <= *deepen && !havesReachable[oid] {
					sent[oid] = true
				}
			}
			resulting := map[string]bool{}
			for oid := range havesReachable {
				resulting[oid] = true
			}
			for oid := range sent {
				resulting[oid] = true
			}
			for oid := range resulting {
				obj := r.objects[oid]
				if obj == nil || obj.typ != "commit" {
					continue
				}
				for _, p := range parseCommit(obj.data).parents {
					if !resulting[p] {
						newShallow = append(newShallow, oid)
						break
					}
				}
			}
			sort.Strings(newShallow)
		} else {
			sent = ancestorCommits(r, wantCommits)
			for oid := range havesReachable {
				delete(sent, oid)
			}
		}
		resultingSet := map[string]bool{}
		for oid := range havesReachable {
			resultingSet[oid] = true
		}
		for oid := range sent {
			resultingSet[oid] = true
		}
		var unshallow []string
		for _, s := range shallow {
			if !resultingSet[s] {
				continue
			}
			obj := r.objects[s]
			if obj == nil || obj.typ != "commit" {
				continue
			}
			allIn := true
			for _, p := range parseCommit(obj.data).parents {
				if !resultingSet[p] {
					allIn = false
					break
				}
			}
			if !allIn {
				continue
			}
			inNew := false
			for _, ns := range newShallow {
				if ns == s {
					inNew = true
					break
				}
			}
			if !inNew {
				unshallow = append(unshallow, s)
			}
		}
		sort.Strings(unshallow)
		var sentList []string
		for oid := range sent {
			sentList = append(sentList, oid)
		}
		objects := closureObjects(r, sentList)
		var havesList []string
		for oid := range havesReachable {
			havesList = append(havesList, oid)
		}
		haveClosure := closureObjects(r, havesList)
		for oid := range haveClosure {
			delete(objects, oid)
		}
		for _, tagOID := range wantTags {
			if tagObj, ok := r.objects[tagOID]; ok {
				if _, inHave := haveClosure[tagOID]; !inHave {
					objects[tagOID] = tagObj
				}
			}
		}
		var parts []any
		if len(haves) > 0 {
			parts = append(parts, "acknowledgments\n")
			var acked []string
			for _, h := range haves {
				if _, ok := r.objects[h]; ok {
					acked = append(acked, h)
				}
			}
			if len(acked) == 0 {
				parts = append(parts, "NAK\n")
			} else {
				for _, h := range acked {
					parts = append(parts, "ACK "+h+"\n")
				}
				parts = append(parts, "ready\n")
			}
			parts = append(parts, "delim")
		}
		if len(shallow) > 0 || deepen != nil || len(newShallow) > 0 || len(unshallow) > 0 {
			parts = append(parts, "shallow-info\n")
			for _, oid := range newShallow {
				parts = append(parts, "shallow "+oid+"\n")
			}
			for _, oid := range unshallow {
				parts = append(parts, "unshallow "+oid+"\n")
			}
			parts = append(parts, "delim")
		}
		if len(objects) > 0 {
			objList := make([]*gitObject, 0, len(objects))
			for _, obj := range objects {
				objList = append(objList, obj)
			}
			pack := writePack(objList)
			parts = append(parts, "packfile\n")
			for off := 0; off < len(pack); off += 65515 {
				end := off + 65515
				if end > len(pack) {
					end = len(pack)
				}
				chunk := append([]byte{1}, pack[off:end]...)
				parts = append(parts, chunk)
			}
			parts = append(parts, "flush")
		} else {
			parts = append(parts, "flush")
		}
		fetchBody := encodePktLines(parts)
		gitCachePut(r, fetchKey, fetchBody)
		return sendGitResp(200, "application/x-git-upload-pack-result", fetchBody)
	}
	return errResp(400, "unknown command "+command)
}

type recvCommand struct {
	oldOID string
	newOID string
	ref    string
}

type cmdResult struct {
	ref    string
	ok     bool
	reason string
}

func handleReceivePack(rq *request, owner, repoName string) *response {
	r, errR := gitRepoOr404(rq, owner, repoName)
	if errR != nil {
		return errR
	}
	body := rq.body
	offset := 0
	var commandLines []string
	packStart := -1
	parseErr := false
	for offset < len(body) {
		if offset+4 > len(body) {
			parseErr = true
			break
		}
		head := string(body[offset : offset+4])
		if !isHex4(head) {
			parseErr = true
			break
		}
		length, _ := hexToInt(head)
		if length == 0 {
			packStart = offset + 4
			break
		}
		if length == 1 {
			offset += 4
			continue
		}
		if offset+length > len(body) {
			parseErr = true
			break
		}
		commandLines = append(commandLines, string(body[offset+4:offset+length]))
		offset += length
	}
	if parseErr {
		return errResp(400, "malformed pkt-line stream")
	}
	if packStart < 0 {
		return errResp(400, "missing flush after the command list")
	}
	var commands []recvCommand
	for _, raw := range commandLines {
		line := raw
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			line = line[:nul]
		}
		parts := strings.Split(strings.TrimSpace(line), " ")
		if len(parts) < 3 || !isOID(parts[0]) || !isOID(parts[1]) {
			return errResp(400, "malformed command line")
		}
		commands = append(commands, recvCommand{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
	}
	report := func(unpackOK bool, unpackDetail string, results []cmdResult) *response {
		var bandPayload [][]byte
		if unpackOK {
			bandPayload = append(bandPayload, pktLine([]byte("unpack ok\n")))
		} else {
			bandPayload = append(bandPayload, pktLine([]byte("unpack "+unpackDetail+"\n")))
		}
		for _, res := range results {
			if res.ok {
				bandPayload = append(bandPayload, pktLine([]byte("ok "+res.ref+"\n")))
			} else {
				bandPayload = append(bandPayload, pktLine([]byte("ng "+res.ref+" "+res.reason+"\n")))
			}
		}
		bandPayload = append(bandPayload, pktFlush)
		inner := bytes.Join(bandPayload, nil)
		outer := pktLine(append([]byte{1}, inner...))
		full := append(outer, pktFlush...)
		return sendGitResp(200, "application/x-git-receive-pack-result", full)
	}
	packBytes := body[packStart:]
	needsPack := false
	for _, c := range commands {
		if c.newOID != zeroOID {
			needsPack = true
			break
		}
	}
	if needsPack {
		if len(packBytes) == 0 {
			return report(false, "missing pack", nil)
		}
		read, err := readPack(packBytes, func(oid string) *gitObject { return r.objects[oid] })
		if err != nil {
			return report(false, "unpack error: "+err.Error(), nil)
		}
		for _, oid := range read.order {
			obj := read.objects[oid]
			if _, ok := r.objects[oid]; !ok {
				r.objects[oid] = obj
			}
			if obj.typ == "commit" {
				if _, ok := r.commitSeq[oid]; !ok {
					r.commitSeq[oid] = r.nextCommitSeq
					r.nextCommitSeq++
				}
			}
		}
	}
	var results []cmdResult
	type apply struct {
		ref    string
		newOID string
	}
	var toApply []apply
	for _, command := range commands {
		current, hasCurrent := r.refs[command.ref]
		if command.newOID == zeroOID {
			if !hasCurrent {
				results = append(results, cmdResult{ref: command.ref, ok: false, reason: "remote ref does not exist"})
			} else if command.oldOID != zeroOID && command.oldOID != current {
				results = append(results, cmdResult{ref: command.ref, ok: false, reason: "non-fast-forward"})
			} else {
				results = append(results, cmdResult{ref: command.ref, ok: true})
				toApply = append(toApply, apply{ref: command.ref, newOID: zeroOID})
			}
			continue
		}
		newObj := r.objects[command.newOID]
		isTagRef := strings.HasPrefix(command.ref, "refs/tags/")
		if newObj == nil || (isTagRef && newObj.typ != "commit" && newObj.typ != "tag") || (!isTagRef && newObj.typ != "commit") {
			results = append(results, cmdResult{ref: command.ref, ok: false, reason: "object not found"})
			continue
		}
		if command.oldOID == zeroOID {
			if hasCurrent {
				results = append(results, cmdResult{ref: command.ref, ok: false, reason: "already exists"})
			} else {
				results = append(results, cmdResult{ref: command.ref, ok: true})
				toApply = append(toApply, apply{ref: command.ref, newOID: command.newOID})
			}
			continue
		}
		if !hasCurrent {
			results = append(results, cmdResult{ref: command.ref, ok: false, reason: "remote ref does not exist"})
			continue
		}
		if current != command.oldOID {
			results = append(results, cmdResult{ref: command.ref, ok: false, reason: "non-fast-forward"})
			continue
		}
		if !isTagRef && command.newOID != command.oldOID {
			if !isAncestor(r, command.newOID, command.oldOID) {
				results = append(results, cmdResult{ref: command.ref, ok: false, reason: "non-fast-forward"})
				continue
			}
		}
		results = append(results, cmdResult{ref: command.ref, ok: true})
		toApply = append(toApply, apply{ref: command.ref, newOID: command.newOID})
	}
	for _, a := range toApply {
		if a.newOID == zeroOID {
			delete(r.refs, a.ref)
		} else {
			r.refs[a.ref] = a.newOID
		}
	}
	// Any receive-pack may have added objects and/or moved refs: the
	// clone-path caches are versioned off the repo, so bump unconditionally.
	bumpGit(r)
	return report(true, "ok", results)
}

var _ = hex.EncodeToString
var _ = fmt.Sprintf
