package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type githubClient struct {
	token  string // read-write PAT
	owner  string
	repo   string
	client *http.Client
}

type ghFileResponse struct {
	SHA     string `json:"sha"`
	Content string `json:"content"`
}

type ghEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // "file" or "dir"
	Path string `json:"path"`
}

type ghFile struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

func newGithubClient(token, owner, repo string) *githubClient {
	return &githubClient{
		token:  token,
		owner:  owner,
		repo:   repo,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *githubClient) listDir(ctx context.Context, path string) ([]ghEntry, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", c.owner, c.repo, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github API %s: status %d", url, resp.StatusCode)
	}

	var entries []ghEntry
	return entries, json.NewDecoder(resp.Body).Decode(&entries)
}

func (c *githubClient) fileContent(ctx context.Context, path string) ([]byte, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", c.owner, c.repo, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github API %s: status %d", url, resp.StatusCode)
	}

	var f ghFile
	if err := json.NewDecoder(resp.Body).Decode(&f); err != nil {
		return nil, err
	}

	// GitHub base64-encodes content with embedded newlines
	raw := strings.ReplaceAll(f.Content, "\n", "")
	return base64.StdEncoding.DecodeString(raw)
}

func (c *githubClient) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "launchpad-api/1")
}
