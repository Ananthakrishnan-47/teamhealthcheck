package orgprovider

import (
	"errors"
	"time"
)

// Trigger identifies what started a sync attempt.
const (
	TriggerManual    = "manual"
	TriggerScheduled = "scheduled"
)

// Terminal outcomes of a sync attempt that ran to a conclusion. There is
// deliberately no "running" status here -- an in-flight run's liveness lives
// in OrgSyncActiveClaim instead, so starting a run can never overwrite the
// previous attempt's result.
const (
	StatusSuccess = "success"
	StatusBlocked = "blocked"
	StatusFailed  = "failed"
)

// WritesStatus is a tri-state report of whether a sync attempt's destructive
// writes actually landed. It is not a boolean: a boolean cannot distinguish
// "confirmed no writes" from "cannot prove either way".
//
//   - WritesNone: the transaction never reached Commit() -- blocked before
//     writing, or a normal error was deliberately rolled back. Deterministic.
//   - WritesApplied: Commit() returned successfully. This means the
//     transaction committed, not that any row actually changed -- a sync
//     whose snapshot exactly matches current state legitimately commits with
//     zero deletions.
//   - WritesUnknown: Commit() itself errored, or a panic/connection loss
//     occurred around that call. The standard ambiguous-commit case: the
//     write may have landed on the server with the client never finding out.
const (
	WritesNone    = "none"
	WritesApplied = "applied"
	WritesUnknown = "unknown"
)

// SkipReason names why a scheduled occurrence was skipped rather than attempted.
const (
	SkipManualRunning    = "manual_sync_running"
	SkipScheduledRunning = "scheduled_sync_running"
	SkipHoldUnresolved   = "hold_unresolved"
	SkipOverdueCatchUp   = "overdue_catch_up_skipped"
)

// ErrSyncCommitAmbiguous means ApplySnapshot's own commit could not be
// confirmed to have succeeded or failed. The caller must record the attempt
// as WritesUnknown, never as a confirmed rollback (WritesNone) and never as a
// claimed success (WritesApplied).
var ErrSyncCommitAmbiguous = errors.New("organization sync: commit outcome could not be confirmed")

// OrgSyncAttempt is the durable record of one sync attempt that ran to a
// conclusion. RecordOrgSyncAttempt persists this and nothing else -- it never
// touches a skip column.
type OrgSyncAttempt struct {
	Trigger    string
	Status     string // StatusSuccess | StatusBlocked | StatusFailed
	StartedAt  time.Time
	FinishedAt time.Time
	InstanceID string

	// ThresholdPercent is nil when the attempt failed before a threshold was
	// ever evaluated.
	ThresholdPercent *float64

	// Users*/Teams* are nil together: set only for Blocked and Success
	// outcomes, where a deletion computation actually ran. Never 0 as a
	// stand-in for "not computed" -- 0 (0%) is a legitimate, safe outcome and
	// must not be confused with "unknown".
	UsersExisting *int
	UsersDeleting *int
	UsersPercent  *float64
	TeamsExisting *int
	TeamsDeleting *int
	TeamsPercent  *float64

	WritesStatus string // WritesNone | WritesApplied | WritesUnknown
	OverrideUsed bool

	UsersSynced *int
	TeamsSynced *int
	Message     string
}

// OrgSyncSkip is the durable record of a scheduled occurrence that was
// skipped rather than attempted.
type OrgSyncSkip struct {
	Reason string // Skip* constant
	At     time.Time
}

// OrgSyncLastRun is the assembled read of org_sync_runs. LastAttempt and
// LastSkip are independently nil: a skip never overwrites the last attempt
// that actually ran.
type OrgSyncLastRun struct {
	LastAttempt *OrgSyncAttempt
	LastSkip    *OrgSyncSkip
	// HoldActive is the durable mass-deletion hold, valid only while
	// HoldRecordedAt is within the TTL -- see LockState in the services
	// package for how this is combined with in-memory hold state.
	HoldActive     bool
	HoldRecordedAt *time.Time
}

// OrgSyncActiveClaim is the liveness claim of a sync currently in flight.
// Attribution and UI accuracy only: nothing in this type is ever consulted to
// decide whether a sync may start -- that is the Postgres advisory lock,
// entirely independent of this claim -- which is what makes a stale claim
// from a killed pod harmless.
type OrgSyncActiveClaim struct {
	InstanceID string
	Trigger    string
	StartedAt  time.Time
	// Stale reports whether this claim is older than the staleness bound, as
	// measured against the database's own clock -- never a replica's local
	// clock, so skewed replicas still agree.
	Stale bool
}
