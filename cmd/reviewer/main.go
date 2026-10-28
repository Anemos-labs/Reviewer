// Command reviewer is the pull request review service: a GitHub App webhook receiver with a queue of Sleipnir reviews.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/anemos-labs/reviewer/internal/github"
	"github.com/anemos-labs/reviewer/internal/queue"
	"github.com/anemos-labs/reviewer/internal/review"
	"github.com/anemos-labs/reviewer/internal/webhook"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("reviewer", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := load(os.Getenv)
	if err != nil {
		return err
	}
	key, err := secret("GITHUB_APP_PRIVATE_KEY")
	if err != nil {
		return err
	}
	app, err := github.NewApp(cfg.appID, key, os.Getenv("GITHUB_API_URL"))
	if err != nil {
		return err
	}
	hook, err := secret("GITHUB_WEBHOOK_SECRET")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.review.Work, 0o755); err != nil {
		return err
	}
	runner := &review.Runner{App: app, Cfg: cfg.review, Log: log}
	q := queue.New(cfg.workers, runner.Run)
	mux := http.NewServeMux()
	mux.Handle("/webhook", &webhook.Handler{Secret: []byte(strings.TrimSpace(string(hook))), Bot: cfg.review.Bot, Enqueue: q.Add, Log: log})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "ok %d waiting\n", q.Len())
	})
	srv := &http.Server{Addr: cfg.addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("listening", "addr", cfg.addr, "workers", cfg.workers, "team", cfg.review.Team, "depth", cfg.review.Depth)
	err = srv.ListenAndServe()
	q.Close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type config struct {
	addr    string
	appID   int64
	workers int
	review  review.Config
}

// load reads the configuration from the environment (see README.md).
func load(get func(string) string) (config, error) {
	def := func(k, d string) string {
		if v := strings.TrimSpace(get(k)); v != "" {
			return v
		}
		return d
	}
	var c config
	var err error
	if c.appID, err = strconv.ParseInt(get("GITHUB_APP_ID"), 10, 64); err != nil || c.appID <= 0 {
		return c, errors.New("GITHUB_APP_ID is the GitHub App's numeric id")
	}
	c.addr = def("REVIEWER_ADDR", ":8080")
	if c.workers, err = strconv.Atoi(def("REVIEWER_WORKERS", "1")); err != nil || c.workers < 1 {
		return c, errors.New("REVIEWER_WORKERS is a number of 1 or more")
	}
	r := review.Config{
		Work:     def("REVIEWER_WORK", "/srv/reviewer/work"),
		Sleipnir: def("SLEIPNIR_BIN", "sleipnir"),
		Depth:    def("REVIEWER_DEPTH", "standard"),
		Model:    get("REVIEWER_MODEL"),
		Image:    get("REVIEWER_SANDBOX_IMAGE"),
		Network:  def("REVIEWER_SANDBOX_NETWORK", "false") == "true",
		Bot:      def("REVIEWER_BOT", "reviewer"),
		Keep:     def("REVIEWER_KEEP", "false") == "true",
		Env:      fields(def("REVIEWER_PASS_ENV", "HEIMDALLM_API_KEY")),
		Roles:    fields(get("REVIEWER_ROLE_MODELS")),
	}
	if r.Team, err = strconv.Atoi(def("REVIEWER_TEAM", "8")); err != nil || r.Team < 2 || r.Team > 32 {
		return c, errors.New("REVIEWER_TEAM is a number from 2 to 32")
	}
	if r.Budget, err = strconv.ParseFloat(def("REVIEWER_BUDGET_USD", "4"), 64); err != nil || r.Budget < 0 {
		return c, errors.New("REVIEWER_BUDGET_USD is an amount in dollars")
	}
	if r.Timeout, err = time.ParseDuration(def("REVIEWER_TIMEOUT", "90m")); err != nil || r.Timeout <= 0 {
		return c, errors.New("REVIEWER_TIMEOUT is a duration such as 90m")
	}
	switch r.Depth {
	case "survey", "standard", "deep":
	default:
		return c, errors.New("REVIEWER_DEPTH is survey, standard or deep")
	}
	c.review = r
	return c, nil
}

// secret reads NAME, or the file NAME_FILE names (for Docker secrets).
func secret(name string) ([]byte, error) {
	if p := os.Getenv(name + "_FILE"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("%s_FILE: %w", name, err)
		}
		return b, nil
	}
	if v := os.Getenv(name); v != "" {
		return []byte(strings.ReplaceAll(v, `\n`, "\n")), nil
	}
	return nil, fmt.Errorf("set %s or %s_FILE", name, name)
}

func fields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' })
}
