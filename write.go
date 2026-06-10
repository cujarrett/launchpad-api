package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
)

// upsertFile creates or updates a file in the GitHub repo using the write token.
func (c *githubClient) upsertFile(ctx context.Context, path, message, content string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", c.owner, c.repo, path)

	sha := c.currentSHA(ctx, url)

	body := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
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
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("github upsert %s: status %d", path, resp.StatusCode)
	}
	return nil
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
