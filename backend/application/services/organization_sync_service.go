package services

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/agopalakrishnan/teams360/backend/domain/orgprovider"
	"github.com/agopalakrishnan/teams360/backend/domain/team"
	"github.com/agopalakrishnan/teams360/backend/domain/user"
	"github.com/agopalakrishnan/teams360/backend/pkg/logger"
	"github.com/agopalakrishnan/teams360/backend/pkg/orgsnapshot"
)

// EnvMaxDeletePercent names the environment variable configuring the
// mass-deletion guard. It is required -- there is no in-code fallback -- and
// its value is set in .env (see .env.example).
const EnvMaxDeletePercent = "ORG_SYNC_MAX_DELETE_PERCENT"

// Errors returned by OrganizationSyncService. Handlers map these to status codes.
var (
	// ErrSyncInProgress means another synchronization is already running.
	ErrSyncInProgress = errors.New("a synchronization is already in progress")
	// ErrProviderNotConfigured means the data provider client is not configured.
	ErrProviderNotConfigured = errors.New("organization provider is not configured")
	// ErrInvalidSnapshot means the provider returned data that breaches the contract.
	ErrInvalidSnapshot = errors.New("provider snapshot failed contract validation")
	// ErrProviderFetchFailed means the fetch to the external provider itself
	// failed (network error, non-200 response, oversized/malformed body). This
	// is distinct from an internal/DB failure: the handler maps it to 502
	// Bad Gateway, since it genuinely reflects an unusable upstream response.
	ErrProviderFetchFailed = errors.New("failed to fetch snapshot from provider")
	// ErrMaxDeletePercentNotConfigured means ORG_SYNC_MAX_DELETE_PERCENT is
	// unset, empty, or not a valid positive number. The mass-deletion guard
	// has no in-code fallback, so a sync cannot proceed without it.
	ErrMaxDeletePercentNotConfigured = errors.New("ORG_SYNC_MAX_DELETE_PERCENT is not configured")
)

// SnapshotFetcher fetches a complete organization snapshot from an external
// provider. The sync service depends on this interface, not a concrete
// client, so a second provider can be added without touching orchestration.
// The fetcher owns its own credentials (read from its own environment
// configuration); it is never handed a token by this service.
type SnapshotFetcher interface {
	FetchSnapshot(ctx context.Context) (*orgsnapshot.Snapshot, error)
}

// SyncResult is the outcome of one synchronization run, returned to the API
// caller. It never carries a token, header, or the raw provider payload.
type SyncResult struct {
	Status string `json:"status"`

	TeamsSynced        int `json:"teamsSynced"`
	UsersSynced        int `json:"usersSynced"`
	MembershipsSynced  int `json:"membershipsSynced"`
	MembershipsRemoved int `json:"membershipsRemoved"`

	HealthChecksDisabled int `json:"healthChecksDisabled"`
	HealthChecksEnabled  int `json:"healthChecksEnabled"`

	UsersDeleted       int `json:"usersDeleted"`
	TeamsDeleted       int `json:"teamsDeleted"`
	ActionItemsDeleted int `json:"actionItemsDeleted"`

	UsersSkipped         int                       `json:"usersSkipped"`
	SkippedUsers         []orgprovider.SkippedUser `json:"skippedUsers,omitempty"`
	ManagerLinksCleared  int                       `json:"managerLinksCleared"`
	TeamLeadsCleared     int                       `json:"teamLeadsCleared"`
	MembershipsDiscarded int                       `json:"membershipsDiscarded"`

	// MassDeletionOverridden reports that this run only completed because an
	// administrator explicitly waived the mass-deletion hold, and MassDeletion
	// carries the counts that were waived. Both are absent on a normal sync.
	MassDeletionOverridden bool                            `json:"massDeletionOverridden,omitempty"`
	MassDeletion           *orgprovider.MassDeletionReport `json:"massDeletion,omitempty"`

	StartedAt   time.Time `json:"startedAt"`
	CompletedAt time.Time `json:"completedAt"`
}

// SyncOptions carries per-request choices an administrator made. It is scoped
// to one Sync call: nothing here is persisted, cached, or carried into the
// next run.
type SyncOptions struct {
	// OverrideMassDeletion waives the mass-deletion percentage guard for this
	// one run only, after an admin reviewed the held counts. The caller is
	// responsible for having established that the requester is an
	// administrator before setting it.
	OverrideMassDeletion bool

	// ConfirmedMassDeletion is the exact counts the admin reviewed before
	// requesting OverrideMassDeletion. See orgprovider.ApplyInput's field of
	// the same name: the override only takes effect when this still matches
	// the freshly recomputed report, since this sync fetches its own fresh
	// snapshot rather than replaying the one that produced the held counts.
	ConfirmedMassDeletion *orgprovider.ConfirmedMassDeletion

	// ActorUserID identifies who asked, for the audit record written when an
	// override is used.
	ActorUserID string
}

// OrganizationSyncService pulls an organization snapshot from the configured
// provider and applies it to THC.
type OrganizationSyncService struct {
	repo     orgprovider.Repository
	fetcher  SnapshotFetcher
	userRepo user.Repository
	teamRepo team.Repository

	// running admits one sync at a time. A second concurrent request is
	// rejected rather than queued: two runs applying overlapping snapshots
	// would interleave their transactions unpredictably.
	running atomic.Bool
}

// NewOrganizationSyncService creates the service. A nil fetcher is tolerated
// at construction (mirrors the "disabled until configured" convention used
// elsewhere); Sync reports the misconfiguration when actually invoked.
func NewOrganizationSyncService(
	repo orgprovider.Repository,
	fetcher SnapshotFetcher,
	userRepo user.Repository,
	teamRepo team.Repository,
) *OrganizationSyncService {
	return &OrganizationSyncService{
		repo:     repo,
		fetcher:  fetcher,
		userRepo: userRepo,
		teamRepo: teamRepo,
	}
}

// Configured reports whether a sync could run at all.
func (s *OrganizationSyncService) Configured() bool {
	return s.fetcher != nil
}

// Sync fetches, validates and applies a snapshot with no overrides. This is
// the normal path and its behaviour is unchanged: a sync over the
// mass-deletion threshold is held.
func (s *OrganizationSyncService) Sync(ctx context.Context) (*SyncResult, error) {
	return s.SyncWithOptions(ctx, SyncOptions{})
}

// SyncWithOptions fetches, validates and applies a snapshot.
//
// Returns ErrSyncInProgress when another run holds the lock. All persistence
// happens in a single transaction, so a failure at any point -- including the
// mass-deletion guard tripping -- leaves THC untouched.
//
// opts.OverrideMassDeletion waives one check and one check only. The snapshot
// is still fetched fresh and revalidated here, the protected-record
// allowlists still apply in persistence, and provider/transaction failures are
// handled exactly as they are for a normal run.
func (s *OrganizationSyncService) SyncWithOptions(ctx context.Context, opts SyncOptions) (*SyncResult, error) {
	if !s.running.CompareAndSwap(false, true) {
		return nil, ErrSyncInProgress
	}
	defer s.running.Store(false)

	log := logger.Get()
	startedAt := time.Now().UTC()

	if s.fetcher == nil {
		return nil, ErrProviderNotConfigured
	}

	snapshot, err := s.fetcher.FetchSnapshot(ctx)
	if err != nil {
		return nil, errors.Join(ErrProviderFetchFailed, err)
	}
	if snapshot == nil {
		return nil, errors.Join(ErrInvalidSnapshot, errors.New("provider returned no snapshot"))
	}

	knownLevels, err := s.repo.KnownHierarchyLevelIDs(ctx)
	if err != nil {
		return nil, err
	}

	filtered, err := FilterSnapshot(snapshot, knownLevels)
	if err != nil {
		// Names a record identity, not a credential, so it is safe to return
		// to an admin and is the only way to diagnose a bad snapshot.
		return nil, errors.Join(ErrInvalidSnapshot, err)
	}

	if err := filtered.Snapshot.ValidationError(); err != nil {
		// Validation errors name records, not credentials, so they are safe to
		// return to an admin and are the only way to diagnose a bad snapshot.
		return nil, errors.Join(ErrInvalidSnapshot, err)
	}

	maxDeletePercent, err := maxDeletePercent()
	if err != nil {
		return nil, err
	}

	applied, err := s.repo.ApplySnapshot(ctx, orgprovider.ApplyInput{
		Snapshot: filtered.Snapshot,
		// The raw, pre-filter payload's own counts -- not len(filtered.Snapshot.*),
		// which only reflects what survived filtering. See ApplyInput's doc.
		IncomingUsers:            len(snapshot.Users),
		IncomingTeams:            len(snapshot.Teams),
		IncomingMemberships:      len(snapshot.Memberships),
		PreservedMemberUserIDs:   filtered.PreservedMemberUserIDs,
		PreserveReportsToUserIDs: filtered.PreserveReportsToUserIDs,
		MaxDeletePercent:         maxDeletePercent,
		OverrideMassDeletion:     opts.OverrideMassDeletion,
		ConfirmedMassDeletion:    opts.ConfirmedMassDeletion,
	})
	if err != nil {
		return nil, err
	}

	// The supervisor chain is a denormalized cache derived from reports_to.
	// Rebuilding it is best-effort and deliberately outside the transaction:
	// the sync has already committed, and a stale cache is recoverable whereas
	// a rolled-back org import is not. A failure here is logged but does not
	// change the sync's reported status -- it is a real (if narrow) gap
	// between "the sync completed" and "every derived structure is
	// consistent," called out explicitly rather than silently claimed away.
	supervisorChainErr := s.rederiveSupervisorChains(ctx, filtered.Snapshot.Teams)

	result := &SyncResult{
		Status:               "completed",
		TeamsSynced:          applied.TeamsSynced,
		UsersSynced:          applied.UsersSynced,
		MembershipsSynced:    applied.MembershipsSynced,
		MembershipsRemoved:   applied.MembershipsRemoved,
		HealthChecksDisabled: applied.HealthChecksDisabled,
		HealthChecksEnabled:  applied.HealthChecksEnabled,
		UsersDeleted:         applied.UsersDeleted,
		TeamsDeleted:         applied.TeamsDeleted,
		ActionItemsDeleted:   applied.ActionItemsDeleted,
		UsersSkipped:         len(filtered.SkippedUsers),
		SkippedUsers:         filtered.SkippedUsers,
		ManagerLinksCleared:  filtered.ClearedManagers,
		TeamLeadsCleared:     filtered.ClearedTeamLeads,
		MembershipsDiscarded: filtered.DroppedMemberships + filtered.DuplicateMemberships,
		StartedAt:            startedAt,
		CompletedAt:          time.Now().UTC(),
	}
	if supervisorChainErr != nil {
		result.Status = "completed_with_warnings"
	}
	if applied.MassDeletionOverride != nil {
		result.MassDeletionOverridden = true
		result.MassDeletion = applied.MassDeletionOverride

		// An override is a deliberate, destructive, human decision, so it gets
		// its own audit record naming who made it and exactly what it waived --
		// not just a line in the completion log below.
		report := applied.MassDeletionOverride
		actor := opts.ActorUserID
		if actor == "" {
			actor = "unknown"
		}
		log.Security("org_sync_mass_deletion_override").
			UserID(actor).
			Details(fmt.Sprintf(
				"administrator overrode the mass-deletion hold: users %d/%d (%.1f%%), teams %d/%d (%.1f%%), memberships cascaded %s, threshold %.1f%%",
				report.Users.Deleting, report.Users.Existing, report.Users.Percent,
				report.Teams.Deleting, report.Teams.Existing, report.Teams.Percent,
				formatCascade(report.Memberships),
				report.Threshold,
			)).
			Log()
	}

	log.WithFields(map[string]interface{}{
		"usersSynced":       result.UsersSynced,
		"teamsSynced":       result.TeamsSynced,
		"membershipsSynced": result.MembershipsSynced,
		"usersDeleted":      result.UsersDeleted,
		"teamsDeleted":      result.TeamsDeleted,
		"usersSkipped":      result.UsersSkipped,
	}).Info("organization provider sync completed")

	return result, nil
}

// formatCascade renders the optional membership cascade for the audit line.
func formatCascade(m *orgprovider.DeletionMetric) string {
	if m == nil {
		return "unmeasured"
	}
	return fmt.Sprintf("%d/%d (%.1f%%)", m.Deleting, m.Existing, m.Percent)
}

// maxDeletePercent reads the mass-deletion threshold from
// ORG_SYNC_MAX_DELETE_PERCENT. There is no in-code default: an unset, empty,
// or invalid value is a configuration error, not something to guess past.
func maxDeletePercent() (float64, error) {
	raw := os.Getenv(EnvMaxDeletePercent)
	if raw == "" {
		return 0, ErrMaxDeletePercentNotConfigured
	}
	value, err := strconv.ParseFloat(raw, 64)
	// Reject NaN/Inf since they can bypass the percentage validation.
	if err != nil || value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, ErrMaxDeletePercentNotConfigured
	}
	return value, nil
}

// rederiveSupervisorChains refreshes team_supervisors for the synced teams,
// mirroring the derivation the admin team handler performs after a lead
// changes. Returns a non-nil error if any team's chain failed to rebuild, so
// the caller can surface that the post-commit step was incomplete.
func (s *OrganizationSyncService) rederiveSupervisorChains(ctx context.Context, teams []orgsnapshot.Team) error {
	if s.userRepo == nil || s.teamRepo == nil {
		return nil
	}

	log := logger.Get()
	var firstErr error

	for _, t := range teams {
		if orgprovider.IsProtectedTeam(t.ID) {
			continue // persistence never updates a protected team, so its supervisor chain must not move either
		}

		stored, err := s.teamRepo.FindByID(ctx, t.ID)
		if err != nil {
			log.Warn("failed to look up team " + t.ID + " for supervisor chain rederivation: " + err.Error())
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if stored == nil || stored.TeamLeadID == nil {
			continue
		}

		supervisors, err := s.userRepo.FindSupervisorChainUp(ctx, *stored.TeamLeadID)
		if err != nil {
			log.Warn("failed to derive supervisor chain for team " + t.ID + ": " + err.Error())
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		chain := make([]*team.SupervisorLink, len(supervisors))
		for i, sup := range supervisors {
			chain[i] = &team.SupervisorLink{UserID: sup.ID, LevelID: sup.HierarchyLevelID}
		}

		if err := s.teamRepo.UpdateSupervisorChain(ctx, t.ID, chain); err != nil {
			log.Warn("failed to save derived supervisor chain for team " + t.ID + ": " + err.Error())
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}
