package main

import (
	"bytes"
	"sort"
	"strings"
	"sync"
)

// The forge's single in-memory store, shared by REST, GraphQL, MCP and git smart-HTTP.

type GitObject struct {
	Type string
	Data []byte
}
type TreeEntry struct{ Mode, Name, Oid string }
type FlatEntry struct{ Mode, Oid string }

var viewerLogin = "adaiversen"
var viewerName = "Ada Iversen"
var viewerEmail = "adaiversen@users.noreply.github.com"
var viewerNodeID = nodeId("User", 1)

type Label struct {
	ID          int
	Name        string
	Color       string
	Description string
}
type Comment struct {
	ID        int
	Body      string
	CreatedAt string
}
type Issue struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string
	Labels    []*Label
	Comments  []*Comment
	CreatedAt string
	UpdatedAt string
}
type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string
	HeadRef        string
	BaseRef        string
	Merged         bool
	Mergeable      bool
	MergeCommitSHA *string
	MergedAt       *string
	CreatedAt      string
	UpdatedAt      string
}
type Repo struct {
	ID            int
	Name          string
	Private       bool
	Description   string
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string
	Objects       map[string]*GitObject
	Refs          map[string]string
	Head          string
	Issues        []*Issue
	IssueCounter  int
	Labels        map[string]*Label
	Pulls         []*Pull
	PullCounter   int
}

type Store struct {
	mu     sync.RWMutex
	repos  map[string]*Repo
	nextId int
}

func newStore() *Store { return &Store{repos: map[string]*Repo{}, nextId: 1000} }

func (s *Store) id() int { s.nextId++; return s.nextId }

func (s *Store) createRepo(name string, private bool, description string) *Repo {
	r := &Repo{
		ID: s.id(), Name: name, Private: private, Description: description,
		DefaultBranch: "main", CreatedAt: trialNow(), UpdatedAt: trialNow(),
		Objects: map[string]*GitObject{}, Refs: map[string]string{}, Head: "refs/heads/main",
		Issues: []*Issue{}, Labels: map[string]*Label{}, Pulls: []*Pull{},
	}
	s.repos[name] = r
	return r
}
func (s *Store) getRepo(owner, name string) *Repo {
	if owner != viewerLogin {
		return nil
	}
	return s.repos[name]
}
func (s *Store) deleteRepo(name string) bool {
	_, ok := s.repos[name]
	delete(s.repos, name)
	return ok
}

func (s *Store) storeObject(r *Repo, typ string, data []byte) string {
	oid := gitOid(typ, data)
	if _, ok := r.Objects[oid]; !ok {
		r.Objects[oid] = &GitObject{Type: typ, Data: data}
	}
	return oid
}
func (s *Store) getObject(r *Repo, oid string) *GitObject {
	return r.Objects[strings.ToLower(oid)]
}

func parseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	i := 0
	for i < len(data) {
		sp := bytes.IndexByte(data[i:], 0x20)
		if sp < 0 {
			break
		}
		mode := string(data[i : i+sp])
		nul := bytes.IndexByte(data[i+sp+1:], 0x00)
		if nul < 0 {
			break
		}
		nameStart := i + sp + 1
		name := string(data[nameStart : nameStart+nul])
		oidStart := nameStart + nul + 1
		if oidStart+20 > len(data) {
			break
		}
		oid := hexEncode(data[oidStart : oidStart+20])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, Oid: oid})
		i = oidStart + 20
	}
	return entries
}

func hexEncode(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0x0f]
	}
	return string(out)
}

type commitInfo struct {
	Tree    string
	Parents []string
}

func parseCommit(data []byte) commitInfo {
	text := string(data)
	blank := strings.Index(text, "\n\n")
	header := text
	if blank >= 0 {
		header = text[:blank]
	}
	var ci commitInfo
	for _, line := range strings.Split(header, "\n") {
		if strings.HasPrefix(line, "tree ") {
			ci.Tree = strings.TrimSpace(line[5:])
		} else if strings.HasPrefix(line, "parent ") {
			ci.Parents = append(ci.Parents, strings.TrimSpace(line[7:]))
		}
	}
	return ci
}

func (s *Store) commitTree(r *Repo, commitOid string) string {
	obj := s.getObject(r, commitOid)
	if obj == nil || obj.Type != "commit" {
		return ""
	}
	return parseCommit(obj.Data).Tree
}

func (s *Store) flattenTree(r *Repo, treeOid, prefix string) map[string]FlatEntry {
	flat := map[string]FlatEntry{}
	if treeOid == "" {
		return flat
	}
	obj := s.getObject(r, treeOid)
	if obj == nil || obj.Type != "tree" {
		return flat
	}
	for _, e := range parseTree(obj.Data) {
		if e.Mode == "40000" {
			for k, v := range s.flattenTree(r, e.Oid, prefix+e.Name+"/") {
				flat[k] = v
			}
		} else {
			flat[prefix+e.Name] = FlatEntry{Mode: e.Mode, Oid: e.Oid}
		}
	}
	return flat
}

type trieNode struct {
	children map[string]*trieNode
	isDir    bool
	entry    FlatEntry
}

func (s *Store) buildTree(r *Repo, flat map[string]FlatEntry) string {
	root := &trieNode{children: map[string]*trieNode{}, isDir: true}
	for path, entry := range flat {
		parts := strings.Split(path, "/")
		node := root
		for i := 0; i < len(parts)-1; i++ {
			child, ok := node.children[parts[i]]
			if !ok {
				child = &trieNode{children: map[string]*trieNode{}, isDir: true}
				node.children[parts[i]] = child
			}
			node = child
		}
		leaf := &trieNode{children: map[string]*trieNode{}, isDir: false, entry: entry}
		node.children[parts[len(parts)-1]] = leaf
	}
	var serialize func(node *trieNode) string
	serialize = func(node *trieNode) string {
		var entries []TreeEntry
		for name, child := range node.children {
			if child.isDir {
				entries = append(entries, TreeEntry{Mode: "40000", Name: name, Oid: serialize(child)})
			} else {
				mode := child.entry.Mode
				if mode == "" {
					mode = "100644"
				}
				entries = append(entries, TreeEntry{Mode: mode, Name: name, Oid: child.entry.Oid})
			}
		}
		sort.Slice(entries, func(i, j int) bool { return treeLess(entries[i], entries[j]) })
		var buf bytes.Buffer
		for _, e := range entries {
			mode := e.Mode
			if mode == "40000" {
				mode = "40000"
			}
			buf.WriteString(mode + " " + e.Name + "\x00")
			oidBytes, _ := hexDecode(e.Oid)
			buf.Write(oidBytes)
		}
		return s.storeObject(r, "tree", buf.Bytes())
	}
	return serialize(root)
}

func hexDecode(s string) ([]byte, error) {
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		hi := hexVal(s[i*2])
		lo := hexVal(s[i*2+1])
		out[i] = hi<<4 | lo
	}
	return out, nil
}
func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return 0
}

func treeLess(a, b TreeEntry) bool {
	an, bn := a.Name, b.Name
	if a.Mode == "40000" {
		an += "/"
	}
	if b.Mode == "40000" {
		bn += "/"
	}
	return an < bn
}

func (s *Store) makeCommit(r *Repo, tree string, parents []string, message string) string {
	epoch := trialEpoch()
	ident := viewerName + " <" + viewerEmail + "> " + itoa64(epoch) + " +0000"
	msg := message
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + ident + "\ncommitter " + ident + "\n\n" + msg)
	return s.storeObject(r, "commit", []byte(b.String()))
}

func itoa64(n int64) string {
	return strconvFormatInt(n)
}

func (s *Store) resolveRef(r *Repo, ref string) string {
	if ref == "" {
		return ""
	}
	if ref == "HEAD" {
		return s.resolveRef(r, r.Head)
	}
	if strings.HasPrefix(ref, "refs/") {
		return r.Refs[ref]
	}
	if v, ok := r.Refs["refs/heads/"+ref]; ok {
		return v
	}
	if v, ok := r.Refs["refs/tags/"+ref]; ok {
		return v
	}
	if isSha40(ref) {
		lower := strings.ToLower(ref)
		if _, ok := r.Objects[lower]; ok {
			return lower
		}
	}
	return ""
}
func (s *Store) branchTip(r *Repo, branch string) string { return r.Refs["refs/heads/"+branch] }

// isAncestor reports whether old is reachable from new, stopping as soon as it is found.
func (s *Store) isAncestor(r *Repo, new, old string) bool {
	if new == old {
		return true
	}
	seen := map[string]bool{}
	queue := []string{new}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == "" || seen[cur] {
			continue
		}
		seen[cur] = true
		if cur == old {
			return true
		}
		obj := s.getObject(r, cur)
		if obj != nil && obj.Type == "commit" {
			queue = append(queue, parseCommit(obj.Data).Parents...)
		}
	}
	return false
}

func (s *Store) ancestors(r *Repo, oid string) map[string]bool {
	seen := map[string]bool{}
	queue := []string{oid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == "" || seen[cur] {
			continue
		}
		seen[cur] = true
		obj := s.getObject(r, cur)
		if obj != nil && obj.Type == "commit" {
			queue = append(queue, parseCommit(obj.Data).Parents...)
		}
	}
	return seen
}
func (s *Store) mergeBase(r *Repo, a, b string) string {
	if a == b {
		return a
	}
	ancB := s.ancestors(r, b)
	queue := []string{a}
	seen := map[string]bool{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == "" || seen[cur] {
			continue
		}
		seen[cur] = true
		if ancB[cur] {
			return cur
		}
		obj := s.getObject(r, cur)
		if obj != nil && obj.Type == "commit" {
			queue = append(queue, parseCommit(obj.Data).Parents...)
		}
	}
	return ""
}

type diffChange struct {
	Path    string
	Status  string
	BaseOid string
	HeadOid string
}

func (s *Store) diffTrees(r *Repo, baseTree, headTree string) []diffChange {
	b := s.flattenTree(r, baseTree, "")
	h := s.flattenTree(r, headTree, "")
	var out []diffChange
	for path, he := range h {
		be, ok := b[path]
		if !ok {
			out = append(out, diffChange{Path: path, Status: "added", HeadOid: he.Oid})
		} else if be.Oid != he.Oid {
			out = append(out, diffChange{Path: path, Status: "modified", BaseOid: be.Oid, HeadOid: he.Oid})
		}
	}
	for path, be := range b {
		if _, ok := h[path]; !ok {
			out = append(out, diffChange{Path: path, Status: "removed", BaseOid: be.Oid})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

type mergeResult struct {
	Conflict bool
	Sha      string
}

func (s *Store) squashMerge(r *Repo, p *Pull) mergeResult {
	headTip := s.resolveRef(r, p.HeadRef)
	baseTip := s.resolveRef(r, p.BaseRef)
	if headTip == "" || baseTip == "" {
		return mergeResult{Conflict: true}
	}
	mb := s.mergeBase(r, headTip, baseTip)
	if mb == "" {
		mb = baseTip
	}
	mbTree := s.commitTree(r, mb)
	baseTree := s.commitTree(r, baseTip)
	headTree := s.commitTree(r, headTip)
	changes := s.diffTrees(r, mbTree, headTree)
	baseFlat := s.flattenTree(r, baseTree, "")
	mbFlat := s.flattenTree(r, mbTree, "")
	headFlat := s.flattenTree(r, headTree, "")
	for _, c := range changes {
		if baseFlat[c.Path].Oid != mbFlat[c.Path].Oid {
			return mergeResult{Conflict: true}
		}
	}
	result := map[string]FlatEntry{}
	for k, v := range baseFlat {
		result[k] = v
	}
	for _, c := range changes {
		if he, ok := headFlat[c.Path]; ok {
			result[c.Path] = he
		} else {
			delete(result, c.Path)
		}
	}
	tree := s.buildTree(r, result)
	sha := s.makeCommit(r, tree, []string{baseTip}, p.Title+" (#"+itoa(p.Number)+")")
	r.Refs["refs/heads/"+p.BaseRef] = sha
	p.Merged = true
	p.State = "closed"
	p.Mergeable = false
	p.MergeCommitSHA = &sha
	now := trialNow()
	p.MergedAt = &now
	p.UpdatedAt = now
	return mergeResult{Conflict: false, Sha: sha}
}

type contentsResult struct {
	CommitSha string
	BlobSha   string
}

func (s *Store) commitContents(r *Repo, branch, path string, content []byte, hasContent bool, message string) contentsResult {
	tip := s.branchTip(r, branch)
	var flat map[string]FlatEntry
	if tip != "" {
		flat = s.flattenTree(r, s.commitTree(r, tip), "")
	} else {
		flat = map[string]FlatEntry{}
	}
	blobSha := ""
	if !hasContent {
		delete(flat, path)
	} else {
		blobSha = s.storeObject(r, "blob", content)
		flat[path] = FlatEntry{Mode: "100644", Oid: blobSha}
	}
	tree := s.buildTree(r, flat)
	var parents []string
	if tip != "" {
		parents = []string{tip}
	}
	commitSha := s.makeCommit(r, tree, parents, message)
	r.Refs["refs/heads/"+branch] = commitSha
	r.UpdatedAt = trialNow()
	return contentsResult{CommitSha: commitSha, BlobSha: blobSha}
}

func itoa(n int) string { return strconvFormatInt(int64(n)) }
