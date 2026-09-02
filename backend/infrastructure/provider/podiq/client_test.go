package podiq_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agopalakrishnan/teams360/backend/infrastructure/provider/podiq"
)

const validSnapshot = `{
  "contractVersion": "1.0",
  "generatedAt": "2026-08-31T05:04:48.240Z",
  "teams": [{"id": "team-1", "name": "Alcatraz", "healthCheckEnabled": false, "teamLeadId": "user-1"}],
  "users": [{"id": "user-1", "username": "alice", "displayName": "Alice", "email": "alice@example.com", "hierarchyLevelId": "level-5"}],
  "memberships": [{"userId": "user-1", "teamId": "team-1"}]
}`

// newTestClient points a client at a stub provider, mirroring how the SSO tests
// stand up a fake token endpoint.
func newTestClient(t *testing.T, handler http.HandlerFunc) *podiq.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return podiq.NewClient(&podiq.Config{BaseURL: server.URL})
}

func TestFetchSnapshotSendsToken(t *testing.T) {
	var gotAPIKey, gotAuth, gotPath, gotAccept string

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(validSnapshot))
	})

	if _, err := client.FetchSnapshot(context.Background(), "secret-token"); err != nil {
		t.Fatalf("FetchSnapshot() error = %v", err)
	}

	// PodIQ authenticates a personal access token from this header; a bearer
	// alone reaches its interactive session path and is rejected.
	if gotAPIKey != "secret-token" {
		t.Errorf("x-api-key = %q, want %q", gotAPIKey, "secret-token")
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer secret-token")
	}
	if gotPath != "/data_provider" {
		t.Errorf("path = %q, want /data_provider", gotPath)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json", gotAccept)
	}
}

// TestFetchSnapshotAuthenticatesAgainstAPIKeyOnlyProvider models PodIQ's actual
// middleware: it reads x-api-key and 401s anything else.
func TestFetchSnapshotAuthenticatesAgainstAPIKeyOnlyProvider(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "pat-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(validSnapshot))
	})

	snapshot, err := client.FetchSnapshot(context.Background(), "pat-token")
	if err != nil {
		t.Fatalf("FetchSnapshot() error = %v", err)
	}
	if snapshot == nil {
		t.Fatal("FetchSnapshot() returned no snapshot")
	}
}

func TestFetchSnapshotDecodesContract(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(validSnapshot))
	})

	snapshot, err := client.FetchSnapshot(context.Background(), "token")
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
	if snapshot.GeneratedAt.IsZero() {
		t.Error("GeneratedAt was not decoded")
	}
	if snapshot.Teams[0].HealthCheckEnabled == nil || *snapshot.Teams[0].HealthCheckEnabled {
		t.Error("healthCheckEnabled=false did not survive decoding as an explicit false")
	}
}

func TestFetchSnapshotRejectsNonOKStatus(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"upstream detail that must not leak"}`))
		})

		_, err := client.FetchSnapshot(context.Background(), "token")
		if err == nil {
			t.Fatalf("status %d: expected an error", status)
		}
		if strings.Contains(err.Error(), "upstream detail") {
			t.Errorf("status %d: error leaked the upstream body: %v", status, err)
		}
	}
}

func TestFetchSnapshotErrorsNeverContainTheToken(t *testing.T) {
	const token = "super-secret-token"

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	_, err := client.FetchSnapshot(context.Background(), token)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error leaked the API token: %v", err)
	}
}

func TestFetchSnapshotRejectsMalformedJSON(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"contractVersion": "1.0", "users": [ this is not json`))
	})

	_, err := client.FetchSnapshot(context.Background(), "token")
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
	if strings.Contains(err.Error(), "this is not json") {
		t.Errorf("error leaked the response fragment: %v", err)
	}
}

func TestFetchSnapshotRequiresConfiguration(t *testing.T) {
	client := podiq.NewClient(nil)

	if client.Configured() {
		t.Error("Configured() should be false without a base URL")
	}
	if _, err := client.FetchSnapshot(context.Background(), "token"); !errors.Is(err, podiq.ErrNotConfigured) {
		t.Errorf("FetchSnapshot() error = %v, want ErrNotConfigured", err)
	}
}

func TestFetchSnapshotRequiresToken(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("provider must not be called without a token")
	})

	if _, err := client.FetchSnapshot(context.Background(), ""); err == nil {
		t.Error("expected an error for an empty token")
	}
}

func TestFetchSnapshotHonoursContextCancellation(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(validSnapshot))
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := client.FetchSnapshot(ctx, "token"); err == nil {
		t.Error("expected an error when the context is already cancelled")
	}
}

func TestLoadConfig(t *testing.T) {
	t.Setenv(podiq.EnvBaseURL, "")
	if podiq.LoadConfig() != nil {
		t.Error("LoadConfig() with unset base URL should return nil")
	}

	t.Setenv(podiq.EnvBaseURL, "https://podiq.example.com/")
	cfg := podiq.LoadConfig()
	if cfg == nil {
		t.Fatal("LoadConfig() returned nil for a configured base URL")
	}
	if cfg.BaseURL != "https://podiq.example.com" {
		t.Errorf("BaseURL = %q, want the trailing slash trimmed", cfg.BaseURL)
	}
}
