package services_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agopalakrishnan/teams360/backend/application/services"
	"github.com/agopalakrishnan/teams360/backend/domain/orgprovider"
	"github.com/agopalakrishnan/teams360/backend/pkg/orgsnapshot"
)

// fakeRunRecorder is a hand-rolled fake of services.SyncRunRecorder, recording
// every call it received for assertion, and letting a test script canned
// return values/errors for each read method.
type fakeRunRecorder struct {
	mu sync.Mutex

	attempts []orgprovider.OrgSyncAttempt
	skips    []orgprovider.OrgSyncSkip

	lastRun    *orgprovider.OrgSyncLastRun
	lastRunErr error

	activeClaim    *orgprovider.OrgSyncActiveClaim
	activeClaimErr error

	claimed []string
	cleared []string

	clearHoldCalls int
}

func (f *fakeRunRecorder) RecordOrgSyncAttempt(_ context.Context, attempt orgprovider.OrgSyncAttempt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, attempt)
	return nil
}

func (f *fakeRunRecorder) RecordOrgSyncSkip(_ context.Context, skip orgprovider.OrgSyncSkip) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.skips = append(f.skips, skip)
	return nil
}

func (f *fakeRunRecorder) GetOrgSyncLastRun(_ context.Context) (*orgprovider.OrgSyncLastRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastRunErr != nil {
		return nil, f.lastRunErr
	}
	if f.lastRun == nil {
		return &orgprovider.OrgSyncLastRun{}, nil
	}
	return f.lastRun, nil
}

func (f *fakeRunRecorder) ClearOrgSyncHold(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clearHoldCalls++
	return nil
}

func (f *fakeRunRecorder) ClaimOrgSyncActive(_ context.Context, instanceID, _ string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimed = append(f.claimed, instanceID)
	return nil
}

func (f *fakeRunRecorder) HeartbeatOrgSyncActive(_ context.Context, _ string, _ time.Time) error {
	return nil
}

func (f *fakeRunRecorder) ClearOrgSyncActive(_ context.Context, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, instanceID)
	return nil
}

func (f *fakeRunRecorder) GetOrgSyncActiveClaim(_ context.Context, _ time.Duration) (*orgprovider.OrgSyncActiveClaim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.activeClaimErr != nil {
		return nil, f.activeClaimErr
	}
	return f.activeClaim, nil
}

func (f *fakeRunRecorder) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.attempts)
}

func (f *fakeRunRecorder) lastAttempt() orgprovider.OrgSyncAttempt {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts[len(f.attempts)-1]
}

// fakeHeldLock records whether Release was called.
type fakeHeldLock struct {
	mu       sync.Mutex
	released bool
}

func (l *fakeHeldLock) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.released = true
}

// fakeLocker is a hand-rolled fake of services.SyncLocker.
type fakeLocker struct {
	// held, when true, means TryLock returns (nil, nil) -- locked elsewhere.
	held bool
	// err, when set, means TryLock fails outright (a configured locker that
	// cannot even attempt the lock).
	err error

	mu      sync.Mutex
	granted []*fakeHeldLock
}

func (l *fakeLocker) TryLock(_ context.Context) (services.HeldLock, error) {
	if l.err != nil {
		return nil, l.err
	}
	if l.held {
		return nil, nil
	}
	lock := &fakeHeldLock{}
	l.mu.Lock()
	l.granted = append(l.granted, lock)
	l.mu.Unlock()
	return lock, nil
}

// genericFailingRepo returns a plain, non-hold, non-ambiguous error from
// ApplySnapshot -- a confirmed rollback, not blocked and not ambiguous.
type genericFailingRepo struct{ err error }

func (r *genericFailingRepo) KnownHierarchyLevelIDs(_ context.Context) (map[string]bool, error) {
	return knownLevels(), nil
}

func (r *genericFailingRepo) ApplySnapshot(_ context.Context, _ orgprovider.ApplyInput) (*orgprovider.ApplyResult, error) {
	return nil, r.err
}

func TestSyncInProgressErrorWrapsTheSentinel(t *testing.T) {
	err := &services.SyncInProgressError{Trigger: orgprovider.TriggerScheduled}
	if !errors.Is(err, services.ErrSyncInProgress) {
		t.Fatal("SyncInProgressError must satisfy errors.Is(err, ErrSyncInProgress) via Unwrap")
	}
}

func TestSyncInProgressErrorNeverGuessesAnUnknownTrigger(t *testing.T) {
	err := &services.SyncInProgressError{Remote: true}
	msg := err.Error()
	if msg == "" {
		t.Fatal("expected a non-empty message")
	}
	// Must not assert a specific trigger it cannot support.
	if strings.Contains(msg, "manual") || strings.Contains(msg, "scheduled") {
		t.Fatalf("an unknown trigger must not be guessed as manual or scheduled, got: %q", msg)
	}
}

// TestLocalConcurrentSyncNamesTheRunningTrigger drives two overlapping
// SyncWithOptions calls on the SAME service instance: the first is gated
// inside ApplySnapshot (channel gating, the repo's own established
// concurrency-test pattern -- see settings_deletion_threshold_test.go's
// gatedFetcher), so the second's tier-1 CAS is guaranteed to lose while the
// first is genuinely still running.
type gatedRepo struct {
	gate    chan struct{}
	started chan struct{}
}

func (r *gatedRepo) KnownHierarchyLevelIDs(_ context.Context) (map[string]bool, error) {
	return knownLevels(), nil
}

func (r *gatedRepo) ApplySnapshot(_ context.Context, in orgprovider.ApplyInput) (*orgprovider.ApplyResult, error) {
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-r.gate
	return &orgprovider.ApplyResult{UsersSynced: len(in.Snapshot.Users)}, nil
}

func TestLocalConcurrentSyncNamesTheRunningTrigger(t *testing.T) {
	t.Setenv(services.EnvMaxDeletePercent, "20")
	repo := &gatedRepo{gate: make(chan struct{}), started: make(chan struct{}, 1)}
	service := services.NewOrganizationSyncService(repo, thresholdFetcher{}, nil, nil)

	firstDone := make(chan error, 1)
	go func() {
		_, err := service.SyncWithOptions(context.Background(), services.SyncOptions{Trigger: orgprovider.TriggerScheduled})
		firstDone <- err
	}()

	select {
	case <-repo.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first sync never reached ApplySnapshot")
	}

	_, err := service.SyncWithOptions(context.Background(), services.SyncOptions{Trigger: orgprovider.TriggerManual})
	var inProgress *services.SyncInProgressError
	if !errors.As(err, &inProgress) {
		t.Fatalf("expected a *SyncInProgressError, got %v", err)
	}
	if inProgress.Remote {
		t.Fatal("a same-replica collision must not be reported as remote")
	}
	if inProgress.Trigger != orgprovider.TriggerScheduled {
		t.Fatalf("expected the conflict to name the ACTUALLY running trigger (scheduled), got %q", inProgress.Trigger)
	}

	close(repo.gate)
	if err := <-firstDone; err != nil {
		t.Fatalf("the first (gated) sync should have completed cleanly, got %v", err)
	}
}

func TestConfiguredLockerErrorFailsSyncClosed(t *testing.T) {
	t.Setenv(services.EnvMaxDeletePercent, "20")
	repo := &recordingRepo{}
	locker := &fakeLocker{err: errors.New("advisory lock connection unavailable")}
	service := services.NewOrganizationSyncService(repo, thresholdFetcher{}, nil, nil,
		services.WithSyncLocker(locker))

	_, err := service.Sync(context.Background())
	if err == nil {
		t.Fatal("a configured locker that errors must fail the sync closed, not proceed unprotected")
	}
	if repo.lastInput.Snapshot != nil {
		t.Fatal("ApplySnapshot must never be reached when the locker fails to even attempt the lock")
	}
}

func TestRemoteLockHeldNamesTriggerAndInstanceWhenReadable(t *testing.T) {
	repo := &recordingRepo{}
	locker := &fakeLocker{held: true}
	recorder := &fakeRunRecorder{
		activeClaim: &orgprovider.OrgSyncActiveClaim{
			InstanceID: "replica-b", Trigger: orgprovider.TriggerScheduled, Stale: false,
		},
	}
	service := services.NewOrganizationSyncService(repo, thresholdFetcher{}, nil, nil,
		services.WithSyncLocker(locker), services.WithSyncRunRecorder(recorder))

	_, err := service.Sync(context.Background())
	var inProgress *services.SyncInProgressError
	if !errors.As(err, &inProgress) {
		t.Fatalf("expected a *SyncInProgressError, got %v", err)
	}
	if !inProgress.Remote {
		t.Fatal("a lock held by another replica must be reported as remote")
	}
	if inProgress.Trigger != orgprovider.TriggerScheduled || inProgress.Instance != "replica-b" {
		t.Fatalf("expected attribution to the remote scheduled run, got %+v", inProgress)
	}
}

func TestRemoteLockHeldDegradesToUnknownTriggerWhenClaimIsUnreadable(t *testing.T) {
	repo := &recordingRepo{}
	locker := &fakeLocker{held: true}
	recorder := &fakeRunRecorder{activeClaimErr: errors.New("connection reset")}
	service := services.NewOrganizationSyncService(repo, thresholdFetcher{}, nil, nil,
		services.WithSyncLocker(locker), services.WithSyncRunRecorder(recorder))

	_, err := service.Sync(context.Background())
	var inProgress *services.SyncInProgressError
	if !errors.As(err, &inProgress) {
		t.Fatalf("expected a *SyncInProgressError, got %v", err)
	}
	if inProgress.Trigger != "" {
		t.Fatalf("an unreadable remote claim must degrade to an unknown trigger, not a guessed one; got %q", inProgress.Trigger)
	}
	// The refusal itself must still be sent -- an unreadable claim must never
	// block the conflict response the caller already knows it must send.
	if !inProgress.Remote {
		t.Fatal("expected the refusal to still be reported as remote")
	}
}

func TestBlockedAttemptRecordsWritesNoneNotUnknown(t *testing.T) {
	t.Setenv(services.EnvMaxDeletePercent, "")
	repo := &holdingRepo{}
	recorder := &fakeRunRecorder{}
	service := services.NewOrganizationSyncService(repo, thresholdFetcher{}, nil, nil,
		services.WithDeleteThresholdStore(&thresholdStore{saved: floatPtr(20)}),
		services.WithSyncRunRecorder(recorder))

	if _, err := service.Sync(context.Background()); err == nil {
		t.Fatal("expected the attempt to be held")
	}

	if recorder.attemptCount() != 1 {
		t.Fatalf("expected exactly one durable attempt record, got %d", recorder.attemptCount())
	}
	attempt := recorder.lastAttempt()
	if attempt.Status != orgprovider.StatusBlocked {
		t.Fatalf("expected status=blocked, got %q", attempt.Status)
	}
	if attempt.WritesStatus != orgprovider.WritesNone {
		t.Fatalf("a blocked attempt writes nothing -- expected writesStatus=none, got %q", attempt.WritesStatus)
	}
	if attempt.ThresholdPercent == nil || *attempt.ThresholdPercent != 20 {
		t.Fatalf("expected the threshold actually judged against to be recorded, got %v", attempt.ThresholdPercent)
	}
	if attempt.UsersDeleting == nil || attempt.UsersPercent == nil {
		t.Fatal("a blocked attempt must record its computed deletion counts/percentages")
	}
}

func TestFailedAttemptBeforeApplySnapshotRecordsWritesNoneAndNoThreshold(t *testing.T) {
	t.Setenv(services.EnvMaxDeletePercent, "20")
	recorder := &fakeRunRecorder{}
	// A fetcher failure happens before ApplySnapshot is ever reached, so the
	// threshold, though already resolved by this point, must NOT be recorded
	// -- only blocked/success outcomes carry a threshold.
	service := services.NewOrganizationSyncService(&recordingRepo{}, failingFetcher{err: errors.New("provider unreachable")}, nil, nil,
		services.WithSyncRunRecorder(recorder))

	if _, err := service.Sync(context.Background()); err == nil {
		t.Fatal("expected a fetch failure")
	}

	attempt := recorder.lastAttempt()
	if attempt.Status != orgprovider.StatusFailed {
		t.Fatalf("expected status=failed, got %q", attempt.Status)
	}
	if attempt.WritesStatus != orgprovider.WritesNone {
		t.Fatalf("a confirmed pre-write failure must record writesStatus=none, got %q", attempt.WritesStatus)
	}
	if attempt.ThresholdPercent != nil {
		t.Fatal("a failed attempt must never carry a threshold, even if one had already been resolved")
	}
}

func TestAmbiguousCommitRecordsWritesUnknownNotNoneOrSuccess(t *testing.T) {
	t.Setenv(services.EnvMaxDeletePercent, "20")
	recorder := &fakeRunRecorder{}
	ambiguous := errors.Join(orgprovider.ErrSyncCommitAmbiguous, errors.New("connection reset during commit"))
	service := services.NewOrganizationSyncService(&genericFailingRepo{err: ambiguous}, thresholdFetcher{}, nil, nil,
		services.WithSyncRunRecorder(recorder))

	if _, err := service.Sync(context.Background()); !errors.Is(err, orgprovider.ErrSyncCommitAmbiguous) {
		t.Fatalf("expected the ambiguous-commit error to propagate, got %v", err)
	}

	attempt := recorder.lastAttempt()
	if attempt.WritesStatus != orgprovider.WritesUnknown {
		t.Fatalf("an ambiguous commit must record writesStatus=unknown, got %q", attempt.WritesStatus)
	}
	if attempt.Status != orgprovider.StatusFailed {
		t.Fatalf("expected status=failed for an ambiguous outcome, got %q", attempt.Status)
	}
}

func TestConfirmedRollbackIsNeverRecordedAsUnknown(t *testing.T) {
	t.Setenv(services.EnvMaxDeletePercent, "20")
	recorder := &fakeRunRecorder{}
	// A plain, non-ambiguous error: the transaction was deliberately rolled
	// back, a fully deterministic outcome.
	service := services.NewOrganizationSyncService(&genericFailingRepo{err: errors.New("constraint violation")}, thresholdFetcher{}, nil, nil,
		services.WithSyncRunRecorder(recorder))

	if _, err := service.Sync(context.Background()); err == nil {
		t.Fatal("expected an error")
	}

	attempt := recorder.lastAttempt()
	if attempt.WritesStatus != orgprovider.WritesNone {
		t.Fatalf("a confirmed rollback must record writesStatus=none, not %q -- conflating it with unknown defeats the tri-state design", attempt.WritesStatus)
	}
}

func TestSchedulerBlockedNeverConsultsTheDurableHold(t *testing.T) {
	t.Setenv(services.EnvMaxDeletePercent, "20")
	recorder := &fakeRunRecorder{
		lastRun: &orgprovider.OrgSyncLastRun{
			HoldActive:     true,
			HoldRecordedAt: timePtr(time.Now().UTC()),
		},
	}
	service := services.NewOrganizationSyncService(&recordingRepo{}, thresholdFetcher{}, nil, nil,
		services.WithSyncRunRecorder(recorder))

	// SchedulerBlocked must read ONLY in-memory state: with no in-memory hold
	// set (this service has never run a sync), it must report false even
	// though the durable flag says held -- otherwise a restart-surviving hold
	// would block the scheduler, contradicting the decision that a restart
	// permits a harmless re-attempt.
	if service.SchedulerBlocked() {
		t.Fatal("SchedulerBlocked must never be true from the durable hold alone")
	}

	// LockState, by contrast, MUST see the durable hold -- this is the
	// structural proof that the two are genuinely different code paths, not
	// the same check filtered two ways.
	state, err := service.LockState(context.Background())
	if err != nil {
		t.Fatalf("unexpected LockState error: %v", err)
	}
	if !state.Held {
		t.Fatal("LockState must be fail-closed on the durable hold_active flag")
	}
}

func TestLockStateFailsClosedOnADurableReadError(t *testing.T) {
	recorder := &fakeRunRecorder{lastRunErr: errors.New("connection reset")}
	service := services.NewOrganizationSyncService(&recordingRepo{}, thresholdFetcher{}, nil, nil,
		services.WithSyncRunRecorder(recorder))

	_, err := service.LockState(context.Background())
	if err == nil {
		t.Fatal("a durable-state read error must be returned, never silently treated as unlocked")
	}
}

func TestClearHoldClearsBothInMemoryAndDurable(t *testing.T) {
	t.Setenv(services.EnvMaxDeletePercent, "")
	repo := &holdingRepo{}
	recorder := &fakeRunRecorder{}
	service := services.NewOrganizationSyncService(repo, thresholdFetcher{}, nil, nil,
		services.WithDeleteThresholdStore(&thresholdStore{saved: floatPtr(20)}),
		services.WithSyncRunRecorder(recorder))

	if _, err := service.Sync(context.Background()); err == nil {
		t.Fatal("expected the attempt to be held")
	}

	if err := service.ClearHold(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recorder.clearHoldCalls != 1 {
		t.Fatalf("expected ClearOrgSyncHold to be called exactly once, got %d", recorder.clearHoldCalls)
	}
	if service.DismissHold() {
		t.Fatal("the in-memory hold should already be clear after ClearHold")
	}
}

func TestClaimAndClearActiveRunAreBothCalled(t *testing.T) {
	t.Setenv(services.EnvMaxDeletePercent, "20")
	recorder := &fakeRunRecorder{}
	service := services.NewOrganizationSyncService(&recordingRepo{}, thresholdFetcher{}, nil, nil,
		services.WithInstanceID("test-instance"), services.WithSyncRunRecorder(recorder))

	if _, err := service.Sync(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(recorder.claimed) != 1 || recorder.claimed[0] != "test-instance" {
		t.Fatalf("expected the active run to be claimed under this instance, got %v", recorder.claimed)
	}
	if len(recorder.cleared) != 1 || recorder.cleared[0] != "test-instance" {
		t.Fatalf("expected the active run claim to be cleared on completion, got %v", recorder.cleared)
	}
}

// failingFetcher always fails, for testing the pre-ApplySnapshot failure path.
type failingFetcher struct{ err error }

func (f failingFetcher) FetchSnapshot(_ context.Context) (*orgsnapshot.Snapshot, error) {
	return nil, f.err
}

func timePtr(t time.Time) *time.Time { return &t }
