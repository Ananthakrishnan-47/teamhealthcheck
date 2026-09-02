package integration_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/agopalakrishnan/teams360/backend/application/services"
	"github.com/agopalakrishnan/teams360/backend/infrastructure/persistence/postgres"
	"github.com/agopalakrishnan/teams360/backend/infrastructure/provider/podiq"
	v1 "github.com/agopalakrishnan/teams360/backend/interfaces/api/v1"
	"github.com/agopalakrishnan/teams360/backend/pkg/tokencrypto"
	"github.com/agopalakrishnan/teams360/backend/tests/testhelpers"
	"github.com/gin-gonic/gin"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// providerAPIToken is the plaintext token the fake provider expects.
const providerAPIToken = "podiq-integration-test-token"

// snapshotWithLevels exercises the shapes the real PodIQ payload contains:
// importable people, an exec on a level THC does not configure, a blank-level
// test account, an explicit healthCheckEnabled=false, and a lead who is a member.
const snapshotWithLevels = `{
  "contractVersion": "1.0",
  "generatedAt": "2026-08-31T05:04:48.240Z",
  "teams": [
    {"id": "sync-team-1", "name": "Alcatraz", "healthCheckEnabled": false, "teamLeadId": "sync-user-1"},
    {"id": "sync-team-2", "name": "Alleppey", "healthCheckEnabled": true}
  ],
  "users": [
    {"id": "sync-user-1", "username": "syncalice", "displayName": "Alice", "email": "syncalice@test.com", "hierarchyLevelId": "level-3"},
    {"id": "sync-user-2", "username": "syncbob", "displayName": "Bob", "email": "syncbob@test.com", "hierarchyLevelId": "level-5", "reportsToId": "sync-user-1"},
    {"id": "sync-exec", "username": "syncceo", "displayName": "Chief", "email": "syncceo@test.com", "hierarchyLevelId": "level-0"},
    {"id": "sync-e2e", "username": "synce2e", "displayName": "E2E", "email": "synce2e@test.com", "hierarchyLevelId": ""}
  ],
  "memberships": [
    {"userId": "sync-user-1", "teamId": "sync-team-1"},
    {"userId": "sync-user-2", "teamId": "sync-team-1"},
    {"userId": "sync-user-2", "teamId": "sync-team-2"}
  ]
}`

var _ = Describe("Integration: Organization Provider Sync", func() {
	var (
		db             *sql.DB
		router         *gin.Engine
		cleanup        func()
		adminToken     string
		memberToken    string
		providerServer *httptest.Server

		// providerBody and providerStatus let each spec shape the upstream reply.
		providerBody   string
		providerStatus int
		// providerGate, when non-nil, blocks the provider until closed.
		providerGate chan struct{}
		// providerHit signals that the provider received a request.
		providerHit chan struct{}
	)

	encryptionKey := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

	// storeToken saves an encrypted provider token, as the settings endpoint would.
	storeToken := func(token string) {
		cipher, err := tokencrypto.NewFromBase64(encryptionKey)
		Expect(err).NotTo(HaveOccurred())
		encrypted, err := cipher.Encrypt(token)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(`
			INSERT INTO organization_provider_credentials (id, provider, api_token_encrypted)
			VALUES (1, 'podiq', $1)
			ON CONFLICT (id) DO UPDATE SET api_token_encrypted = EXCLUDED.api_token_encrypted
		`, encrypted)
		Expect(err).NotTo(HaveOccurred())
	}

	doSync := func(token string) *httptest.ResponseRecorder {
		req, err := http.NewRequest(http.MethodPost, "/api/v1/admin/organization-provider/sync", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	countRows := func(query string, args ...any) int {
		var n int
		Expect(db.QueryRow(query, args...).Scan(&n)).To(Succeed())
		return n
	}

	BeforeEach(func() {
		os.Setenv("JWT_SECRET", "test-secret-key-for-integration-tests")
		os.Setenv(tokencrypto.EnvKey, encryptionKey)
		gin.SetMode(gin.TestMode)

		db, cleanup = testhelpers.SetupTestDatabase()

		providerBody = snapshotWithLevels
		providerStatus = http.StatusOK
		providerGate = nil
		providerHit = make(chan struct{}, 10)

		providerServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			providerHit <- struct{}{}
			if r.Header.Get("Authorization") != "Bearer "+providerAPIToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if providerGate != nil {
				<-providerGate
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(providerStatus)
			_, _ = w.Write([]byte(providerBody))
		}))
		os.Setenv(podiq.EnvBaseURL, providerServer.URL)

		jwtService := services.NewJWTService()
		adminPair, err := jwtService.GenerateTokenPair(context.Background(), "admin", "admin", "admin@test.com", "level-admin", nil)
		Expect(err).NotTo(HaveOccurred())
		adminToken = adminPair.AccessToken

		memberPair, err := jwtService.GenerateTokenPair(context.Background(), "member", "member", "member@test.com", "level-5", nil)
		Expect(err).NotTo(HaveOccurred())
		memberToken = memberPair.AccessToken

		cipher := tokencrypto.Load()
		Expect(cipher).NotTo(BeNil())

		providerRepo := postgres.NewOrganizationProviderRepository(db)
		userRepo := postgres.NewUserRepository(db)
		teamRepo := postgres.NewTeamRepository(db)
		podiqClient := podiq.NewClient(podiq.LoadConfig())
		syncService := services.NewOrganizationSyncService(providerRepo, podiqClient, cipher, userRepo, teamRepo)

		router = gin.New()
		v1.SetupOrganizationProviderRoutes(router, providerRepo, syncService, cipher, jwtService)

		storeToken(providerAPIToken)
	})

	AfterEach(func() {
		providerServer.Close()
		cleanup()
		os.Unsetenv("JWT_SECRET")
		os.Unsetenv(tokencrypto.EnvKey)
		os.Unsetenv(podiq.EnvBaseURL)
	})

	Describe("POST /api/v1/admin/organization-provider/sync", func() {
		It("should import users, teams and memberships for an admin", func() {
			w := doSync(adminToken)
			Expect(w.Code).To(Equal(http.StatusOK), w.Body.String())

			var result services.SyncResult
			Expect(json.Unmarshal(w.Body.Bytes(), &result)).To(Succeed())

			Expect(result.Status).To(Equal("completed"))
			Expect(result.UsersSynced).To(Equal(2), "only the two users on configured levels import")
			Expect(result.TeamsSynced).To(Equal(2))
			Expect(result.MembershipsSynced).To(Equal(3))
			Expect(result.StartedAt).NotTo(BeZero())
			Expect(result.CompletedAt).NotTo(BeZero())

			Expect(countRows(`SELECT COUNT(*) FROM users WHERE id LIKE 'sync-user-%'`)).To(Equal(2))
			Expect(countRows(`SELECT COUNT(*) FROM teams WHERE id LIKE 'sync-team-%'`)).To(Equal(2))
			Expect(countRows(`SELECT COUNT(*) FROM team_members WHERE team_id LIKE 'sync-team-%'`)).To(Equal(3))
		})

		It("should set the manager link once every user exists", func() {
			Expect(doSync(adminToken).Code).To(Equal(http.StatusOK))

			var reportsTo sql.NullString
			Expect(db.QueryRow(`SELECT reports_to FROM users WHERE id = 'sync-user-2'`).Scan(&reportsTo)).To(Succeed())
			Expect(reportsTo.Valid).To(BeTrue())
			Expect(reportsTo.String).To(Equal("sync-user-1"))
		})

		It("should skip users whose hierarchy level is not configured and report them", func() {
			w := doSync(adminToken)
			Expect(w.Code).To(Equal(http.StatusOK))

			var result services.SyncResult
			Expect(json.Unmarshal(w.Body.Bytes(), &result)).To(Succeed())

			Expect(result.UsersSkipped).To(Equal(2))
			Expect(result.SkippedUsers).To(HaveLen(2))

			reasons := map[string]string{}
			for _, s := range result.SkippedUsers {
				reasons[s.UserID] = s.Reason
			}
			Expect(reasons["sync-exec"]).To(Equal("unknown_hierarchy_level"))
			Expect(reasons["sync-e2e"]).To(Equal("missing_hierarchy_level"))

			Expect(countRows(`SELECT COUNT(*) FROM users WHERE id IN ('sync-exec','sync-e2e')`)).To(Equal(0))
		})

		It("should map healthCheckEnabled onto teams", func() {
			w := doSync(adminToken)
			Expect(w.Code).To(Equal(http.StatusOK))

			var result services.SyncResult
			Expect(json.Unmarshal(w.Body.Bytes(), &result)).To(Succeed())

			var team1, team2 bool
			Expect(db.QueryRow(`SELECT health_check_enabled FROM teams WHERE id = 'sync-team-1'`).Scan(&team1)).To(Succeed())
			Expect(db.QueryRow(`SELECT health_check_enabled FROM teams WHERE id = 'sync-team-2'`).Scan(&team2)).To(Succeed())
			Expect(team1).To(BeFalse(), "healthCheckEnabled=false should apply")
			Expect(team2).To(BeTrue(), "healthCheckEnabled=true should apply")

			// A team created by this sync was never participating, so nothing was
			// switched off. The warning counts real transitions, not initial state.
			Expect(result.HealthChecksDisabled).To(BeZero())
		})

		It("should report how many participating teams it switches off", func() {
			// The case an admin needs warning about: teams already running health
			// checks in THC, which the provider reports as not participating.
			_, err := db.Exec(`
				INSERT INTO teams (id, name, health_check_enabled) VALUES ('sync-team-1', 'Alcatraz', true)
			`)
			Expect(err).NotTo(HaveOccurred())

			w := doSync(adminToken)
			Expect(w.Code).To(Equal(http.StatusOK), w.Body.String())

			var result services.SyncResult
			Expect(json.Unmarshal(w.Body.Bytes(), &result)).To(Succeed())
			Expect(result.HealthChecksDisabled).To(Equal(1))

			var enabled bool
			Expect(db.QueryRow(`SELECT health_check_enabled FROM teams WHERE id = 'sync-team-1'`).Scan(&enabled)).To(Succeed())
			Expect(enabled).To(BeFalse())
		})

		It("should preserve the existing flag when the provider omits it", func() {
			_, err := db.Exec(`
				INSERT INTO teams (id, name, health_check_enabled) VALUES ('sync-team-3', 'Andalusia', false)
			`)
			Expect(err).NotTo(HaveOccurred())

			providerBody = `{
				"contractVersion": "1.0",
				"generatedAt": "2026-08-31T05:04:48.240Z",
				"teams": [{"id": "sync-team-3", "name": "Andalusia Renamed"}],
				"users": [{"id": "sync-user-1", "username": "syncalice", "displayName": "Alice", "email": "syncalice@test.com", "hierarchyLevelId": "level-3"}],
				"memberships": []
			}`

			Expect(doSync(adminToken).Code).To(Equal(http.StatusOK))

			var enabled bool
			var name string
			Expect(db.QueryRow(`SELECT name, health_check_enabled FROM teams WHERE id = 'sync-team-3'`).Scan(&name, &enabled)).To(Succeed())
			Expect(name).To(Equal("Andalusia Renamed"), "other fields still update")
			Expect(enabled).To(BeFalse(), "an omitted flag must not be overwritten")
		})

		It("should be idempotent when run twice", func() {
			Expect(doSync(adminToken).Code).To(Equal(http.StatusOK))
			first := countRows(`SELECT COUNT(*) FROM team_members WHERE team_id LIKE 'sync-team-%'`)

			w := doSync(adminToken)
			Expect(w.Code).To(Equal(http.StatusOK), w.Body.String())

			var result services.SyncResult
			Expect(json.Unmarshal(w.Body.Bytes(), &result)).To(Succeed())
			Expect(result.UsersSynced).To(Equal(2))
			Expect(result.MembershipsRemoved).To(BeZero(), "a repeat sync should not churn membership rows")

			Expect(countRows(`SELECT COUNT(*) FROM users WHERE id LIKE 'sync-user-%'`)).To(Equal(2))
			Expect(countRows(`SELECT COUNT(*) FROM team_members WHERE team_id LIKE 'sync-team-%'`)).To(Equal(first))
		})

		It("should make the provider authoritative for membership while protecting skipped users", func() {
			Expect(doSync(adminToken).Code).To(Equal(http.StatusOK))

			// A person the provider no longer lists on the team...
			_, err := db.Exec(`
				INSERT INTO users (id, username, email, full_name, hierarchy_level_id, password_hash)
				VALUES ('sync-stale', 'syncstale', 'syncstale@test.com', 'Stale', 'level-5', '')
			`)
			Expect(err).NotTo(HaveOccurred())
			_, err = db.Exec(`INSERT INTO team_members (team_id, user_id) VALUES ('sync-team-1', 'sync-stale')`)
			Expect(err).NotTo(HaveOccurred())

			// ...and a skipped user who already belongs to it.
			_, err = db.Exec(`
				INSERT INTO users (id, username, email, full_name, hierarchy_level_id, password_hash)
				VALUES ('sync-exec', 'syncceo', 'syncceo@test.com', 'Chief', 'level-1', '')
			`)
			Expect(err).NotTo(HaveOccurred())
			_, err = db.Exec(`INSERT INTO team_members (team_id, user_id) VALUES ('sync-team-1', 'sync-exec')`)
			Expect(err).NotTo(HaveOccurred())

			w := doSync(adminToken)
			Expect(w.Code).To(Equal(http.StatusOK), w.Body.String())

			var result services.SyncResult
			Expect(json.Unmarshal(w.Body.Bytes(), &result)).To(Succeed())
			Expect(result.MembershipsRemoved).To(Equal(1))

			Expect(countRows(
				`SELECT COUNT(*) FROM team_members WHERE team_id = 'sync-team-1' AND user_id = 'sync-stale'`,
			)).To(Equal(0), "a member the provider dropped should be removed")

			Expect(countRows(
				`SELECT COUNT(*) FROM team_members WHERE team_id = 'sync-team-1' AND user_id = 'sync-exec'`,
			)).To(Equal(1), "a skipped user must keep the membership THC already had")
		})

		It("should reject a non-admin", func() {
			w := doSync(memberToken)
			Expect(w.Code).To(Equal(http.StatusForbidden))
			Expect(countRows(`SELECT COUNT(*) FROM users WHERE id LIKE 'sync-user-%'`)).To(Equal(0))
		})

		It("should reject an unauthenticated request", func() {
			req, err := http.NewRequest(http.MethodPost, "/api/v1/admin/organization-provider/sync", nil)
			Expect(err).NotTo(HaveOccurred())
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			Expect(w.Code).To(Equal(http.StatusUnauthorized))
		})

		It("should return 409 when a sync is already running", func() {
			providerGate = make(chan struct{})

			done := make(chan int, 1)
			go func() {
				defer GinkgoRecover()
				done <- doSync(adminToken).Code
			}()

			// Wait until the first sync is inside the provider call and holding the lock.
			Eventually(providerHit, "5s").Should(Receive())

			second := doSync(adminToken)
			Expect(second.Code).To(Equal(http.StatusConflict), second.Body.String())

			close(providerGate)
			Eventually(done, "10s").Should(Receive(Equal(http.StatusOK)))
		})

		It("should write nothing when the provider fails", func() {
			providerStatus = http.StatusInternalServerError
			providerBody = `{"error":"upstream exploded"}`

			w := doSync(adminToken)
			Expect(w.Code).To(Equal(http.StatusBadGateway))
			Expect(w.Body.String()).NotTo(ContainSubstring("upstream exploded"))

			Expect(countRows(`SELECT COUNT(*) FROM users WHERE id LIKE 'sync-user-%'`)).To(Equal(0))
			Expect(countRows(`SELECT COUNT(*) FROM teams WHERE id LIKE 'sync-team-%'`)).To(Equal(0))
		})

		It("should write nothing when the snapshot breaches the contract", func() {
			providerBody = `{
				"contractVersion": "2.0",
				"generatedAt": "2026-08-31T05:04:48.240Z",
				"teams": [{"id": "sync-team-1", "name": "Alcatraz"}],
				"users": [{"id": "sync-user-1", "username": "syncalice", "displayName": "Alice", "email": "syncalice@test.com", "hierarchyLevelId": "level-3"}],
				"memberships": []
			}`

			w := doSync(adminToken)
			Expect(w.Code).To(Equal(http.StatusBadGateway))
			Expect(w.Body.String()).To(ContainSubstring("contract version"))

			Expect(countRows(`SELECT COUNT(*) FROM users WHERE id LIKE 'sync-user-%'`)).To(Equal(0))
			Expect(countRows(`SELECT COUNT(*) FROM teams WHERE id LIKE 'sync-team-%'`)).To(Equal(0))
		})

		It("should refuse to sync when no token is stored", func() {
			_, err := db.Exec(`DELETE FROM organization_provider_credentials`)
			Expect(err).NotTo(HaveOccurred())

			w := doSync(adminToken)
			Expect(w.Code).To(Equal(http.StatusBadRequest))
			Expect(w.Body.String()).To(ContainSubstring("not configured"))
		})

		It("should never disclose the provider token", func() {
			w := doSync(adminToken)
			Expect(w.Code).To(Equal(http.StatusOK))
			Expect(w.Body.String()).NotTo(ContainSubstring(providerAPIToken))

			var stored string
			Expect(db.QueryRow(`SELECT api_token_encrypted FROM organization_provider_credentials WHERE id = 1`).Scan(&stored)).To(Succeed())
			Expect(stored).NotTo(ContainSubstring(providerAPIToken))
		})
	})

	Describe("Organization provider settings", func() {
		getSettings := func(token string) *httptest.ResponseRecorder {
			req, err := http.NewRequest(http.MethodGet, "/api/v1/admin/settings/organization-provider", nil)
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			return w
		}

		It("should report configuration status without revealing the token", func() {
			w := getSettings(adminToken)
			Expect(w.Code).To(Equal(http.StatusOK))
			Expect(w.Body.String()).NotTo(ContainSubstring(providerAPIToken))

			var settings map[string]any
			Expect(json.Unmarshal(w.Body.Bytes(), &settings)).To(Succeed())
			Expect(settings["configured"]).To(BeTrue())
			Expect(settings["baseUrlConfigured"]).To(BeTrue())
			Expect(settings["encryptionConfigured"]).To(BeTrue())
			Expect(settings["readyToSync"]).To(BeTrue())

			for _, forbidden := range []string{"apiToken", "token", "apiTokenEncrypted"} {
				Expect(settings).NotTo(HaveKey(forbidden))
			}
		})

		It("should store a token that can then be used to sync", func() {
			_, err := db.Exec(`DELETE FROM organization_provider_credentials`)
			Expect(err).NotTo(HaveOccurred())

			body := strings.NewReader(`{"apiToken":"` + providerAPIToken + `"}`)
			req, err := http.NewRequest(http.MethodPut, "/api/v1/admin/settings/organization-provider", body)
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+adminToken)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			Expect(w.Code).To(Equal(http.StatusOK), w.Body.String())
			Expect(w.Body.String()).NotTo(ContainSubstring(providerAPIToken))

			var stored string
			Expect(db.QueryRow(`SELECT api_token_encrypted FROM organization_provider_credentials WHERE id = 1`).Scan(&stored)).To(Succeed())
			Expect(stored).NotTo(ContainSubstring(providerAPIToken))

			Expect(doSync(adminToken).Code).To(Equal(http.StatusOK))
		})

		It("should reject an empty token", func() {
			req, err := http.NewRequest(http.MethodPut, "/api/v1/admin/settings/organization-provider", strings.NewReader(`{"apiToken":"   "}`))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+adminToken)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			Expect(w.Code).To(Equal(http.StatusBadRequest))
		})

		It("should reject a non-admin", func() {
			Expect(getSettings(memberToken).Code).To(Equal(http.StatusForbidden))
		})
	})
})
