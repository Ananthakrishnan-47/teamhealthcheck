-- Durable record of organization-sync activity: the most recent attempt that
-- ran and reached a conclusion (org_sync_runs), and the liveness claim of a
-- run currently in flight (org_sync_active_runs).
--
-- These are event tables written by the sync itself, not admin settings, so
-- they deliberately do NOT live on app_settings: a completed sync would
-- otherwise bump app_settings.updated_at, the column the settings endpoints
-- use to reason about admin edits.

-- HISTORY: the most recent attempt that actually ran and reached a
-- conclusion. There is deliberately no "running" status here -- an in-flight
-- run lives in org_sync_active_runs instead, so starting a run can never
-- overwrite the previous attempt's result. Singleton, seeded below, so reads
-- never have to special-case "no row yet".
CREATE TABLE IF NOT EXISTS org_sync_runs (
    id INTEGER PRIMARY KEY DEFAULT 1 CONSTRAINT org_sync_runs_singleton CHECK (id = 1),

    -- last_* is NULL in every column below when no attempt has ever run.
    last_trigger TEXT DEFAULT NULL
        CONSTRAINT org_sync_runs_last_trigger_allowed
            CHECK (last_trigger IS NULL OR last_trigger IN ('manual', 'scheduled')),
    last_status TEXT DEFAULT NULL
        CONSTRAINT org_sync_runs_last_status_allowed
            CHECK (last_status IS NULL OR last_status IN ('success', 'blocked', 'failed')),
    last_started_at  TIMESTAMP WITH TIME ZONE DEFAULT NULL,
    last_finished_at TIMESTAMP WITH TIME ZONE DEFAULT NULL,
    last_instance_id TEXT DEFAULT NULL,

    -- The threshold THAT attempt was judged against, stored rather than
    -- re-resolved, so changing the setting cannot retroactively re-label a
    -- blocked run. NULL when the attempt failed before a threshold was ever
    -- evaluated.
    last_threshold_percent DOUBLE PRECISION DEFAULT NULL
        CONSTRAINT org_sync_runs_last_threshold_range
            CHECK (last_threshold_percent IS NULL
                   OR (last_threshold_percent >= 1 AND last_threshold_percent <= 100)),

    -- Nullable, never 0: a failed attempt never computed these, and 0 would
    -- read as a safe sync -- a lie. Set only for blocked/success outcomes.
    last_users_existing INTEGER DEFAULT NULL,
    last_users_deleting INTEGER DEFAULT NULL,
    last_users_percent  DOUBLE PRECISION DEFAULT NULL,
    last_teams_existing INTEGER DEFAULT NULL,
    last_teams_deleting INTEGER DEFAULT NULL,
    last_teams_percent  DOUBLE PRECISION DEFAULT NULL,

    -- Tri-state, not boolean: 'none' is a confirmed rollback or a blocked
    -- attempt (no write was ever attempted); 'applied' means Commit()
    -- returned successfully (independent of whether any row actually
    -- changed); 'unknown' means Commit() itself errored or the connection was
    -- lost at that exact moment, so the outcome cannot be proven either way.
    last_writes_status TEXT NOT NULL DEFAULT 'none'
        CONSTRAINT org_sync_runs_last_writes_status_allowed
            CHECK (last_writes_status IN ('none', 'applied', 'unknown')),
    last_override_used BOOLEAN NOT NULL DEFAULT false,

    last_users_synced INTEGER DEFAULT NULL,
    last_teams_synced INTEGER DEFAULT NULL,
    last_message TEXT DEFAULT NULL,

    -- Durable mass-deletion hold: freezes threshold editing (via LockState's
    -- fail-closed OR with the in-memory hold) on every replica, closing the
    -- bypass where an admin resolves a hold on one replica by editing the
    -- threshold on another. Set only on a blocked attempt; cleared only by a
    -- successful sync, an explicit admin dismissal, or TTL expiry -- never by
    -- an unrelated later failure, which would silently reopen the bypass.
    hold_active      BOOLEAN NOT NULL DEFAULT false,
    hold_recorded_at TIMESTAMP WITH TIME ZONE DEFAULT NULL,

    -- Skips live in their own columns, written only by RecordOrgSyncSkip,
    -- which never touches any last_* attempt column above -- a skip cannot
    -- physically displace a result.
    last_skip_reason TEXT DEFAULT NULL
        CONSTRAINT org_sync_runs_last_skip_reason_allowed
            CHECK (last_skip_reason IS NULL OR last_skip_reason IN
                   ('manual_sync_running', 'scheduled_sync_running', 'hold_unresolved', 'overdue_catch_up_skipped')),
    last_skip_at TIMESTAMP WITH TIME ZONE DEFAULT NULL,

    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

INSERT INTO org_sync_runs (id) VALUES (1) ON CONFLICT DO NOTHING;

-- LIVENESS: attribution and UI accuracy only. Nothing here is ever consulted
-- to decide whether a sync may start -- that is the Postgres advisory lock,
-- entirely independent of this table -- which is exactly what makes a stale
-- row from a killed pod harmless. Deliberately NOT seeded: row absence IS the
-- meaningful "nothing is running" state, so id=1 bounds this to at most one
-- row, not exactly one.
CREATE TABLE IF NOT EXISTS org_sync_active_runs (
    id INTEGER PRIMARY KEY DEFAULT 1 CONSTRAINT org_sync_active_runs_singleton CHECK (id = 1),
    instance_id TEXT NOT NULL,
    trigger TEXT NOT NULL
        CONSTRAINT org_sync_active_runs_trigger_allowed
            CHECK (trigger IN ('manual', 'scheduled')),
    started_at   TIMESTAMP WITH TIME ZONE NOT NULL,
    heartbeat_at TIMESTAMP WITH TIME ZONE NOT NULL
);
