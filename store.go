// Store: deterministic RNG (scenario seed), viewer derivation, repos,
// tree/commit helpers, merges, global ids — a faithful port of main.ts.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const u64Mask = 0xffffffffffffffff

const scenarioSeed = 0x6a09e667f3bcc909

func fnv1a64(text string) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		hash ^= uint64(text[i])
		hash = hash * 0x100000001b3
	}
	return hash
}

func mix64(value uint64) uint64 {
	z := value + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func scenarioSeedFor(label string) uint64 {
	return mix64(scenarioSeed ^ fnv1a64(label))
}

type rng struct {
	state uint64
}

func newRng(seed uint64) *rng {
	if seed == 0 {
		seed = scenarioSeed
	}
	return &rng{state: seed}
}

func (r *rng) nextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545f4914f6cdd1d
}

func (r *rng) nextU32() uint32 {
	return uint32(r.nextU64() >> 32)
}

func (r *rng) nextInt(maxExclusive int) int {
	if maxExclusive == 1 {
		return 0
	}
	const range32 = uint64(1) << 32
	rem := uint32(range32 % uint64(maxExclusive))
	limit := -rem // 2^32 - rem (0 when rem == 0)
	for {
		v := r.nextU32()
		if v < limit {
			return int(v % uint32(maxExclusive))
		}
	}
}

func (r *rng) pick(items []string) string {
	return items[r.nextInt(len(items))]
}

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}
var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

func deriveViewerLogin() string {
	r := newRng(scenarioSeedFor("users"))
	used := map[string]bool{}
	first := ""
	for i := 0; i < 12; i++ {
		f := r.pick(firstNames)
		l := r.pick(lastNames)
		base := f + l
		login := base
		suffix := 2
		for used[login] {
			login = fmt.Sprintf("%s-%d", base, suffix)
			suffix++
		}
		used[login] = true
		if i == 0 {
			first = login
		}
	}
	return first
}

const viewerID = 1000
const viewerName = "ada iversen"

var viewerLogin = deriveViewerLogin() // 'adaiversen' under the fixed seed
var viewerEmail = viewerLogin + "@example.invalid"

// Deterministic clock.
const fixedEpochSec = 1735689600 // 2025-01-01T00:00:00Z
const fixedISO = "2025-01-01T00:00:00Z"

var commitCounter int64

func nextCommitTimestamp() int64 {
	commitCounter++
	return fixedEpochSec + commitCounter
}

// --- Store types ---

type label struct {
	name  string
	color string
}

type comment struct {
	id   int
	body string
}

type issue struct {
	id       int
	number   int
	title    string
	body     string
	state    string // "open" | "closed"
	labels   []*label
	comments []*comment
	seq      int
}

type pull struct {
	id             int
	number         int
	title          string
	body           string
	state          string
	head           string
	base           string
	headSha        string
	baseSha        string
	merged         bool
	mergedAt       string
	mergeCommitSha string
	mergeBase      string // "" means null
	hasMergeBase   bool
	seq            int
}

type repo struct {
	id            int
	owner         string
	name          string
	description   string
	private       bool
	defaultBranch string
	refs          map[string]string
	refOrder      []string // insertion order (not used for output; refs are sorted)
	objects       map[string]*gitObject
	commitSeq     map[string]int
	nextCommitSeq int
	issues        map[int]*issue
	pulls         map[int]*pull
	labels        map[string]*label
	labelOrder    []string
	nextNumber    int
	nextCommentID int
	createdSeq    int
	// gitVersion bumps on every refs/objects mutation; gitCache holds
	// whole clone-path response bodies keyed by (version + request params).
	// The cache is only read/written under reqMu, so no extra locking.
	gitVersion int64
	gitCache   map[string][]byte
}

var repos = map[string]*repo{}
var nextRepoID = 1
var nextGlobalID = 1
var createdSeqCounter = 0

func repoKey(owner, name string) string { return owner + "/" + name }

func getRepo(owner, name string) *repo { return repos[repoKey(owner, name)] }

func createRepoRecord(owner, name, description string, isPrivate bool) *repo {
	createdSeqCounter++
	r := &repo{
		id:            nextRepoID,
		owner:         owner,
		name:          name,
		description:   description,
		private:       isPrivate,
		defaultBranch: "main",
		refs:          map[string]string{},
		objects:       map[string]*gitObject{},
		commitSeq:     map[string]int{},
		nextCommitSeq: 1,
		issues:        map[int]*issue{},
		pulls:         map[int]*pull{},
		labels:        map[string]*label{},
		nextNumber:    1,
		nextCommentID: 1,
		createdSeq:    createdSeqCounter,
	}
	nextRepoID++
	repos[repoKey(owner, name)] = r
	return r
}

func viewerRepos() []*repo {
	var out []*repo
	for _, r := range repos {
		if r.owner == viewerLogin {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].createdSeq < out[j].createdSeq })
	return out
}

func bumpGit(r *repo) {
	r.gitVersion++
	r.gitCache = nil
}

func storeObject(r *repo, typ string, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := r.objects[oid]; !ok {
		r.objects[oid] = &gitObject{typ: typ, data: data}
		bumpGit(r)
	}
	return oid
}

func storeCommit(r *repo, data []byte) string {
	oid := storeObject(r, "commit", data)
	if _, ok := r.commitSeq[oid]; !ok {
		r.commitSeq[oid] = r.nextCommitSeq
		r.nextCommitSeq++
	}
	return oid
}

// ancestorCommits: all ancestors of tips (inclusive).
func ancestorCommits(r *repo, tips []string) map[string]bool {
	seen := map[string]bool{}
	var stack []string
	for _, tip := range tips {
		if isOID(tip) && !seen[tip] {
			stack = append(stack, tip)
		}
	}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		obj := r.objects[oid]
		if obj == nil || obj.typ != "commit" {
			continue
		}
		seen[oid] = true
		for _, parent := range parseCommit(obj.data).parents {
			if !seen[parent] {
				stack = append(stack, parent)
			}
		}
	}
	return seen
}

func isAncestor(r *repo, descendant, ancestor string) bool {
	if descendant == ancestor {
		return true
	}
	seen := map[string]bool{}
	stack := []string{descendant}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == ancestor {
			return true
		}
		if seen[oid] {
			continue
		}
		obj := r.objects[oid]
		if obj == nil || obj.typ != "commit" {
			continue
		}
		seen[oid] = true
		for _, parent := range parseCommit(obj.data).parents {
			if !seen[parent] {
				stack = append(stack, parent)
			}
		}
	}
	return false
}

func mergeBase(r *repo, a, b string) (string, bool) {
	aAnc := ancestorCommits(r, []string{a})
	bAnc := ancestorCommits(r, []string{b})
	best := ""
	bestSeq := -1
	found := false
	for oid := range aAnc {
		if !bAnc[oid] {
			continue
		}
		seq := r.commitSeq[oid]
		if seq > bestSeq {
			bestSeq = seq
			best = oid
			found = true
		}
	}
	return best, found
}

// closureObjects: commits, their trees (recursive), blobs, and peeled tags.
func closureObjects(r *repo, commitOids []string) map[string]*gitObject {
	out := map[string]*gitObject{}
	queue := append([]string{}, commitOids...)
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		obj := r.objects[oid]
		if obj == nil {
			continue
		}
		if _, ok := out[oid]; ok {
			continue
		}
		out[oid] = obj
		switch obj.typ {
		case "commit":
			queue = append(queue, parseCommit(obj.data).tree)
		case "tree":
			for _, e := range parseTree(obj.data) {
				queue = append(queue, e.oid)
			}
		case "tag":
			target, _ := parseTag(obj.data)
			queue = append(queue, target)
		}
	}
	return out
}

func clientHasCommits(r *repo, haves []string, shallow []string) map[string]bool {
	boundaries := map[string]bool{}
	for _, s := range shallow {
		boundaries[s] = true
	}
	seen := map[string]bool{}
	stack := append([]string{}, haves...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		obj := r.objects[oid]
		if obj == nil || obj.typ != "commit" {
			continue
		}
		seen[oid] = true
		if boundaries[oid] {
			continue
		}
		for _, parent := range parseCommit(obj.data).parents {
			if !seen[parent] {
				stack = append(stack, parent)
			}
		}
	}
	return seen
}

func flattenTree(r *repo, treeOID string, hasTree bool, prefix string) map[string]string {
	out := map[string]string{}
	if !hasTree {
		return out
	}
	obj := r.objects[treeOID]
	if obj == nil || obj.typ != "tree" {
		return out
	}
	for _, e := range parseTree(obj.data) {
		path := e.name
		if prefix != "" {
			path = prefix + "/" + e.name
		}
		if e.mode == "40000" {
			for p, oid := range flattenTree(r, e.oid, true, path) {
				out[p] = oid
			}
		} else {
			out[path] = e.oid
		}
	}
	return out
}

func buildTreeFromFlat(r *repo, flat map[string]string) string {
	var build func(paths [][2]string) string
	build = func(paths [][2]string) string {
		dirs := map[string][][2]string{}
		var dirOrder []string
		var files []treeEntry
		for _, kv := range paths {
			path, oid := kv[0], kv[1]
			slash := strings.IndexByte(path, '/')
			if slash < 0 {
				files = append(files, treeEntry{mode: "100644", name: path, oid: oid})
			} else {
				dir := path[:slash]
				rest := path[slash+1:]
				if _, ok := dirs[dir]; !ok {
					dirOrder = append(dirOrder, dir)
				}
				dirs[dir] = append(dirs[dir], [2]string{rest, oid})
			}
		}
		for _, dir := range dirOrder {
			files = append(files, treeEntry{mode: "40000", name: dir, oid: build(dirs[dir])})
		}
		return storeObject(r, "tree", buildTree(files))
	}
	paths := make([][2]string, 0, len(flat))
	for p, oid := range flat {
		paths = append(paths, [2]string{p, oid})
	}
	// TS iterates flat.entries() (insertion order from flattenTree); buildTree
	// sorts, so input order is unobservable. Sort paths for determinism.
	sort.Slice(paths, func(i, j int) bool { return paths[i][0] < paths[j][0] })
	return build(paths)
}

func commitTree(r *repo, commitOID string, hasCommit bool) (string, bool) {
	if !hasCommit {
		return "", false
	}
	obj := r.objects[commitOID]
	if obj == nil || obj.typ != "commit" {
		return "", false
	}
	return parseCommit(obj.data).tree, true
}

func commitFileChange(r *repo, branch, path string, content []byte, hasContent bool, message string) string {
	tip, hasTip := r.refs["refs/heads/"+branch]
	baseTree, hasBaseTree := commitTree(r, tip, hasTip)
	flat := flattenTree(r, baseTree, hasBaseTree, "")
	if !hasContent {
		delete(flat, path)
	} else {
		flat[path] = storeObject(r, "blob", content)
	}
	newTree := buildTreeFromFlat(r, flat)
	timestamp := nextCommitTimestamp()
	author := viewerName + " <" + viewerEmail + ">"
	var parents []string
	if hasTip {
		parents = []string{tip}
	}
	data := buildCommit(newTree, parents, message, timestamp, author)
	oid := storeCommit(r, data)
	r.refs["refs/heads/"+branch] = oid
	bumpGit(r)
	return oid
}

func threeWayMerge(r *repo, baseOID string, hasBase bool, headOID string, hasHead bool, mergeBaseOID string, hasMB bool) (map[string]string, bool) {
	baseTree, hasBaseTree := commitTree(r, baseOID, hasBase)
	headTree, hasHeadTree := commitTree(r, headOID, hasHead)
	mbTree, hasMBTree := commitTree(r, mergeBaseOID, hasMB)
	baseFlat := flattenTree(r, baseTree, hasBaseTree, "")
	headFlat := flattenTree(r, headTree, hasHeadTree, "")
	mbFlat := flattenTree(r, mbTree, hasMBTree, "")
	merged := map[string]string{}
	for k, v := range baseFlat {
		merged[k] = v
	}
	paths := map[string]bool{}
	for p := range mbFlat {
		paths[p] = true
	}
	for p := range headFlat {
		paths[p] = true
	}
	for path := range paths {
		mb, hasMB := mbFlat[path]
		head, hasHead := headFlat[path]
		base, hasBase := baseFlat[path]
		// TS: head !== mb / base !== mb, where a missing entry is undefined.
		headChanged := !sameOpt(head, hasHead, mb, hasMB)
		baseChanged := !sameOpt(base, hasBase, mb, hasMB)
		if !headChanged {
			continue
		}
		if !baseChanged {
			if !hasHead {
				delete(merged, path)
			} else {
				merged[path] = head
			}
		} else if !sameOpt(head, hasHead, base, hasBase) {
			return nil, false
		}
	}
	return merged, true
}

func sameOpt(a string, hasA bool, b string, hasB bool) bool {
	if hasA != hasB {
		return false
	}
	return a == b
}

func isMergeable(r *repo, p *pull) bool {
	if !p.hasMergeBase {
		return true
	}
	headTip, _ := r.refs["refs/heads/"+p.head]
	if headTip == "" {
		headTip = p.headSha
	}
	baseTip, _ := r.refs["refs/heads/"+p.base]
	if baseTip == "" {
		baseTip = p.baseSha
	}
	_, ok := threeWayMerge(r, baseTip, true, headTip, true, p.mergeBase, true)
	return ok
}

func globalID(kind string, repoID int, number int, hasNumber bool) string {
	raw := kind + ":" + strconv.Itoa(repoID)
	if hasNumber {
		raw += ":" + strconv.Itoa(number)
	}
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

type parsedGlobalID struct {
	kind   string
	repoID int
	number int
	hasNum bool
}

// parseGlobalId mirrors the TS version, including its lenient base64 decode
// and JS Number() semantics ("" -> 0).
func parseGlobalID(id string) (*parsedGlobalID, bool) {
	raw, ok := lenientB64Decode(id)
	if !ok {
		return nil, false
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return nil, false
	}
	repoIDf := jsParseNumber(parts[1])
	if repoIDf != float64(int64(repoIDf)) || repoIDf > 9.007199254740992e15 || repoIDf < -9.007199254740992e15 {
		return nil, false
	}
	out := &parsedGlobalID{kind: parts[0], repoID: int(repoIDf)}
	if len(parts) >= 3 {
		nf := jsParseNumber(parts[2])
		if nf == float64(int64(nf)) && nf <= 9.007199254740992e15 && nf >= -9.007199254740992e15 {
			out.number = int(nf)
			out.hasNum = true
		} else {
			out.hasNum = false
		}
	}
	return out, true
}

// lenientB64Decode mirrors Node's Buffer.from(s, 'base64'): ignores
// non-alphabet chars, tolerates missing padding.
func lenientB64Decode(s string) ([]byte, bool) {
	var clean []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' {
			clean = append(clean, c)
		}
	}
	// Node drops a trailing partial quantum when length % 4 == 1.
	if len(clean)%4 == 1 {
		clean = clean[:len(clean)-1]
	}
	for len(clean)%4 != 0 {
		clean = append(clean, '=')
	}
	out, err := base64.StdEncoding.DecodeString(string(clean))
	if err != nil {
		return nil, false
	}
	return out, true
}

func repoByID(id int) *repo {
	for _, r := range repos {
		if r.id == id {
			return r
		}
	}
	return nil
}

// identDate formats a commit ident timestamp like JS new Date(ts*1000).toISOString().
func identDate(ts int64) string {
	return time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

func hexDecode(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

var _ = bytes.MinRead
