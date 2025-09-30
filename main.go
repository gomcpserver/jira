package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---- Debug helpers ----

var debug = os.Getenv("DEBUG") == "1"

func init() {
	// Timestamp + microseconds + short file:line for easier troubleshooting
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)
}

func debugf(format string, args ...any) {
	if debug {
		log.Printf("[DEBUG] "+format, args...)
	}
}

func maskEmail(s string) string {
	// keep first char and domain, mask the rest: a***@example.com
	if s == "" || !strings.Contains(s, "@") {
		return s
	}
	parts := strings.SplitN(s, "@", 2)
	user, dom := parts[0], parts[1]
	if len(user) <= 1 {
		return "*@" + dom
	}
	return user[:1] + strings.Repeat("*", len(user)-1) + "@" + dom
}

func tokenInfo(token string) string {
	// Only return length, never the token content
	return fmt.Sprintf("len=%d", len(token))
}

// ---- HTTP logging transport (safe) ----

type loggingTransport struct {
	base http.RoundTripper
}

func (t loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if debug {
		// Avoid leaking headers like Authorization
		debugf("HTTP %s %s", req.Method, req.URL.String())
	}
	resp, err := t.base.RoundTrip(req)
	if debug && resp != nil {
		debugf("HTTP -> %s", resp.Status)
	}
	return resp, err
}

func wrapClientForDebug(c *http.Client) *http.Client {
	if !debug {
		return c
	}
	rt := c.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	c2 := *c
	c2.Transport = loggingTransport{base: rt}
	return &c2
}

// ---- Jira client (minimal) ----

type JiraClient struct {
	BaseURL string
	Auth    string // "Basic <base64(email:token)>"
	Client  *http.Client
}

func NewJiraClientFromEnv() (*JiraClient, error) {
	baseURL := strings.TrimRight(os.Getenv("JIRA_INSTANCE_URL"), "/")
	email := os.Getenv("JIRA_USER_EMAIL")
	token := os.Getenv("JIRA_API_TOKEN")

	if debug {
		debugf("Load env: JIRA_INSTANCE_URL=%q", baseURL)
		debugf("Load env: JIRA_USER_EMAIL=%q (masked=%q)", email, maskEmail(email))
		debugf("Load env: JIRA_API_TOKEN (%s)", tokenInfo(token))
	}

	if baseURL == "" || email == "" || token == "" {
		return nil, errors.New("JIRA_INSTANCE_URL, JIRA_USER_EMAIL, JIRA_API_TOKEN must be set")
	}
	if _, err := url.ParseRequestURI(baseURL); err != nil {
		return nil, fmt.Errorf("invalid JIRA_INSTANCE_URL: %w", err)
	}
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+token))
	cl := &http.Client{Timeout: 30 * time.Second}
	cl = wrapClientForDebug(cl)

	return &JiraClient{
		BaseURL: baseURL,
		Auth:    auth,
		Client:  cl,
	}, nil
}

func (c *JiraClient) doJSON(ctx context.Context, method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		if debug {
			// Body might contain user text; safe to log as JSON if needed
			debugf("Jira request body: %s", string(b))
		}
		r = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.Auth)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("jira %s %s failed: %s - %s", method, path, resp.Status, string(b))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type JiraIssue struct {
	ID     string         `json:"id,omitempty"`
	Key    string         `json:"key,omitempty"`
	Self   string         `json:"self,omitempty"`
	Fields map[string]any `json:"fields,omitempty"`
}

type JiraSearchResult struct {
	StartAt    int         `json:"startAt"`
	MaxResults int         `json:"maxResults"`
	Total      int         `json:"total"`
	Issues     []JiraIssue `json:"issues"`
}

func (c *JiraClient) GetIssue(ctx context.Context, key string) (*JiraIssue, error) {
	var out JiraIssue
	if err := c.doJSON(ctx, http.MethodGet, "/rest/api/3/issue/"+url.PathEscape(key), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *JiraClient) Search(ctx context.Context, jql string, max int) (*JiraSearchResult, error) {
	if max <= 0 || max > 1000 {
		max = 50
	}
	q := url.Values{}
	q.Set("jql", jql)
	q.Set("maxResults", fmt.Sprintf("%d", max))
	var out JiraSearchResult
	if err := c.doJSON(ctx, http.MethodGet, "/rest/api/3/search?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *JiraClient) AddComment(ctx context.Context, key, body string) error {
	req := map[string]string{"body": body}
	return c.doJSON(ctx, http.MethodPost, "/rest/api/3/issue/"+url.PathEscape(key)+"/comment", req, nil)
}

func (c *JiraClient) CreateIssue(ctx context.Context, projectKey, issueType, summary, description string) (*JiraIssue, error) {
	payload := map[string]any{
		"fields": map[string]any{
			"project":     map[string]any{"key": projectKey},
			"summary":     summary,
			"description": description,
			"issuetype":   map[string]any{"name": issueType},
		},
	}
	var out JiraIssue
	if err := c.doJSON(ctx, http.MethodPost, "/rest/api/3/issue", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---- MCP server (v0.8.0 API) ----

func main() {
	ctx := context.Background()

	jc, err := NewJiraClientFromEnv()
	if err != nil {
		log.Fatalf("init error: %v", err)
	}
	debugf("Starting MCP server: name=%s version=%s", "jira", "0.1.0")

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "jira",
		Version: "0.1.0",
	}, nil)

	// get_issue(key)
	type getIssueArgs struct {
		Key string `json:"key" jsonschema:"Jira issue key, e.g. PROJ-123"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_issue",
		Title:       "Get Issue",
		Description: "Get a Jira issue by key",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getIssueArgs) (*mcp.CallToolResult, any, error) {
		debugf("tool=get_issue args={key:%q}", args.Key)
		iss, err := jc.GetIssue(ctx, args.Key)
		if err != nil {
			debugf("tool=get_issue error=%v", err)
			return nil, nil, err
		}
		return &mcp.CallToolResult{StructuredContent: iss}, nil, nil
	})

	// search_issues(jql, max_results?)
	type searchArgs struct {
		JQL        string `json:"jql"`
		MaxResults int    `json:"max_results,omitempty"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_issues",
		Title:       "Search Issues",
		Description: "Search Jira with JQL",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
		debugf("tool=search_issues args={jql:%q,max:%d}", args.JQL, args.MaxResults)
		res, err := jc.Search(ctx, args.JQL, args.MaxResults)
		if err != nil {
			debugf("tool=search_issues error=%v", err)
			return nil, nil, err
		}
		return &mcp.CallToolResult{StructuredContent: res}, nil, nil
	})

	// add_comment(key, body)
	type addCommentArgs struct {
		Key  string `json:"key"`
		Body string `json:"body"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "add_comment",
		Title:       "Add Comment",
		Description: "Add a comment to a Jira issue",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args addCommentArgs) (*mcp.CallToolResult, any, error) {
		preview := args.Body
		if len(preview) > 80 {
			preview = preview[:80] + "..."
		}
		debugf("tool=add_comment args={key:%q, body-preview:%q}", args.Key, preview)
		if err := jc.AddComment(ctx, args.Key, args.Body); err != nil {
			debugf("tool=add_comment error=%v", err)
			return nil, nil, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "ok"}},
		}, nil, nil
	})

	// create_issue(project_key, issue_type, summary, description?)
	type createIssueArgs struct {
		ProjectKey  string `json:"project_key"`
		IssueType   string `json:"issue_type"`
		Summary     string `json:"summary"`
		Description string `json:"description,omitempty"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "create_issue",
		Title:       "Create Issue",
		Description: "Create a Jira issue",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args createIssueArgs) (*mcp.CallToolResult, any, error) {
		debugf("tool=create_issue args={project:%q,type:%q,summary:%q,desc-len:%d}",
			args.ProjectKey, args.IssueType, args.Summary, len(args.Description))
		iss, err := jc.CreateIssue(ctx, args.ProjectKey, args.IssueType, args.Summary, args.Description)
		if err != nil {
			debugf("tool=create_issue error=%v", err)
			return nil, nil, err
		}
		return &mcp.CallToolResult{StructuredContent: iss}, nil, nil
	})

	// Run over stdio (for IDE/hosts)
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
