// Package webhook receives the GitHub App's webhooks: it checks each delivery's signature, keeps the events that ask for a
// review (a pull request opened, pushed to, reopened or marked ready; a comment asking the bot to review), and hands them on.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
)

// Job is a review to run: a pull request of a repository an installation of the app covers.
type Job struct {
	Installation int64
	Repo         string // owner/name
	Number       int
	// Head is the commit the event saw, when it named one; the worker reads the pull request again before it starts.
	Head string
	// Reason says what asked for it: opened, synchronize, reopened, ready_for_review or command.
	Reason string
	// CommentID is the comment that asked, for a command (the worker reacts to it).
	CommentID int64
}

// Handler serves the webhook endpoint.
type Handler struct {
	Secret []byte
	// Bot is the name a comment mentions to command the reviewer ("reviewer" for "@reviewer review").
	Bot     string
	Enqueue func(Job)
	Log     *slog.Logger
}

// maxBody is the largest delivery read; GitHub caps payloads at 25 MB, and the events the reviewer reads are far smaller.
const maxBody = 5 << 20

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if !Valid(h.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	job, ok, why := Parse(event, body, h.Bot)
	if h.Log != nil {
		h.Log.Info("webhook", "event", event, "delivery", r.Header.Get("X-GitHub-Delivery"), "queued", ok, "why", why)
	}
	if ok {
		h.Enqueue(job)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Valid checks a delivery's X-Hub-Signature-256 against the webhook secret, in constant time. An empty secret accepts nothing.
func Valid(secret, body []byte, header string) bool {
	if len(secret) == 0 {
		return false
	}
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}

type payload struct {
	Action       string `json:"action"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest *struct {
		Number int  `json:"number"`
		Draft  bool `json:"draft"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
	Issue *struct {
		Number      int       `json:"number"`
		PullRequest *struct{} `json:"pull_request"`
	} `json:"issue"`
	Comment *struct {
		ID                int64  `json:"id"`
		Body              string `json:"body"`
		AuthorAssociation string `json:"author_association"`
		User              struct {
			Type string `json:"type"`
		} `json:"user"`
	} `json:"comment"`
}

// trusted are the author associations whose comments may start a review: a review costs model time, so a stranger's comment
// on a public repository does not.
var trusted = map[string]bool{"OWNER": true, "MEMBER": true, "COLLABORATOR": true}

// Parse turns a delivery into a job, or says why it is not one.
func Parse(event string, body []byte, bot string) (Job, bool, string) {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return Job{}, false, "not JSON"
	}
	if p.Installation.ID == 0 || p.Repository.FullName == "" {
		return Job{}, false, "no installation or repository"
	}
	j := Job{Installation: p.Installation.ID, Repo: p.Repository.FullName}
	switch event {
	case "pull_request":
		if p.PullRequest == nil {
			return Job{}, false, "no pull request"
		}
		switch p.Action {
		case "opened", "synchronize", "reopened", "ready_for_review":
		default:
			return Job{}, false, "action " + p.Action
		}
		if p.PullRequest.Draft {
			return Job{}, false, "draft"
		}
		j.Number, j.Head, j.Reason = p.PullRequest.Number, p.PullRequest.Head.SHA, p.Action
		return j, true, ""
	case "issue_comment":
		if p.Action != "created" || p.Issue == nil || p.Issue.PullRequest == nil || p.Comment == nil {
			return Job{}, false, "not a new comment on a pull request"
		}
		if p.Comment.User.Type == "Bot" {
			return Job{}, false, "a bot's comment"
		}
		if !Commands(bot, p.Comment.Body) {
			return Job{}, false, "no command"
		}
		if !trusted[p.Comment.AuthorAssociation] {
			return Job{}, false, "author " + p.Comment.AuthorAssociation + " may not command reviews"
		}
		j.Number, j.Reason, j.CommentID = p.Issue.Number, "command", p.Comment.ID
		return j, true, ""
	}
	return Job{}, false, "event " + event
}

// Commands reports whether a comment asks the bot for a review: "@bot review" at the start of a line.
func Commands(bot, body string) bool {
	if bot == "" {
		return false
	}
	re := regexp.MustCompile(`(?mi)^\s*@` + regexp.QuoteMeta(bot) + `(\[bot\])?\s+review\b`)
	return re.MatchString(body)
}
