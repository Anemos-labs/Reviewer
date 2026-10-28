// Package queue runs reviews with a fixed number of workers. A pull request has at most one review waiting and one running: a
// newer request replaces the waiting one, and stops the running one when it is for a newer commit, since its result would be
// stale before it is posted.
package queue

import (
	"context"
	"strconv"
	"sync"

	"github.com/anemos-labs/reviewer/internal/webhook"
)

// Queue holds the waiting reviews, oldest pull request first.
type Queue struct {
	run func(context.Context, webhook.Job)

	mu      sync.Mutex
	cond    *sync.Cond
	order   []string
	waiting map[string]webhook.Job
	running map[string]context.CancelFunc
	closed  bool
	wg      sync.WaitGroup
}

// New starts workers that each run one review at a time with run.
func New(workers int, run func(context.Context, webhook.Job)) *Queue {
	q := &Queue{run: run, waiting: map[string]webhook.Job{}, running: map[string]context.CancelFunc{}}
	q.cond = sync.NewCond(&q.mu)
	for range max(workers, 1) {
		q.wg.Add(1)
		go q.work()
	}
	return q
}

func key(j webhook.Job) string { return j.Repo + "#" + strconv.Itoa(j.Number) }

// Add queues a review. A command always counts as new work; a push stops the review running for the same pull request.
func (q *Queue) Add(j webhook.Job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	k := key(j)
	if _, ok := q.waiting[k]; !ok {
		q.order = append(q.order, k)
	}
	q.waiting[k] = j
	if stop, ok := q.running[k]; ok && j.Reason == "synchronize" {
		stop()
	}
	q.cond.Signal()
}

// Len is how many reviews wait.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.order)
}

// Close stops taking reviews, stops the running ones and waits for the workers.
func (q *Queue) Close() {
	q.mu.Lock()
	q.closed = true
	for _, stop := range q.running {
		stop()
	}
	q.cond.Broadcast()
	q.mu.Unlock()
	q.wg.Wait()
}

func (q *Queue) work() {
	defer q.wg.Done()
	for {
		q.mu.Lock()
		var j webhook.Job
		var k string
		for {
			if q.closed {
				q.mu.Unlock()
				return
			}
			if i := q.next(); i >= 0 {
				k = q.order[i]
				j = q.waiting[k]
				q.order = append(q.order[:i], q.order[i+1:]...)
				delete(q.waiting, k)
				break
			}
			q.cond.Wait()
		}
		ctx, stop := context.WithCancel(context.Background())
		q.running[k] = stop
		q.mu.Unlock()

		q.run(ctx, j)

		q.mu.Lock()
		stop()
		delete(q.running, k)
		q.cond.Broadcast()
		q.mu.Unlock()
	}
}

// next is the first waiting pull request with no review running, or -1.
func (q *Queue) next() int {
	for i, k := range q.order {
		if _, busy := q.running[k]; !busy {
			return i
		}
	}
	return -1
}
