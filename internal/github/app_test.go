package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testApp(t *testing.T, api string) *App {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	a, err := NewApp(42, pemKey, api)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestJWTIsSignedByTheApp(t *testing.T) {
	a := testApp(t, "")
	tok, err := a.JWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt: %s", tok)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&a.Key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature: %v", err)
	}
	var claims map[string]any
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(b, &claims)
	if claims["iss"] != "42" || claims["exp"].(float64)-claims["iat"].(float64) != 600 {
		t.Fatalf("claims: %v", claims)
	}
	if _, err := NewApp(1, []byte("not pem"), ""); err == nil {
		t.Error("a key that is not PEM was taken")
	}
}

func TestInstallationTokenIsReused(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/7/access_tokens" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "unexpected "+r.URL.Path, 400)
			return
		}
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_x", "expires_at": time.Now().Add(time.Hour)})
	}))
	defer srv.Close()
	a := testApp(t, srv.URL)
	for range 3 {
		c, err := a.Client(context.Background(), 7)
		if err != nil || c.Token != "ghs_x" {
			t.Fatalf("client: %v %+v", err, c)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("token asked %d times", calls.Load())
	}
}
