// Package dataprovider is the HTTP client for the external organization-data
// provider integration.
//
// It speaks only the provider-neutral snapshot contract (pkg/orgsnapshot) and
// deliberately knows nothing about THC's domain models or database, so
// swapping the underlying provider means adding a sibling package, not
// editing the sync service.
package dataprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/agopalakrishnan/teams360/backend/pkg/orgsnapshot"
)

const (
	// EnvBaseURL and EnvAPIToken name the environment variables the data
	// provider configuration is read from. Both are configuration keys
	// only -- the value read from EnvAPIToken is sent to the provider in the
	// x-api-key header, never as a bearer token and never under a header
	// named after the variable.
	EnvBaseURL  = "DATA_PROVIDER_BASE_URL"
	EnvAPIToken = "DATA_PROVIDER_API_TOKEN"

	// snapshotPath is the provider's deployed endpoint. It is not
	// configurable: the contract, not the deployment, decides where the
	// snapshot lives.
	snapshotPath = "/org-snapshot"

	// apiKeyHeader is the only credential header the provider's deployed
	// authentication middleware reads for a machine credential. A personal
	// access token in this header resolves to its owning user and is
	// restricted to safe (read-only) HTTP methods; a request without this
	// header falls through to the provider's interactive SSO/JWT path via
	// Authorization: Bearer, which a raw API token would fail. Do not add
	// Authorization or any other header for this request.
	apiKeyHeader = "x-api-key"

	// requestTimeout bounds a single snapshot fetch. Snapshots run to a few MB
	// for a large org.
	requestTimeout = 30 * time.Second

	// maxResponseBytes caps how much we will read from the provider, so a
	// misbehaving or hostile upstream cannot exhaust memory.
	maxResponseBytes = 64 << 20 // 64 MiB
)

// ErrNilConfig is returned by NewClient when no configuration is supplied.
var ErrNilConfig = errors.New("dataprovider: config must not be nil")

// ErrEmptyToken is returned by NewClient when the configured token is empty.
var ErrEmptyToken = errors.New("dataprovider: API token must not be empty")

// Config holds data provider settings loaded from the environment.
type Config struct {
	BaseURL  string
	APIToken string
}

// LoadConfig reads data provider settings from the environment.
//
// Returns (nil, nil) when EnvBaseURL is unset, following the nil-means-disabled
// convention used elsewhere in this codebase (email.LoadConfig). Returns an
// error when the base URL is set but the token is missing, since that
// configuration is incomplete and syncing would send an unauthenticated
// request.
func LoadConfig() (*Config, error) {
	baseURL := os.Getenv(EnvBaseURL)
	if baseURL == "" {
		return nil, nil
	}
	token := os.Getenv(EnvAPIToken)
	if token == "" {
		return nil, fmt.Errorf("dataprovider: %s is required when %s is set", EnvAPIToken, EnvBaseURL)
	}
	return &Config{BaseURL: strings.TrimRight(baseURL, "/"), APIToken: token}, nil
}

// Client fetches organization snapshots from the configured data provider.
type Client struct {
	baseURL *url.URL
	token   string
	http    *http.Client
}

// NewClient builds a validated Client. It rejects a nil config, an empty
// token, and a base URL that is not absolute, not http/https, or has no
// hostname -- all at construction time, so a misconfigured deployment fails
// loudly at startup rather than on the first sync attempt.
func NewClient(cfg *Config) (*Client, error) {
	if cfg == nil {
		return nil, ErrNilConfig
	}
	if strings.TrimSpace(cfg.APIToken) == "" {
		return nil, ErrEmptyToken
	}

	baseURL, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("dataprovider: invalid base URL %q: %w", cfg.BaseURL, err)
	}
	if !baseURL.IsAbs() || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Hostname() == "" {
		return nil, fmt.Errorf("dataprovider: base URL must be an absolute http(s) URL with a hostname, got %q", cfg.BaseURL)
	}

	return &Client{
		baseURL: baseURL,
		token:   cfg.APIToken,
		http: &http.Client{
			Timeout:       requestTimeout,
			CheckRedirect: rejectCrossOriginRedirect,
		},
	}, nil
}

// rejectCrossOriginRedirect stops the client from following a redirect to a
// different host than the original request, so the x-api-key header (which
// net/http forwards on same-origin redirects) can never leak to another host.
func rejectCrossOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	origin := via[0].URL
	if req.URL.Scheme != origin.Scheme || req.URL.Host != origin.Host {
		return fmt.Errorf("dataprovider: refusing cross-origin redirect from %s to %s", origin, req.URL)
	}
	return nil
}

// FetchSnapshot calls GET {baseURL}/org-snapshot with the configured x-api-key
// and decodes the provider-neutral snapshot.
//
// Errors never include the response body or the token. An upstream failure can
// echo request headers or embed record data, and this error text reaches an
// admin's browser -- so only the status code is reported.
func (c *Client) FetchSnapshot(ctx context.Context) (*orgsnapshot.Snapshot, error) {
	target := c.baseURL.String() + snapshotPath

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("dataprovider: failed to build provider request: %w", err)
	}
	req.Header.Set(apiKeyHeader, c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// net/http wraps the request URL into its errors. That is safe here: the
		// token travels in a header rather than the query string.
		return nil, fmt.Errorf("dataprovider: provider request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Drain a bounded amount so the connection can be reused, but discard it.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("dataprovider: provider returned HTTP %d", resp.StatusCode)
	}

	var snapshot orgsnapshot.Snapshot
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes))
	if err := decoder.Decode(&snapshot); err != nil {
		// The decoder error can quote the offending JSON fragment, which is
		// provider record data. Report the shape of the failure only.
		return nil, errors.New("dataprovider: provider response was not a valid organization snapshot")
	}

	return &snapshot, nil
}
