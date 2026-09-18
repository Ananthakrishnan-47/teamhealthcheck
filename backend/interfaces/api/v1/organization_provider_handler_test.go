package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/agopalakrishnan/teams360/backend/application/services"
	"github.com/agopalakrishnan/teams360/backend/domain/orgprovider"
	"github.com/agopalakrishnan/teams360/backend/infrastructure/dataprovider"
	v1 "github.com/agopalakrishnan/teams360/backend/interfaces/api/v1"
	"github.com/agopalakrishnan/teams360/backend/interfaces/dto"
	"github.com/agopalakrishnan/teams360/backend/pkg/orgsnapshot"
	"github.com/gin-gonic/gin"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// fakeSyncRepository stands in for the postgres repository so the handler's own
// behaviour -- what it accepts, who it lets override, and what it returns --
// can be pinned down without a database. It records the ApplyInput it was given,
// which is how these specs prove the override travelled from the request body
// to persistence (or, for a non-admin, never got there).
type fakeSyncRepository struct {
	lastInput  orgprovider.ApplyInput
	calls      int
	holdReport *orgprovider.MassDeletionReport
}

func (f *fakeSyncRepository) KnownHierarchyLevelIDs(_ context.Context) (map[string]bool, error) {
	return map[string]bool{"level-3": true, "level-5": true}, nil
}

func (f *fakeSyncRepository) ApplySnapshot(_ context.Context, in orgprovider.ApplyInput) (*orgprovider.ApplyResult, error) {
	f.calls++
	f.lastInput = in

	// Mirror the real repository's contract: the guard is evaluated either way,
	// and only the override decides whether its verdict is returned or waived.
	if f.holdReport != nil {
		report := *f.holdReport
		if !in.OverrideMassDeletion {
			return nil, &orgprovider.MassDeletionHoldError{Report: report}
		}
		return &orgprovider.ApplyResult{UsersDeleted: report.Users.Deleting, MassDeletionOverride: &report}, nil
	}
	return &orgprovider.ApplyResult{UsersSynced: len(in.Snapshot.Users)}, nil
}

// fakeFetcher returns a valid, minimal snapshot, or -- when asked -- a
// failure or a snapshot that breaches the contract.
type fakeFetcher struct {
	err        error
	badVersion bool
}

func (f *fakeFetcher) FetchSnapshot(_ context.Context) (*orgsnapshot.Snapshot, error) {
	if f.err != nil {
		return nil, f.err
	}
	version := "1.0"
	if f.badVersion {
		version = "2.0"
	}
	return &orgsnapshot.Snapshot{
		ContractVersion: version,
		GeneratedAt:     time.Now().UTC(),
		Teams:           []orgsnapshot.Team{{ID: "t1", Name: "Team One"}},
		Users: []orgsnapshot.User{
			{ID: "u1", Username: "userone", DisplayName: "User One", Email: "u1@test.com", HierarchyLevelID: "level-3"},
		},
		Memberships: []orgsnapshot.Membership{{UserID: "u1", TeamID: "t1"}},
	}, nil
}

var _ = Describe("Organization Provider Handler: mass-deletion override", func() {
	var (
		router      *gin.Engine
		repo        *fakeSyncRepository
		adminToken  string
		memberToken string
	)

	// heldReport is an over-threshold proposal: 6 of 20 users and 3 of 10 teams
	// at a 20% threshold, plus an informational membership cascade.
	heldReport := func() *orgprovider.MassDeletionReport {
		report := orgprovider.BuildMassDeletionReport(20, 6, 10, 3, 20)
		memberships := orgprovider.NewDeletionMetric(orgprovider.DeletionKindCascaded, 50, 8, 20, false)
		report.Memberships = &memberships
		report.WithIncoming(14, 7, 42)
		return &report
	}

	// rewireFetcher rebuilds the router around a different provider fetcher,
	// keeping the same repository so call counts stay observable.
	rewireFetcher := func(fetcher services.SnapshotFetcher) {
		syncService := services.NewOrganizationSyncService(repo, fetcher, nil, nil)
		router = gin.New()
		v1.SetupOrganizationProviderRoutes(router, syncService, services.NewJWTService())
	}

	postSync := func(token, body string) *httptest.ResponseRecorder {
		var req *http.Request
		var err error
		if body == "" {
			req, err = http.NewRequest(http.MethodPost, "/api/v1/admin/organization-provider/sync", nil)
		} else {
			req, err = http.NewRequest(http.MethodPost, "/api/v1/admin/organization-provider/sync", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		}
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	BeforeEach(func() {
		os.Setenv("JWT_SECRET", "test-secret-key-for-handler-tests")
		os.Setenv(dataprovider.EnvBaseURL, "https://provider.invalid")
		os.Setenv(dataprovider.EnvAPIToken, "handler-test-token")
		gin.SetMode(gin.TestMode)

		repo = &fakeSyncRepository{}
		syncService := services.NewOrganizationSyncService(repo, &fakeFetcher{}, nil, nil)

		jwtService := services.NewJWTService()
		adminPair, err := jwtService.GenerateTokenPair(context.Background(), "admin", "admin", "admin@test.com", "level-admin", nil)
		Expect(err).NotTo(HaveOccurred())
		adminToken = adminPair.AccessToken

		memberPair, err := jwtService.GenerateTokenPair(context.Background(), "member", "member", "member@test.com", "level-5", nil)
		Expect(err).NotTo(HaveOccurred())
		memberToken = memberPair.AccessToken

		router = gin.New()
		v1.SetupOrganizationProviderRoutes(router, syncService, jwtService)
	})

	AfterEach(func() {
		os.Unsetenv("JWT_SECRET")
		os.Unsetenv(dataprovider.EnvBaseURL)
		os.Unsetenv(dataprovider.EnvAPIToken)
	})

	It("returns the deletion counts, percentages and threshold when the sync is held", func() {
		repo.holdReport = heldReport()

		w := postSync(adminToken, "")
		Expect(w.Code).To(Equal(http.StatusConflict), w.Body.String())

		var body dto.MassDeletionHoldResponseDTO
		Expect(json.Unmarshal(w.Body.Bytes(), &body)).To(Succeed())
		Expect(body.Code).To(Equal(dto.CodeMassDeletionHold))
		Expect(body.Applied).To(BeFalse())
		Expect(body.Error).To(ContainSubstring("held for review"))

		Expect(body.MassDeletion).NotTo(BeNil())
		Expect(body.MassDeletion.Threshold).To(Equal(20.0))
		Expect(body.MassDeletion.Users.Existing).To(Equal(20))
		Expect(body.MassDeletion.Users.Deleting).To(Equal(6))
		Expect(body.MassDeletion.Users.Percent).To(Equal(30.0))
		Expect(body.MassDeletion.Users.ContributesToHold).To(BeTrue())
		Expect(body.MassDeletion.Users.Incoming).To(Equal(14), "what the provider sent must reach the client, so a hold can be diagnosed")
		Expect(body.MassDeletion.Teams.Incoming).To(Equal(7))
		Expect(body.MassDeletion.Teams.Percent).To(Equal(30.0))
		Expect(body.MassDeletion.Memberships).NotTo(BeNil())
		Expect(body.MassDeletion.Memberships.Kind).To(Equal(orgprovider.DeletionKindCascaded))
		Expect(body.MassDeletion.Memberships.ContributesToHold).To(BeFalse())
	})

	It("does not ask persistence to override on a normal sync", func() {
		w := postSync(adminToken, "")
		Expect(w.Code).To(Equal(http.StatusOK), w.Body.String())
		Expect(repo.lastInput.OverrideMassDeletion).To(BeFalse())
		Expect(repo.lastInput.MaxDeletePercent).To(Equal(20.0), "the production default threshold must be unchanged")
	})

	It("passes an explicit admin override through to persistence", func() {
		repo.holdReport = heldReport()

		w := postSync(adminToken, `{"overrideMassDeletion": true}`)
		Expect(w.Code).To(Equal(http.StatusOK), w.Body.String())
		Expect(repo.lastInput.OverrideMassDeletion).To(BeTrue())
		Expect(repo.lastInput.MaxDeletePercent).To(Equal(20.0), "an override waives the verdict, it never changes the threshold")

		var result services.SyncResult
		Expect(json.Unmarshal(w.Body.Bytes(), &result)).To(Succeed())
		Expect(result.MassDeletionOverridden).To(BeTrue())
		Expect(result.MassDeletion).NotTo(BeNil())
		Expect(result.MassDeletion.Users.Deleting).To(Equal(6))
	})

	It("scopes the override to the request carrying it", func() {
		repo.holdReport = heldReport()

		Expect(postSync(adminToken, `{"overrideMassDeletion": true}`).Code).To(Equal(http.StatusOK))
		Expect(postSync(adminToken, "").Code).To(Equal(http.StatusConflict), "the next sync must be held again")
		Expect(repo.lastInput.OverrideMassDeletion).To(BeFalse())
	})

	It("refuses an override from a non-admin without reaching persistence", func() {
		repo.holdReport = heldReport()

		w := postSync(memberToken, `{"overrideMassDeletion": true}`)
		Expect(w.Code).To(Equal(http.StatusForbidden), w.Body.String())
		Expect(repo.calls).To(BeZero(), "an unauthorized override must never reach the database")
	})

	It("rejects a malformed body rather than silently syncing", func() {
		w := postSync(adminToken, `{"overrideMassDeletion": "yes"}`)
		Expect(w.Code).To(Equal(http.StatusBadRequest), w.Body.String())
		Expect(repo.calls).To(BeZero())
	})

	It("still validates the snapshot when an override is requested", func() {
		repo.holdReport = heldReport()
		rewireFetcher(&fakeFetcher{badVersion: true})

		w := postSync(adminToken, `{"overrideMassDeletion": true}`)
		Expect(w.Code).To(Equal(http.StatusBadGateway), w.Body.String())
		Expect(w.Body.String()).To(ContainSubstring("contract"))
		Expect(repo.calls).To(BeZero(), "an override waives the deletion guard, never snapshot validation")
	})

	It("still fails an override when the provider itself fails", func() {
		repo.holdReport = heldReport()
		rewireFetcher(&fakeFetcher{err: os.ErrDeadlineExceeded})

		w := postSync(adminToken, `{"overrideMassDeletion": true}`)
		Expect(w.Code).To(Equal(http.StatusBadGateway), w.Body.String())
		Expect(repo.calls).To(BeZero(), "a provider failure must stop the sync even with an override")
	})
})
