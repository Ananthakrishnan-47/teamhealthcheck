package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/agopalakrishnan/teams360/backend/domain/orgprovider"
	"github.com/lib/pq"
)

// OrganizationProviderRepository implements orgprovider.Repository.
type OrganizationProviderRepository struct {
	db *sql.DB
}

// NewOrganizationProviderRepository creates a new repository instance.
func NewOrganizationProviderRepository(db *sql.DB) orgprovider.Repository {
	return &OrganizationProviderRepository{db: db}
}

// GetCredentials returns the stored credential, or (nil, nil) when none is set.
func (r *OrganizationProviderRepository) GetCredentials(ctx context.Context) (*orgprovider.Credentials, error) {
	var creds orgprovider.Credentials

	err := r.db.QueryRowContext(ctx, `
		SELECT provider, api_token_encrypted, updated_at
		FROM organization_provider_credentials
		WHERE id = 1
	`).Scan(&creds.Provider, &creds.APITokenEncrypted, &creds.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query provider credentials: %w", err)
	}

	return &creds, nil
}

// SaveCredentials stores ciphertext for the given provider.
func (r *OrganizationProviderRepository) SaveCredentials(ctx context.Context, provider, apiTokenEncrypted string) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO organization_provider_credentials (id, provider, api_token_encrypted, updated_at)
		VALUES (1, $1, $2, NOW())
		ON CONFLICT (id) DO UPDATE SET
			provider = EXCLUDED.provider,
			api_token_encrypted = EXCLUDED.api_token_encrypted,
			updated_at = NOW()
	`, provider, apiTokenEncrypted)

	if err != nil {
		return fmt.Errorf("failed to save provider credentials: %w", err)
	}
	return nil
}

// KnownHierarchyLevelIDs returns the level IDs configured in this deployment.
func (r *OrganizationProviderRepository) KnownHierarchyLevelIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM hierarchy_levels`)
	if err != nil {
		return nil, fmt.Errorf("failed to query hierarchy levels: %w", err)
	}
	defer rows.Close()

	levels := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan hierarchy level: %w", err)
		}
		levels[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error reading hierarchy levels: %w", err)
	}

	return levels, nil
}

// ApplySnapshot writes a filtered snapshot in one transaction.
//
// This deliberately does not reuse UserRepository.Update / TeamRepository.Update.
// Those rebuild team_members from opposite directions — one deletes by user_id,
// the other by team_id — so driving both across a single sync would have each
// erase the other's writes. They also cannot join a caller's transaction, which
// would make an all-or-nothing sync impossible.
func (r *OrganizationProviderRepository) ApplySnapshot(ctx context.Context, in orgprovider.ApplyInput) (*orgprovider.ApplyResult, error) {
	if in.Snapshot == nil {
		return nil, fmt.Errorf("snapshot is required")
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	result := &orgprovider.ApplyResult{}

	if err := countHealthCheckTransitions(ctx, tx, in, result); err != nil {
		return nil, err
	}
	if err := upsertSnapshotUsers(ctx, tx, in); err != nil {
		return nil, err
	}
	if err := applySnapshotReportsTo(ctx, tx, in); err != nil {
		return nil, err
	}
	if err := upsertSnapshotTeams(ctx, tx, in); err != nil {
		return nil, err
	}
	if err := replaceSnapshotMemberships(ctx, tx, in, result); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit sync transaction: %w", err)
	}

	result.UsersSynced = len(in.Snapshot.Users)
	result.TeamsSynced = len(in.Snapshot.Teams)
	result.MembershipsSynced = len(in.Snapshot.Memberships)

	return result, nil
}

// countHealthCheckTransitions measures how many teams actually change state, so
// the sync can report "N teams disabled" rather than leaving it to be discovered.
func countHealthCheckTransitions(ctx context.Context, tx *sql.Tx, in orgprovider.ApplyInput, result *orgprovider.ApplyResult) error {
	var disabling, enabling []string
	for _, t := range in.Snapshot.Teams {
		if t.HealthCheckEnabled == nil {
			continue // omitted preserves the existing value
		}
		if *t.HealthCheckEnabled {
			enabling = append(enabling, t.ID)
		} else {
			disabling = append(disabling, t.ID)
		}
	}

	if len(disabling) > 0 {
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM teams WHERE id = ANY($1) AND health_check_enabled = true
		`, pq.Array(disabling)).Scan(&result.HealthChecksDisabled); err != nil {
			return fmt.Errorf("failed to count health check transitions: %w", err)
		}
	}

	if len(enabling) > 0 {
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM teams WHERE id = ANY($1) AND health_check_enabled = false
		`, pq.Array(enabling)).Scan(&result.HealthChecksEnabled); err != nil {
			return fmt.Errorf("failed to count health check transitions: %w", err)
		}
	}

	return nil
}

// upsertSnapshotUsers writes users with reports_to left alone. The column is a
// self-referencing foreign key, so manager links can only be set once every row
// in the snapshot exists — see applySnapshotReportsTo.
//
// password_hash and auth_type are set on insert but never overwritten on
// conflict: a person who already signs in to THC locally must keep their
// credentials when they also appear in a provider snapshot.
func upsertSnapshotUsers(ctx context.Context, tx *sql.Tx, in orgprovider.ApplyInput) error {
	for _, u := range in.Snapshot.Users {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO users (id, username, email, full_name, hierarchy_level_id, reports_to, password_hash, auth_type, updated_at)
			VALUES ($1, $2, $3, $4, $5, NULL, '', 'sso', CURRENT_TIMESTAMP)
			ON CONFLICT (id) DO UPDATE SET
				username = EXCLUDED.username,
				email = EXCLUDED.email,
				full_name = EXCLUDED.full_name,
				hierarchy_level_id = EXCLUDED.hierarchy_level_id,
				updated_at = CURRENT_TIMESTAMP
		`, u.ID, u.Username, u.Email, u.DisplayName, u.HierarchyLevelID)

		if err != nil {
			// users.username and users.email are UNIQUE, so a provider record
			// colliding with a different existing THC user surfaces here rather
			// than as a conflict on id. Name the record so it can be fixed.
			return fmt.Errorf("failed to upsert user %q (%s): %w", u.Username, u.ID, err)
		}
	}
	return nil
}

// applySnapshotReportsTo writes manager links now that every snapshot user exists.
func applySnapshotReportsTo(ctx context.Context, tx *sql.Tx, in orgprovider.ApplyInput) error {
	for _, u := range in.Snapshot.Users {
		if in.PreserveReportsToUserIDs[u.ID] {
			// The provider named a manager we could not import. Treat that as
			// "no statement" and keep whatever THC already had, rather than
			// asserting this person now reports to nobody.
			continue
		}

		var reportsTo interface{}
		if u.ReportsToID != nil {
			reportsTo = *u.ReportsToID
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE users SET reports_to = $2, updated_at = CURRENT_TIMESTAMP WHERE id = $1
		`, u.ID, reportsTo); err != nil {
			return fmt.Errorf("failed to set manager for user %q: %w", u.ID, err)
		}
	}
	return nil
}

// upsertSnapshotTeams writes teams. team_lead_id and health_check_enabled use the
// bound parameters rather than EXCLUDED so that a nil (field omitted by the
// provider) preserves the existing THC value, per the contract's tri-state rule.
func upsertSnapshotTeams(ctx context.Context, tx *sql.Tx, in orgprovider.ApplyInput) error {
	for _, t := range in.Snapshot.Teams {
		var teamLead interface{}
		if t.TeamLeadID != nil {
			teamLead = *t.TeamLeadID
		}
		var healthCheck interface{}
		if t.HealthCheckEnabled != nil {
			healthCheck = *t.HealthCheckEnabled
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO teams (id, name, team_lead_id, health_check_enabled, updated_at)
			VALUES ($1, $2, $3, COALESCE($4::boolean, true), CURRENT_TIMESTAMP)
			ON CONFLICT (id) DO UPDATE SET
				name = EXCLUDED.name,
				team_lead_id = COALESCE($3::varchar, teams.team_lead_id),
				health_check_enabled = COALESCE($4::boolean, teams.health_check_enabled),
				updated_at = CURRENT_TIMESTAMP
		`, t.ID, t.Name, teamLead, healthCheck); err != nil {
			return fmt.Errorf("failed to upsert team %q (%s): %w", t.Name, t.ID, err)
		}
	}
	return nil
}

// replaceSnapshotMemberships makes the provider authoritative for the membership
// of every team it reports, while protecting rows belonging to users the sync
// could not import — otherwise skipping a user would quietly remove them from
// every team they belong to.
func replaceSnapshotMemberships(ctx context.Context, tx *sql.Tx, in orgprovider.ApplyInput, result *orgprovider.ApplyResult) error {
	membersByTeam := make(map[string][]string, len(in.Snapshot.Teams))
	for _, t := range in.Snapshot.Teams {
		membersByTeam[t.ID] = nil
	}
	for _, m := range in.Snapshot.Memberships {
		membersByTeam[m.TeamID] = append(membersByTeam[m.TeamID], m.UserID)
	}

	for _, t := range in.Snapshot.Teams {
		keep := append([]string{}, membersByTeam[t.ID]...)
		keep = append(keep, in.PreservedMemberUserIDs...)

		res, err := tx.ExecContext(ctx, `
			DELETE FROM team_members WHERE team_id = $1 AND user_id <> ALL($2)
		`, t.ID, pq.Array(keep))
		if err != nil {
			return fmt.Errorf("failed to prune members of team %q: %w", t.ID, err)
		}
		if removed, err := res.RowsAffected(); err == nil {
			result.MembershipsRemoved += int(removed)
		}

		for _, userID := range membersByTeam[t.ID] {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO team_members (team_id, user_id) VALUES ($1, $2)
				ON CONFLICT (team_id, user_id) DO NOTHING
			`, t.ID, userID); err != nil {
				return fmt.Errorf("failed to add user %q to team %q: %w", userID, t.ID, err)
			}
		}
	}

	return nil
}
