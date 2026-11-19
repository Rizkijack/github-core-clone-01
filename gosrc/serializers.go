package main

import "strconv"

func strconvFormatInt(n int64) string { return strconv.FormatInt(n, 10) }

func ownerJson() map[string]any {
	return map[string]any{"login": viewerLogin, "id": 1, "node_id": viewerNodeID, "type": "User"}
}

func repoJson(r *Repo) map[string]any {
	visibility := "public"
	if r.Private {
		visibility = "private"
	}
	return map[string]any{
		"id": r.ID, "node_id": nodeId("Repository", r.ID), "name": r.Name,
		"full_name": viewerLogin + "/" + r.Name, "owner": ownerJson(),
		"private": r.Private, "description": r.Description, "fork": false,
		"default_branch": r.DefaultBranch, "created_at": r.CreatedAt, "updated_at": r.UpdatedAt,
		"has_issues": true, "has_issues_enabled": true, "visibility": visibility,
	}
}

func labelJson(l *Label) map[string]any {
	return map[string]any{
		"id": l.ID, "node_id": nodeId("Label", l.ID), "name": l.Name,
		"color": l.Color, "description": l.Description, "default": false,
	}
}

func issueJson(i *Issue) map[string]any {
	labels := make([]any, 0, len(i.Labels))
	for _, l := range i.Labels {
		labels = append(labels, labelJson(l))
	}
	return map[string]any{
		"id": i.ID, "node_id": nodeId("Issue", i.ID), "number": i.Number,
		"title": i.Title, "body": i.Body, "state": i.State, "labels": labels,
		"user": map[string]any{"login": viewerLogin, "id": 1, "node_id": viewerNodeID},
		"comments": len(i.Comments), "created_at": i.CreatedAt, "updated_at": i.UpdatedAt,
	}
}

func commentJson(c *Comment) map[string]any {
	return map[string]any{
		"id": c.ID, "node_id": nodeId("IssueComment", c.ID), "body": c.Body,
		"user": map[string]any{"login": viewerLogin}, "created_at": c.CreatedAt, "updated_at": c.CreatedAt,
	}
}

func pullJson(s *Store, r *Repo, p *Pull) map[string]any {
	headSha := s.resolveRef(r, p.HeadRef)
	if headSha == "" {
		headSha = zeroOID
	}
	baseSha := s.resolveRef(r, p.BaseRef)
	if baseSha == "" {
		baseSha = zeroOID
	}
	mergeable := p.Mergeable
	if p.Merged {
		mergeable = false
	}
	var mergeCommit any
	if p.MergeCommitSHA != nil {
		mergeCommit = *p.MergeCommitSHA
	}
	var mergedAt any
	if p.MergedAt != nil {
		mergedAt = *p.MergedAt
	}
	return map[string]any{
		"id": p.ID, "node_id": nodeId("PullRequest", p.ID), "number": p.Number,
		"state": p.State, "title": p.Title, "body": p.Body,
		"head": map[string]any{"ref": p.HeadRef, "sha": headSha, "label": viewerLogin + ":" + p.HeadRef},
		"base": map[string]any{"ref": p.BaseRef, "sha": baseSha, "label": viewerLogin + ":" + p.BaseRef},
		"merged": p.Merged, "mergeable": mergeable, "merge_commit_sha": mergeCommit,
		"merged_at": mergedAt, "created_at": p.CreatedAt, "updated_at": p.UpdatedAt,
		"user": map[string]any{"login": viewerLogin},
	}
}
