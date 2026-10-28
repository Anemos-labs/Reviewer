// Package github is the little of GitHub's REST API the reviewer needs, authenticated as a GitHub App: a JWT signed with the
// app's private key buys a short-lived installation token for the repository a webhook came from.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// App is a GitHub App: its id and private key, and the API it talks to.
type App struct {
	ID   int64
	Key  *rsa.PrivateKey
	API  string // https://api.github.com unless GitHub Enterprise
	HTTP *http.Client

	mu     sync.Mutex
	tokens map[int64]token
	now    func() time.Time
}

type token struct {
	value   string
	expires time.Time
}

// NewApp reads the app's PEM private key (PKCS#1 as GitHub issues it, or PKCS#8).
func NewApp(id int64, pemKey []byte, api string) (*App, error) {
	blk, _ := pem.Decode(pemKey)
	if blk == nil {
		return nil, errors.New("the GitHub App private key is not PEM")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		key = k
	} else if k8, err8 := x509.ParsePKCS8PrivateKey(blk.Bytes); err8 == nil {
		rk, ok := k8.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("the GitHub App private key is not an RSA key")
		}
		key = rk
	} else {
		return nil, fmt.Errorf("the GitHub App private key cannot be read: %w", err)
	}
	if api == "" {
		api = "https://api.github.com"
	}
	return &App{ID: id, Key: key, API: strings.TrimRight(api, "/"), HTTP: &http.Client{Timeout: 30 * time.Second}, tokens: map[int64]token{}, now: time.Now}, nil
}

// JWT is the app's own token (RS256), valid for nine minutes from a minute ago, as GitHub asks to allow for clock drift.
func (a *App) JWT() (string, error) {
	now := a.now()
	head := b64(`{"alg":"RS256","typ":"JWT"}`)
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": strconv.FormatInt(a.ID, 10)})
	signing := head + "." + b64(string(claims))
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// Token is an installation token, reused until five minutes before it expires.
func (a *App) Token(ctx context.Context, installation int64) (string, error) {
	a.mu.Lock()
	if t, ok := a.tokens[installation]; ok && a.now().Add(5*time.Minute).Before(t.expires) {
		a.mu.Unlock()
		return t.value, nil
	}
	a.mu.Unlock()
	jwt, err := a.JWT()
	if err != nil {
		return "", err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := a.do(ctx, "Bearer "+jwt, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installation), nil, &out); err != nil {
		return "", fmt.Errorf("installation token: %w", err)
	}
	a.mu.Lock()
	a.tokens[installation] = token{out.Token, out.ExpiresAt}
	a.mu.Unlock()
	return out.Token, nil
}

// Client is the API as one installation.
func (a *App) Client(ctx context.Context, installation int64) (*Client, error) {
	t, err := a.Token(ctx, installation)
	if err != nil {
		return nil, err
	}
	return &Client{app: a, auth: "token " + t, Token: t}, nil
}

// Error is an API answer outside 2xx.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string { return fmt.Sprintf("GitHub API %d: %s", e.Status, e.Body) }

func (a *App) do(ctx context.Context, auth, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.API+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "anemos-reviewer")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return err
	}
	if res.StatusCode/100 != 2 {
		return &Error{Status: res.StatusCode, Body: strings.TrimSpace(string(data[:min(len(data), 2000)]))}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}
