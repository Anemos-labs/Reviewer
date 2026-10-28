package queue

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/anemos-labs/reviewer/internal/webhook"
)

// A push while a pull request is reviewed stops that review and the newest request runs next; requests that wait for the same
// pull request collapse into the newest one.
func TestNewestWins(t *testing.T) {
	started := make(chan webhook.Job, 10)
	var mu sync.Mutex
	var stopped []string
	release := make(chan struct{})
	q := New(1, func(ctx context.Context, j webhook.Job) {
		started <- j
		select {
		case <-ctx.Done():
			mu.Lock()
			stopped = append(stopped, j.Head)
			mu.Unlock()
		case <-release:
		}
	})
	defer q.Close()
	q.Add(webhook.Job{Repo: "o/r", Number: 1, Head: "a", Reason: "opened"})
	if j := <-started; j.Head != "a" {
		t.Fatalf("first: %+v", j)
	}
	q.Add(webhook.Job{Repo: "o/r", Number: 1, Head: "b", Reason: "synchronize"})
	q.Add(webhook.Job{Repo: "o/r", Number: 1, Head: "c", Reason: "synchronize"})
	select {
	case j := <-started:
		if j.Head != "c" {
			t.Fatalf("next: %+v", j)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the newest request did not run")
	}
	close(release)
	mu.Lock()
	defer mu.Unlock()
	if len(stopped) != 1 || stopped[0] != "a" {
		t.Fatalf("stopped: %v", stopped)
	}
}

// A command does not stop the review running: it waits for it.
func TestCommandWaits(t *testing.T) {
	started := make(chan string, 10)
	release := make(chan struct{})
	q := New(2, func(ctx context.Context, j webhook.Job) {
		started <- j.Head
		select {
		case <-ctx.Done():
			started <- "stopped " + j.Head
		case <-release:
		}
	})
	defer q.Close()
	q.Add(webhook.Job{Repo: "o/r", Number: 1, Head: "a", Reason: "opened"})
	<-started
	q.Add(webhook.Job{Repo: "o/r", Number: 1, Head: "a", Reason: "command"})
	select {
	case s := <-started:
		t.Fatalf("ran while the first was running: %s", s)
	case <-time.After(200 * time.Millisecond):
	}
	release <- struct{}{}
	if s := <-started; s != "a" {
		t.Fatalf("after: %s", s)
	}
	close(release)
}
