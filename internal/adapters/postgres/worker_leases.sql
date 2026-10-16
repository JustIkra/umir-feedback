-- Reference fragment: worker_leases (C4). Leases of background work for services
-- with a database. fence grows with every new term and never goes back; a
-- released lease keeps its row (holder '' and expires_at -infinity), so the
-- fence stays monotonic.
CREATE TABLE worker_leases (
    name       text        PRIMARY KEY,
    holder     text        NOT NULL,
    fence      bigint      NOT NULL,
    expires_at timestamptz NOT NULL
);
