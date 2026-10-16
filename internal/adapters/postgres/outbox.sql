-- Reference fragment: outbox (C4). One table shape for every service; services
-- take it without columns of their own. The row is written in the transaction of
-- the business change and published by a relay.
--
-- position orders the rows; claim_fence names one Claim call (outbox_claim_fence_seq
-- only grows), and claimed_by with claim_fence identify the lease holder. A row
-- is pending while published_at IS NULL.
CREATE SEQUENCE outbox_claim_fence_seq AS bigint;

CREATE TABLE outbox (
    id               uuid        PRIMARY KEY,
    position         bigserial   NOT NULL UNIQUE,
    exchange         text        NOT NULL,
    routing_key      text        NOT NULL,
    headers          jsonb       NOT NULL DEFAULT '{}'::jsonb
                     CONSTRAINT outbox_headers_object CHECK (jsonb_typeof(headers) = 'object'),
    body             jsonb       NOT NULL,
    org_id           text,
    restaurant_id    text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    not_before       timestamptz NOT NULL DEFAULT now(),
    published_at     timestamptz,
    routed           boolean,
    attempts         integer     NOT NULL DEFAULT 0,
    claimed_by       text,
    claim_fence      bigint,
    claim_expires_at timestamptz
);

CREATE INDEX outbox_pending_idx ON outbox (position) WHERE published_at IS NULL;
CREATE INDEX outbox_published_idx ON outbox (published_at) WHERE published_at IS NOT NULL;
