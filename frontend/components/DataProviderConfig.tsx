"use client";

import { useState, useEffect, useRef } from "react";
import { AlertCircle, CheckCircle2, Loader2, RefreshCw, ShieldAlert } from "lucide-react";
import {
  getOrganizationProviderSettings,
  syncOrganizationProvider,
  getMassDeletionHold,
  clearAdminCache,
  DeletionMetric,
  MassDeletionReport,
  OrganizationProviderSettings,
  OrganizationSyncResult,
} from "@/lib/api/admin";
import { readSyncState, writeSyncState, clearSyncState, isStale, SYNC_STALE_TIMEOUT_MS, getActiveSyncPromise, setActiveSyncPromise } from "@/lib/admin-sync-state";

/**
 * Manual trigger for the external organization-data provider sync.
 *
 * The provider credential (DATA_PROVIDER_BASE_URL / DATA_PROVIDER_API_TOKEN)
 * is environment configuration on the backend -- there is deliberately no
 * token-entry UI here. Syncing can create, update, and hard-delete users,
 * teams, and memberships, so it is deliberately manual: an admin decides when
 * the organization changes shape.
 */
/** Renders a percentage the way the backend calculated it, e.g. "25.0%". */
function formatPercent(value: number): string {
  return `${value.toFixed(1)}%`;
}

/**
 * Renders one metric as "deleted / existing = percent%", using the backend's
 * own label for what the count means. A cascaded count is never called a
 * deletion: those rows go because the database removes them with their owner.
 */
function metricSentence(metric: DeletionMetric): string {
  const label = metric.kind === "cascaded" ? "cascaded" : "deleted";
  return `${metric.deleting} ${label} / ${metric.existing} existing = ${formatPercent(metric.percent)}`;
}

/**
 * Explains WHY a count is what it is: how many records the provider actually
 * sent for this entity type. A big deletion next to a small incoming count is
 * an incomplete payload, not a real mass departure -- which is the single most
 * common cause of a surprising hold, and is otherwise invisible from the UI.
 */
function incomingSentence(metric: DeletionMetric): string {
  return `provider sent ${metric.incoming}`;
}

export default function DataProviderConfig() {
  const [settings, setSettings] = useState<OrganizationProviderSettings | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
// Distinguish an unfetched/failed settings request from an unconfigured provider
  const [settingsLoadError, setSettingsLoadError] = useState<string | null>(null);

  // Initialized synchronously from localStorage so a remount (tab switch,
  // navigation, or page reload) never paints an enabled button before the
  // persisted in-progress state has a chance to disable it.
  const [syncing, setSyncing] = useState(() => {
    const persisted = readSyncState();
    return !!persisted && persisted.status === "in_progress" && !isStale(persisted);
  });
  const [syncResult, setSyncResult] = useState<OrganizationSyncResult | null>(null);
  // The counts behind a backend hold. Present only while a hold is unresolved:
  // it is what the admin reviews before deciding whether to override, and it is
  // cleared the moment another sync starts so a stale hold can never authorize
  // a later run.
  const [hold, setHold] = useState<MassDeletionReport | null>(null);
  // Two-step confirmation for the override, matching the destructive-action
  // pattern used elsewhere in admin: clicking Sync Anyway reveals a confirm
  // button rather than firing the request.
  const [confirmingOverride, setConfirmingOverride] = useState(false);
  const staleTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Shared by the original click and by a remount that reattached to the
  // still-running request, so both paths clear state and update the UI
  // identically regardless of which component instance observes completion.
  const settleSync = (outcome: { ok: true; result: OrganizationSyncResult } | { ok: false; error: any }) => {
    if (staleTimerRef.current) clearTimeout(staleTimerRef.current);
    clearSyncState();
    setActiveSyncPromise(null);
    setSyncing(false);
    if (outcome.ok) {
      setSyncResult(outcome.result);
      // The admin client caches users and teams for two minutes. Without this
      // the screen would keep showing pre-sync counts.
      clearAdminCache();
    } else {
      // A hold is not a failure to retry blindly -- it is a decision the admin
      // has to make, so it gets its own state and its own banner.
      const heldReport = getMassDeletionHold(outcome.error);
      if (heldReport) {
        setHold(heldReport);
        setConfirmingOverride(false);
      }
      setError(outcome.error?.message || "Synchronization failed");
    }
  };

  useEffect(() => {
    loadSettings();

    let cancelled = false;

    const liveSync = getActiveSyncPromise();
    if (liveSync) {
      // SPA navigation/tab switch: the JS module graph survived, so the
      // original request is still genuinely running. Reattach to it instead
      // of guessing from a snapshot -- this is authoritative, not a timeout.
      setSyncing(true);
      liveSync.then(
        (result) => {
          if (cancelled) return;
          settleSync({ ok: true, result: result as OrganizationSyncResult });
        },
        (err) => {
          if (cancelled) return;
          settleSync({ ok: false, error: err });
        }
      );
    } else {
      // No live promise in this session -- either nothing is running, or a
      // hard reload destroyed the module graph along with the real request
      // (which the backend also aborts server-side; see admin-sync-state.ts).
      // Fall back to the persisted snapshot, bounded by a stale timeout so a
      // reload that outlives the real sync never disables the button forever.
      const persisted = readSyncState();
      if (persisted && persisted.status === "in_progress") {
        if (isStale(persisted)) {
          clearSyncState();
          setSyncing(false);
        } else {
          setSyncing(true);
          const remaining = SYNC_STALE_TIMEOUT_MS - (Date.now() - persisted.startedAt);
          staleTimerRef.current = setTimeout(() => {
            clearSyncState();
            setSyncing(false);
          }, remaining);
        }
      }
    }

    return () => {
      cancelled = true;
      if (staleTimerRef.current) clearTimeout(staleTimerRef.current);
    };
  }, []);

  const loadSettings = async () => {
    setLoading(true);
    setSettingsLoadError(null);
    try {
      setSettings(await getOrganizationProviderSettings());
    } catch (err: any) {
      setSettingsLoadError(err.message || "Failed to load provider settings");
    } finally {
      setLoading(false);
    }
  };

  /**
   * Runs a sync. `overrideMassDeletion` is only ever true on the explicit,
   * confirmed Sync Anyway path -- a hold is never retried automatically, and
   * the plain Sync Now button always sends a normal request.
   */
  const handleSync = async (overrideMassDeletion = false) => {
    // Guard as well as disable: a double-submit must not reach the backend,
    // which would answer the second call with a 409.
    if (syncing) return;

    // Persisted before the request starts so a reload/navigation immediately
    // after the click still finds an in-progress record to restore.
    writeSyncState("in_progress");
    setSyncing(true);
    setError(null);
    setSyncResult(null);
    setHold(null);
    setConfirmingOverride(false);

    const promise = syncOrganizationProvider({ overrideMassDeletion });
    setActiveSyncPromise(promise);
    try {
      const result = await promise;
      settleSync({ ok: true, result });
    } catch (err: any) {
      settleSync({ ok: false, error: err });
    }
  };

  if (loading) {
    return (
      <div className="text-center py-8">
        <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-indigo-600 mx-auto mb-4"></div>
        <p className="text-gray-500">Loading provider settings...</p>
      </div>
    );
  }

  const readyToSync = settings?.readyToSync ?? false;
  // Only render the "not configured" guidance once settings actually loaded
  // and said so -- a failed fetch (settingsLoadError) means readiness is
  // simply unknown, not that the provider is unconfigured.
  const showNotConfigured = !!settings && !readyToSync;

  return (
    <div>
      <div className="flex justify-between items-center mb-4">
        <h3 className="text-lg font-medium text-gray-900">Organization Data Provider</h3>
        <button
          data-testid="sync-now-btn"
          onClick={() => handleSync()}
          disabled={syncing || !readyToSync || !!settingsLoadError}
          aria-label="Sync organization data from provider"
          aria-busy={syncing}
          className="flex items-center gap-2 px-4 py-2 bg-indigo-600 text-white rounded-lg hover:bg-indigo-700 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
        >
          {syncing ? (
            <Loader2 className="w-4 h-4 animate-spin" />
          ) : (
            <RefreshCw className="w-4 h-4" />
          )}
          {syncing ? "Syncing..." : "Sync Now"}
        </button>
      </div>

      <p className="text-sm text-gray-500 mb-4">
        Pulls people, teams and team membership from Data Provider and applies them to Team360. The Data Provider is
        authoritative: a user or team no longer reported by Data Provider is removed from Team360.
      </p>

      {settingsLoadError && (
        <div
          data-testid="provider-settings-load-error"
          className="mb-4 flex items-start gap-3 p-4 bg-red-50 border border-red-200 rounded-lg"
        >
          <AlertCircle className="w-5 h-5 text-red-600 mt-0.5 flex-shrink-0" />
          <div>
            <p className="text-sm font-medium text-red-900">Couldn't load provider settings</p>
            <p className="text-sm text-red-700 mt-1">{settingsLoadError}</p>
            <button
              onClick={loadSettings}
              className="text-sm text-red-800 underline mt-2"
            >
              Retry
            </button>
          </div>
        </div>
      )}

      {showNotConfigured && (
        <div
          data-testid="provider-not-configured"
          className="mb-4 flex items-start gap-3 p-4 bg-amber-50 border border-amber-200 rounded-lg"
        >
          <AlertCircle className="w-5 h-5 text-amber-600 mt-0.5 flex-shrink-0" />
          <div>
            <p className="text-sm font-medium text-amber-800">Provider Not Ready</p>
            <ul className="text-sm text-amber-700 mt-1 list-disc list-inside space-y-0.5">
              {!settings?.baseUrlConfigured && (
                <li>
                  Set <code className="bg-amber-100 px-1 rounded">DATA_PROVIDER_BASE_URL</code> on
                  the API service.
                </li>
              )}
              {!settings?.tokenConfigured && (
                <li>
                  Set <code className="bg-amber-100 px-1 rounded">DATA_PROVIDER_API_TOKEN</code> on
                  the API service.
                </li>
              )}
            </ul>
          </div>
        </div>
      )}

      {hold && (
        <div
          data-testid="sync-mass-deletion-hold"
          role="alert"
          className="mb-4 p-4 bg-red-50 border-2 border-red-400 rounded-lg flex items-start gap-3"
        >
          <ShieldAlert className="w-5 h-5 text-red-600 mt-0.5 flex-shrink-0" />
          <div className="w-full">
            <p className="font-medium text-red-900">Sync held for review</p>
            <ul className="text-sm text-red-800 mt-2 space-y-1">
              <li data-testid="sync-hold-users">
                <strong>Users:</strong> {metricSentence(hold.users)}{" "}
                <span className="text-red-600">({incomingSentence(hold.users)})</span>
              </li>
              <li data-testid="sync-hold-teams">
                <strong>Teams:</strong> {metricSentence(hold.teams)}{" "}
                <span className="text-red-600">({incomingSentence(hold.teams)})</span>
              </li>
            </ul>
            <p className="text-xs text-red-700 mt-2" data-testid="sync-hold-threshold">
              "Existing" counts only the users/teams the provider is allowed to manage &mdash; it
              excludes the permanent admin and the fixed demo/E2E accounts and teams, which can
              never be deleted by a sync. Configured threshold: {formatPercent(hold.threshold)}.
            </p>

            {!confirmingOverride ? (
              <button
                data-testid="sync-anyway-btn"
                onClick={() => setConfirmingOverride(true)}
                disabled={syncing}
                className="mt-3 px-3 py-1.5 text-sm bg-red-600 text-white rounded-lg hover:bg-red-700 disabled:opacity-50 disabled:cursor-not-allowed"
              >
                Sync Anyway
              </button>
            ) : (
              <div data-testid="sync-anyway-confirm" className="mt-3">
                <p className="text-sm font-medium text-red-900">
                  Apply this sync and permanently delete {hold.users.deleting} user(s) and{" "}
                  {hold.teams.deleting} team(s)? This cannot be undone.
                </p>
                <div className="flex gap-2 mt-2">
                  <button
                    data-testid="sync-anyway-confirm-btn"
                    onClick={() => handleSync(true)}
                    disabled={syncing}
                    className="px-3 py-1.5 text-sm bg-red-600 text-white rounded-lg hover:bg-red-700 disabled:opacity-50 disabled:cursor-not-allowed"
                  >
                    Yes, sync anyway
                  </button>
                  <button
                    data-testid="sync-anyway-cancel-btn"
                    onClick={() => setConfirmingOverride(false)}
                    className="px-3 py-1.5 text-sm text-red-800 underline"
                  >
                    Cancel
                  </button>
                </div>
              </div>
            )}
          </div>
        </div>
      )}

      {error && !hold && (
        <div
          data-testid="provider-error"
          className="mb-4 p-4 bg-red-50 border border-red-200 rounded-lg flex items-start gap-3"
        >
          <AlertCircle className="w-5 h-5 text-red-600 mt-0.5 flex-shrink-0" />
          <div>
            <p className="font-medium text-red-900">Error</p>
            <p className="text-sm text-red-700">{error}</p>
          </div>
        </div>
      )}

      {syncResult && (
        <div
          data-testid="sync-result"
          className="mb-4 p-4 bg-green-50 border border-green-200 rounded-lg flex items-start gap-3"
        >
          <CheckCircle2 className="w-5 h-5 text-green-600 mt-0.5 flex-shrink-0" />
          <div>
            <p className="font-medium text-green-900">Sync complete</p>
            {syncResult.massDeletionOverridden && (
              <p className="text-sm text-amber-700" data-testid="sync-override-applied">
                Applied with an administrator override of the mass-deletion hold.
              </p>
            )}
            <p className="text-sm text-green-700">
              {syncResult.usersSynced} users, {syncResult.teamsSynced} teams and{" "}
              {syncResult.membershipsSynced} team memberships synchronized.
            </p>
            {(syncResult.usersDeleted > 0 || syncResult.teamsDeleted > 0) && (
              <p className="text-sm text-green-700 mt-1" data-testid="sync-deleted">
                {syncResult.usersDeleted} user(s) and {syncResult.teamsDeleted} team(s) removed
                (no longer reported by the provider).
              </p>
            )}
            {syncResult.membershipsRemoved > 0 && (
              <p className="text-sm text-green-700 mt-1">
                {syncResult.membershipsRemoved} stale team membership(s) removed.
              </p>
            )}
            {syncResult.actionItemsDeleted > 0 && (
              <p className="text-sm text-amber-700 mt-1" data-testid="sync-action-items-deleted">
                {syncResult.actionItemsDeleted} action item(s) removed along with their deleted
                user or team.
              </p>
            )}
          </div>
        </div>
      )}

      {syncResult && syncResult.healthChecksDisabled > 0 && (
        <div
          data-testid="sync-health-check-warning"
          className="mb-4 flex items-start gap-3 p-4 bg-amber-50 border border-amber-200 rounded-lg"
        >
          <AlertCircle className="w-5 h-5 text-amber-600 mt-0.5 flex-shrink-0" />
          <div>
            <p className="text-sm font-medium text-amber-800">
              Health checks switched off for {syncResult.healthChecksDisabled} team(s)
            </p>
            <p className="text-sm text-amber-700 mt-1">
              The provider reported these teams as not participating in health checks.
            </p>
          </div>
        </div>
      )}
    </div>
  );
}
