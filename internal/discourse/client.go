package discourse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Post is a Discourse post as returned by /posts.json.
type Post struct {
	ID         int64  `json:"id"`
	TopicID    int64  `json:"topic_id"`
	PostNumber int    `json:"post_number"`
	Raw        string `json:"raw"`
	Username   string `json:"username"`
	CreatedAt  string `json:"created_at"`
}

// Topic is a Discourse topic as returned by /latest.json.
type Topic struct {
	ID         int64  `json:"id"`
	Title      string `json:"title"`
	Slug       string `json:"slug"`
	CategoryID int64  `json:"category_id"`
}

// Client is a minimal Discourse REST client authenticated with an API key.
type Client struct {
	BaseURL     string
	APIKey      string
	APIUsername string
	HTTP        *http.Client
}

// New builds a Discourse client.
func New(baseURL, apiKey, apiUsername string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{
		BaseURL:     strings.TrimRight(baseURL, "/"),
		APIKey:      apiKey,
		APIUsername: apiUsername,
		HTTP:        hc,
	}
}

// LatestTopics returns the most recent topics.
func (c *Client) LatestTopics(ctx context.Context) ([]Topic, error) {
	var out struct {
		TopicList struct {
			Topics []Topic `json:"topics"`
		} `json:"topic_list"`
	}
	if err := c.do(ctx, http.MethodGet, "/latest.json", nil, &out); err != nil {
		return nil, err
	}
	return out.TopicList.Topics, nil
}

// LatestPosts returns the most recent posts across the forum.
func (c *Client) LatestPosts(ctx context.Context) ([]Post, error) {
	var out struct {
		LatestPosts []Post `json:"latest_posts"`
	}
	if err := c.do(ctx, http.MethodGet, "/posts.json", nil, &out); err != nil {
		return nil, err
	}
	return out.LatestPosts, nil
}

// CreatePost posts raw (markdown) into an existing topic and returns the new
// post.
func (c *Client) CreatePost(ctx context.Context, topicID int64, raw string) (Post, error) {
	body := map[string]any{"topic_id": topicID, "raw": raw}
	var out Post
	if err := c.do(ctx, http.MethodPost, "/posts.json", body, &out); err != nil {
		return Post{}, err
	}
	return out, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.APIKey != "" {
		req.Header.Set("Api-Key", c.APIKey)
		req.Header.Set("Api-Username", c.APIUsername)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, truncateForLog(b))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// truncateForLog bounds how much of a Discourse error body lands in an
// error string, so a large HTML error page doesn't bloat logs.
func truncateForLog(b []byte) string {
	const max = 500
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "... (truncated)"
}
