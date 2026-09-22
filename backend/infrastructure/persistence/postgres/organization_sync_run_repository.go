package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/agopalakrishnan/teams360/backend/domain/orgprovider"
)

// RecordOrgSyncAttempt persists an attempt that did NOT commit any
// destructive writes: a blocked hold, or a failure at any point in
// ApplySnapshot. Both rely on that function's own deferred Rollback(), so by
// construction nothing destructive ever accompanies them -- there is no
// crash window to close here, unlike the success path, which
// ApplySnapshot records internally in its own transaction (see
// recordSuccessAttempt in organization_provider_repository.go). This method
// must never be called for a success outcome.
//
// hold_active is written only for a Blocked outcome (set true) -- a Failed
// outcome leaves it untouched, since a later unrelated technical failure must
// never silently clear a hold an earlier attempt correctly raised.
func (r *OrganizationProviderRepository) RecordOrgSyncAttempt(ctx context.Context, attempt orgprovider.OrgSyncAttempt) error {
	if attempt.Status == orgprovider.StatusSuccess {
		return fmt.Errorf("RecordOrgSyncAttempt must not be called for a success outcome; ApplySnapshot records that internally")
	}

	// One query, not two near-duplicate strings: a blocked outcome sets the
	// durable hold via a CASE on $17 (isBlocked); every other outcome
	// (failed) leaves hold_active/hold_recorded_at untouched by falling
	// through to their own current value, since a later unrelated technical
	// failure must never silently clear a hold an earlier attempt correctly
	// raised.
	isBlocked := attempt.Status == orgprovider.StatusBlocked
	args := []any{
		attempt.Trigger, attempt.Status, attempt.StartedAt, attempt.FinishedAt, attempt.InstanceID,
		attempt.ThresholdPercent,
		attempt.UsersExisting, attempt.UsersDeleting, attempt.UsersPercent,
		attempt.TeamsExisting, attempt.TeamsDeleting, attempt.TeamsPercent,
		attempt.WritesStatus, attempt.OverrideUsed,
		attempt.UsersSynced, attempt.TeamsSynced,
		isBlocked,
	}

	const query = `
		UPDATE org_sync_runs SET
			last_trigger = $1, last_status = $2, last_started_at = $3, last_finished_at = $4,
			last_instance_id = $5, last_threshold_percent = $6,
			last_users_existing = $7, last_users_deleting = $8, last_users_percent = $9,
			last_teams_existing = $10, last_teams_deleting = $11, last_teams_percent = $12,
			last_writes_status = $13, last_override_used = $14,
			last_users_synced = $15, last_teams_synced = $16,
			hold_active = CASE WHEN $17 THEN true ELSE hold_active END,
			hold_recorded_at = CASE WHEN $17 THEN $4 ELSE hold_recorded_at END,
			updated_at = NOW()
		WHERE id = 1
	`

	if _, err := r.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("failed to record sync attempt: %w", err)
	}
	return nil
}

// RecordOrgSyncSkip persists that a scheduled occurrence was skipped rather
// than attempted. Names only last_skip_* columns -- it cannot, by
// construction, displace the last attempt that actually ran.
func (r *OrganizationProviderRepository) RecordOrgSyncSkip(ctx context.Context, skip orgprovider.OrgSyncSkip) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE org_sync_runs SET
			last_skip_reason = $1,
			last_skip_at = $2,
			updated_at = NOW()
		WHERE id = 1
	`, skip.Reason, skip.At)
	if err != nil {
		return fmt.Errorf("failed to record sync skip: %w", err)
	}
	return nil
}

// GetOrgSyncLastRun reads the assembled last-attempt/last-skip/hold state.
func (r *OrganizationProviderRepository) GetOrgSyncLastRun(ctx context.Context) (*orgprovider.OrgSyncLastRun, error) {
	var (
		trigger, status, instanceID, writesStatus, message         sql.NullString
		startedAt, finishedAt                                      sql.NullTime
		thresholdPercent, usersPercent, teamsPercent               sql.NullFloat64
		usersExisting, usersDeleting, teamsExisting, teamsDeleting sql.NullInt64
		usersSynced, teamsSynced                                   sql.NullInt64
		overrideUsed, holdActive                                   bool
		holdRecordedAt                                             sql.NullTime
		skipReason                                                 sql.NullString
		skipAt                                                     sql.NullTime
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT
			last_trigger, last_status, last_started_at, last_finished_at, last_instance_id,
			last_threshold_percent,
			last_users_existing, last_users_deleting, last_users_percent,
			last_teams_existing, last_teams_deleting, last_teams_percent,
			last_writes_status, last_override_used, last_users_synced, last_teams_synced, last_message,
			hold_active, hold_recorded_at,
			last_skip_reason, last_skip_at
		FROM org_sync_runs WHERE id = 1
	`).Scan(
		&trigger, &status, &startedAt, &finishedAt, &instanceID,
		&thresholdPercent,
		&usersExisting, &usersDeleting, &usersPercent,
		&teamsExisting, &teamsDeleting, &teamsPercent,
		&writesStatus, &overrideUsed, &usersSynced, &teamsSynced, &message,
		&holdActive, &holdRecordedAt,
		&skipReason, &skipAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			// The row is seeded by migration 000023 and never deleted; this
			// should not happen, but "never run" is the honest fallback.
			return &orgprovider.OrgSyncLastRun{}, nil
		}
		return nil, fmt.Errorf("failed to read organization sync last run: %w", err)
	}

	result := &orgprovider.OrgSyncLastRun{HoldActive: holdActive}
	if holdRecordedAt.Valid {
		t := holdRecordedAt.Time
		result.HoldRecordedAt = &t
	}

	if status.Valid {
		attempt := &orgprovider.OrgSyncAttempt{
			Trigger:      trigger.String,
			Status:       status.String,
			StartedAt:    startedAt.Time,
			FinishedAt:   finishedAt.Time,
			InstanceID:   instanceID.String,
			WritesStatus: writesStatus.String,
			OverrideUsed: overrideUsed,
			Message:      message.String,
		}
		if thresholdPercent.Valid {
			v := thresholdPercent.Float64
			attempt.ThresholdPercent = &v
		}
		if usersExisting.Valid {
			v := int(usersExisting.Int64)
			attempt.UsersExisting = &v
		}
		if usersDeleting.Valid {
			v := int(usersDeleting.Int64)
			attempt.UsersDeleting = &v
		}
		if usersPercent.Valid {
			v := usersPercent.Float64
			attempt.UsersPercent = &v
		}
		if teamsExisting.Valid {
			v := int(teamsExisting.Int64)
			attempt.TeamsExisting = &v
		}
		if teamsDeleting.Valid {
			v := int(teamsDeleting.Int64)
			attempt.TeamsDeleting = &v
		}
		if teamsPercent.Valid {
			v := teamsPercent.Float64
			attempt.TeamsPercent = &v
		}
		if usersSynced.Valid {
			v := int(usersSynced.Int64)
			attempt.UsersSynced = &v
		}
		if teamsSynced.Valid {
			v := int(teamsSynced.Int64)
			attempt.TeamsSynced = &v
		}
		result.LastAttempt = attempt
	}

	if skipReason.Valid {
		result.LastSkip = &orgprovider.OrgSyncSkip{Reason: skipReason.String, At: skipAt.Time}
	}

	return result, nil
}

// ClearOrgSyncHold clears the durable mass-deletion hold unconditionally.
// Called by the "dismiss without syncing" admin action ALONGSIDE (never
// instead of) the in-memory DismissHold, so the escape hatch works even on a
// replica that never itself ran the held sync.
func (r *OrganizationProviderRepository) ClearOrgSyncHold(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE org_sync_runs SET hold_active = false, hold_recorded_at = NULL, updated_at = NOW()
		WHERE id = 1
	`)
	if err != nil {
		return fmt.Errorf("failed to clear organization sync hold: %w", err)
	}
	return nil
}

// ClaimOrgSyncActive records that a run has started, for cross-replica
// attribution. Best-effort: a failure here must never fail the sync -- the
// caller logs and continues. A replica holding the advisory lock may
// overwrite unconditionally, which evicts any orphaned claim from a
// previous, uncleanly-terminated run.
func (r *OrganizationProviderRepository) ClaimOrgSyncActive(ctx context.Context, instanceID, trigger string, startedAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO org_sync_active_runs (id, instance_id, trigger, started_at, heartbeat_at)
		VALUES (1, $1, $2, $3, $3)
		ON CONFLICT (id) DO UPDATE SET
			instance_id = EXCLUDED.instance_id,
			trigger = EXCLUDED.trigger,
			started_at = EXCLUDED.started_at,
			heartbeat_at = EXCLUDED.heartbeat_at
	`, instanceID, trigger, startedAt)
	if err != nil {
		return fmt.Errorf("failed to claim organization sync active run: %w", err)
	}
	return nil
}

// HeartbeatOrgSyncActive refreshes the claim's liveness timestamp. Best-effort.
func (r *OrganizationProviderRepository) HeartbeatOrgSyncActive(ctx context.Context, instanceID string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE org_sync_active_runs SET heartbeat_at = $1 WHERE id = 1 AND instance_id = $2
	`, at, instanceID)
	if err != nil {
		return fmt.Errorf("failed to update organization sync active run heartbeat: %w", err)
	}
	return nil
}

// ClearOrgSyncActive removes the claim. Best-effort: scoped to instanceID so
// a replica can never clear a claim it does not itself hold.
func (r *OrganizationProviderRepository) ClearOrgSyncActive(ctx context.Context, instanceID string) error {
	_, err := r.db.ExecContext(ctx, `
		DELETE FROM org_sync_active_runs WHERE id = 1 AND instance_id = $1
	`, instanceID)
	if err != nil {
		return fmt.Errorf("failed to clear organization sync active run: %w", err)
	}
	return nil
}

// GetOrgSyncActiveClaim reads the current claim, if any, resolving staleness
// against the DATABASE's own clock (NOW()) -- never a replica's local clock,
// so skewed replicas still agree. Returns (nil, nil) when no run is claimed.
// Attribution and UI accuracy only: nothing here is ever consulted to decide
// whether a sync may start.
func (r *OrganizationProviderRepository) GetOrgSyncActiveClaim(ctx context.Context, staleAfter time.Duration) (*orgprovider.OrgSyncActiveClaim, error) {
	var instanceID, trigger string
	var startedAt, heartbeatAt time.Time
	var stale bool
	// staleAfter.String() (Go's "30m0s" shape) is not valid Postgres interval
	// syntax -- pass seconds and build the interval in SQL via make_interval,
	// so the comparison stays against the database's own clock throughout.
	err := r.db.QueryRowContext(ctx, `
		SELECT instance_id, trigger, started_at, heartbeat_at, (heartbeat_at < NOW() - make_interval(secs => $1))
		FROM org_sync_active_runs WHERE id = 1
	`, staleAfter.Seconds()).Scan(&instanceID, &trigger, &startedAt, &heartbeatAt, &stale)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read organization sync active run: %w", err)
	}
	return &orgprovider.OrgSyncActiveClaim{
		InstanceID: instanceID,
		Trigger:    trigger,
		StartedAt:  startedAt,
		Stale:      stale,
	}, nil
}
