package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// upsertFile creates or updates a file in the GitHub repo using the write token.
// Retries once on 409 (Conflict) by re-fetching the SHA, which handles the case
// where currentSHA returned "" due to a transient GitHub error but the file exists.
func (c *githubClient) upsertFile(ctx context.Context, path, message, content string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", c.owner, c.repo, path)
	encoded := base64.StdEncoding.EncodeToString([]byte(content))

	for attempt := range 2 {
		sha := c.currentSHA(ctx, url)

		body := map[string]any{
			"message": message,
			"content": encoded,
			"committer": map[string]string{
				"name":  "launchpad",
				"email": "launchpad@homelab",
			},
		}
		if sha != "" {
			body["sha"] = sha
		}

		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		c.setHeaders(req)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.client.Do(req)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
			return nil
		}
		if resp.StatusCode == http.StatusConflict && attempt == 0 {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		return fmt.Errorf("github upsert %s: status %d", path, resp.StatusCode)
	}
	return fmt.Errorf("github upsert %s: exhausted retries", path)
}

// deleteFile deletes a file from the GitHub repo.
// SHA of the current file is required by the GitHub API.
func (c *githubClient) deleteFile(ctx context.Context, path, message string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", c.owner, c.repo, path)

	sha := c.currentSHA(ctx, url)
	if sha == "" {
		return fmt.Errorf("file not found or could not retrieve SHA: %s", path)
	}

	body := map[string]any{
		"message": message,
		"sha":     sha,
		"committer": map[string]string{
			"name":  "launchpad",
			"email": "launchpad@homelab",
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github delete %s: status %d", path, resp.StatusCode)
	}
	return nil
}

// batchFile is one file to write as part of an atomic multi-file commit.
type batchFile struct {
	Path    string
	Content string
}

// upsertFilesAtomic writes multiple files in a single commit via the Git Data
// API (tree/commit/ref), so a batch of related resources (e.g. an Api plus
// its database/storage add-ons) either all land or none do.
//
// Other endpoints (e.g. phase recording) write to the same branch via
// separate single-file commits, so the ref can move between this function's
// read of it and its write to it. GitHub rejects that write as a
// non-fast-forward. The retry rebuilds the tree and commit from the branch's
// current tip rather than just re-attempting the ref update, because a tree
// built against a stale base is itself invalid once the tip has moved.
func (c *githubClient) upsertFilesAtomic(ctx context.Context, files []batchFile, message string) error {
	const branch = "main"
	// GitHub's Git References API is inconsistent about singular vs. plural:
	// GET is under "git/ref/{ref}" but PATCH (update) is under "git/refs/{ref}".
	getRefURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/ref/heads/%s", c.owner, c.repo, branch)
	updateRefURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/refs/heads/%s", c.owner, c.repo, branch)

	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
		}

		var ref struct {
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		if err := c.apiJSON(ctx, http.MethodGet, getRefURL, nil, &ref); err != nil {
			return fmt.Errorf("get ref: %w", err)
		}
		baseCommitSHA := ref.Object.SHA

		var baseCommit struct {
			Tree struct {
				SHA string `json:"sha"`
			} `json:"tree"`
		}
		commitURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/commits/%s", c.owner, c.repo, baseCommitSHA)
		if err := c.apiJSON(ctx, http.MethodGet, commitURL, nil, &baseCommit); err != nil {
			return fmt.Errorf("get base commit: %w", err)
		}

		treeEntries := make([]map[string]any, 0, len(files))
		for _, f := range files {
			treeEntries = append(treeEntries, map[string]any{
				"path":    f.Path,
				"mode":    "100644",
				"type":    "blob",
				"content": f.Content,
			})
		}
		var newTree struct {
			SHA string `json:"sha"`
		}
		treeURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/trees", c.owner, c.repo)
		if err := c.apiJSON(ctx, http.MethodPost, treeURL, map[string]any{
			"base_tree": baseCommit.Tree.SHA,
			"tree":      treeEntries,
		}, &newTree); err != nil {
			return fmt.Errorf("create tree: %w", err)
		}

		var newCommit struct {
			SHA string `json:"sha"`
		}
		newCommitURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/commits", c.owner, c.repo)
		// Without an explicit author/committer, GitHub attributes the commit to
		// whoever owns the PAT (a personal account here) instead of this bot identity.
		committer := map[string]string{"name": "launchpad", "email": "launchpad@homelab"}
		if err := c.apiJSON(ctx, http.MethodPost, newCommitURL, map[string]any{
			"message":   message,
			"tree":      newTree.SHA,
			"parents":   []string{baseCommitSHA},
			"author":    committer,
			"committer": committer,
		}, &newCommit); err != nil {
			return fmt.Errorf("create commit: %w", err)
		}

		err := c.apiJSON(ctx, http.MethodPatch, updateRefURL, map[string]any{
			"sha": newCommit.SHA,
		}, nil)
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("update ref: %w", err)
	}

	return fmt.Errorf("upsertFilesAtomic: exhausted retries: %w", lastErr)
}

// apiJSON does a JSON GitHub API request and decodes the response into out
// (if non-nil). It reads and includes the response body on error, since
// GitHub's error messages (e.g. why a ref update or tree creation failed)
// are only in the body, not the status code.
func (c *githubClient) apiJSON(ctx context.Context, method, url string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *githubClient) currentSHA(ctx context.Context, url string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	c.setHeaders(req)
	resp, err := c.client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	var f ghFileResponse
	if err := json.NewDecoder(resp.Body).Decode(&f); err != nil {
		return ""
	}
	return f.SHA
}
