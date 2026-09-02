// Package podiq is the HTTP client for the PodIQ organization-data provider.
//
// It speaks only the provider-neutral snapshot contract (pkg/orgsnapshot) and
// deliberately knows nothing about THC's domain models or database, so swapping
// PodIQ for another provider means adding a sibling package, not editing the
// sync service.
package podiq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agopalakrishnan/teams360/backend/pkg/orgsnapshot"
)

const (
	// EnvBaseURL names the environment variable holding the PodIQ origin.
	EnvBaseURL = "PODIQ_BASE_URL"

	// snapshotPath is the provider's existing endpoint. It is not configurable:
	// the contract, not the deployment, decides where the snapshot lives.
	snapshotPath = "/data_provider"

	// requestTimeout bounds a single snapshot fetch. Snapshots run to a few MB
	// for a large org, so this is longer than the 10s used for OAuth token
	// exchange in sso_handler.go.
	requestTimeout = 30 * time.Second

	// maxResponseBytes caps how much we will read from the provider, so a
	// misbehaving or hostile upstream cannot exhaust memory.
	maxResponseBytes = 64 << 20 // 64 MiB
)

// ErrNotConfigured is returned when PODIQ_BASE_URL is unset.
var ErrNotConfigured = errors.New("PodIQ base URL is not configured")

// Config holds PodIQ settings loaded from the environment.
type Config struct {
	BaseURL string
}

// LoadConfig reads PodIQ settings from the environment.
// Returns nil if PODIQ_BASE_URL is not set, following the nil-means-disabled
// convention used by email.LoadConfig.
func LoadConfig() *Config {
	baseURL := os.Getenv(EnvBaseURL)
	if baseURL == "" {
		return nil
	}
	return &Config{BaseURL: strings.TrimRight(baseURL, "/")}
}

// Client fetches organization snapshots from PodIQ.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds a Client. A nil config yields a client whose calls fail with
// ErrNotConfigured, so callers can construct unconditionally and report the
// misconfiguration at request time rather than crashing at startup.
func NewClient(cfg *Config) *Client {
	c := &Client{http: &http.Client{Timeout: requestTimeout}}
	if cfg != nil {
		c.baseURL = cfg.BaseURL
	}
	return c
}

// Configured reports whether a base URL is present.
func (c *Client) Configured() bool {
	return c != nil && c.baseURL != ""
}

// FetchSnapshot calls GET {baseURL}/data_provider with a bearer token and
// decodes the provider-neutral snapshot.
//
// Errors never include the response body or the token. An upstream failure can
// echo request headers or embed record data, and this error text reaches an
// admin's browser — so only the status code is reported.
func (c *Client) FetchSnapshot(ctx context.Context, token string) (*orgsnapshot.Snapshot, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	if token == "" {
		return nil, errors.New("PodIQ API token is empty")
	}

	url := c.baseURL + snapshotPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build provider request: %w", err)
	}
	// PodIQ reads its personal access token from x-api-key; an Authorization
	// bearer is treated as an interactive session JWT and rejected. Both are
	// sent so the client also works against a provider that expects a bearer,
	// which costs nothing: each side reads its own header and ignores the other.
	req.Header.Set("x-api-key", token)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// net/http wraps the request URL into its errors. That is safe here on
		// two counts: the token travels in a header rather than the query
		// string, and net/http redacts any userinfo password in PODIQ_BASE_URL
		// before building the error. Never move the token into the URL.
		return nil, fmt.Errorf("provider request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Drain a bounded amount so the connection can be reused, but discard it.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}

	var snapshot orgsnapshot.Snapshot
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes))
	if err := decoder.Decode(&snapshot); err != nil {
		// The decoder error can quote the offending JSON fragment, which is
		// provider record data. Report the shape of the failure only.
		return nil, errors.New("provider response was not a valid organization snapshot")
	}

	return &snapshot, nil
}
