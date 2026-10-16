-- Feedback storage, generation 1 (clean slate, nothing is imported: R1, U4).
-- Applied by `feedback migrate` under the owner role umir_feedback_owner only.

-- umir:include storage_meta
-- umir:include worker_leases
-- umir:include outbox
-- umir:include inbox

-- Restaurants that sent submissions; fixes the restaurant -> organization link
-- for the composite keys below. No personal data.
CREATE TABLE feedback_scopes (
    tenant_id uuid PRIMARY KEY,
    org_id    uuid NOT NULL,
    UNIQUE (org_id, tenant_id)
);

-- Deletion fences: a restaurant or an organization deleted in Auth. Kept
-- without a time limit; no personal data.
CREATE TABLE feedback_scope_fences (
    kind       text        NOT NULL CHECK (kind IN ('organization', 'restaurant')),
    scope_id   uuid        NOT NULL,
    deleted_at timestamptz NOT NULL,
    event_id   text        NOT NULL,
    revision   bigint      NOT NULL,
    PRIMARY KEY (kind, scope_id)
);

-- Live (not erased) rows per namespace: 'anonymous' or 'restaurant:<uuid>'.
CREATE TABLE feedback_quota (
    scope_key text   PRIMARY KEY,
    live_rows integer NOT NULL CHECK (live_rows >= 0)
);

-- The last Auth registry revision this service has reconciled.
CREATE TABLE feedback_registry_state (
    singleton  boolean     PRIMARY KEY DEFAULT true CHECK (singleton),
    revision   bigint      NOT NULL DEFAULT 0 CHECK (revision >= 0),
    checked_at timestamptz
);
INSERT INTO feedback_registry_state (singleton) VALUES (true);

CREATE SEQUENCE feedback_job_fence_seq AS bigint;

CREATE TABLE feedback_submissions (
    pk              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    id              uuid        NOT NULL,
    namespace_kind  text        NOT NULL CHECK (namespace_kind IN ('anonymous', 'app')),
    user_id         uuid,
    org_id          uuid,
    tenant_id       uuid,
    name            text,
    email           text,
    phone           text,
    message         text,
    consent_version text        NOT NULL,
    consent_purpose text        NOT NULL,
    accepted_at     timestamptz NOT NULL,
    received_at     timestamptz NOT NULL,
    fingerprint     text        NOT NULL CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    payload_cleared_at timestamptz,
    erased_at       timestamptz,
    purge_at        timestamptz NOT NULL,
    tombstone_until timestamptz,
    CONSTRAINT feedback_namespace_shape CHECK (
        (namespace_kind = 'anonymous' AND user_id IS NULL AND org_id IS NULL AND tenant_id IS NULL)
        OR (namespace_kind = 'app' AND user_id IS NOT NULL AND org_id IS NOT NULL AND tenant_id IS NOT NULL)),
    CONSTRAINT feedback_erased_has_no_personal_data CHECK (
        erased_at IS NULL OR (name IS NULL AND email IS NULL AND phone IS NULL AND message IS NULL)),
    CONSTRAINT feedback_live_has_contact CHECK (erased_at IS NOT NULL OR (name IS NOT NULL AND email IS NOT NULL)),
    CONSTRAINT feedback_tombstone_matches_erasure CHECK ((erased_at IS NULL) = (tombstone_until IS NULL)),
    CONSTRAINT feedback_cleared_payload CHECK (payload_cleared_at IS NULL OR (message IS NULL AND phone IS NULL)),
    FOREIGN KEY (org_id, tenant_id) REFERENCES feedback_scopes (org_id, tenant_id),
    UNIQUE (pk, org_id, tenant_id)
);

CREATE UNIQUE INDEX feedback_submissions_anonymous_id ON feedback_submissions (id) WHERE namespace_kind = 'anonymous';
CREATE UNIQUE INDEX feedback_submissions_app_id ON feedback_submissions (user_id, org_id, tenant_id, id) WHERE namespace_kind = 'app';
CREATE INDEX feedback_submissions_list ON feedback_submissions (org_id, tenant_id, received_at, pk)
    WHERE namespace_kind = 'app' AND erased_at IS NULL;
CREATE INDEX feedback_submissions_purge ON feedback_submissions (purge_at) WHERE erased_at IS NULL;
CREATE INDEX feedback_submissions_tombstone ON feedback_submissions (tombstone_until) WHERE erased_at IS NOT NULL;
CREATE INDEX feedback_submissions_email ON feedback_submissions (email) WHERE erased_at IS NULL;
CREATE INDEX feedback_submissions_order ON feedback_submissions (received_at, pk);

CREATE TABLE feedback_jobs (
    id               uuid        PRIMARY KEY,
    submission_pk    bigint      NOT NULL REFERENCES feedback_submissions (pk) ON DELETE CASCADE,
    org_id           uuid,
    tenant_id        uuid,
    channel          text        NOT NULL CHECK (channel IN ('smtp', 'telegram')),
    state            text        NOT NULL CHECK (state IN ('pending', 'in-flight', 'sent', 'failed')),
    attempts         integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at  timestamptz,
    retry_started_at timestamptz,
    started_at       timestamptz,
    confirmed_at     timestamptz,
    error_code       text        NOT NULL DEFAULT '',
    provider_id      text        NOT NULL DEFAULT '',
    fence            bigint      NOT NULL DEFAULT 0,
    last_wake_at     timestamptz,
    UNIQUE (submission_pk, channel),
    -- The scope of a job is the scope of its submission.
    FOREIGN KEY (submission_pk, org_id, tenant_id) REFERENCES feedback_submissions (pk, org_id, tenant_id) ON DELETE CASCADE
);

CREATE INDEX feedback_jobs_due ON feedback_jobs (channel, next_attempt_at) WHERE state = 'pending';
CREATE INDEX feedback_jobs_running ON feedback_jobs (channel, started_at) WHERE state = 'in-flight';

-- Row level security (R8, C5). The context comes from the transaction wrapper:
-- umir.actor is tenant, anonymous or system; no context means no rows.
ALTER TABLE feedback_submissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE feedback_submissions FORCE ROW LEVEL SECURITY;
CREATE POLICY feedback_submissions_scope ON feedback_submissions
    USING (
        NULLIF(current_setting('umir.actor', true), '') = 'system'
        OR (namespace_kind = 'app'
            AND NULLIF(current_setting('umir.actor', true), '') = 'tenant'
            AND org_id::text = NULLIF(current_setting('umir.org_id', true), '')
            AND tenant_id::text = NULLIF(current_setting('umir.restaurant_id', true), ''))
        OR (namespace_kind = 'anonymous' AND NULLIF(current_setting('umir.actor', true), '') = 'anonymous'))
    WITH CHECK (
        NULLIF(current_setting('umir.actor', true), '') = 'system'
        OR (namespace_kind = 'app'
            AND NULLIF(current_setting('umir.actor', true), '') = 'tenant'
            AND org_id::text = NULLIF(current_setting('umir.org_id', true), '')
            AND tenant_id::text = NULLIF(current_setting('umir.restaurant_id', true), ''))
        OR (namespace_kind = 'anonymous' AND NULLIF(current_setting('umir.actor', true), '') = 'anonymous'));

ALTER TABLE feedback_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE feedback_jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY feedback_jobs_scope ON feedback_jobs
    USING (
        NULLIF(current_setting('umir.actor', true), '') = 'system'
        OR (tenant_id IS NOT NULL
            AND NULLIF(current_setting('umir.actor', true), '') = 'tenant'
            AND org_id::text = NULLIF(current_setting('umir.org_id', true), '')
            AND tenant_id::text = NULLIF(current_setting('umir.restaurant_id', true), ''))
        OR (tenant_id IS NULL AND NULLIF(current_setting('umir.actor', true), '') = 'anonymous'))
    WITH CHECK (
        NULLIF(current_setting('umir.actor', true), '') = 'system'
        OR (tenant_id IS NOT NULL
            AND NULLIF(current_setting('umir.actor', true), '') = 'tenant'
            AND org_id::text = NULLIF(current_setting('umir.org_id', true), '')
            AND tenant_id::text = NULLIF(current_setting('umir.restaurant_id', true), ''))
        OR (tenant_id IS NULL AND NULLIF(current_setting('umir.actor', true), '') = 'anonymous'));

-- Privileges of the runtime role umir_feedback. The grantee is fixed by the
-- deployment; the session setting umir.runtime_role exists for the integration
-- tests, which run with randomly named roles. If the role does not exist the
-- migration fails, which is what an operator needs to see.
DO $grants$
DECLARE
    runtime_role text := COALESCE(NULLIF(current_setting('umir.runtime_role', true), ''), 'umir_feedback');
BEGIN
    EXECUTE format('GRANT SELECT ON storage_meta, schema_migrations TO %I', runtime_role);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON outbox, inbox, worker_leases, feedback_scopes, feedback_scope_fences, feedback_quota, feedback_registry_state, feedback_submissions, feedback_jobs TO %I', runtime_role);
    EXECUTE format('GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO %I', runtime_role);
END
$grants$;
