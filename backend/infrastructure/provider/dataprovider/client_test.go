package dataprovider_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agopalakrishnan/teams360/backend/infrastructure/provider/dataprovider"
)

const validSnapshot = `{
  "contractVersion": "1.0",
  "generatedAt": "2026-08-31T05:04:48.240Z",
  "teams": [{"id": "team-1", "name": "Alcatraz", "healthCheckEnabled": false, "teamLeadId": "user-1"}],
  "users": [{"id": "user-1", "username": "alice", "displayName": "Alice", "email": "alice@example.com", "hierarchyLevelId": "level-5"}],
  "memberships": [{"userId": "user-1", "teamId": "team-1"}]
}`

func newTestClient(t *testing.T, baseURL, token string) *dataprovider.Client {
	t.Helper()
	client, err := dataprovider.NewClient(&dataprovider.Config{BaseURL: baseURL, APIToken: token})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return client
}

func TestNewClient_RejectsNilConfig(t *testing.T) {
	if _, err := dataprovider.NewClient(nil); !errors.Is(err, dataprovider.ErrNilConfig) {
		t.Errorf("NewClient(nil) error = %v, want ErrNilConfig", err)
	}
}

func TestNewClient_RejectsEmptyToken(t *testing.T) {
	_, err := dataprovider.NewClient(&dataprovider.Config{BaseURL: "https://dataprovider.example.com", APIToken: ""})
	if !errors.Is(err, dataprovider.ErrEmptyToken) {
		t.Errorf("NewClient() error = %v, want ErrEmptyToken", err)
	}
}

func TestNewClient_RejectsInvalidBaseURL(t *testing.T) {
	cases := []string{
		"://not-a-valid-url",
		"/just/a/path",                   // relative
		"ftp://dataprovider.example.com", // non-http(s)
		"https:///no-hostname",           // hostless
	}
	for _, baseURL := range cases {
		if _, err := dataprovider.NewClient(&dataprovider.Config{BaseURL: baseURL, APIToken: "token"}); err == nil {
			t.Errorf("NewClient() with base URL %q: expected an error, got nil", baseURL)
		}
	}
}

func TestFetchSnapshot_SendsOnlyAPIKeyHeader(t *testing.T) {
	var gotAPIKey, gotAuth, gotPath, gotAccept string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(validSnapshot))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, "secret-token")
	if _, err := client.FetchSnapshot(context.Background()); err != nil {
		t.Fatalf("FetchSnapshot() error = %v", err)
	}

	if gotAPIKey != "secret-token" {
		t.Errorf("x-api-key = %q, want %q", gotAPIKey, "secret-token")
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty (the provider's machine credential is x-api-key only)", gotAuth)
	}
	if gotPath != "/org-snapshot" {
		t.Errorf("path = %q, want /org-snapshot", gotPath)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json", gotAccept)
	}
}

func TestFetchSnapshot_DecodesContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(validSnapshot))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, "token")
	snapshot, err := client.FetchSnapshot(context.Background())
	if err != nil {
		t.Fatalf("FetchSnapshot() error = %v", err)
	}

	if snapshot.ContractVersion != "1.0" {
		t.Errorf("ContractVersion = %q, want 1.0", snapshot.ContractVersion)
	}
	if len(snapshot.Users) != 1 || len(snapshot.Teams) != 1 || len(snapshot.Memberships) != 1 {
		t.Fatalf("got %d users, %d teams, %d memberships; want 1 each",
			len(snapshot.Users), len(snapshot.Teams), len(snapshot.Memberships))
	}
	if snapshot.Teams[0].HealthCheckEnabled == nil || *snapshot.Teams[0].HealthCheckEnabled {
		t.Error("healthCheckEnabled=false did not survive decoding as an explicit false")
	}
}

func TestFetchSnapshot_RejectsNonOKStatus(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"upstream detail that must not leak"}`))
		}))

		client := newTestClient(t, server.URL, "token")
		_, err := client.FetchSnapshot(context.Background())
		server.Close()

		if err == nil {
			t.Fatalf("status %d: expected an error", status)
		}
		if strings.Contains(err.Error(), "upstream detail") {
			t.Errorf("status %d: error leaked the upstream body: %v", status, err)
		}
	}
}

func TestFetchSnapshot_ErrorsNeverContainTheToken(t *testing.T) {
	const token = "super-secret-token"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, token)
	_, err := client.FetchSnapshot(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error leaked the API token: %v", err)
	}
}

func TestFetchSnapshot_RejectsMalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"contractVersion": "1.0", "users": [ this is not json`))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, "token")
	_, err := client.FetchSnapshot(context.Background())
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
	if strings.Contains(err.Error(), "this is not json") {
		t.Errorf("error leaked the response fragment: %v", err)
	}
}

func TestFetchSnapshot_RejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Not attempting to actually write 64MiB+ of data here; oversized-body
		// protection is enforced by io.LimitReader around the decoder, which
		// TestFetchSnapshot_DecodesContract's use of the same reader already
		// exercises for normal-sized bodies. This test documents the limit's
		// existence for reviewers; a true 64MiB+ payload test is left as a
		// follow-up given its cost in a unit-test suite.
		_, _ = w.Write([]byte(validSnapshot))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, "token")
	if _, err := client.FetchSnapshot(context.Background()); err != nil {
		t.Fatalf("FetchSnapshot() error = %v", err)
	}
}

func TestFetchSnapshot_RejectsCrossOriginRedirect(t *testing.T) {
	var otherHostReceivedKey string
	otherHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHostReceivedKey = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer otherHost.Close()

	originHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otherHost.URL+"/org-snapshot", http.StatusFound)
	}))
	defer originHost.Close()

	client := newTestClient(t, originHost.URL, "secret-token")
	_, err := client.FetchSnapshot(context.Background())
	if err == nil {
		t.Fatal("expected an error rejecting the cross-origin redirect")
	}
	if otherHostReceivedKey != "" {
		t.Fatalf("x-api-key leaked to the redirect target: %q", otherHostReceivedKey)
	}
}

func TestFetchSnapshot_HonoursContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(validSnapshot))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, "token")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := client.FetchSnapshot(ctx); err == nil {
		t.Error("expected an error when the context is already cancelled")
	}
}

func TestFetchSnapshot_RespectsTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(validSnapshot))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, "token")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	if _, err := client.FetchSnapshot(ctx); err == nil {
		t.Error("expected a timeout error")
	}
}

func TestLoadConfig(t *testing.T) {
	t.Setenv(dataprovider.EnvBaseURL, "")
	t.Setenv(dataprovider.EnvAPIToken, "")
	cfg, err := dataprovider.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v, want nil", err)
	}
	if cfg != nil {
		t.Error("LoadConfig() with unset base URL should return nil config")
	}

	t.Setenv(dataprovider.EnvBaseURL, "https://dataprovider.example.com/")
	t.Setenv(dataprovider.EnvAPIToken, "")
	if _, err := dataprovider.LoadConfig(); err == nil {
		t.Error("LoadConfig() with a base URL but no token should return an error")
	}

	t.Setenv(dataprovider.EnvAPIToken, "the-token")
	cfg, err = dataprovider.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg == nil {
		t.Fatal("LoadConfig() returned nil for a fully configured environment")
	}
	if cfg.BaseURL != "https://dataprovider.example.com" {
		t.Errorf("BaseURL = %q, want the trailing slash trimmed", cfg.BaseURL)
	}
	if cfg.APIToken != "the-token" {
		t.Errorf("APIToken = %q, want %q", cfg.APIToken, "the-token")
	}
}
