package crq

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"time"
)

var ErrNotFound = errors.New("github resource not found")

type APIError struct {
	Method string
	URL    string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github %s %s failed: %d %s", e.Method, e.URL, e.Status, strings.TrimSpace(e.Body))
}

type GitHub struct {
	token      string
	httpClient *http.Client
	apiBase    string
	graphBase  string
}

func NewGitHub(ctx context.Context) (*GitHub, error) {
	token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GH_TOKEN"))
	}
	if token == "" {
		out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
		if err == nil {
			token = strings.TrimSpace(string(out))
		}
	}
	if token == "" {
		return nil, errors.New("GitHub token not found (set GITHUB_TOKEN/GH_TOKEN or run 'gh auth login')")
	}
	return &GitHub{
		token:      token,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		apiBase:    "https://api.github.com",
		graphBase:  "https://api.github.com/graphql",
	}, nil
}

func (g *GitHub) request(ctx context.Context, method, path string, in, out any) error {
	body, err := encodeBody(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, g.apiBase+path, body)
	if err != nil {
		return err
	}
	g.decorate(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, resp.Body)
		return ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{Method: method, URL: path, Status: resp.StatusCode, Body: string(b)}
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (g *GitHub) requestPaged(ctx context.Context, path string, out any) error {
	value := reflect.ValueOf(out)
	if value.Kind() != reflect.Pointer || value.Elem().Kind() != reflect.Slice {
		return errors.New("paged output must be pointer to slice")
	}
	next := g.apiBase + path
	for next != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return err
		}
		g.decorate(req)
		resp, err := g.httpClient.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			return ErrNotFound
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return &APIError{Method: http.MethodGet, URL: next, Status: resp.StatusCode, Body: string(b)}
		}
		page := reflect.New(value.Elem().Type()).Interface()
		if err := json.NewDecoder(resp.Body).Decode(page); err != nil {
			resp.Body.Close()
			return err
		}
		resp.Body.Close()
		value.Elem().Set(reflect.AppendSlice(value.Elem(), reflect.ValueOf(page).Elem()))
		next = nextPage(resp.Header.Get("Link"))
	}
	return nil
}

func (g *GitHub) GraphQL(ctx context.Context, query string, variables map[string]any, out any) error {
	payload := map[string]any{"query": query, "variables": variables}
	body, err := encodeBody(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.graphBase, body)
	if err != nil {
		return err
	}
	g.decorate(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{Method: http.MethodPost, URL: g.graphBase, Status: resp.StatusCode, Body: string(b)}
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		return errors.New(envelope.Errors[0].Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(envelope.Data, out)
}

func (g *GitHub) decorate(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("User-Agent", "crq/"+Version)
}

func encodeBody(in any) (io.Reader, error) {
	if in == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(in); err != nil {
		return nil, err
	}
	return &buf, nil
}

func nextPage(link string) string {
	for _, part := range strings.Split(link, ",") {
		sections := strings.Split(part, ";")
		if len(sections) < 2 || !strings.Contains(sections[1], `rel="next"`) {
			continue
		}
		raw := strings.TrimSpace(sections[0])
		raw = strings.TrimPrefix(raw, "<")
		raw = strings.TrimSuffix(raw, ">")
		return raw
	}
	return ""
}

func repoPath(repo string) string {
	owner, name, _ := strings.Cut(repo, "/")
	return url.PathEscape(owner) + "/" + url.PathEscape(name)
}

func addQuery(path string, values url.Values) string {
	if strings.Contains(path, "?") {
		return path + "&" + values.Encode()
	}
	return path + "?" + values.Encode()
}

type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
}

type Pull struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Merged bool `json:"merged"`
}

type RepoInfo struct {
	DefaultBranch string `json:"default_branch"`
}

type IssueComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	URL       string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

type Review struct {
	ID          int64     `json:"id"`
	Body        string    `json:"body"`
	CommitID    string    `json:"commit_id"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submitted_at"`
	HTMLURL     string    `json:"html_url"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
}

type ReviewComment struct {
	ID                  int64     `json:"id"`
	PullRequestReviewID int64     `json:"pull_request_review_id"`
	Body                string    `json:"body"`
	Path                string    `json:"path"`
	Line                int       `json:"line"`
	OriginalLine        int       `json:"original_line"`
	CommitID            string    `json:"commit_id"`
	OriginalCommitID    string    `json:"original_commit_id"`
	URL                 string    `json:"html_url"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	User                struct {
		Login string `json:"login"`
	} `json:"user"`
}

type Reaction struct {
	ID   int64 `json:"id"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (g *GitHub) GetIssue(ctx context.Context, repo string, number int) (Issue, error) {
	var out Issue
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", repoPath(repo), number), nil, &out)
	return out, err
}

func (g *GitHub) PatchIssue(ctx context.Context, repo string, number int, title, body string) error {
	in := map[string]string{"title": title, "body": body}
	return g.request(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", repoPath(repo), number), in, nil)
}

func (g *GitHub) CreateIssue(ctx context.Context, repo, title, body string) (Issue, error) {
	var out Issue
	err := g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues", repoPath(repo)), map[string]string{
		"title": title,
		"body":  body,
	}, &out)
	return out, err
}

func (g *GitHub) GetPull(ctx context.Context, repo string, pr int) (Pull, error) {
	var out Pull
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repoPath(repo), pr), nil, &out)
	return out, err
}

func (g *GitHub) ListPulls(ctx context.Context, repo string, query url.Values) ([]Pull, error) {
	var out []Pull
	path := fmt.Sprintf("/repos/%s/pulls?per_page=100", repoPath(repo))
	if len(query) > 0 {
		path += "&" + query.Encode()
	}
	err := g.requestPaged(ctx, path, &out)
	return out, err
}

func (g *GitHub) CreatePull(ctx context.Context, repo, base, head, title, body string, draft bool) (Pull, error) {
	var out Pull
	err := g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/pulls", repoPath(repo)), map[string]any{
		"base":  base,
		"head":  head,
		"title": title,
		"body":  body,
		"draft": draft,
	}, &out)
	return out, err
}

func (g *GitHub) ListIssueComments(ctx context.Context, repo string, issue int) ([]IssueComment, error) {
	var out []IssueComment
	err := g.requestPaged(ctx, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repoPath(repo), issue), &out)
	return out, err
}

func (g *GitHub) ListReviewComments(ctx context.Context, repo string, pr int) ([]ReviewComment, error) {
	var out []ReviewComment
	err := g.requestPaged(ctx, fmt.Sprintf("/repos/%s/pulls/%d/comments?per_page=100", repoPath(repo), pr), &out)
	return out, err
}

func (g *GitHub) ListReviews(ctx context.Context, repo string, pr int) ([]Review, error) {
	var out []Review
	err := g.requestPaged(ctx, fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=100", repoPath(repo), pr), &out)
	return out, err
}

func (g *GitHub) PostIssueComment(ctx context.Context, repo string, issue int, body string) (IssueComment, error) {
	var out IssueComment
	err := g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repoPath(repo), issue), map[string]string{"body": body}, &out)
	return out, err
}

func (g *GitHub) ListCommentReactions(ctx context.Context, repo string, commentID int64) ([]Reaction, error) {
	var out []Reaction
	err := g.requestPaged(ctx, fmt.Sprintf("/repos/%s/issues/comments/%d/reactions?per_page=100", repoPath(repo), commentID), &out)
	return out, err
}

func (g *GitHub) SearchOpenPRs(ctx context.Context, target string, byRepo bool, limit int) ([]SearchPR, error) {
	var out []SearchPR
	query := "type:pr state:open archived:false "
	if byRepo {
		query += "repo:" + target
	} else {
		query += "user:" + target
	}
	page := 1
	for len(out) < limit {
		values := url.Values{}
		values.Set("q", query)
		values.Set("per_page", "100")
		values.Set("page", strconv.Itoa(page))
		values.Set("sort", "updated")
		values.Set("order", "desc")
		var result struct {
			Items []struct {
				Number        int    `json:"number"`
				RepositoryURL string `json:"repository_url"`
			} `json:"items"`
		}
		if err := g.request(ctx, http.MethodGet, "/search/issues?"+values.Encode(), nil, &result); err != nil {
			return out, err
		}
		if len(result.Items) == 0 {
			break
		}
		for _, item := range result.Items {
			repo := strings.TrimPrefix(item.RepositoryURL, "https://api.github.com/repos/")
			if repo != "" {
				out = append(out, SearchPR{Repo: repo, Number: item.Number})
			}
			if len(out) >= limit {
				break
			}
		}
		if len(result.Items) < 100 {
			break
		}
		page++
	}
	return out, nil
}

type SearchPR struct {
	Repo   string
	Number int
}

type gitRef struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}

type gitCommit struct {
	SHA  string `json:"sha"`
	Tree struct {
		SHA string `json:"sha"`
	} `json:"tree"`
	Committer struct {
		Date time.Time `json:"date"`
	} `json:"committer"`
}

type gitTree struct {
	SHA  string `json:"sha"`
	Tree []struct {
		Path string `json:"path"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"tree"`
}

type gitBlob struct {
	SHA      string `json:"sha"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

func (g *GitHub) GetRef(ctx context.Context, repo, ref string) (string, error) {
	var out gitRef
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/git/ref/heads/%s", repoPath(repo), refPath(ref)), nil, &out)
	if err != nil {
		return "", err
	}
	return out.Object.SHA, nil
}

func (g *GitHub) CreateRef(ctx context.Context, repo, ref, sha string) error {
	return g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/git/refs", repoPath(repo)), map[string]string{
		"ref": "refs/heads/" + ref,
		"sha": sha,
	}, nil)
}

func (g *GitHub) UpdateRef(ctx context.Context, repo, ref, sha string, force bool) error {
	return g.request(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/git/refs/heads/%s", repoPath(repo), refPath(ref)), map[string]any{
		"sha":   sha,
		"force": force,
	}, nil)
}

func (g *GitHub) CreateBlob(ctx context.Context, repo string, content []byte) (string, error) {
	var out gitBlob
	err := g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/git/blobs", repoPath(repo)), map[string]string{
		"content":  string(content),
		"encoding": "utf-8",
	}, &out)
	return out.SHA, err
}

func (g *GitHub) CreateTree(ctx context.Context, repo, baseTree string, entries []map[string]any) (string, error) {
	in := map[string]any{"tree": entries}
	if baseTree != "" {
		in["base_tree"] = baseTree
	}
	var out gitTree
	err := g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/git/trees", repoPath(repo)), in, &out)
	return out.SHA, err
}

func (g *GitHub) CreateCommit(ctx context.Context, repo, message, tree string, parents []string) (string, error) {
	var out gitCommit
	err := g.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/git/commits", repoPath(repo)), map[string]any{
		"message": message,
		"tree":    tree,
		"parents": parents,
	}, &out)
	return out.SHA, err
}

func (g *GitHub) GetCommit(ctx context.Context, repo, sha string) (gitCommit, error) {
	var out gitCommit
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/git/commits/%s", repoPath(repo), url.PathEscape(sha)), nil, &out)
	return out, err
}

func (g *GitHub) GetTree(ctx context.Context, repo, sha string) (gitTree, error) {
	var out gitTree
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/git/trees/%s?recursive=1", repoPath(repo), url.PathEscape(sha)), nil, &out)
	return out, err
}

func (g *GitHub) GetBlob(ctx context.Context, repo, sha string) ([]byte, error) {
	var out gitBlob
	if err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/git/blobs/%s", repoPath(repo), url.PathEscape(sha)), nil, &out); err != nil {
		return nil, err
	}
	if out.Encoding == "base64" {
		return base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	}
	return []byte(out.Content), nil
}

func (g *GitHub) RepoExists(ctx context.Context, repo string) bool {
	return g.request(ctx, http.MethodGet, "/repos/"+repoPath(repo), nil, nil) == nil
}

func (g *GitHub) GetRepo(ctx context.Context, repo string) (RepoInfo, error) {
	var out RepoInfo
	err := g.request(ctx, http.MethodGet, "/repos/"+repoPath(repo), nil, &out)
	return out, err
}

func refPath(ref string) string {
	parts := strings.Split(ref, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
