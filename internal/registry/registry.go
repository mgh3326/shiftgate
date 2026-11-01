// Package registry resolves an image tag to its manifest digest through the
// OCI distribution HTTP API, so `shiftgate follow` can notice a new push
// without the registry or CI ever reaching the host.
package registry

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Credentials authenticate to a private registry.
type Credentials struct {
	Username string
	Password string // password or access token
}

// LoadCredentials reads an env-style file with REGISTRY_USERNAME and
// REGISTRY_PASSWORD lines. Other keys, blank lines and # comments are
// ignored. Values are never logged.
func LoadCredentials(path string) (*Credentials, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("registry_auth_file: %w", err)
	}
	defer f.Close()
	var c Credentials
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		switch strings.TrimSpace(k) {
		case "REGISTRY_USERNAME":
			c.Username = v
		case "REGISTRY_PASSWORD":
			c.Password = v
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("registry_auth_file %s: %w", path, err)
	}
	if c.Username == "" || c.Password == "" {
		return nil, fmt.Errorf("registry_auth_file %s: needs REGISTRY_USERNAME and REGISTRY_PASSWORD", path)
	}
	return &c, nil
}

// Client talks to registries over HTTPS.
type Client struct {
	// HTTP is the client to use (default: 30s timeout).
	HTTP *http.Client
	// Endpoint maps a registry host to its base URL. Nil means
	// https://<host>, with docker.io mapped to registry-1.docker.io.
	Endpoint func(host string) string
}

var (
	tagRE    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// manifestAccept lists every manifest type a tag may point at. Multi-arch
// images resolve to the index digest, which is what `docker pull
// image@digest` (and a CI push) uses.
var manifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// ValidTag reports whether tag is a valid image tag.
func ValidTag(tag string) bool { return tagRE.MatchString(tag) }

// Split returns the registry host and repository path of image
// (a repository without tag or digest). Docker Hub names get docker.io and
// the library/ prefix.
func Split(image string) (host, repo string) {
	first, rest, ok := strings.Cut(image, "/")
	if ok && (first == "docker.io" || first == "index.docker.io") {
		return Split(rest) // docker.io/nginx is library/nginx, like docker pull
	}
	if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return first, rest
	}
	if !ok {
		return "docker.io", "library/" + image
	}
	return "docker.io", image
}

func (c Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c Client) base(host string) string {
	if c.Endpoint != nil {
		return strings.TrimRight(c.Endpoint(host), "/")
	}
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	return "https://" + host
}

// Digest resolves image:tag to the manifest digest the registry serves.
func (c Client) Digest(ctx context.Context, image, tag string, cred *Credentials) (string, error) {
	if !ValidTag(tag) {
		return "", fmt.Errorf("invalid tag %q", tag)
	}
	host, repo := Split(image)
	u := c.base(host) + "/v2/" + repo + "/manifests/" + tag
	auth := ""
	for attempt := 0; ; attempt++ {
		resp, err := c.manifest(ctx, http.MethodHead, u, auth)
		if err != nil {
			return "", err
		}
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			auth, err = c.authorize(ctx, resp.Header.Get("WWW-Authenticate"), repo, cred)
			if err != nil {
				return "", err
			}
			continue
		case resp.StatusCode == http.StatusNotFound:
			return "", fmt.Errorf("%s:%s not found in the registry", image, tag)
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return "", fmt.Errorf("registry refused %s:%s (HTTP %d); check registry_auth_file", image, tag, resp.StatusCode)
		case resp.StatusCode != http.StatusOK:
			return "", fmt.Errorf("registry answered HTTP %d for %s:%s", resp.StatusCode, image, tag)
		}
		if d := resp.Header.Get("Docker-Content-Digest"); digestRE.MatchString(d) {
			return d, nil
		}
		// Some registries omit the header on HEAD: hash the manifest itself.
		return c.hashManifest(ctx, u, auth)
	}
}

func (c Client) manifest(ctx context.Context, method, u, auth string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", manifestAccept)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry: %w", stripURL(err))
	}
	return resp, nil
}

func (c Client) hashManifest(ctx context.Context, u, auth string) (string, error) {
	resp, err := c.manifest(ctx, http.MethodGet, u, auth)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry answered HTTP %d for the manifest", resp.StatusCode)
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(resp.Body, 4<<20)); err != nil {
		return "", fmt.Errorf("registry: %w", stripURL(err))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// authorize answers a 401 challenge: a bearer token from the realm
// (anonymous unless cred is set), or basic auth when the registry asks.
func (c Client) authorize(ctx context.Context, challenge, repo string, cred *Credentials) (string, error) {
	scheme, params := parseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "basic":
		if cred == nil {
			return "", errors.New("registry needs credentials (set registry_auth_file)")
		}
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.SetBasicAuth(cred.Username, cred.Password)
		return req.Header.Get("Authorization"), nil
	case "bearer":
	default:
		return "", fmt.Errorf("registry sent an unsupported auth challenge %q", scheme)
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Scheme != "https" || realm.Host == "" {
		return "", errors.New("registry token realm must be an https URL")
	}
	q := realm.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + repo + ":pull"
	}
	q.Set("scope", scope)
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	if cred != nil {
		req.SetBasicAuth(cred.Username, cred.Password)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return "", fmt.Errorf("registry token: %w", stripURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry token endpoint answered HTTP %d", resp.StatusCode)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return "", fmt.Errorf("registry token: %w", err)
	}
	t := tok.Token
	if t == "" {
		t = tok.AccessToken
	}
	if t == "" {
		return "", errors.New("registry token endpoint returned no token")
	}
	return "Bearer " + t, nil
}

// parseChallenge parses `Bearer realm="…",service="…",scope="…"`.
func parseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params := map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; {
		k, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		var v string
		if strings.HasPrefix(after, `"`) {
			end := strings.Index(after[1:], `"`)
			if end < 0 {
				break
			}
			v, rest = after[1:1+end], after[2+end:]
		} else {
			v, rest, _ = strings.Cut(after, ",")
		}
		params[k] = v
		rest = strings.TrimLeft(strings.TrimSpace(rest), ",")
		rest = strings.TrimSpace(rest)
	}
	return scheme, params
}

// stripURL drops the request URL from a transport error; the query of a
// token request may name the repository and tokens never appear in URLs,
// but keeping URLs out of messages is the simple rule.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
