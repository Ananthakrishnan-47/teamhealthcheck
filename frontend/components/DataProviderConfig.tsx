"use client";

import { useState, useEffect } from "react";
import { AlertCircle, CheckCircle2, Loader2, RefreshCw, Save } from "lucide-react";
import {
  getOrganizationProviderSettings,
  updateOrganizationProviderToken,
  syncOrganizationProvider,
  clearAdminCache,
  OrganizationProviderSettings,
  OrganizationSyncResult,
} from "@/lib/api/admin";

/**
 * Configuration and manual trigger for the external organization-data provider.
 *
 * Syncing rewrites users, teams and memberships, so it is deliberately manual:
 * an admin decides when the organization changes shape.
 */
export default function DataProviderConfig() {
  const [settings, setSettings] = useState<OrganizationProviderSettings | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // Token form
  const [token, setToken] = useState("");
  const [savingToken, setSavingToken] = useState(false);
  const [tokenSaved, setTokenSaved] = useState(false);

  // Sync
  const [syncing, setSyncing] = useState(false);
  const [syncResult, setSyncResult] = useState<OrganizationSyncResult | null>(null);

  useEffect(() => {
    loadSettings();
  }, []);

  const loadSettings = async () => {
    setLoading(true);
    setError(null);
    try {
      setSettings(await getOrganizationProviderSettings());
    } catch (err: any) {
      setError(err.message || "Failed to load provider settings");
    } finally {
      setLoading(false);
    }
  };

  const handleSaveToken = async () => {
    if (!token.trim()) return;

    setSavingToken(true);
    setError(null);
    setTokenSaved(false);
    try {
      await updateOrganizationProviderToken(token.trim());
      // Drop the plaintext as soon as it has been handed over; it is
      // write-only and can never be read back.
      setToken("");
      setTokenSaved(true);
      setTimeout(() => setTokenSaved(false), 3000);
      await loadSettings();
    } catch (err: any) {
      setError(err.message || "Failed to save provider token");
    } finally {
      setSavingToken(false);
    }
  };

  const handleSync = async () => {
    // Guard as well as disable: a double-submit must not reach the backend,
    // which would answer the second call with a 409.
    if (syncing) return;

    setSyncing(true);
    setError(null);
    setSyncResult(null);
    try {
      const result = await syncOrganizationProvider();
      setSyncResult(result);
      // The admin client caches users and teams for two minutes. Without this
      // the screen would keep showing pre-sync counts.
      clearAdminCache();
    } catch (err: any) {
      setError(err.message || "Synchronization failed");
    } finally {
      setSyncing(false);
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

  return (
    <div>
      <div className="flex justify-between items-center mb-4">
        <h3 className="text-lg font-medium text-gray-900">Organization Data Provider</h3>
        <button
          data-testid="sync-now-btn"
          onClick={handleSync}
          disabled={syncing || !readyToSync}
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
        Pulls people, teams and team membership from your provider and applies them to Team360.
        Existing users and teams are never deleted.
      </p>

      {!readyToSync && (
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
                  Set <code className="bg-amber-100 px-1 rounded">PODIQ_BASE_URL</code> on the API
                  service.
                </li>
              )}
              {!settings?.encryptionConfigured && (
                <li>
                  Set <code className="bg-amber-100 px-1 rounded">TOKEN_ENCRYPTION_KEY</code> to a
                  base64-encoded 32-byte key.
                </li>
              )}
              {!settings?.configured && <li>Save a provider API token below.</li>}
            </ul>
          </div>
        </div>
      )}

      {error && (
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
            <p className="text-sm text-green-700">
              {syncResult.usersSynced} users, {syncResult.teamsSynced} teams and{" "}
              {syncResult.membershipsSynced} team memberships synchronized.
            </p>
            {syncResult.usersSkipped > 0 && (
              <p className="text-sm text-green-700 mt-1" data-testid="sync-skipped">
                {syncResult.usersSkipped} user(s) skipped because their hierarchy level is not
                configured in Team360.
              </p>
            )}
            {syncResult.membershipsRemoved > 0 && (
              <p className="text-sm text-green-700 mt-1">
                {syncResult.membershipsRemoved} stale team membership(s) removed.
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

      <div className="p-4 border border-gray-200 rounded-lg">
        <label htmlFor="provider-api-token" className="block text-sm font-medium text-gray-700 mb-1">
          Provider API Token
        </label>
        <p className="text-xs text-gray-500 mb-2">
          {settings?.configured
            ? "A token is stored. Entering a new one replaces it. Tokens can never be read back."
            : "No token stored yet."}
        </p>
        <div className="flex items-center gap-2">
          <input
            id="provider-api-token"
            data-testid="provider-token-input"
            type="password"
            autoComplete="off"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            placeholder={settings?.configured ? "Enter a new token to replace" : "Paste the API token"}
            className="flex-1 px-3 py-2 border border-gray-300 rounded-lg focus:outline-none focus:ring-2 focus:ring-indigo-500"
          />
          <button
            data-testid="save-provider-token-btn"
            onClick={handleSaveToken}
            disabled={savingToken || !token.trim()}
            aria-label="Save provider API token"
            className="flex items-center gap-2 px-4 py-2 bg-indigo-600 text-white rounded-lg hover:bg-indigo-700 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
          >
            {savingToken ? <Loader2 className="w-4 h-4 animate-spin" /> : <Save className="w-4 h-4" />}
            {savingToken ? "Saving..." : "Save Token"}
          </button>
        </div>
        {tokenSaved && (
          <p className="text-green-600 text-sm mt-2" data-testid="provider-token-saved">
            Token saved
          </p>
        )}
      </div>
    </div>
  );
}
