package services

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/agopalakrishnan/teams360/backend/domain/orgprovider"
	"github.com/agopalakrishnan/teams360/backend/domain/team"
	"github.com/agopalakrishnan/teams360/backend/domain/user"
	"github.com/agopalakrishnan/teams360/backend/pkg/logger"
	"github.com/agopalakrishnan/teams360/backend/pkg/orgsnapshot"
)

// Errors returned by OrganizationSyncService. Handlers map these to status codes.
var (
	// ErrSyncInProgress means another synchronization is already running.
	ErrSyncInProgress = errors.New("a synchronization is already in progress")
	// ErrProviderNotConfigured means no API token has been stored.
	ErrProviderNotConfigured = errors.New("organization provider is not configured")
	// ErrEncryptionNotConfigured means the token cannot be decrypted.
	ErrEncryptionNotConfigured = errors.New("token encryption is not configured")
	// ErrInvalidSnapshot means the provider returned data that breaches the contract.
	ErrInvalidSnapshot = errors.New("provider snapshot failed contract validation")
)

// SnapshotFetcher fetches an organization snapshot from an external provider.
// The sync service depends on this interface rather than a concrete client so a
// second provider can be added without touching orchestration.
type SnapshotFetcher interface {
	FetchSnapshot(ctx context.Context, token string) (*orgsnapshot.Snapshot, error)
	Configured() bool
}

// TokenCipher decrypts the stored provider token.
type TokenCipher interface {
	Decrypt(encoded string) (string, error)
}

// SyncResult is the outcome of one synchronization run.
type SyncResult struct {
	Status string `json:"status"`

	TeamsSynced        int `json:"teamsSynced"`
	UsersSynced        int `json:"usersSynced"`
	MembershipsSynced  int `json:"membershipsSynced"`
	MembershipsRemoved int `json:"membershipsRemoved"`

	HealthChecksDisabled int `json:"healthChecksDisabled"`
	HealthChecksEnabled  int `json:"healthChecksEnabled"`

	UsersSkipped         int                       `json:"usersSkipped"`
	SkippedUsers         []orgprovider.SkippedUser `json:"skippedUsers,omitempty"`
	ManagerLinksCleared  int                       `json:"managerLinksCleared"`
	TeamLeadsCleared     int                       `json:"teamLeadsCleared"`
	MembershipsDiscarded int                       `json:"membershipsDiscarded"`

	StartedAt   time.Time `json:"startedAt"`
	CompletedAt time.Time `json:"completedAt"`
}

// OrganizationSyncService pulls an organization snapshot from the configured
// provider and applies it to THC.
type OrganizationSyncService struct {
	repo     orgprovider.Repository
	fetcher  SnapshotFetcher
	cipher   TokenCipher
	userRepo user.Repository
	teamRepo team.Repository

	// running admits one sync at a time. A second concurrent request is
	// rejected rather than queued: two runs applying overlapping snapshots
	// would interleave their transactions unpredictably.
	running atomic.Bool
}

// NewOrganizationSyncService creates the service. A nil cipher or an
// unconfigured fetcher is tolerated at construction; Sync reports the
// misconfiguration when it is actually invoked.
func NewOrganizationSyncService(
	repo orgprovider.Repository,
	fetcher SnapshotFetcher,
	cipher TokenCipher,
	userRepo user.Repository,
	teamRepo team.Repository,
) *OrganizationSyncService {
	return &OrganizationSyncService{
		repo:     repo,
		fetcher:  fetcher,
		cipher:   cipher,
		userRepo: userRepo,
		teamRepo: teamRepo,
	}
}

// Configured reports whether a sync could run: a provider URL and a usable cipher.
func (s *OrganizationSyncService) Configured() bool {
	return s.fetcher != nil && s.fetcher.Configured() && s.cipher != nil
}

// Sync fetches, validates and applies a snapshot.
//
// Returns ErrSyncInProgress when another run holds the lock. All persistence
// happens in a single transaction, so a failure at any point leaves THC
// untouched.
func (s *OrganizationSyncService) Sync(ctx context.Context) (*SyncResult, error) {
	if !s.running.CompareAndSwap(false, true) {
		return nil, ErrSyncInProgress
	}
	defer s.running.Store(false)

	log := logger.Get()
	startedAt := time.Now().UTC()

	token, err := s.resolveToken(ctx)
	if err != nil {
		return nil, err
	}

	snapshot, err := s.fetcher.FetchSnapshot(ctx, token)
	if err != nil {
		return nil, err
	}

	knownLevels, err := s.repo.KnownHierarchyLevelIDs(ctx)
	if err != nil {
		return nil, err
	}

	filtered := FilterSnapshot(snapshot, knownLevels)

	if err := filtered.Snapshot.ValidationError(); err != nil {
		// Validation errors name records, not credentials, so they are safe to
		// return to an admin and are the only way to diagnose a bad snapshot.
		return nil, errors.Join(ErrInvalidSnapshot, err)
	}

	applied, err := s.repo.ApplySnapshot(ctx, orgprovider.ApplyInput{
		Snapshot:                 filtered.Snapshot,
		PreservedMemberUserIDs:   filtered.PreservedMemberUserIDs,
		PreserveReportsToUserIDs: filtered.PreserveReportsToUserIDs,
	})
	if err != nil {
		return nil, err
	}

	// The supervisor chain is a denormalized cache derived from reports_to.
	// Rebuilding it is best-effort and deliberately outside the transaction:
	// the sync has already committed, and a stale cache is recoverable whereas
	// a rolled-back org import is not.
	s.rederiveSupervisorChains(ctx, filtered.Snapshot.Teams)

	result := &SyncResult{
		Status:               "completed",
		TeamsSynced:          applied.TeamsSynced,
		UsersSynced:          applied.UsersSynced,
		MembershipsSynced:    applied.MembershipsSynced,
		MembershipsRemoved:   applied.MembershipsRemoved,
		HealthChecksDisabled: applied.HealthChecksDisabled,
		HealthChecksEnabled:  applied.HealthChecksEnabled,
		UsersSkipped:         len(filtered.SkippedUsers),
		SkippedUsers:         filtered.SkippedUsers,
		ManagerLinksCleared:  filtered.ClearedManagers,
		TeamLeadsCleared:     filtered.ClearedTeamLeads,
		MembershipsDiscarded: filtered.DroppedMemberships + filtered.DuplicateMemberships,
		StartedAt:            startedAt,
		CompletedAt:          time.Now().UTC(),
	}

	log.WithFields(map[string]interface{}{
		"usersSynced":          result.UsersSynced,
		"teamsSynced":          result.TeamsSynced,
		"membershipsSynced":    result.MembershipsSynced,
		"usersSkipped":         result.UsersSkipped,
		"healthChecksDisabled": result.HealthChecksDisabled,
	}).Info("organization provider sync completed")

	return result, nil
}

// resolveToken loads and decrypts the stored provider token.
func (s *OrganizationSyncService) resolveToken(ctx context.Context) (string, error) {
	if s.fetcher == nil || !s.fetcher.Configured() {
		return "", ErrProviderNotConfigured
	}
	if s.cipher == nil {
		return "", ErrEncryptionNotConfigured
	}

	creds, err := s.repo.GetCredentials(ctx)
	if err != nil {
		return "", err
	}
	if creds == nil || creds.APITokenEncrypted == "" {
		return "", ErrProviderNotConfigured
	}

	token, err := s.cipher.Decrypt(creds.APITokenEncrypted)
	if err != nil {
		return "", err
	}

	return token, nil
}

// rederiveSupervisorChains refreshes team_supervisors for the synced teams,
// mirroring the derivation the admin team handler performs after a lead changes.
func (s *OrganizationSyncService) rederiveSupervisorChains(ctx context.Context, teams []orgsnapshot.Team) {
	if s.userRepo == nil || s.teamRepo == nil {
		return
	}

	log := logger.Get()

	for _, t := range teams {
		stored, err := s.teamRepo.FindByID(ctx, t.ID)
		if err != nil || stored == nil || stored.TeamLeadID == nil {
			continue
		}

		supervisors, err := s.userRepo.FindSupervisorChainUp(ctx, *stored.TeamLeadID)
		if err != nil {
			log.Warn("failed to derive supervisor chain for team " + t.ID + ": " + err.Error())
			continue
		}

		chain := make([]*team.SupervisorLink, len(supervisors))
		for i, sup := range supervisors {
			chain[i] = &team.SupervisorLink{UserID: sup.ID, LevelID: sup.HierarchyLevelID}
		}

		if err := s.teamRepo.UpdateSupervisorChain(ctx, t.ID, chain); err != nil {
			log.Warn("failed to save derived supervisor chain for team " + t.ID + ": " + err.Error())
		}
	}
}
