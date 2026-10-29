# Homelab Postgres Operator

This is a Kubernetes operator for managing PostgreSQL instances, designed for homelab and smaller-scale deployments. It provides a simple way to create and manage PostgreSQL clusters and databases using Custom Resource Definitions (CRDs).

## Introduction

The operator introduces two CRDs:

-   `PostgresCluster`: Represents a PostgreSQL instance.
-   `PostgresDatabase`: Represents a database and user within a `PostgresCluster`.

The main goal of this operator is to simplify the management of PostgreSQL in a Kubernetes environment, especially for applications that need their own dedicated databases and users.

## `PostgresCluster`

A `PostgresCluster` resource deploys a PostgreSQL instance using a `StatefulSet`. By default, it uses a `pgvector/pgvector:pg16` image, making it suitable for applications that require vector embeddings.

When a `PostgresCluster` is created, the operator automatically:

1.  Creates a `StatefulSet` to run the PostgreSQL pod.
2.  Creates a headless `Service` to expose the PostgreSQL instance within the cluster.
3.  Creates a `Secret` named `<cluster-name>-admin-secret` containing the administrative credentials (`host`, `port`, `user`, `password`, `url`).

### Example `PostgresCluster`

```yaml
apiVersion: postgres.homelab.mortenolsen.pro/v1
kind: PostgresCluster
metadata:
  name: my-postgres-cluster
spec:
  # Optional: specify a different image
  # image: postgres:16
```

## `PostgresDatabase`

A `PostgresDatabase` resource creates a new database and a corresponding user with a randomly generated password within a specified `PostgresCluster`.

When a `PostgresDatabase` is created, the operator:

1.  Connects to the referenced `PostgresCluster` using its admin credentials.
2.  Creates a new database.
3.  Creates a new user with a strong, randomly generated password.
4.  Grants all privileges on the new database to the new user.
5.  Creates a `Secret` named `<database-name>-connection` with the connection details (`host`, `port`, `database`, `user`, `password`, `url`) for the new database.

### Example `PostgresDatabase`

```yaml
apiVersion: postgres.homelab.mortenolsen.pro/v1
kind: PostgresDatabase
metadata:
  name: my-app-database
spec:
  clusterRef:
    name: my-postgres-cluster # Name of the PostgresCluster
  databaseName: my_app_db
  userName: my_app_user
  reclaimPolicy: Retain # Can be Retain or Delete
```

## Ownership repair during reconciliation

The operator repairs ownership of existing public tables, partitioned tables,
sequences, views, materialized views, functions, standalone types and domains
so the application role can run its own migrations. Correctly owned objects
are skipped using the PostgreSQL catalogs; a normal reconcile must not take
exclusive locks on application tables.

Each object that needs repair is changed in a separate transaction with a
two-second `lock_timeout`. The whole ownership repair has a 30-second context
deadline. Failure rolls back the current transaction and returns an error to
the controller for retry, rather than reporting the database as ready. Locks
from earlier successful repairs are released before the next object is touched.
The timeout is local to these transactions; application sessions are unchanged.

The contention regression test requires a fresh, empty, disposable PostgreSQL
database and a test superuser. It creates test objects and a test role; never
point it at an application database:

```sh
OWNERSHIP_TEST_DSN='host=127.0.0.1 port=55432 user=test dbname=ownership sslmode=disable' \
  go test ./internal/controller -run '^TestOwnershipRepair$' -count=1 -v
```

The test covers initial ownership repair, repeated reconciliation, an application
holding a read lock on an already owned table, and lock timeout followed by retry
for a table with the wrong owner. `make test` skips this test when the DSN is unset.

## Reclaim Policy

The `reclaimPolicy` field in the `PostgresDatabase` spec controls what happens when a `PostgresDatabase` resource is deleted.

-   **`Retain` (default):** When the `PostgresDatabase` resource is deleted, the underlying database and user in PostgreSQL are **not** removed. This is a safety measure to prevent accidental data loss. The connection secret is also retained.

-   **`Delete`:** When the `PostgresDatabase` resource is deleted, the underlying database and user in PostgreSQL are **deleted**. Use this with caution. The connection secret is also deleted.

## End-to-end PostgreSQL fixture

`make test-e2e` runs in an isolated Kind cluster. The non-root PostgreSQL fixture
uses a `pgdata` child directory inside its PVC: UID 999 can create and own that
directory without trying to chmod the root-owned mount point. A `pg_isready`
readiness probe gates the connection and database reconciliation checks.

## Release ownership

See [Operator releases](docs/releases.md) for approved Forgejo tags, publication
checks, version immutability and recovery. Main merges validate without publishing.
