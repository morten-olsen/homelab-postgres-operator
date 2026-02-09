# AGENTS.md - Postgres Operator

Guidance for AI coding agents working in this repository.

## Project Overview

Kubernetes operator for PostgreSQL management (Go 1.24 + Kubebuilder v4.10.1).

- **Module:** `github.com/morten-olsen/homelab-postgres-operator`
- **PostgresCluster**: PostgreSQL server connection
- **PostgresDatabase**: Database and user within a PostgresCluster

## Commands

| Task | Command |
|------|---------|
| Build | `make build` |
| Run locally | `make run` |
| Docker build | `make docker-build IMG=<image>` |
| Install CRDs | `make install` |
| Deploy | `make deploy IMG=<image>` |
| Lint | `make lint` / `make lint-fix` |
| Format | `make fmt` |
| All tests | `make test` |
| Single test | `go test -v ./internal/controller -run "TestName"` |
| Ginkgo test | `go test -v ./internal/controller -ginkgo.focus="description"` |
| E2E tests | `make test-e2e` |

## Code Generation

Run after modifying `api/v1/*_types.go`:

```bash
make generate && make manifests && make copy-crds
```

## Project Structure

```
api/v1/                    # CRD type definitions
cmd/main.go                # Operator entry point
internal/controller/       # Controller implementations
config/                    # Kubernetes manifests (Kustomize)
charts/postgres-operator/  # Helm chart
```

## Code Style Guidelines

### Import Ordering

Organize imports in groups separated by blank lines:

1. Standard library
2. Third-party packages (side-effect imports with `_` at end)
3. Kubernetes packages
4. Controller-runtime packages
5. Internal packages

```go
import (
    "context"
    "fmt"

    "github.com/go-logr/logr"
    _ "github.com/lib/pq"
    postgresv1 "github.com/morten-olsen/homelab-postgres-operator/api/v1"
    corev1 "k8s.io/api/core/v1"
    "k8s.io/apimachinery/pkg/api/errors"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"
)
```

### Formatting

- **Indentation:** Tabs (Go standard)
- **Braces:** Opening brace on same line (K&R style)
- **Line length:** No hard limit, but keep reasonable
- Run `make fmt` before committing

### Naming Conventions

- **Variables/Functions:** camelCase
- **Exported identifiers:** PascalCase
- **Types:** PascalCase with descriptive suffixes (`PostgresClusterReconciler`)
- **Test files:** `*_test.go`
- **Package names:** lowercase, single word

### Error Handling

1. Check and return early:
```go
if err != nil {
    log.Error(err, "Failed to get resource")
    return ctrl.Result{}, err
}
```

2. Wrap errors with context:
```go
return fmt.Errorf("failed to get secret %s/%s: %w", namespace, name, err)
```

3. Update status conditions on errors:
```go
if err != nil {
    r.setCondition(resource, "Ready", false, err.Error())
    r.Status().Update(ctx, resource)
    return ctrl.Result{}, err
}
```

4. Defer cleanup with error handling:
```go
defer func() {
    if closeErr := db.Close(); closeErr != nil {
        log.Error(closeErr, "Failed to close connection")
    }
}()
```

### Kubebuilder Markers

Use markers for CRD validation:
```go
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:default:=5432
// +optional
```

RBAC markers for permissions:
```go
// +kubebuilder:rbac:groups=postgres.homelab.mortenolsen.pro,resources=postgresclusters,verbs=get;list;watch
```

### Documentation

- Function docs start with function name
- Field docs explain purpose and constraints
- Use `// +optional` marker for optional API fields

## Testing Patterns

### Ginkgo/Gomega (BDD Style)

```go
var _ = Describe("Controller", func() {
    Context("When reconciling", func() {
        BeforeEach(func() { /* setup */ })
        AfterEach(func() { /* cleanup */ })
        
        It("should reconcile successfully", func() {
            Expect(result.Requeue).To(BeFalse())
        })
    })
})
```

### Assertions

- Use `Expect()` for immediate checks
- Use `Eventually()` for async operations
- Use `Consistently()` to verify state doesn't change

### SQL Mocking

Use `go-sqlmock` for database tests:
```go
db, mock, err := sqlmock.New()
mock.ExpectQuery("SELECT").WillReturnRows(rows)
```

## Key Dependencies

- **controller-runtime:** Kubernetes controller framework
- **client-go:** Kubernetes client
- **ginkgo/v2:** Testing framework
- **gomega:** Matcher library
- **lib/pq:** PostgreSQL driver
- **go-sqlmock:** SQL mocking for tests

## Security Notes

- Passwords are never stored in status fields
- Use URL-safe password generation (no special chars)
- Connection secrets are managed by the operator

## Database Naming Convention

- Format: `{namespace}_{name}`
- Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
