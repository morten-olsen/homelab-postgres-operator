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

## Reclaim Policy

The `reclaimPolicy` field in the `PostgresDatabase` spec controls what happens when a `PostgresDatabase` resource is deleted.

-   **`Retain` (default):** When the `PostgresDatabase` resource is deleted, the underlying database and user in PostgreSQL are **not** removed. This is a safety measure to prevent accidental data loss. The connection secret is also retained.

-   **`Delete`:** When the `PostgresDatabase` resource is deleted, the underlying database and user in PostgreSQL are **deleted**. Use this with caution. The connection secret is also deleted.