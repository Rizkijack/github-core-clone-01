package main

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
)

func commitMessage(data []byte) string {
	text := string(data)
	i := strings.Index(text, "\n\n")
	msg := ""
	if i >= 0 {
		msg = text[i+2:]
	}
	return strings.TrimSuffix(msg, "\n")
}

type pathFound struct {
	IsDir   bool
	Oid     string
	TreeOid string
}

func lookupPath(s *Store, r *Repo, commitOid, path string) *pathFound {
	if commitOid == "" {
		return nil
	}
	treeOid := s.commitTree(r, commitOid)
	clean := strings.Trim(path, "/")
	if clean == "" {
		if treeOid != "" {
			return &pathFound{IsDir: true, TreeOid: treeOid}
		}
		return nil
	}
	parts := strings.Split(clean, "/")
	for i, part := range parts {
		obj := s.getObject(r, treeOid)
		if obj == nil || obj.Type != "tree" {
			return nil
		}
		var entry *TreeEntry
		for _, e := range parseTree(obj.Data) {
			if e.Name == part {
				ee := e
				entry = &ee
				break
			}
		}
		if entry == nil {
			return nil
		}
		if i == len(parts)-1 {
			if entry.Mode == "40000" {
				return &pathFound{IsDir: true, TreeOid: entry.Oid}
			}
			return &pathFound{IsDir: false, Oid: entry.Oid}
		}
		if entry.Mode != "40000" {
			return nil
		}
		treeOid = entry.Oid
	}
	return nil
}

func queryInt(ctx *Ctx, key string, def int) int {
	v := ctx.Query.Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n == 0 {
		return def
	}
	return n
}

func listRepos(s *Store, ctx *Ctx, w http.ResponseWriter) {
	perPage := queryInt(ctx, "per_page", 30)
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	page := queryInt(ctx, "page", 1)
	if page < 1 {
		page = 1
	}
	all := make([]*Repo, 0, len(s.repos))
	for _, r := range s.repos {
		all = append(all, r)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	start := (page - 1) * perPage
	end := start + perPage
	if start > len(all) {
		start = len(all)
	}
	if end > len(all) {
		end = len(all)
	}
	items := make([]any, 0, end-start)
	for _, r := range all[start:end] {
		items = append(items, repoJson(r))
	}
	extra := map[string]string{}
	if end < len(all) {
		next := "http://" + ctx.Host + "/api/v3/user/repos?per_page=" + itoa(perPage) + "&page=" + itoa(page+1)
		extra["Link"] = "<" + next + ">; rel=\"next\""
	}
	jsonReply(w, 200, items, extra)
}

func handleRest(s *Store, ctx *Ctx, w http.ResponseWriter) {
	if !ctx.Authed {
		unauth(w)
		return
	}
	p := strings.TrimPrefix(ctx.Pathname, "/api/v3")
	p = strings.Trim(p, "/")
	var seg []string
	if p != "" {
		seg = strings.Split(p, "/")
	}
	m := ctx.Method

	if p == "" && m == "GET" {
		jsonReply(w, 200, map[string]any{
			"current_user_url": "http://" + ctx.Host + "/api/v3/user",
			"rate_limit_url":   "http://" + ctx.Host + "/api/v3/rate_limit",
		}, map[string]string{"X-OAuth-Scopes": "repo, read:org"})
		return
	}
	if p == "meta" && m == "GET" {
		jsonReply(w, 200, map[string]any{"installed_version": "1.0.0", "verifiable_password_authentication": false}, nil)
		return
	}
	if p == "user" && m == "GET" {
		jsonReply(w, 200, map[string]any{"login": viewerLogin, "id": 1, "node_id": viewerNodeID, "type": "User"}, nil)
		return
	}
	if p == "user/repos" {
		if m == "GET" {
			listRepos(s, ctx, w)
			return
		}
		if m == "POST" {
			v, ok := parseJson(ctx.RawBody)
			if !ok {
				badReq(w, "Invalid request body")
				return
			}
			body := asMap(v)
			name := getStr(body, "name")
			if name == "" {
				jsonReply(w, 422, map[string]any{"message": "Repository name is required"}, nil)
				return
			}
			if _, exists := s.repos[name]; exists {
				jsonReply(w, 422, map[string]any{"message": "Repository creation failed: name already exists on this account"}, nil)
				return
			}
			repo := s.createRepo(name, getBool(body, "private", true), getStr(body, "description"))
			if getBool(body, "auto_init", false) {
				s.commitContents(repo, "main", "README.md", []byte("# "+name+"\n"), true, "Initial commit")
			}
			jsonReply(w, 201, repoJson(repo), nil)
			return
		}
	}

	if len(seg) >= 3 && seg[0] == "repos" {
		owner, _ := urlDecode(seg[1])
		name, _ := urlDecode(seg[2])
		repo := s.getRepo(owner, name)
		if repo == nil {
			notFound(w)
			return
		}
		handleRepo(s, ctx, w, repo, seg[3:])
		return
	}
	notFound(w)
}

func handleRepo(s *Store, ctx *Ctx, w http.ResponseWriter, repo *Repo, rest []string) {
	m := ctx.Method
	r0 := ""
	if len(rest) > 0 {
		r0 = rest[0]
	}

	if len(rest) == 0 {
		if m == "GET" {
			jsonReply(w, 200, repoJson(repo), nil)
			return
		}
		if m == "PATCH" {
			v, ok := parseJson(ctx.RawBody)
			if !ok {
				badReq(w, "Invalid request body")
				return
			}
			body := asMap(v)
			if d, ok := body["description"].(string); ok {
				repo.Description = d
			}
			if pr, ok := body["private"].(bool); ok {
				repo.Private = pr
			}
			if db, ok := body["default_branch"].(string); ok {
				repo.DefaultBranch = db
			}
			repo.UpdatedAt = trialNow()
			jsonReply(w, 200, repoJson(repo), nil)
			return
		}
		if m == "DELETE" {
			s.deleteRepo(repo.Name)
			w.Header().Set("X-GitHub-Request-Id", "del")
			w.WriteHeader(204)
			return
		}
	}

	if r0 == "contents" {
		pathParts := make([]string, 0, len(rest)-1)
		for _, x := range rest[1:] {
			d, _ := urlDecode(x)
			pathParts = append(pathParts, d)
		}
		path := strings.Join(pathParts, "/")
		ref := ctx.Query.Get("ref")
		if ref == "" {
			ref = ctx.Query.Get("branch")
		}
		if ref == "" {
			ref = repo.DefaultBranch
		}
		tip := s.resolveRef(repo, ref)
		if m == "GET" {
			found := lookupPath(s, repo, tip, path)
			if found == nil {
				notFound(w)
				return
			}
			if !found.IsDir {
				obj := s.getObject(repo, found.Oid)
				name := path
				if i := strings.LastIndex(path, "/"); i >= 0 {
					name = path[i+1:]
				}
				jsonReply(w, 200, map[string]any{
					"type": "file", "encoding": "base64", "content": b64encode(obj.Data),
					"sha": found.Oid, "size": len(obj.Data), "name": name, "path": path,
				}, nil)
				return
			}
			obj := s.getObject(repo, found.TreeOid)
			entries := []any{}
			for _, e := range parseTree(obj.Data) {
				t := "file"
				if e.Mode == "40000" {
					t = "dir"
				}
				ep := e.Name
				if path != "" {
					ep = path + "/" + e.Name
				}
				entries = append(entries, map[string]any{"type": t, "name": e.Name, "path": ep, "sha": e.Oid})
			}
			jsonReply(w, 200, entries, nil)
			return
		}
		if m == "PUT" {
			v, ok := parseJson(ctx.RawBody)
			if !ok {
				badReq(w, "Invalid request body")
				return
			}
			body := asMap(v)
			branch := getStr(body, "branch")
			if branch == "" {
				branch = repo.DefaultBranch
			}
			content := b64decode(getStr(body, "content"))
			cur := lookupPath(s, repo, s.branchTip(repo, branch), path)
			existingOid := ""
			if cur != nil && !cur.IsDir {
				existingOid = cur.Oid
			}
			if existingOid != "" {
				sha := strings.ToLower(getStr(body, "sha"))
				if sha == "" || sha != existingOid {
					jsonReply(w, 409, map[string]any{"message": "The file has been modified since the provided sha (stale sha)"}, nil)
					return
				}
			}
			msg := getStr(body, "message")
			if msg == "" {
				msg = "Update " + path
			}
			res := s.commitContents(repo, branch, path, content, true, msg)
			status := 201
			if existingOid != "" {
				status = 200
			}
			name := path
			if i := strings.LastIndex(path, "/"); i >= 0 {
				name = path[i+1:]
			}
			jsonReply(w, status, map[string]any{
				"content": map[string]any{"sha": res.BlobSha, "encoding": "base64", "name": name, "path": path},
				"commit":  map[string]any{"sha": res.CommitSha},
			}, nil)
			return
		}
		if m == "DELETE" {
			v, ok := parseJson(ctx.RawBody)
			if !ok {
				badReq(w, "Invalid request body")
				return
			}
			body := asMap(v)
			branch := getStr(body, "branch")
			if branch == "" {
				branch = repo.DefaultBranch
			}
			cur := lookupPath(s, repo, s.branchTip(repo, branch), path)
			if cur == nil || cur.IsDir {
				notFound(w)
				return
			}
			if sha := getStr(body, "sha"); sha != "" && strings.ToLower(sha) != cur.Oid {
				jsonReply(w, 409, map[string]any{"message": "stale sha"}, nil)
				return
			}
			msg := getStr(body, "message")
			if msg == "" {
				msg = "Delete " + path
			}
			res := s.commitContents(repo, branch, path, nil, false, msg)
			jsonReply(w, 200, map[string]any{"commit": map[string]any{"sha": res.CommitSha}}, nil)
			return
		}
	}

	if r0 == "branches" {
		if len(rest) == 1 && m == "GET" {
			items := []any{}
			var names []string
			for k := range repo.Refs {
				if strings.HasPrefix(k, "refs/heads/") {
					names = append(names, k)
				}
			}
			sort.Strings(names)
			for _, k := range names {
				items = append(items, map[string]any{"name": strings.TrimPrefix(k, "refs/heads/"), "commit": map[string]any{"sha": repo.Refs[k]}})
			}
			jsonReply(w, 200, items, nil)
			return
		}
		if len(rest) == 2 && m == "GET" {
			sha := repo.Refs["refs/heads/"+rest[1]]
			if sha == "" {
				notFound(w)
				return
			}
			jsonReply(w, 200, map[string]any{"name": rest[1], "commit": map[string]any{"sha": sha}}, nil)
			return
		}
	}

	if r0 == "commits" && m == "GET" {
		if len(rest) == 1 {
			ref := ctx.Query.Get("sha")
			if ref == "" {
				ref = repo.DefaultBranch
			}
			tip := s.resolveRef(repo, ref)
			if tip == "" {
				jsonReply(w, 200, []any{}, nil)
				return
			}
			var order []string
			seen := map[string]bool{}
			queue := []string{tip}
			for len(queue) > 0 {
				cur := queue[0]
				queue = queue[1:]
				if seen[cur] {
					continue
				}
				seen[cur] = true
				obj := s.getObject(repo, cur)
				if obj != nil && obj.Type == "commit" {
					order = append(order, cur)
					queue = append(queue, parseCommit(obj.Data).Parents...)
				}
			}
			perPage := queryInt(ctx, "per_page", 30)
			if perPage > 100 {
				perPage = 100
			}
			if perPage > len(order) {
				perPage = len(order)
			}
			items := []any{}
			for _, sha := range order[:perPage] {
				obj := s.getObject(repo, sha)
				items = append(items, map[string]any{"sha": sha, "commit": map[string]any{"message": commitMessage(obj.Data)}, "node_id": nodeId("Commit", 0)})
			}
			jsonReply(w, 200, items, nil)
			return
		}
		if len(rest) == 2 {
			obj := s.getObject(repo, rest[1])
			if obj == nil || obj.Type != "commit" {
				notFound(w)
				return
			}
			jsonReply(w, 200, map[string]any{"sha": rest[1], "commit": map[string]any{"message": commitMessage(obj.Data)}}, nil)
			return
		}
	}

	if r0 == "git" && len(rest) >= 2 && rest[1] == "refs" {
		if len(rest) == 2 && m == "POST" {
			v, ok := parseJson(ctx.RawBody)
			if !ok {
				badReq(w, "Invalid request body")
				return
			}
			body := asMap(v)
			ref := getStr(body, "ref")
			sha := getStr(body, "sha")
			if ref == "" || !isSha40(sha) {
				jsonReply(w, 422, map[string]any{"message": "ref and sha are required"}, nil)
				return
			}
			lower := strings.ToLower(sha)
			if s.getObject(repo, lower) == nil {
				jsonReply(w, 422, map[string]any{"message": "Object does not exist"}, nil)
				return
			}
			if _, exists := repo.Refs[ref]; exists {
				jsonReply(w, 422, map[string]any{"message": "Reference already exists"}, nil)
				return
			}
			repo.Refs[ref] = lower
			jsonReply(w, 201, map[string]any{"ref": ref, "object": map[string]any{"sha": lower, "type": "commit"}}, nil)
			return
		}
		if len(rest) >= 3 && m == "GET" {
			ref := "refs/" + strings.Join(rest[2:], "/")
			sha := repo.Refs[ref]
			if sha == "" {
				notFound(w)
				return
			}
			jsonReply(w, 200, map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": "commit"}}, nil)
			return
		}
		if len(rest) == 2 && m == "GET" {
			items := []any{}
			var refs []string
			for ref := range repo.Refs {
				refs = append(refs, ref)
			}
			sort.Strings(refs)
			for _, ref := range refs {
				items = append(items, map[string]any{"ref": ref, "object": map[string]any{"sha": repo.Refs[ref]}})
			}
			jsonReply(w, 200, items, nil)
			return
		}
	}

	if r0 == "labels" {
		if len(rest) == 1 && m == "POST" {
			v, ok := parseJson(ctx.RawBody)
			if !ok {
				badReq(w, "Invalid request body")
				return
			}
			body := asMap(v)
			name := getStr(body, "name")
			if name == "" {
				jsonReply(w, 422, map[string]any{"message": "Label name is required"}, nil)
				return
			}
			if _, exists := repo.Labels[name]; exists {
				jsonReply(w, 422, map[string]any{"message": "Label already exists"}, nil)
				return
			}
			color := getStr(body, "color")
			if color == "" {
				color = "ededed"
			}
			label := &Label{ID: s.id(), Name: name, Color: color, Description: getStr(body, "description")}
			repo.Labels[name] = label
			jsonReply(w, 201, labelJson(label), nil)
			return
		}
		if len(rest) == 1 && m == "GET" {
			items := []any{}
			var names []string
			for n := range repo.Labels {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				items = append(items, labelJson(repo.Labels[n]))
			}
			jsonReply(w, 200, items, nil)
			return
		}
	}

	if r0 == "issues" {
		if len(rest) == 1 {
			if m == "POST" {
				v, ok := parseJson(ctx.RawBody)
				if !ok {
					badReq(w, "Invalid request body")
					return
				}
				body := asMap(v)
				title := getStr(body, "title")
				if title == "" {
					jsonReply(w, 422, map[string]any{"message": "Issue title is required"}, nil)
					return
				}
				var labels []*Label
				if arr, ok := body["labels"].([]any); ok {
					for _, ln := range arr {
						name, _ := ln.(string)
						l := repo.Labels[name]
						if l == nil {
							jsonReply(w, 422, map[string]any{"message": "Label \"" + name + "\" does not exist"}, nil)
							return
						}
						labels = append(labels, l)
					}
				}
				repo.IssueCounter++
				issue := &Issue{ID: s.id(), Number: repo.IssueCounter, Title: title, Body: getStr(body, "body"), State: "open", Labels: labels, Comments: []*Comment{}, CreatedAt: trialNow(), UpdatedAt: trialNow()}
				repo.Issues = append(repo.Issues, issue)
				jsonReply(w, 201, issueJson(issue), nil)
				return
			}
			if m == "GET" {
				state := ctx.Query.Get("state")
				if state == "" {
					state = "open"
				}
				direction := ctx.Query.Get("direction")
				if direction == "" {
					direction = "desc"
				}
				var items []*Issue
				for _, i := range repo.Issues {
					if state == "all" || i.State == state {
						items = append(items, i)
					}
				}
				sortIssues(items, direction == "asc")
				perPage := queryInt(ctx, "per_page", 30)
				if perPage > 100 {
					perPage = 100
				}
				if perPage > len(items) {
					perPage = len(items)
				}
				out := []any{}
				for _, i := range items[:perPage] {
					out = append(out, issueJson(i))
				}
				jsonReply(w, 200, out, nil)
				return
			}
		}
		if len(rest) == 2 {
			n, _ := strconv.Atoi(rest[1])
			issue := findIssue(repo, n)
			if issue == nil {
				notFound(w)
				return
			}
			if m == "GET" {
				jsonReply(w, 200, issueJson(issue), nil)
				return
			}
			if m == "PATCH" {
				v, ok := parseJson(ctx.RawBody)
				if !ok {
					badReq(w, "Invalid request body")
					return
				}
				body := asMap(v)
				if st := getStr(body, "state"); st == "open" || st == "closed" {
					issue.State = st
				}
				if t, ok := body["title"].(string); ok {
					issue.Title = t
				}
				if b, ok := body["body"].(string); ok {
					issue.Body = b
				}
				issue.UpdatedAt = trialNow()
				jsonReply(w, 200, issueJson(issue), nil)
				return
			}
		}
		if len(rest) == 3 && rest[2] == "comments" {
			n, _ := strconv.Atoi(rest[1])
			issue := findIssue(repo, n)
			if issue == nil {
				notFound(w)
				return
			}
			if m == "POST" {
				v, ok := parseJson(ctx.RawBody)
				if !ok {
					badReq(w, "Invalid request body")
					return
				}
				body := asMap(v)
				comment := &Comment{ID: s.id(), Body: getStr(body, "body"), CreatedAt: trialNow()}
				issue.Comments = append(issue.Comments, comment)
				jsonReply(w, 201, commentJson(comment), nil)
				return
			}
			if m == "GET" {
				out := []any{}
				for _, c := range issue.Comments {
					out = append(out, commentJson(c))
				}
				jsonReply(w, 200, out, nil)
				return
			}
		}
	}

	if r0 == "pulls" {
		if len(rest) == 1 {
			if m == "POST" {
				v, ok := parseJson(ctx.RawBody)
				if !ok {
					badReq(w, "Invalid request body")
					return
				}
				body := asMap(v)
				head := getStr(body, "head")
				base := getStr(body, "base")
				if head == "" || base == "" {
					jsonReply(w, 422, map[string]any{"message": "head and base are required"}, nil)
					return
				}
				headRef := head
				if i := strings.LastIndex(head, ":"); i >= 0 {
					headRef = head[i+1:]
				}
				if s.resolveRef(repo, headRef) == "" {
					jsonReply(w, 422, map[string]any{"message": "head ref \"" + headRef + "\" does not exist"}, nil)
					return
				}
				if s.resolveRef(repo, base) == "" {
					jsonReply(w, 422, map[string]any{"message": "base ref \"" + base + "\" does not exist"}, nil)
					return
				}
				repo.PullCounter++
				pull := &Pull{ID: s.id(), Number: repo.PullCounter, Title: getStr(body, "title"), Body: getStr(body, "body"), State: "open", HeadRef: headRef, BaseRef: base, Mergeable: true, CreatedAt: trialNow(), UpdatedAt: trialNow()}
				repo.Pulls = append(repo.Pulls, pull)
				jsonReply(w, 201, pullJson(s, repo, pull), nil)
				return
			}
			if m == "GET" {
				state := ctx.Query.Get("state")
				if state == "" {
					state = "open"
				}
				var items []*Pull
				for _, p := range repo.Pulls {
					if state == "all" || p.State == state {
						items = append(items, p)
					}
				}
				sortPulls(items)
				perPage := queryInt(ctx, "per_page", 30)
				if perPage > 100 {
					perPage = 100
				}
				if perPage > len(items) {
					perPage = len(items)
				}
				out := []any{}
				for _, p := range items[:perPage] {
					out = append(out, pullJson(s, repo, p))
				}
				jsonReply(w, 200, out, nil)
				return
			}
		}
		if len(rest) >= 2 {
			n, _ := strconv.Atoi(rest[1])
			pull := findPull(repo, n)
			if pull == nil {
				notFound(w)
				return
			}
			if len(rest) == 2 && m == "GET" {
				jsonReply(w, 200, pullJson(s, repo, pull), nil)
				return
			}
			if len(rest) == 3 && rest[2] == "files" && m == "GET" {
				headTip := s.resolveRef(repo, pull.HeadRef)
				baseTip := s.resolveRef(repo, pull.BaseRef)
				var changes []diffChange
				if headTip != "" && baseTip != "" {
					changes = s.diffTrees(repo, s.commitTree(repo, baseTip), s.commitTree(repo, headTip))
				}
				files := []any{}
				for _, c := range changes {
					additions, deletions := 0, 0
					hl, bl := 0, 0
					if c.HeadOid != "" {
						if o := s.getObject(repo, c.HeadOid); o != nil {
							hl = len(strings.Split(string(o.Data), "\n"))
						}
					}
					if c.BaseOid != "" {
						if o := s.getObject(repo, c.BaseOid); o != nil {
							bl = len(strings.Split(string(o.Data), "\n"))
						}
					}
					if c.Status == "added" {
						additions = maxInt(hl, 1)
					} else if c.Status == "removed" {
						deletions = maxInt(bl, 1)
					} else {
						additions = maxInt(hl-bl, 1)
						deletions = maxInt(bl-hl, 0)
					}
					sha := c.HeadOid
					if sha == "" {
						sha = c.BaseOid
					}
					files = append(files, map[string]any{"filename": c.Path, "status": c.Status, "additions": additions, "deletions": deletions, "changes": additions + deletions, "sha": sha})
				}
				jsonReply(w, 200, files, nil)
				return
			}
			if len(rest) == 3 && rest[2] == "merge" && m == "PUT" {
				v, ok := parseJson(ctx.RawBody)
				if !ok {
					badReq(w, "Invalid request body")
					return
				}
				body := asMap(v)
				if pull.Merged {
					jsonReply(w, 405, map[string]any{"message": "Pull request is already merged"}, nil)
					return
				}
				result := s.squashMerge(repo, pull)
				if result.Conflict {
					jsonReply(w, 405, map[string]any{"message": "Pull request is not mergeable (conflict)"}, nil)
					return
				}
				method := getStr(body, "merge_method")
				if method == "" {
					method = "squash"
				}
				jsonReply(w, 200, map[string]any{"sha": result.Sha, "merged": true, "message": "Merge pull request #" + itoa(pull.Number) + " (" + method + ")"}, nil)
				return
			}
		}
	}

	notFound(w)
}

func findIssue(r *Repo, n int) *Issue {
	for _, i := range r.Issues {
		if i.Number == n {
			return i
		}
	}
	return nil
}
func findPull(r *Repo, n int) *Pull {
	for _, p := range r.Pulls {
		if p.Number == n {
			return p
		}
	}
	return nil
}
func sortIssues(items []*Issue, asc bool) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0; j-- {
			a, b := items[j-1], items[j]
			swap := false
			if asc {
				swap = a.Number > b.Number
			} else {
				swap = a.Number < b.Number
			}
			if swap {
				items[j-1], items[j] = items[j], items[j-1]
			} else {
				break
			}
		}
	}
}
func sortPulls(items []*Pull) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j-1].Number < items[j].Number; j-- {
			items[j-1], items[j] = items[j], items[j-1]
		}
	}
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
