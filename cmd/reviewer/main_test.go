package main

import (
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	env := map[string]string{"GITHUB_APP_ID": "12", "REVIEWER_TEAM": "16", "REVIEWER_ROLE_MODELS": "a=x, b=y", "REVIEWER_TIMEOUT": "2h"}
	c, err := load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.appID != 12 || c.review.Team != 16 || len(c.review.Roles) != 2 || c.review.Timeout != 2*time.Hour || c.review.Depth != "standard" || c.review.Bot != "reviewer" ||
		len(c.review.Env) != 1 || c.review.Env[0] != "HEIMDALLM_API_KEY" {
		t.Fatalf("config: %+v", c)
	}
	for k, v := range map[string]string{"GITHUB_APP_ID": "x", "REVIEWER_TEAM": "4", "REVIEWER_DEPTH": "huge", "REVIEWER_TIMEOUT": "soon"} {
		bad := map[string]string{"GITHUB_APP_ID": "12", k: v}
		if _, err := load(func(k string) string { return bad[k] }); err == nil {
			t.Errorf("%s=%s was taken", k, v)
		}
	}
}
