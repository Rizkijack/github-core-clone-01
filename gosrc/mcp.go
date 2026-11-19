package main

import (
	"net/http"
	"strings"
)

var serverInfo = map[string]any{"name": "forge-mcp", "version": "1.0.0"}
var supportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type toolSchema struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
	Required   []string       `json:"required"`
}
type toolDef struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	InputSchema toolSchema `json:"inputSchema"`
}

func strProp() map[string]any { return map[string]any{"type": "string"} }

var tools = []toolDef{
	{Name: "create_repo", Description: "Create a new repository owned by the authenticated user.", InputSchema: toolSchema{Type: "object", Properties: map[string]any{"name": strProp(), "description": strProp(), "private": map[string]any{"type": "boolean"}, "owner": strProp()}, Required: []string{"name"}}},
	{Name: "get_repo", Description: "Get a repository by owner and name.", InputSchema: toolSchema{Type: "object", Properties: map[string]any{"owner": strProp(), "repo": strProp()}, Required: []string{"owner", "repo"}}},
	{Name: "create_issue", Description: "Create an issue in a repository.", InputSchema: toolSchema{Type: "object", Properties: map[string]any{"owner": strProp(), "repo": strProp(), "title": strProp(), "body": strProp()}, Required: []string{"owner", "repo", "title"}}},
	{Name: "list_issues", Description: "List issues in a repository, optionally filtered by state.", InputSchema: toolSchema{Type: "object", Properties: map[string]any{"owner": strProp(), "repo": strProp(), "state": strProp()}, Required: []string{"owner", "repo"}}},
	{Name: "create_pull_request", Description: "Open a pull request from a head branch into a base branch.", InputSchema: toolSchema{Type: "object", Properties: map[string]any{"owner": strProp(), "repo": strProp(), "title": strProp(), "body": strProp(), "head": strProp(), "base": strProp()}, Required: []string{"owner", "repo", "title", "head", "base"}}},
	{Name: "merge_pull_request", Description: "Squash-merge a pull request by number.", InputSchema: toolSchema{Type: "object", Properties: map[string]any{"owner": strProp(), "repo": strProp(), "pull_number": map[string]any{"type": "integer"}}, Required: []string{"owner", "repo", "pull_number"}}},
}

func normTool(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		}
	}
	return b.String()
}
func resolveTool(name string) *toolDef {
	n := normTool(name)
	for i := range tools {
		if normTool(tools[i].Name) == n {
			return &tools[i]
		}
	}
	for i := range tools {
		tn := normTool(tools[i].Name)
		if len(tn) < len(n) && strings.HasSuffix(n, tn) {
			return &tools[i]
		}
	}
	return nil
}

func rpcResult(w http.ResponseWriter, id any, result any) {
	jsonReply(w, 200, map[string]any{"jsonrpc": "2.0", "id": id, "result": result}, nil)
}
func rpcError(w http.ResponseWriter, id any, code int, message string, status int) {
	jsonReply(w, status, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}, nil)
}
func toolText(w http.ResponseWriter, id any, obj any, isError bool) {
	text, _ := jsonMarshal(obj)
	rpcResult(w, id, map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(text)}},
		"structuredContent": obj,
		"isError":           isError,
	})
}

type toolOut struct {
	obj     any
	isError bool
	message string
}

func callTool(s *Store, tool *toolDef, args map[string]any) toolOut {
	for _, req := range tool.InputSchema.Required {
		v, ok := args[req]
		if !ok || v == nil || v == "" {
			return toolOut{isError: true, message: "missing required argument: " + req}
		}
	}
	owner := getStr(args, "owner")
	if owner == "" {
		owner = viewerLogin
	}
	repoName := getStr(args, "repo")
	if repoName == "" {
		repoName = getStr(args, "name")
	}
	repo := s.getRepo(owner, repoName)
	switch tool.Name {
	case "create_repo":
		name := getStr(args, "name")
		if r, ok := s.repos[name]; ok {
			return toolOut{obj: repoJson(r)}
		}
		r := s.createRepo(name, getBool(args, "private", true), getStr(args, "description"))
		return toolOut{obj: repoJson(r)}
	case "get_repo":
		if repo == nil {
			return toolOut{isError: true, message: "repository not found"}
		}
		return toolOut{obj: repoJson(repo)}
	case "create_issue":
		if repo == nil {
			return toolOut{isError: true, message: "repository not found"}
		}
		repo.IssueCounter++
		i := &Issue{ID: s.id(), Number: repo.IssueCounter, Title: getStr(args, "title"), Body: getStr(args, "body"), State: "open", Labels: []*Label{}, Comments: []*Comment{}, CreatedAt: trialNow(), UpdatedAt: trialNow()}
		repo.Issues = append(repo.Issues, i)
		return toolOut{obj: issueJson(i)}
	case "list_issues":
		if repo == nil {
			return toolOut{isError: true, message: "repository not found"}
		}
		state := getStr(args, "state")
		if state == "" {
			state = "open"
		}
		items := []any{}
		for _, i := range repo.Issues {
			if state == "all" || i.State == state {
				items = append(items, issueJson(i))
			}
		}
		return toolOut{obj: items}
	case "create_pull_request":
		if repo == nil {
			return toolOut{isError: true, message: "repository not found"}
		}
		head, base := getStr(args, "head"), getStr(args, "base")
		if s.resolveRef(repo, head) == "" || s.resolveRef(repo, base) == "" {
			return toolOut{isError: true, message: "head or base ref does not exist"}
		}
		repo.PullCounter++
		p := &Pull{ID: s.id(), Number: repo.PullCounter, Title: getStr(args, "title"), Body: getStr(args, "body"), State: "open", HeadRef: head, BaseRef: base, Mergeable: true, CreatedAt: trialNow(), UpdatedAt: trialNow()}
		repo.Pulls = append(repo.Pulls, p)
		return toolOut{obj: pullJson(s, repo, p)}
	case "merge_pull_request":
		if repo == nil {
			return toolOut{isError: true, message: "repository not found"}
		}
		p := findPull(repo, getInt(args, "pull_number", 0))
		if p == nil {
			return toolOut{isError: true, message: "pull request not found"}
		}
		result := s.squashMerge(repo, p)
		if result.Conflict {
			return toolOut{isError: true, message: "pull request is not mergeable"}
		}
		return toolOut{obj: pullJson(s, repo, p)}
	default:
		return toolOut{isError: true, message: "unknown tool"}
	}
}

func handleMcp(s *Store, ctx *Ctx, w http.ResponseWriter) {
	if ctx.Method == "DELETE" {
		w.Header().Set("X-GitHub-Request-Id", "mcp")
		w.WriteHeader(200)
		return
	}
	if !ctx.Authed {
		unauth(w)
		return
	}
	var msg any
	if err := jsonUnmarshal(ctx.RawBody, &msg); err != nil {
		rpcError(w, nil, -32700, "Parse error", 400)
		return
	}
	mm := asMap(msg)
	id := mm["id"]
	method := getStr(mm, "method")
	params := asMap(mm["params"])
	if method == "notifications/initialized" || (id == nil && strings.HasPrefix(method, "notifications/")) {
		w.Header().Set("X-GitHub-Request-Id", "mcp")
		w.WriteHeader(202)
		return
	}
	if method == "server/discover" {
		rpcResult(w, id, map[string]any{"supportedVersions": supportedVersions, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": serverInfo})
		return
	}
	if method == "initialize" {
		pv := getStr(params, "protocolVersion")
		if pv == "" {
			pv = supportedVersions[0]
		}
		rpcResult(w, id, map[string]any{"protocolVersion": pv, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": serverInfo})
		return
	}
	if method == "tools/list" {
		rpcResult(w, id, map[string]any{"tools": tools})
		return
	}
	if method == "tools/call" {
		tool := resolveTool(getStr(params, "name"))
		if tool == nil {
			rpcError(w, id, -32601, "Unknown tool: "+getStr(params, "name"), 200)
			return
		}
		out := callTool(s, tool, asMap(params["arguments"]))
		if out.isError {
			msg := out.message
			if msg == "" {
				msg = "tool error"
			}
			toolText(w, id, map[string]any{"message": msg}, true)
			return
		}
		toolText(w, id, out.obj, false)
		return
	}
	rpcError(w, id, -32601, "Method not found: "+method, 200)
}
