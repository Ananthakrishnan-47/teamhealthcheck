// Package orgprovider models the external organization-data provider integration:
// the stored credential, the outcome of a sync, and the persistence contract the
// infrastructure layer implements.
package orgprovider

import (
	"context"
	"time"

	"github.com/agopalakrishnan/teams360/backend/pkg/orgsnapshot"
)

// Credentials is the stored credential for the active provider.
// APITokenEncrypted is ciphertext; the plaintext token is never held here.
type Credentials struct {
	Provider          string    `json:"provider"`
	APITokenEncrypted string    `json:"-"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

// Skip reasons reported when a snapshot record cannot be imported.
const (
	// SkipReasonMissingLevel means the record carried no hierarchy level.
	SkipReasonMissingLevel = "missing_hierarchy_level"
	// SkipReasonUnknownLevel means the level is not configured in this deployment.
	SkipReasonUnknownLevel = "unknown_hierarchy_level"
)

// SkippedUser records a snapshot user that was not imported, and why.
// The organization snapshot contract leaves hierarchy-level existence to sync
// time, because the snapshot carries level references but no level definitions.
type SkippedUser struct {
	UserID           string `json:"userId"`
	Username         string `json:"username"`
	HierarchyLevelID string `json:"hierarchyLevelId"`
	Reason           string `json:"reason"`
}

// ApplyInput carries a filtered, already-validated snapshot into persistence,
// along with the corrections the filter had to make.
type ApplyInput struct {
	// Snapshot contains only importable records.
	Snapshot *orgsnapshot.Snapshot

	// PreservedMemberUserIDs are users excluded from the snapshot whose existing
	// team_members rows must survive the per-team membership replace. Without
	// this, skipping a user would silently strip them from every team.
	PreservedMemberUserIDs []string

	// PreserveReportsToUserIDs are users whose manager was skipped. The snapshot
	// no longer names a manager for them, but that absence is an artefact of
	// filtering rather than a statement by the provider, so their existing
	// reports_to is left untouched.
	PreserveReportsToUserIDs map[string]bool
}

// ApplyResult reports what a sync changed.
type ApplyResult struct {
	TeamsSynced        int `json:"teamsSynced"`
	UsersSynced        int `json:"usersSynced"`
	MembershipsSynced  int `json:"membershipsSynced"`
	MembershipsRemoved int `json:"membershipsRemoved"`

	// HealthChecksDisabled counts teams that were participating in health checks
	// and are switched off by this sync. Surfaced prominently because a provider
	// that reports healthCheckEnabled=false org-wide would otherwise silently
	// disable every team.
	HealthChecksDisabled int `json:"healthChecksDisabled"`
	// HealthChecksEnabled counts teams switched on by this sync.
	HealthChecksEnabled int `json:"healthChecksEnabled"`
}

// Repository is the persistence contract for the provider integration.
type Repository interface {
	// GetCredentials returns the stored credential, or (nil, nil) when none is set.
	GetCredentials(ctx context.Context) (*Credentials, error)

	// SaveCredentials stores ciphertext for the given provider, replacing any
	// existing credential.
	SaveCredentials(ctx context.Context, provider, apiTokenEncrypted string) error

	// KnownHierarchyLevelIDs returns the level IDs configured in this deployment.
	KnownHierarchyLevelIDs(ctx context.Context) (map[string]bool, error)

	// ApplySnapshot writes the snapshot in a single transaction. It never deletes
	// users or teams; on any error nothing is committed.
	ApplySnapshot(ctx context.Context, in ApplyInput) (*ApplyResult, error)
}
