// Source: umir-factory reference/durable@26166c8ba197

// Package postgres is the reference PostgreSQL adapter of the durable stack: the
// pool, transactions that retry and carry the row-level security context,
// migrations, the storage generation, worker leases, the transactional outbox
// and the inbox. Services copy it to internal/adapters/postgres.
//
// Roles. Each service has an owner role (umir_<svc>_owner, owns the tables, used
// only by the migrate job) and a runtime role (umir_<svc>, not the owner,
// NOBYPASSRLS). The service calls AssertRuntimeRole at start. Runtime
// transactions that touch tenant data use InScopedTx or InSystemTx, which set
// the umir.* settings with set_config(..., true), so nothing outlives the
// transaction on a pooled connection.
//
// Time. Leases, outbox claims and retention compare timestamps. When
// Config.Clock is nil they use the database clock (clock_timestamp()), the one
// time source shared by all replicas; tests set Config.Clock to move time
// without sleeping.
//
// Privileges of the runtime role. The migration job (owner role) grants them;
// the package does not ship a GRANT helper. The runtime role gets:
//
//   - SELECT only on storage_meta and schema_migrations: the service reads the
//     storage generation and the schema version, and never writes them; INSERT,
//     UPDATE, DELETE and TRUNCATE on them belong to the owner;
//   - SELECT, INSERT, UPDATE and DELETE on outbox, inbox and worker_leases, and
//     on the service's own tables;
//   - USAGE on outbox_claim_fence_seq (Claim calls nextval) and on the sequence
//     behind outbox.position (a bigserial; inserting rows needs USAGE on it), plus
//     USAGE on the sequences of the service's serial columns;
//   - USAGE on the schema, and neither CREATE on it nor membership in the owner
//     role; AssertRuntimeRole checks the last two.
//
// Residual risk of the system actor. The reference policies let a transaction
// through when umir.actor is 'system', and the runtime role may call
// set_config('umir.actor', 'system', true) like any role may set a custom
// setting. Row level security therefore protects against mistakes of the
// application, such as a query without a scope or with another tenant's
// identifier; it does not protect against SQL injection that runs under the
// runtime role, because injected SQL can claim the system actor. Keep every
// statement parameterized, and treat the runtime role as able to read the data of
// every tenant of its service.
//
// Policies. RLSPolicySQL compares the column with a text setting, so it fits text
// columns; for a uuid column pass RLSUUIDColumn() so that the setting is cast.
//
// Migrations. schema_migrations keeps the version and the name of each applied
// migration, with no checksum: an edited applied file is not detected. Do not
// change an applied migration; add a new one.
//
// SQL fragments. StorageMetaSQL, WorkerLeasesSQL, OutboxSQL and InboxSQL return
// the reference DDL. A service migration includes one with the line
//
//	-- umir:include <storage_meta|worker_leases|outbox|inbox>
//
// which Migrate and VerifySchema replace by the fragment text.
package postgres
