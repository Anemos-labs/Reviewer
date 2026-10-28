package review

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anemos-labs/reviewer/internal/github"
)

const findingsJSON = `{"scan":"review-20261028-0500-abcdef1","commit":"abcdef1234567","base":"0123456789","depth":"standard","phase":"done",
"counts":{"files":1,"funcs":2,"lines":30,"shards":1,"points":3,"leads":3,"confirmed":1,"unproven":1,"rejected":1,"reproduced":1},
"findings":[
{"id":"L1","title":"Command injection in Ping","category":"command-injection","severity":"critical","status":"confirmed","impact":"critical","reach":"external","exploit":"trivial",
 "claim":"Anyone can run shell commands by sending host=x;id to /ping.","by":"hunter","fix":"Pass host as its own argument.",
 "trace":[{"file":"server/h.go","line":9,"quote":"host := q.Get(\"host\")","note":"from the query"},{"file":"server/h.go","line":10,"quote":"exec.Command(\"sh\", \"-c\", \"ping \"+host)","note":"into a shell"}],
 "verdicts":[{"by":"v1","role":"verifier","confirm":true,"note":"ok"},{"by":"v2","role":"defender","confirm":true,"note":"no guard"}],
 "proofs":[{"by":"tester","script":"curl ...","outcome":"reproduced","output":"SLEIPNIR-REPRODUCED uid=1000"}],
 "anchor":{"file":"server/h.go","line":10,"quote":"exec.Command(\"sh\", \"-c\", \"ping \"+host)"}},
{"id":"L2","title":"Path traversal","category":"path-traversal","severity":"high","status":"unproven","claim":"Reads any file.","trace":[{"file":"server/h.go","line":20,"quote":"open(p)","note":"sink"}],"anchor":{"file":"server/h.go","line":20,"quote":"open(p)"}},
{"id":"L3","title":"Not real","category":"injection","severity":"low","status":"rejected","claim":"x","trace":[{"file":"a","line":1,"quote":"x","note":"x"}]}
]}`

const mapJSON = `{"nodes":[{"id":"S1","label":"server/h.go","kind":"shard","summary":"The ping handler | runs a shell"},{"id":"P1","kind":"point","label":"p"}]}`

func writeReport(t *testing.T) string {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "findings.json"), []byte(findingsJSON), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "map.json"), []byte(mapJSON), 0o644)
	return dir
}

func TestFormat(t *testing.T) {
	r, err := Load(writeReport(t))
	if err != nil {
		t.Fatal(err)
	}
	in := r.Inline()
	if len(in) != 1 || in[0].ID != "L1" || len(r.Unproven()) != 1 {
		t.Fatalf("inline %d, unproven %d", len(in), len(r.Unproven()))
	}
	c := Comment("o/r", r.Commit, "reviewer", in[0])
	for _, want := range []string{"🔴 Critical", "Command injection", "https://github.com/o/r/blob/abcdef1234567/server/h.go#L10", "one of them arguing it was a false positive",
		"reproduced in a sandbox", "Suggested fix", "Prompt for AI agents", "@reviewer review", "anemos-reviewer:finding " + Fingerprint(in[0])} {
		if !strings.Contains(c, want) {
			t.Errorf("comment lacks %q:\n%s", want, c)
		}
	}
	if Posted([]string{c, "other"})[Fingerprint(in[0])] != true {
		t.Error("a posted finding is not found again")
	}
	w := Walkthrough("o/r", r, "")
	for _, want := range []string{WalkthroughMarker, "The ping handler \\| runs a shell", "**1 actionable finding**", "1 more confirmed by verifiers", "Path traversal", "1 lead was rejected", "`review-20261028-0500-abcdef1`"} {
		if !strings.Contains(w, want) {
			t.Errorf("walkthrough lacks %q:\n%s", want, w)
		}
	}
	if b := ReviewBody("o/r", r, 1); !strings.Contains(b, "Actionable comments posted: 1") {
		t.Errorf("body: %s", b)
	}
	moved := in[0]
	moved.Anchor = &Cite{File: moved.Anchor.File, Line: 99, Quote: "  " + moved.Anchor.Quote}
	if Fingerprint(moved) != Fingerprint(in[0]) {
		t.Error("a finding that moved down the file is seen as new")
	}
}

func TestFenceOutlastsBackticks(t *testing.T) {
	f := fence("a ``` b", "")
	if !strings.HasPrefix(f, "````\n") {
		t.Fatalf("fence: %q", f)
	}
}

func TestSlug(t *testing.T) {
	g := "Review (standard) of the change from 0123456 to abcdef1: ...\n\nSpec: /w/src/.sleipnir/scan/review-20261028-0500-abcdef1/BRIEF.md"
	if Slug(g) != "review-20261028-0500-abcdef1" || Slug("nothing") != "" {
		t.Fatalf("slug: %q", Slug(g))
	}
}

func TestEnvironKeepsGitHubOut(t *testing.T) {
	t.Setenv("HEIMDALLM_API_KEY", "k")
	t.Setenv("GITHUB_TOKEN", "secret")
	t.Setenv("OTHER", "x")
	env := strings.Join(Environ([]string{"HEIMDALLM_API_KEY", "GITHUB_TOKEN"}), "\n")
	if !strings.Contains(env, "HEIMDALLM_API_KEY=k") || strings.Contains(env, "secret") || strings.Contains(env, "OTHER") {
		t.Fatalf("env: %s", env)
	}
}

// The review posts the confirmed finding once, inline on its anchor, and not again on the next push.
func TestPostSkipsWhatWasPosted(t *testing.T) {
	var mu sync.Mutex
	var reviews []github.Review
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "t", "expires_at": time.Now().Add(time.Hour)})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/3/comments"):
			var cs []map[string]any
			for _, b := range posted {
				cs = append(cs, map[string]any{"id": 1, "body": b, "user": map[string]string{"type": "Bot"}})
			}
			_ = json.NewEncoder(w).Encode(cs)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls/3/reviews"):
			var rv github.Review
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &rv)
			reviews = append(reviews, rv)
			for _, c := range rv.Comments {
				posted = append(posted, c.Body)
			}
			w.WriteHeader(200)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, 400)
		}
	}))
	defer srv.Close()
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	app, err := github.NewApp(1, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	gh, err := app.Client(context.Background(), 9)
	if err != nil {
		t.Fatal(err)
	}
	rep, _ := Load(writeReport(t))
	rn := &Runner{App: app, Cfg: Config{Bot: "reviewer"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	pr := &github.PullRequest{Number: 3}
	pr.Base.Repo.FullName = "o/r"
	for range 2 {
		if _, err := rn.post(context.Background(), gh, pr, rep); err != nil {
			t.Fatal(err)
		}
	}
	if len(reviews) != 1 || len(reviews[0].Comments) != 1 {
		t.Fatalf("reviews: %+v", reviews)
	}
	c := reviews[0].Comments[0]
	if c.Path != "server/h.go" || c.Line != 10 || c.Side != "RIGHT" || reviews[0].Event != "COMMENT" || reviews[0].CommitID != "abcdef1234567" {
		t.Fatalf("comment: %+v", c)
	}
}
