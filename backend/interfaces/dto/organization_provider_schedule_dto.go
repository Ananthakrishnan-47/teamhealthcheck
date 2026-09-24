package dto

// OrgSyncScheduleDTO reports the automatic organization-sync schedule.
// AvailableFrequencies/DefaultPercent-style bounds are served to the client
// rather than hardcoded in the UI, mirroring OrgSyncDeletionThreshold.
type OrgSyncScheduleDTO struct {
	Enabled   bool   `json:"enabled"`
	Frequency string `json:"frequency,omitempty"`
	// NextRunAt is RFC3339, nil while disabled.
	NextRunAt *string `json:"nextRunAt,omitempty"`
	// AvailableFrequencies is the fixed set of choices the UI presents.
	// Never raw cron syntax.
	AvailableFrequencies []string `json:"availableFrequencies"`
	// DefaultFrequency is used only on the very first enable, when no
	// frequency has ever been saved.
	DefaultFrequency string `json:"defaultFrequency"`
}

// UpdateOrgSyncScheduleRequest is the body of PUT .../schedule. Enabled is a
// pointer so an absent/null value is rejected outright instead of being read
// as false. Frequency is optional -- omitted means "keep whatever is saved,
// or default to Weekly on a genuinely first-ever enable".
type UpdateOrgSyncScheduleRequest struct {
	Enabled   *bool   `json:"enabled"`
	Frequency *string `json:"frequency"`
}

// OrgSyncLastRunDTO is the persisted result of the organization sync's most
// recent activity. LastAttempt and LastSkip are independently nil/absent: a
// skip never overwrites the last attempt that actually ran.
type OrgSyncLastRunDTO struct {
	LastAttempt *OrgSyncAttemptDTO `json:"lastAttempt,omitempty"`
	LastSkip    *OrgSyncSkipDTO    `json:"lastSkip,omitempty"`
}

// OrgSyncAttemptDTO is one sync attempt that ran to a conclusion (success,
// blocked, or failed). Count/percent fields are nil together for a failed
// attempt that never evaluated a threshold -- never 0, which would read as a
// safe sync.
type OrgSyncAttemptDTO struct {
	Trigger    string `json:"trigger"`
	Status     string `json:"status"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`

	ThresholdPercent *float64 `json:"thresholdPercent,omitempty"`
	UsersExisting    *int     `json:"usersExisting,omitempty"`
	UsersDeleting    *int     `json:"usersDeleting,omitempty"`
	UsersPercent     *float64 `json:"usersPercent,omitempty"`
	TeamsExisting    *int     `json:"teamsExisting,omitempty"`
	TeamsDeleting    *int     `json:"teamsDeleting,omitempty"`
	TeamsPercent     *float64 `json:"teamsPercent,omitempty"`

	// WritesStatus is "none", "applied", or "unknown" -- see
	// orgprovider.WritesStatus's doc for the precise definition of each.
	// "applied" means the transaction committed, not that any row changed;
	// always shown alongside the counts above, never alone.
	WritesStatus string `json:"writesStatus"`
	OverrideUsed bool   `json:"overrideUsed"`

	UsersSynced *int   `json:"usersSynced,omitempty"`
	TeamsSynced *int   `json:"teamsSynced,omitempty"`
	Message     string `json:"message"`
}

// OrgSyncSkipDTO is the most recent scheduled occurrence that was skipped
// rather than attempted.
type OrgSyncSkipDTO struct {
	Reason string `json:"reason"`
	At     string `json:"at"`
}
