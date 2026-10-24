package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	postgresv1 "github.com/morten-olsen/homelab-postgres-operator/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// Exercise the credential SQL failure boundary with errors that echo the SQL,
// as a database server or driver can do. Only synthetic credentials are used.
func TestPasswordStatementLogging(t *testing.T) {
	const password = "synthetic-password'with-quote"
	escapedPassword := strings.ReplaceAll(password, "'", "''")
	for _, operation := range []string{"CREATE", "ALTER"} {
		query := fmt.Sprintf(`%s USER "test_app" WITH PASSWORD '%s';`, operation, escapedPassword)
		for _, kind := range []string{"driver", "postgres"} {
			t.Run(operation+"/"+kind, func(t *testing.T) {
				db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				statementErr := errors.New("driver rejected " + query)
				if kind == "postgres" {
					statementErr = fmt.Errorf("wrapped: %w", &pq.Error{
						Code: "42601", Message: "syntax error near " + escapedPassword,
						Detail: password, Where: query, InternalQuery: query,
					})
				}
				createQuery := fmt.Sprintf(`CREATE USER "test_app" WITH PASSWORD '%s';`, escapedPassword)
				if operation == "CREATE" {
					mock.ExpectExec(createQuery).WillReturnError(statementErr)
				} else {
					mock.ExpectExec(createQuery).WillReturnError(&pq.Error{Code: "42710", Message: "role already exists"})
					mock.ExpectExec(query).WillReturnError(statementErr)
					// Preserve the existing non-critical ALTER failure behavior; stop
					// at the next operation without touching a real database.
					mock.ExpectExec(`CREATE DATABASE "test_app" OWNER "test_app";`).WillReturnError(errors.New("database unavailable"))
				}
				scheme := runtime.NewScheme()
				if err := postgresv1.AddToScheme(scheme); err != nil {
					t.Fatal(err)
				}
				resource := &postgresv1.PostgresDatabase{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "app"}}
				k8s := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(resource).WithObjects(resource).Build()
				r := &PostgresDatabaseReconciler{Client: k8s, Scheme: scheme}
				var output bytes.Buffer
				logger := zap.New(zap.WriteTo(&output))
				result, returnedErr := r.createDatabaseAndUser(context.Background(), resource, db, password, "localhost", "5432", "admin", "synthetic-admin", logger)
				if result == nil || returnedErr == nil {
					t.Fatal("expected failed reconcile result")
				}
				stored := &postgresv1.PostgresDatabase{}
				if err := k8s.Get(context.Background(), types.NamespacedName{Namespace: "test", Name: "app"}, stored); err != nil {
					t.Fatal(err)
				}
				for name, text := range map[string]string{
					"logs": output.String(), "returned error": returnedErr.Error(),
					"condition": fmt.Sprint(stored.Status),
				} {
					for _, forbidden := range []string{password, escapedPassword, "WITH PASSWORD", "CREATE USER", "ALTER USER"} {
						if strings.Contains(text, forbidden) {
							t.Errorf("%s contains credential SQL or password", name)
						}
					}
				}
				if operation == "CREATE" && errors.Unwrap(returnedErr) != nil {
					t.Error("returned error retains unsafe driver cause")
				}
				if !strings.Contains(output.String(), `"user":"test_app"`) {
					t.Error("missing safe user context")
				}
				if kind == "postgres" && !strings.Contains(output.String(), "SQLSTATE 42601") {
					t.Error("missing safe SQLSTATE diagnostic")
				}
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestPasswordStatementErrorRejectsInvalidSQLState(t *testing.T) {
	for _, code := range []pq.ErrorCode{"", "synthetic-password", "42\n01", "42'01"} {
		err := passwordStatementError("Failed to create user", &pq.Error{Code: code, Message: "synthetic-password"})
		if err.Error() != "Failed to create user" {
			t.Fatal("invalid SQLSTATE was retained")
		}
	}
}
