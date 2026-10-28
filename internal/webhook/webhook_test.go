package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestValid(t *testing.T) {
	body := `{"a":1}`
	if !Valid([]byte("s"), []byte(body), sign("s", body)) {
		t.Error("a good signature was refused")
	}
	for _, h := range []string{sign("other", body), "", "sha1=abc", "sha256=zz"} {
		if Valid([]byte("s"), []byte(body), h) {
			t.Errorf("%q was accepted", h)
		}
	}
	if Valid(nil, []byte(body), sign("", body)) {
		t.Error("an empty secret accepted a delivery")
	}
}

const prEvent = `{"action":"%s","installation":{"id":7},"repository":{"full_name":"o/r"},"pull_request":{"number":12,"draft":%s,"head":{"sha":"abc"}}}`

func TestParse(t *testing.T) {
	cases := []struct {
		event, body string
		ok          bool
		why         string
	}{
		{"pull_request", strings.Replace(strings.Replace(prEvent, "%s", "opened", 1), "%s", "false", 1), true, ""},
		{"pull_request", strings.Replace(strings.Replace(prEvent, "%s", "synchronize", 1), "%s", "false", 1), true, ""},
		{"pull_request", strings.Replace(strings.Replace(prEvent, "%s", "opened", 1), "%s", "true", 1), false, "draft"},
		{"pull_request", strings.Replace(strings.Replace(prEvent, "%s", "closed", 1), "%s", "false", 1), false, "action closed"},
		{"issue_comment", `{"action":"created","installation":{"id":7},"repository":{"full_name":"o/r"},"issue":{"number":12,"pull_request":{}},"comment":{"id":5,"body":"thanks\n@reviewer review please","author_association":"MEMBER","user":{"type":"User"}}}`, true, ""},
		{"issue_comment", `{"action":"created","installation":{"id":7},"repository":{"full_name":"o/r"},"issue":{"number":12,"pull_request":{}},"comment":{"id":5,"body":"@reviewer review","author_association":"NONE","user":{"type":"User"}}}`, false, "may not command"},
		{"issue_comment", `{"action":"created","installation":{"id":7},"repository":{"full_name":"o/r"},"issue":{"number":12},"comment":{"id":5,"body":"@reviewer review","author_association":"OWNER","user":{"type":"User"}}}`, false, "not a new comment on a pull request"},
		{"issue_comment", `{"action":"created","installation":{"id":7},"repository":{"full_name":"o/r"},"issue":{"number":12,"pull_request":{}},"comment":{"id":5,"body":"@reviewer review","author_association":"OWNER","user":{"type":"Bot"}}}`, false, "a bot's comment"},
		{"push", `{"installation":{"id":7},"repository":{"full_name":"o/r"}}`, false, "event push"},
	}
	for _, c := range cases {
		j, ok, why := Parse(c.event, []byte(c.body), "reviewer")
		if ok != c.ok || !strings.Contains(why, c.why) {
			t.Errorf("%s %s: %v %q", c.event, c.body, ok, why)
		}
		if ok && (j.Installation != 7 || j.Repo != "o/r" || j.Number != 12) {
			t.Errorf("job: %+v", j)
		}
	}
}

func TestCommands(t *testing.T) {
	for body, want := range map[string]bool{
		"@reviewer review":          true,
		"  @Reviewer   review now":  true,
		"@reviewer[bot] review":     true,
		"please @reviewer review":   false,
		"@reviewer reviewing later": false,
		"@reviewerx review":         false,
	} {
		if Commands("reviewer", body) != want {
			t.Errorf("Commands(%q) != %v", body, want)
		}
	}
}

func TestHandler(t *testing.T) {
	var got []Job
	h := &Handler{Secret: []byte("s"), Bot: "reviewer", Enqueue: func(j Job) { got = append(got, j) }}
	body := strings.Replace(strings.Replace(prEvent, "%s", "opened", 1), "%s", "false", 1)
	for _, c := range []struct {
		sig  string
		want int
	}{{sign("s", body), http.StatusAccepted}, {sign("x", body), http.StatusUnauthorized}} {
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-Hub-Signature-256", c.sig)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("status %d, want %d", rec.Code, c.want)
		}
	}
	if len(got) != 1 {
		t.Fatalf("queued %d", len(got))
	}
}
