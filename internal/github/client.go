package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// Client is the API as one installation of the app. Token is the installation token itself, for git over https; it never
// leaves the reviewer's own process (the sandbox gets none).
type Client struct {
	app   *App
	auth  string
	Token string
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	return c.app.do(ctx, c.auth, method, path, in, out)
}

// PullRequest is the part of a pull request the reviewer reads.
type PullRequest struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
	Head   Ref    `json:"head"`
	Base   Ref    `json:"base"`
	User   User   `json:"user"`
	URL    string `json:"html_url"`
}

// Ref is one side of a pull request.
type Ref struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo struct {
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
		Private  bool   `json:"private"`
	} `json:"repo"`
}

// User is an account.
type User struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

// PR reads a pull request.
func (c *Client) PR(ctx context.Context, repo string, number int) (*PullRequest, error) {
	var pr PullRequest
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), nil, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// Comment is an issue comment on a pull request.
type Comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User User   `json:"user"`
}

// Comments lists a pull request's issue comments, up to 300.
func (c *Client) Comments(ctx context.Context, repo string, number int) ([]Comment, error) {
	var all []Comment
	for page := 1; page <= 3; page++ {
		var cs []Comment
		q := url.Values{"per_page": {"100"}, "page": {fmt.Sprint(page)}}
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d/comments?%s", repo, number, q.Encode()), nil, &cs); err != nil {
			return nil, err
		}
		all = append(all, cs...)
		if len(cs) < 100 {
			break
		}
	}
	return all, nil
}

// Comment posts an issue comment.
func (c *Client) Comment(ctx context.Context, repo string, number int, body string) (*Comment, error) {
	var out Comment
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number), map[string]string{"body": body}, &out)
	return &out, err
}

// EditComment replaces an issue comment's body.
func (c *Client) EditComment(ctx context.Context, repo string, id int64, body string) error {
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/comments/%d", repo, id), map[string]string{"body": body}, nil)
}

// ReviewComment is one inline comment of a review, on a line of the pull request's head (side RIGHT).
type ReviewComment struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	StartLine int    `json:"start_line,omitempty"`
	Side      string `json:"side"`
	StartSide string `json:"start_side,omitempty"`
	Body      string `json:"body"`
}

// ReviewComments lists a pull request's inline review comments, up to 300.
func (c *Client) ReviewComments(ctx context.Context, repo string, number int) ([]Comment, error) {
	var all []Comment
	for page := 1; page <= 3; page++ {
		var cs []Comment
		q := url.Values{"per_page": {"100"}, "page": {fmt.Sprint(page)}}
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d/comments?%s", repo, number, q.Encode()), nil, &cs); err != nil {
			return nil, err
		}
		all = append(all, cs...)
		if len(cs) < 100 {
			break
		}
	}
	return all, nil
}

// Review is a pull request review to post.
type Review struct {
	CommitID string          `json:"commit_id"`
	Body     string          `json:"body"`
	Event    string          `json:"event"` // COMMENT: the reviewer never approves or blocks
	Comments []ReviewComment `json:"comments,omitempty"`
}

// PostReview posts a review with its inline comments in one call.
func (c *Client) PostReview(ctx context.Context, repo string, number int, r Review) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, number), r, nil)
}

// React adds a reaction to an issue comment (eyes: seen; rocket: done).
func (c *Client) React(ctx context.Context, repo string, commentID int64, content string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/comments/%d/reactions", repo, commentID), map[string]string{"content": content}, nil)
}
