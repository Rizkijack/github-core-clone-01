package main

import (
	"encoding/base64"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------- parser ----------
type gqlValue struct {
	Kind   string
	Name   string
	Value  any
	Items  []*gqlValue
	Fields map[string]*gqlValue
}
type gqlField struct {
	Alias string
	Name  string
	Args  map[string]*gqlValue
	Sel   *selectionSet
}
type gqlFragment struct {
	TypeCondition string
	Sel           *selectionSet
}
type selectionSet struct {
	Fields    []*gqlField
	Fragments []*gqlFragment
}
type gqlOperation struct {
	Type string
	Name string
	Sel  *selectionSet
}

var wsRe = regexp.MustCompile(`[\s,]`)
var nameStartRe = regexp.MustCompile(`[A-Za-z_]`)
var nameRe = regexp.MustCompile(`[A-Za-z0-9_]`)
var numStartRe = regexp.MustCompile(`[0-9-]`)
var numRe = regexp.MustCompile(`[0-9.eE+\-]`)

func tokenize(src string) []string {
	var tokens []string
	i := 0
	for i < len(src) {
		c := src[i]
		if c == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if wsRe.MatchString(string(c)) {
			i++
			continue
		}
		if c == '"' {
			j := i + 1
			out := "\""
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					out += string(src[j]) + string(src[j+1])
					j += 2
				} else {
					out += string(src[j])
					j++
				}
			}
			out += "\""
			tokens = append(tokens, out)
			i = j + 1
			continue
		}
		if nameStartRe.MatchString(string(c)) {
			j := i
			for j < len(src) && nameRe.MatchString(string(src[j])) {
				j++
			}
			tokens = append(tokens, src[i:j])
			i = j
			continue
		}
		if numStartRe.MatchString(string(c)) {
			j := i
			for j < len(src) && numRe.MatchString(string(src[j])) {
				j++
			}
			tokens = append(tokens, src[i:j])
			i = j
			continue
		}
		if strings.HasPrefix(src[i:], "...") {
			tokens = append(tokens, "...")
			i += 3
			continue
		}
		tokens = append(tokens, string(c))
		i++
	}
	return tokens
}

type gqlParser struct {
	t []string
	p int
}

func (p *gqlParser) peek() string {
	if p.p < len(p.t) {
		return p.t[p.p]
	}
	return ""
}
func (p *gqlParser) next() string {
	if p.p < len(p.t) {
		v := p.t[p.p]
		p.p++
		return v
	}
	return ""
}
func (p *gqlParser) expect(x string) {
	if p.next() != x {
		panic("expected " + x)
	}
}
func (p *gqlParser) parseDocument() []*gqlOperation {
	var ops []*gqlOperation
	for p.p < len(p.t) {
		ops = append(ops, p.parseOperation())
	}
	return ops
}
func (p *gqlParser) parseOperation() *gqlOperation {
	typ := "query"
	name := ""
	kw := p.peek()
	if kw == "query" || kw == "mutation" || kw == "subscription" {
		typ = p.next()
		pk := p.peek()
		if pk != "" && pk != "(" && nameStartRe.MatchString(string(pk[0])) {
			name = p.next()
		}
	}
	if p.peek() == "(" {
		p.skipVarDefs()
	}
	sel := p.parseSelectionSet()
	return &gqlOperation{Type: typ, Name: name, Sel: sel}
}
func (p *gqlParser) skipVarDefs() {
	p.expect("(")
	depth := 1
	for depth > 0 && p.p < len(p.t) {
		x := p.next()
		if x == "(" {
			depth++
		} else if x == ")" {
			depth--
		}
	}
}
func (p *gqlParser) parseSelectionSet() *selectionSet {
	p.expect("{")
	ss := &selectionSet{}
	for p.peek() != "}" && p.peek() != "" {
		if p.peek() == "..." {
			p.next()
			if p.peek() == "on" {
				p.next()
				tc := p.next()
				ss.Fragments = append(ss.Fragments, &gqlFragment{TypeCondition: tc, Sel: p.parseSelectionSet()})
			} else {
				p.next()
			}
			continue
		}
		ss.Fields = append(ss.Fields, p.parseField())
	}
	p.expect("}")
	return ss
}
func (p *gqlParser) parseField() *gqlField {
	name := p.next()
	alias := ""
	if p.peek() == ":" {
		p.next()
		alias = name
		name = p.next()
	}
	f := &gqlField{Alias: alias, Name: name, Args: map[string]*gqlValue{}}
	if p.peek() == "(" {
		f.Args = p.parseArgs()
	}
	if p.peek() == "{" {
		f.Sel = p.parseSelectionSet()
	}
	return f
}
func (p *gqlParser) parseArgs() map[string]*gqlValue {
	p.expect("(")
	args := map[string]*gqlValue{}
	for p.peek() != ")" && p.peek() != "" {
		name := p.next()
		p.expect(":")
		args[name] = p.parseValue()
	}
	p.expect(")")
	return args
}
func (p *gqlParser) parseValue() *gqlValue {
	tk := p.peek()
	if tk == "$" {
		p.next()
		return &gqlValue{Kind: "var", Name: p.next()}
	}
	if tk == "[" {
		p.next()
		v := &gqlValue{Kind: "list"}
		for p.peek() != "]" && p.peek() != "" {
			v.Items = append(v.Items, p.parseValue())
		}
		p.expect("]")
		return v
	}
	if tk == "{" {
		p.next()
		v := &gqlValue{Kind: "object", Fields: map[string]*gqlValue{}}
		for p.peek() != "}" && p.peek() != "" {
			n := p.next()
			p.expect(":")
			v.Fields[n] = p.parseValue()
		}
		p.expect("}")
		return v
	}
	if len(tk) > 0 && tk[0] == '"' {
		p.next()
		var s any
		if err := jsonUnmarshal([]byte(tk), &s); err == nil {
			return &gqlValue{Kind: "string", Value: s}
		}
		return &gqlValue{Kind: "string", Value: strings.Trim(tk, "\"")}
	}
	if len(tk) > 0 && (tk[0] == '-' || (tk[0] >= '0' && tk[0] <= '9')) {
		p.next()
		n, _ := strconv.ParseFloat(tk, 64)
		return &gqlValue{Kind: "int", Value: n}
	}
	if tk == "true" || tk == "false" {
		p.next()
		return &gqlValue{Kind: "bool", Value: tk == "true"}
	}
	if tk == "null" {
		p.next()
		return &gqlValue{Kind: "null"}
	}
	p.next()
	return &gqlValue{Kind: "enum", Value: tk}
}

func evalValue(v *gqlValue, vars map[string]any) any {
	switch v.Kind {
	case "var":
		if x, ok := vars[v.Name]; ok {
			return x
		}
		return nil
	case "list":
		out := make([]any, 0, len(v.Items))
		for _, x := range v.Items {
			out = append(out, evalValue(x, vars))
		}
		return out
	case "object":
		o := map[string]any{}
		for k, x := range v.Fields {
			o[k] = evalValue(x, vars)
		}
		return o
	case "null":
		return nil
	default:
		return v.Value
	}
}
func evalArgs(args map[string]*gqlValue, vars map[string]any) map[string]any {
	o := map[string]any{}
	for k, v := range args {
		o[k] = evalValue(v, vars)
	}
	return o
}

// ---------- schema / introspection ----------
var schemaFields = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body", "labels", "comments", "createdAt", "updatedAt"},
	"PullRequest": {"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files", "title", "body"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
	"User":        {"login", "id", "name"},
}
var schemaEnums = map[string][]string{
	"SearchType":        {"ISSUE", "REPOSITORY", "USER", "DISCUSSION"},
	"IssueState":        {"OPEN", "CLOSED"},
	"PullRequestState":  {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":    {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"OrderDirection":    {"ASC", "DESC"},
	"IssueOrderField":   {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
}

func introspect(name string) any {
	if f, ok := schemaFields[name]; ok {
		return &gqlObj{Type: "__Type", Data: map[string]any{"name": name, "fields": f, "enumValues": nil}}
	}
	if e, ok := schemaEnums[name]; ok {
		return &gqlObj{Type: "__Type", Data: map[string]any{"name": name, "fields": nil, "enumValues": e}}
	}
	return nil
}

type gqlObj struct {
	Type string
	Data any
}
type pullCtx struct {
	r *Repo
	p *Pull
}

func repoByNodeId(s *Store, id string) *Repo {
	d := decodeNodeId(id)
	if d == nil || d.Type != "Repository" {
		return nil
	}
	for _, r := range s.repos {
		if r.ID == d.DatabaseID {
			return r
		}
	}
	return nil
}
func issueByNodeId(s *Store, id string) *Issue {
	d := decodeNodeId(id)
	if d == nil || d.Type != "Issue" {
		return nil
	}
	for _, r := range s.repos {
		for _, i := range r.Issues {
			if i.ID == d.DatabaseID {
				return i
			}
		}
	}
	return nil
}
func pullByNodeId(s *Store, id string) *pullCtx {
	d := decodeNodeId(id)
	if d == nil || d.Type != "PullRequest" {
		return nil
	}
	for _, r := range s.repos {
		for _, p := range r.Pulls {
			if p.ID == d.DatabaseID {
				return &pullCtx{r, p}
			}
		}
	}
	return nil
}

func encodeCursor(idx int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + itoa(idx)))
}
func decodeCursor(c string) int {
	raw, err := base64.StdEncoding.DecodeString(c)
	if err != nil {
		return -1
	}
	s := string(raw)
	if strings.HasPrefix(s, "cursor:") {
		n, err := strconv.Atoi(s[7:])
		if err == nil {
			return n
		}
	}
	return -1
}

func issuesConnection(r *Repo, args map[string]any) any {
	list := append([]*Issue{}, r.Issues...)
	if states, ok := args["states"].([]any); ok && len(states) > 0 {
		filtered := []*Issue{}
		for _, i := range list {
			for _, st := range states {
				if ss, ok := st.(string); ok && strings.ToUpper(i.State) == ss {
					filtered = append(filtered, i)
					break
				}
			}
		}
		list = filtered
	}
	dir := -1
	if ob, ok := args["orderBy"].(map[string]any); ok {
		if ob["direction"] == "ASC" {
			dir = 1
		}
	}
	sort.SliceStable(list, func(a, b int) bool { return dir*(list[a].Number-list[b].Number) < 0 })
	first := 100
	if f, ok := args["first"].(float64); ok {
		first = int(f)
	}
	if first < 0 {
		first = 0
	}
	afterIdx := -1
	if a, ok := args["after"].(string); ok {
		afterIdx = decodeCursor(a)
	}
	start := afterIdx + 1
	if start > len(list) {
		start = len(list)
	}
	end := start + first
	if end > len(list) {
		end = len(list)
	}
	page := list[start:end]
	hasNextPage := end < len(list)
	var endCursor any
	if len(page) > 0 {
		endCursor = encodeCursor(end - 1)
	}
	nodes := make([]any, 0, len(page))
	for _, i := range page {
		nodes = append(nodes, &gqlObj{Type: "Issue", Data: i})
	}
	return &gqlObj{Type: "IssueConnection", Data: map[string]any{
		"nodes":    nodes,
		"pageInfo": &gqlObj{Type: "PageInfo", Data: map[string]any{"hasNextPage": hasNextPage, "endCursor": endCursor}},
	}}
}

type resolverFn func(s *Store, data any, args map[string]any) any

var resolvers = map[string]map[string]resolverFn{
	"Query": {
		"viewer": func(s *Store, d any, a map[string]any) any {
			return &gqlObj{Type: "User", Data: map[string]any{"login": viewerLogin}}
		},
		"repository": func(s *Store, d any, a map[string]any) any {
			r := s.getRepo(getStr(a, "owner"), getStr(a, "name"))
			if r == nil {
				return nil
			}
			return &gqlObj{Type: "Repository", Data: r}
		},
		"node": func(s *Store, d any, a map[string]any) any {
			id := getStr(a, "id")
			dd := decodeNodeId(id)
			if dd == nil {
				return nil
			}
			if dd.Type == "Repository" {
				r := repoByNodeId(s, id)
				if r != nil {
					return &gqlObj{Type: "Repository", Data: r}
				}
			}
			if dd.Type == "Issue" {
				if i := issueByNodeId(s, id); i != nil {
					return &gqlObj{Type: "Issue", Data: i}
				}
			}
			if dd.Type == "PullRequest" {
				if f := pullByNodeId(s, id); f != nil {
					return &gqlObj{Type: "PullRequest", Data: f}
				}
			}
			return nil
		},
		"__type": func(s *Store, d any, a map[string]any) any { return introspect(getStr(a, "name")) },
	},
	"Mutation": {
		"createIssue": func(s *Store, d any, a map[string]any) any {
			input := asMap(a["input"])
			r := repoByNodeId(s, getStr(input, "repositoryId"))
			if r == nil {
				return nil
			}
			r.IssueCounter++
			i := &Issue{ID: s.id(), Number: r.IssueCounter, Title: getStr(input, "title"), Body: getStr(input, "body"), State: "open", Labels: []*Label{}, Comments: []*Comment{}, CreatedAt: trialNow(), UpdatedAt: trialNow()}
			r.Issues = append(r.Issues, i)
			return &gqlObj{Type: "CreateIssuePayload", Data: map[string]any{"issue": &gqlObj{Type: "Issue", Data: i}}}
		},
		"closeIssue": func(s *Store, d any, a map[string]any) any {
			input := asMap(a["input"])
			i := issueByNodeId(s, getStr(input, "issueId"))
			if i == nil {
				return nil
			}
			i.State = "closed"
			i.UpdatedAt = trialNow()
			return &gqlObj{Type: "CloseIssuePayload", Data: map[string]any{"issue": &gqlObj{Type: "Issue", Data: i}}}
		},
		"createPullRequest": func(s *Store, d any, a map[string]any) any {
			input := asMap(a["input"])
			r := repoByNodeId(s, getStr(input, "repositoryId"))
			if r == nil {
				return nil
			}
			head, base := getStr(input, "headRefName"), getStr(input, "baseRefName")
			if s.resolveRef(r, head) == "" || s.resolveRef(r, base) == "" {
				return nil
			}
			r.PullCounter++
			p := &Pull{ID: s.id(), Number: r.PullCounter, Title: getStr(input, "title"), Body: getStr(input, "body"), State: "open", HeadRef: head, BaseRef: base, Mergeable: true, CreatedAt: trialNow(), UpdatedAt: trialNow()}
			r.Pulls = append(r.Pulls, p)
			return &gqlObj{Type: "CreatePullRequestPayload", Data: map[string]any{"pullRequest": &gqlObj{Type: "PullRequest", Data: &pullCtx{r, p}}}}
		},
		"mergePullRequest": func(s *Store, d any, a map[string]any) any {
			input := asMap(a["input"])
			f := pullByNodeId(s, getStr(input, "pullRequestId"))
			if f == nil {
				return nil
			}
			s.squashMerge(f.r, f.p)
			return &gqlObj{Type: "MergePullRequestPayload", Data: map[string]any{"pullRequest": &gqlObj{Type: "PullRequest", Data: f}}}
		},
		"addComment": func(s *Store, d any, a map[string]any) any {
			input := asMap(a["input"])
			i := issueByNodeId(s, getStr(input, "subjectId"))
			if i == nil {
				return nil
			}
			c := &Comment{ID: s.id(), Body: getStr(input, "body"), CreatedAt: trialNow()}
			i.Comments = append(i.Comments, c)
			return &gqlObj{Type: "AddCommentPayload", Data: map[string]any{"commentEdge": &gqlObj{Type: "IssueCommentEdge", Data: map[string]any{"node": &gqlObj{Type: "IssueComment", Data: c}}}}}
		},
	},
	"Repository": {
		"id":               func(s *Store, d any, a map[string]any) any { return nodeId("Repository", d.(*Repo).ID) },
		"databaseId":       func(s *Store, d any, a map[string]any) any { return d.(*Repo).ID },
		"name":             func(s *Store, d any, a map[string]any) any { return d.(*Repo).Name },
		"nameWithOwner":    func(s *Store, d any, a map[string]any) any { return viewerLogin + "/" + d.(*Repo).Name },
		"owner":            func(s *Store, d any, a map[string]any) any { return &gqlObj{Type: "User", Data: map[string]any{"login": viewerLogin}} },
		"defaultBranchRef": func(s *Store, d any, a map[string]any) any { r := d.(*Repo); if s.branchTip(r, r.DefaultBranch) == "" { return nil }; return &gqlObj{Type: "Ref", Data: map[string]any{"name": r.DefaultBranch}} },
		"viewerPermission": func(s *Store, d any, a map[string]any) any { return "ADMIN" },
		"hasIssuesEnabled": func(s *Store, d any, a map[string]any) any { return true },
		"issue":            func(s *Store, d any, a map[string]any) any { r := d.(*Repo); n := getInt(a, "number", 0); i := findIssue(r, n); if i == nil { return nil }; return &gqlObj{Type: "Issue", Data: i} },
		"issues":           func(s *Store, d any, a map[string]any) any { return issuesConnection(d.(*Repo), a) },
		"pullRequests": func(s *Store, d any, a map[string]any) any {
			r := d.(*Repo)
			list := append([]*Pull{}, r.Pulls...)
			sortPulls(list)
			first := getInt(a, "first", 100)
			if first > len(list) {
				first = len(list)
			}
			nodes := []any{}
			for _, p := range list[:first] {
				nodes = append(nodes, &gqlObj{Type: "PullRequest", Data: &pullCtx{r, p}})
			}
			return &gqlObj{Type: "PullRequestConnection", Data: map[string]any{"nodes": nodes}}
		},
		"pullRequest": func(s *Store, d any, a map[string]any) any { r := d.(*Repo); p := findPull(r, getInt(a, "number", 0)); if p == nil { return nil }; return &gqlObj{Type: "PullRequest", Data: &pullCtx{r, p}} },
	},
	"Ref":  {"name": func(s *Store, d any, a map[string]any) any { return asMap(d)["name"] }},
	"User": {
		"login": func(s *Store, d any, a map[string]any) any { return asMap(d)["login"] },
		"id":    func(s *Store, d any, a map[string]any) any { return 1 },
		"name":  func(s *Store, d any, a map[string]any) any { return viewerName },
	},
	"Issue": {
		"id":     func(s *Store, d any, a map[string]any) any { return nodeId("Issue", d.(*Issue).ID) },
		"number": func(s *Store, d any, a map[string]any) any { return d.(*Issue).Number },
		"title":  func(s *Store, d any, a map[string]any) any { return d.(*Issue).Title },
		"state":  func(s *Store, d any, a map[string]any) any { return strings.ToUpper(d.(*Issue).State) },
		"body":   func(s *Store, d any, a map[string]any) any { return d.(*Issue).Body },
	},
	"PullRequest": {
		"id":          func(s *Store, d any, a map[string]any) any { return nodeId("PullRequest", d.(*pullCtx).p.ID) },
		"number":      func(s *Store, d any, a map[string]any) any { return d.(*pullCtx).p.Number },
		"state":       func(s *Store, d any, a map[string]any) any { p := d.(*pullCtx).p; if p.Merged { return "MERGED" }; return strings.ToUpper(p.State) },
		"merged":      func(s *Store, d any, a map[string]any) any { return d.(*pullCtx).p.Merged },
		"headRefName": func(s *Store, d any, a map[string]any) any { return d.(*pullCtx).p.HeadRef },
		"baseRefName": func(s *Store, d any, a map[string]any) any { return d.(*pullCtx).p.BaseRef },
		"mergeable":   func(s *Store, d any, a map[string]any) any { p := d.(*pullCtx).p; if p.Merged { return "UNKNOWN" }; if p.Mergeable { return "MERGEABLE" }; return "CONFLICTING" },
		"files": func(s *Store, d any, a map[string]any) any {
			f := d.(*pullCtx)
			headTip := s.resolveRef(f.r, f.p.HeadRef)
			baseTip := s.resolveRef(f.r, f.p.BaseRef)
			var changes []diffChange
			if headTip != "" && baseTip != "" {
				changes = s.diffTrees(f.r, s.commitTree(f.r, baseTip), s.commitTree(f.r, headTip))
			}
			first := getInt(a, "first", 100)
			if first > len(changes) {
				first = len(changes)
			}
			nodes := []any{}
			for _, c := range changes[:first] {
				nodes = append(nodes, &gqlObj{Type: "PullRequestChangedFile", Data: map[string]any{"path": c.Path}})
			}
			return &gqlObj{Type: "PullRequestChangedFileConnection", Data: map[string]any{"nodes": nodes}}
		},
	},
	"IssueConnection": {
		"nodes":    func(s *Store, d any, a map[string]any) any { return asMap(d)["nodes"] },
		"pageInfo": func(s *Store, d any, a map[string]any) any { return asMap(d)["pageInfo"] },
	},
	"PullRequestConnection":            {"nodes": func(s *Store, d any, a map[string]any) any { return asMap(d)["nodes"] }},
	"PullRequestChangedFileConnection": {"nodes": func(s *Store, d any, a map[string]any) any { return asMap(d)["nodes"] }},
	"PullRequestChangedFile":           {"path": func(s *Store, d any, a map[string]any) any { return asMap(d)["path"] }},
	"PageInfo": {
		"hasNextPage": func(s *Store, d any, a map[string]any) any { return asMap(d)["hasNextPage"] },
		"endCursor":   func(s *Store, d any, a map[string]any) any { return asMap(d)["endCursor"] },
	},
	"CreateIssuePayload":       {"issue": func(s *Store, d any, a map[string]any) any { return asMap(d)["issue"] }},
	"CloseIssuePayload":        {"issue": func(s *Store, d any, a map[string]any) any { return asMap(d)["issue"] }},
	"CreatePullRequestPayload": {"pullRequest": func(s *Store, d any, a map[string]any) any { return asMap(d)["pullRequest"] }},
	"MergePullRequestPayload":  {"pullRequest": func(s *Store, d any, a map[string]any) any { return asMap(d)["pullRequest"] }},
	"AddCommentPayload":        {"commentEdge": func(s *Store, d any, a map[string]any) any { return asMap(d)["commentEdge"] }},
	"IssueCommentEdge":         {"node": func(s *Store, d any, a map[string]any) any { return asMap(d)["node"] }},
	"IssueComment": {
		"id":   func(s *Store, d any, a map[string]any) any { return nodeId("IssueComment", d.(*Comment).ID) },
		"body": func(s *Store, d any, a map[string]any) any { return d.(*Comment).Body },
	},
	"__Type": {
		"name": func(s *Store, d any, a map[string]any) any { return asMap(d)["name"] },
		"fields": func(s *Store, d any, a map[string]any) any {
			f := asMap(d)["fields"]
			if f == nil {
				return nil
			}
			out := []any{}
			for _, n := range f.([]string) {
				out = append(out, &gqlObj{Type: "__Field", Data: map[string]any{"name": n}})
			}
			return out
		},
		"enumValues": func(s *Store, d any, a map[string]any) any {
			e := asMap(d)["enumValues"]
			if e == nil {
				return nil
			}
			out := []any{}
			for _, n := range e.([]string) {
				out = append(out, &gqlObj{Type: "__EnumValue", Data: map[string]any{"name": n}})
			}
			return out
		},
	},
	"__Field":     {"name": func(s *Store, d any, a map[string]any) any { return asMap(d)["name"] }},
	"__EnumValue": {"name": func(s *Store, d any, a map[string]any) any { return asMap(d)["name"] }},
}

func projectValue(s *Store, value any, sel *selectionSet, vars map[string]any, errors *[]any) any {
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case []any:
		out := make([]any, 0, len(v))
		for _, x := range v {
			out = append(out, projectValue(s, x, sel, vars, errors))
		}
		return out
	case *gqlObj:
		if sel == nil {
			return v.Data
		}
		result := map[string]any{}
		var apply func(ss *selectionSet)
		apply = func(ss *selectionSet) {
			for _, field := range ss.Fields {
				key := field.Name
				if field.Alias != "" {
					key = field.Alias
				}
				typeResolvers := resolvers[v.Type]
				var resolver resolverFn
				if typeResolvers != nil {
					resolver = typeResolvers[field.Name]
				}
				if resolver == nil {
					*errors = append(*errors, map[string]any{"message": "Cannot query field \"" + field.Name + "\" on type \"" + v.Type + "\"."})
					result[key] = nil
					continue
				}
				args := evalArgs(field.Args, vars)
				rv := resolver(s, v.Data, args)
				if field.Sel != nil {
					result[key] = projectValue(s, rv, field.Sel, vars, errors)
				} else {
					if obj, ok := rv.(*gqlObj); ok {
						result[key] = obj.Data
					} else {
						result[key] = rv
					}
				}
			}
			for _, frag := range ss.Fragments {
				if frag.TypeCondition == v.Type || frag.TypeCondition == "Node" {
					apply(frag.Sel)
				}
			}
		}
		apply(sel)
		return result
	default:
		return value
	}
}

func executeOperation(s *Store, op *gqlOperation, vars map[string]any) any {
	errors := []any{}
	rootType := "Query"
	if op.Type == "mutation" {
		rootType = "Mutation"
	}
	data := projectValue(s, &gqlObj{Type: rootType, Data: nil}, op.Sel, vars, &errors)
	if len(errors) > 0 {
		return map[string]any{"data": data, "errors": errors}
	}
	return map[string]any{"data": data}
}

func handleGraphQL(s *Store, ctx *Ctx, w http.ResponseWriter) {
	if !ctx.Authed {
		unauth(w)
		return
	}
	v, ok := parseJson(ctx.RawBody)
	if !ok {
		badReq(w, "Invalid JSON body")
		return
	}
	run := func(entry any) (result any) {
		defer func() {
			if rec := recover(); rec != nil {
				result = map[string]any{"data": nil, "errors": []any{map[string]any{"message": toString(rec)}}}
			}
		}()
		em := asMap(entry)
		query := getStr(em, "query")
		ops := (&gqlParser{t: tokenize(query)}).parseDocument()
		var op *gqlOperation
		if opName := getStr(em, "operationName"); opName != "" {
			for _, o := range ops {
				if o.Name == opName {
					op = o
					break
				}
			}
		}
		if op == nil && len(ops) > 0 {
			op = ops[0]
		}
		if op == nil {
			return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "No operation"}}}
		}
		vars := asMap(em["variables"])
		return executeOperation(s, op, vars)
	}
	if arr, ok := v.([]any); ok {
		out := make([]any, 0, len(arr))
		for _, e := range arr {
			out = append(out, run(e))
		}
		jsonReply(w, 200, out, nil)
		return
	}
	jsonReply(w, 200, run(v), nil)
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return "error"
}
