package services

import (
	"context"
	"time"

	"github.com/agopalakrishnan/teams360/backend/domain/orgprovider"
)

// SyncRunRecorder is the durable-persistence surface for organization sync
// history and liveness. The service depends on this narrow interface, not
// the whole orgprovider.Repository, mirroring how WithDeleteThresholdStore
// already narrows orgRepo to just the threshold read.
//
// A nil SyncRunRecorder means "no durable run recording configured" --
// tolerated for unit tests, which construct the service with no database at
// all. Every method here is best-effort from the sync's own point of view: a
// failure to record a blocked/failed attempt or a heartbeat must be logged,
// never allowed to change the sync's own return value (see
// SyncWithOptions). The one exception is the success path, which
// ApplySnapshot itself records inside its own transaction and which DOES fail
// the sync closed on a recording failure -- see recordSuccessAttempt.
type SyncRunRecorder interface {
	// RecordOrgSyncAttempt persists a Blocked or Failed outcome. Must never
	// be called with StatusSuccess -- ApplySnapshot records that internally.
	RecordOrgSyncAttempt(ctx context.Context, attempt orgprovider.OrgSyncAttempt) error
	// RecordOrgSyncSkip persists that a scheduled occurrence was skipped.
	RecordOrgSyncSkip(ctx context.Context, skip orgprovider.OrgSyncSkip) error
	// GetOrgSyncLastRun reads the assembled last-attempt/last-skip/hold state.
	GetOrgSyncLastRun(ctx context.Context) (*orgprovider.OrgSyncLastRun, error)
	// ClearOrgSyncHold clears the durable hold unconditionally -- called by
	// the dismiss-without-syncing admin action alongside DismissHold.
	ClearOrgSyncHold(ctx context.Context) error

	// ClaimOrgSyncActive/HeartbeatOrgSyncActive/ClearOrgSyncActive/
	// GetOrgSyncActiveClaim manage the liveness claim used for cross-replica
	// attribution only -- never consulted to decide whether a sync may start.
	ClaimOrgSyncActive(ctx context.Context, instanceID, trigger string, startedAt time.Time) error
	HeartbeatOrgSyncActive(ctx context.Context, instanceID string, at time.Time) error
	ClearOrgSyncActive(ctx context.Context, instanceID string) error
	GetOrgSyncActiveClaim(ctx context.Context, staleAfter time.Duration) (*orgprovider.OrgSyncActiveClaim, error)
}

// ActiveClaimStaleAfter bounds how long an active-run claim is trusted before
// it is treated as orphaned (e.g. from a SIGKILLed pod). Compared against the
// database's own clock, never a replica's local clock.
const ActiveClaimStaleAfter = 2 * time.Minute

// WithSyncRunRecorder wires durable run-history persistence into the sync
// service. Without it, blocked/failed attempts and skips are not recorded,
// and LockState falls back to in-memory-only state (today's behaviour).
func WithSyncRunRecorder(runs SyncRunRecorder) Option {
	return func(s *OrganizationSyncService) {
		s.runs = runs
	}
}
