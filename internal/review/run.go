package review

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/anemos-labs/reviewer/internal/github"
	"github.com/anemos-labs/reviewer/internal/webhook"
)

// Config is how reviews run.
type Config struct {
	// Work is where pull requests are cloned, one fresh directory per review. The Docker sandbox mounts the clone at its own
	// path, so when the reviewer runs in a container this directory is bind-mounted at the same path on the host.
	Work string
	// Sleipnir is the sleipnir binary.
	Sleipnir string
	// Team is how many agents review; Depth the Sleipnir depth (survey, standard, deep).
	Team  int
	Depth string
	// Model is the default model, and Roles the per-role models (role=model), as sleipnir swarm takes them.
	Model string
	Roles []string
	// Budget caps a review's model spend in US dollars; Timeout its length.
	Budget  float64
	Timeout time.Duration
	// Image is the sandbox image (empty: Sleipnir's default); Network lets reproducers reach the network.
	Image   string
	Network bool
	// Env names the variables of the reviewer's environment the sleipnir process gets (the model keys). The GitHub token is
	// never one of them.
	Env []string
	// Bot is the name comments mention to command a review.
	Bot string
	// Keep leaves the clone and its logs after a review, for debugging.
	Keep bool
}

// Runner runs reviews for the queue.
type Runner struct {
	App *github.App
	Cfg Config
	Log *slog.Logger
}

// Run reviews one pull request: it reads it again, says in the walkthrough that a review is running, clones the head, runs a
// Sleipnir review of the change from the base, and posts the walkthrough and the confirmed findings. A review stopped by a newer
// push says so and posts nothing else.
func (rn *Runner) Run(ctx context.Context, j webhook.Job) {
	log := rn.Log.With("repo", j.Repo, "pr", j.Number, "reason", j.Reason)
	gh, err := rn.App.Client(ctx, j.Installation)
	if err != nil {
		log.Error("github client", "err", err)
		return
	}
	pr, err := gh.PR(ctx, j.Repo, j.Number)
	if err != nil {
		log.Error("read pull request", "err", err)
		return
	}
	if pr.State != "open" || (pr.Draft && j.Reason != "command") {
		log.Info("skipped", "state", pr.State, "draft", pr.Draft)
		return
	}
	if j.CommentID != 0 {
		_ = gh.React(ctx, j.Repo, j.CommentID, "eyes")
	}
	walk := rn.walkthrough(ctx, gh, j, nil, fmt.Sprintf("Reviewing `%s` (%d agents, %s depth). This comment updates when it is done.", short(pr.Head.SHA), rn.Cfg.Team, rn.Cfg.Depth))
	rep, err := rn.review(ctx, gh, pr, log)
	switch {
	case ctx.Err() != nil:
		rn.update(context.WithoutCancel(ctx), gh, j, walk, nil, fmt.Sprintf("The review of `%s` was stopped: a newer commit is being reviewed.", short(pr.Head.SHA)))
		return
	case err != nil:
		log.Error("review", "err", err)
		rn.update(ctx, gh, j, walk, rep, fmt.Sprintf("The review of `%s` failed: %s", short(pr.Head.SHA), oneLine(firstLine(err.Error()))))
		return
	}
	posted, err := rn.post(ctx, gh, pr, rep)
	if err != nil {
		log.Error("post review", "err", err)
	}
	rn.update(ctx, gh, j, walk, rep, "")
	if j.CommentID != 0 {
		_ = gh.React(ctx, j.Repo, j.CommentID, "rocket")
	}
	log.Info("reviewed", "head", pr.Head.SHA, "phase", rep.Phase, "findings", len(rep.Inline()), "posted", posted)
}

// walkthrough posts the walkthrough comment, or edits the one already there, and returns its id.
func (rn *Runner) walkthrough(ctx context.Context, gh *github.Client, j webhook.Job, rep *Report, state string) int64 {
	body := Walkthrough(j.Repo, rep, state)
	cs, err := gh.Comments(ctx, j.Repo, j.Number)
	if err == nil {
		for _, c := range cs {
			if c.User.Type == "Bot" && strings.Contains(c.Body, WalkthroughMarker) {
				if err := gh.EditComment(ctx, j.Repo, c.ID, body); err != nil {
					rn.Log.Warn("edit walkthrough", "err", err)
				}
				return c.ID
			}
		}
	}
	c, err := gh.Comment(ctx, j.Repo, j.Number, body)
	if err != nil {
		rn.Log.Warn("post walkthrough", "err", err)
		return 0
	}
	return c.ID
}

func (rn *Runner) update(ctx context.Context, gh *github.Client, j webhook.Job, id int64, rep *Report, state string) {
	if id == 0 {
		rn.walkthrough(ctx, gh, j, rep, state)
		return
	}
	if err := gh.EditComment(ctx, j.Repo, id, Walkthrough(j.Repo, rep, state)); err != nil {
		rn.Log.Warn("edit walkthrough", "err", err)
	}
}

// post posts a review with the confirmed findings not posted on an earlier commit, and returns how many it posted.
func (rn *Runner) post(ctx context.Context, gh *github.Client, pr *github.PullRequest, rep *Report) (int, error) {
	repo := pr.Base.Repo.FullName
	old, err := gh.ReviewComments(ctx, repo, pr.Number)
	if err != nil {
		return 0, err
	}
	var bodies []string
	for _, c := range old {
		if c.User.Type == "Bot" {
			bodies = append(bodies, c.Body)
		}
	}
	seen := Posted(bodies)
	var comments []github.ReviewComment
	for _, f := range rep.Inline() {
		if seen[Fingerprint(f)] {
			continue
		}
		at := f.at()
		c := github.ReviewComment{Path: at.File, Line: at.Line, Side: "RIGHT", Body: Comment(repo, rep.Commit, rn.Cfg.Bot, f)}
		if end := at.Line + strings.Count(strings.TrimRight(at.Quote, "\n"), "\n"); end > at.Line {
			c.StartLine, c.StartSide, c.Line = at.Line, "RIGHT", end
		}
		comments = append(comments, c)
	}
	if len(comments) == 0 { // nothing new: the walkthrough says where the review stands
		return 0, nil
	}
	rv := github.Review{CommitID: rep.Commit, Event: "COMMENT", Body: ReviewBody(repo, rep, len(comments)), Comments: comments}
	err = gh.PostReview(ctx, repo, pr.Number, rv)
	var apiErr *github.Error
	if errors.As(err, &apiErr) && apiErr.Status == 422 && len(comments) > 0 {
		// a line GitHub does not show in the pull request's diff refuses the whole review: post the findings in its body
		rv.Body, rv.Comments = ReviewBody(repo, rep, 0), nil
		for _, c := range comments {
			rv.Body += fmt.Sprintf("\n---\n\n`%s:%d`\n\n%s", c.Path, c.Line, c.Body)
		}
		err = gh.PostReview(ctx, repo, pr.Number, rv)
	}
	if err != nil {
		return 0, err
	}
	return len(comments), nil
}

// review clones the pull request's head and runs the Sleipnir review in it. It returns the report even when the run ended early
// (a timeout or the budget), as long as the review was created.
func (rn *Runner) review(ctx context.Context, gh *github.Client, pr *github.PullRequest, log *slog.Logger) (*Report, error) {
	dir, err := os.MkdirTemp(rn.Cfg.Work, fmt.Sprintf("%s-%d-", strings.ReplaceAll(pr.Base.Repo.FullName, "/", "-"), pr.Number))
	if err != nil {
		return nil, err
	}
	if !rn.Cfg.Keep {
		defer os.RemoveAll(dir)
	}
	src := filepath.Join(dir, "src")
	if err := clone(ctx, gh.Token, pr, src); err != nil {
		return nil, err
	}
	goal, err := rn.sleipnir(ctx, src, nil, "scan", "--start", "--diff", pr.Base.SHA, "--depth", rn.Cfg.Depth)
	if err != nil {
		return nil, fmt.Errorf("start the review: %w", err)
	}
	goal = strings.TrimSpace(goal)
	slug := Slug(goal)
	if slug == "" {
		return nil, fmt.Errorf("sleipnir printed no review goal: %q", firstLine(goal))
	}
	logf, err := os.Create(filepath.Join(dir, "swarm.log"))
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	rctx, cancel := context.WithTimeout(ctx, rn.Cfg.Timeout)
	defer cancel()
	_, runErr := rn.sleipnir(rctx, src, logf, rn.swarmArgs(goal)...)
	rep, err := Load(filepath.Join(src, ".sleipnir", "scan", slug))
	if err != nil {
		return nil, errors.Join(runErr, err)
	}
	if runErr != nil && ctx.Err() == nil && !errors.Is(rctx.Err(), context.DeadlineExceeded) {
		log.Warn("swarm ended with an error", "err", runErr)
	}
	return rep, nil
}

func (rn *Runner) swarmArgs(goal string) []string {
	sb := "docker"
	if rn.Cfg.Image != "" {
		sb += "=" + rn.Cfg.Image
	}
	args := []string{"swarm", fmt.Sprint(max(rn.Cfg.Team, 2)), "--sandbox", sb, "--mode", "bypass"}
	if rn.Cfg.Network {
		args = append(args, "--sandbox-network")
	}
	if rn.Cfg.Budget > 0 {
		args = append(args, "--budget-usd", fmt.Sprint(rn.Cfg.Budget))
	}
	if rn.Cfg.Model != "" {
		args = append(args, "--model", rn.Cfg.Model)
	}
	for _, r := range rn.Cfg.Roles {
		args = append(args, "--role-model", r)
	}
	return append(args, goal)
}

// sleipnir runs the sleipnir binary in dir with only the configured variables of the environment, and returns its stdout (or
// writes it to out).
func (rn *Runner) sleipnir(ctx context.Context, dir string, out io.Writer, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, rn.Cfg.Sleipnir, args...)
	cmd.Dir = dir
	cmd.Env = Environ(rn.Cfg.Env)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 30 * time.Second
	var buf strings.Builder
	if out != nil {
		cmd.Stdout, cmd.Stderr = out, out
	} else {
		cmd.Stdout = &buf
		cmd.Stderr = &limited{w: &buf, n: 4000}
	}
	err := cmd.Run()
	return buf.String(), err
}

// Environ is the environment of the sleipnir process: what it needs to run (PATH, HOME, the Docker host) and the named
// variables, nothing else.
func Environ(names []string) []string {
	keep := append([]string{"PATH", "HOME", "USER", "TMPDIR", "DOCKER_HOST", "DOCKER_CONFIG", "SSL_CERT_FILE", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY"}, names...)
	var env []string
	for _, k := range keep {
		if strings.HasPrefix(k, "GITHUB_") || k == "GH_TOKEN" {
			continue
		}
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

var slugRE = regexp.MustCompile(`(?m)^Spec: (?:.*[\\/])?\.sleipnir[\\/]scan[\\/]([a-z0-9][a-z0-9-]{0,62})[\\/]BRIEF\.md\s*$`)

// Slug is the review a goal printed by sleipnir scan --start drives.
func Slug(goal string) string {
	if m := slugRE.FindStringSubmatch(goal); m != nil {
		return m[1]
	}
	return ""
}

// clone fetches the pull request's head and its base commit into dir and checks the head out. The token goes to git through its
// environment, never on a command line or into the clone's config, and git's hooks, fsmonitor and symbolic links are off so
// nothing in the pull request runs on the reviewer's machine.
func clone(ctx context.Context, token string, pr *github.PullRequest, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	auth := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	url := "https://github.com/" + pr.Base.Repo.FullName + ".git"
	safe := []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "core.symlinks=false", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always"}
	steps := [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", url},
		{"fetch", "-q", "--no-tags", "--no-recurse-submodules", "origin", fmt.Sprintf("+refs/pull/%d/head:refs/remotes/origin/pr", pr.Number), "+refs/heads/" + pr.Base.Ref + ":refs/remotes/origin/base"},
		{"checkout", "-q", "--detach", pr.Head.SHA},
	}
	for i, s := range steps {
		cmd := exec.CommandContext(ctx, "git", append(safe, s...)...)
		cmd.Dir = dir
		cmd.Env = append(Environ(nil), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if i == 2 {
			cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraHeader", "GIT_CONFIG_VALUE_0="+auth)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v: %s", s[0], err, strings.ReplaceAll(strings.TrimSpace(string(out)), token, "***"))
		}
	}
	return nil
}

type limited struct {
	w io.Writer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p[:min(len(p), l.n)]
	l.n -= len(q)
	if _, err := l.w.Write(q); err != nil {
		return 0, err
	}
	return len(p), nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
