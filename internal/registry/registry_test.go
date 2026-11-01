package registry_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mgh3326/shiftgate/internal/registry"
)

func TestSplit(t *testing.T) {
	for in, want := range map[string][2]string{
		"ghcr.io/example/web":          {"ghcr.io", "example/web"},
		"registry.example.test:5000/a": {"registry.example.test:5000", "a"},
		"localhost/a/b":                {"localhost", "a/b"},
		"nginx":                        {"docker.io", "library/nginx"},
		"example/web":                  {"docker.io", "example/web"},
		"docker.io/nginx":              {"docker.io", "library/nginx"},
		"docker.io/library/nginx":      {"docker.io", "library/nginx"},
		"index.docker.io/example/web":  {"docker.io", "example/web"},
	} {
		if h, r := registry.Split(in); h != want[0] || r != want[1] {
			t.Errorf("Split(%q) = %q %q, want %v", in, h, r, want)
		}
	}
}

// A registry that answers HEAD without Docker-Content-Digest and asks for
// basic auth: the client authenticates and hashes the manifest body.
func TestDigestBasicAuthAndHashFallback(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2}`)
	sum := sha256.Sum256(manifest)
	want := "sha256:" + hex.EncodeToString(sum[:])
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "robot" || p != "pw" {
			w.Header().Set("WWW-Authenticate", `Basic realm="reg"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
			t.Errorf("Accept lacks the OCI index type: %q", r.Header.Get("Accept"))
		}
		if r.Method == http.MethodGet {
			w.Write(manifest)
		}
	}))
	defer srv.Close()
	c := registry.Client{HTTP: srv.Client(), Endpoint: func(string) string { return srv.URL }}
	if _, err := c.Digest(context.Background(), "ghcr.io/a/b", "main", nil); err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("basic challenge without credentials: %v", err)
	}
	got, err := c.Digest(context.Background(), "ghcr.io/a/b", "main", &registry.Credentials{Username: "robot", Password: "pw"})
	if err != nil || got != want {
		t.Fatalf("digest %q %v, want %q", got, err, want)
	}
}

// The token realm must be https: a plain-http realm would expose credentials.
func TestDigestRefusesPlainHTTPRealm(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="http://auth.example.test/token",service="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := registry.Client{HTTP: srv.Client(), Endpoint: func(string) string { return srv.URL }}
	if _, err := c.Digest(context.Background(), "ghcr.io/a/b", "main", &registry.Credentials{Username: "u", Password: "p"}); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("want https realm refusal, got %v", err)
	}
}

func TestLoadCredentials(t *testing.T) {
	p := filepath.Join(t.TempDir(), "reg.env")
	os.WriteFile(p, []byte("export REGISTRY_USERNAME=\"robot\"\nOTHER=1\nREGISTRY_PASSWORD=tok=en\n"), 0o600)
	c, err := registry.LoadCredentials(p)
	if err != nil || c.Username != "robot" || c.Password != "tok=en" {
		t.Fatalf("%+v %v", c, err)
	}
	os.WriteFile(p, []byte("REGISTRY_USERNAME=robot\n"), 0o600)
	if _, err := registry.LoadCredentials(p); err == nil {
		t.Fatal("missing password must be an error")
	}
}
