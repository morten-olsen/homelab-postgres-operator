/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUTHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	_ "github.com/lib/pq" // PostgreSQL driver
	postgresv1 "github.com/morten-olsen/homelab-postgres-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	postgresDatabaseFinalizer = "postgresdatabase.finalizers.homelab.mortenolsen.pro"
	conditionReasonReady      = "Ready"
)

// getDatabaseName returns the database name, using the format {namespace}_{name}.
// If DatabaseName is specified in the spec, it's used for backward compatibility.
func getDatabaseName(postgresDatabase *postgresv1.PostgresDatabase) string {
	if postgresDatabase.Spec.DatabaseName != "" {
		return postgresDatabase.Spec.DatabaseName
	}
	return fmt.Sprintf("%s_%s", postgresDatabase.Namespace, postgresDatabase.Name)
}

// getUserName returns the username, using the format {namespace}_{name}.
// If UserName is specified in the spec, it's used for backward compatibility.
func getUserName(postgresDatabase *postgresv1.PostgresDatabase) string {
	if postgresDatabase.Spec.UserName != "" {
		return postgresDatabase.Spec.UserName
	}
	return fmt.Sprintf("%s_%s", postgresDatabase.Namespace, postgresDatabase.Name)
}

// quotePostgreSQLIdentifier quotes a PostgreSQL identifier to handle special characters.
// It escapes any double quotes in the identifier and wraps it in double quotes.
func quotePostgreSQLIdentifier(identifier string) string {
	// Escape double quotes by doubling them
	escaped := strings.ReplaceAll(identifier, `"`, `""`)
	// Wrap in double quotes
	return `"` + escaped + `"`
}

// dropDatabaseAndUser drops the database and user from the PostgreSQL instance.
func dropDatabaseAndUser(ctx context.Context, postgresDatabase *postgresv1.PostgresDatabase, db *sql.DB, log logr.Logger) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck

	databaseName := getDatabaseName(postgresDatabase)
	userName := getUserName(postgresDatabase)

	// Drop database
	dropDBQuery := fmt.Sprintf("DROP DATABASE IF EXISTS %s;", quotePostgreSQLIdentifier(databaseName))
	log.Info("Dropping database", "query", dropDBQuery)
	if _, err := tx.ExecContext(ctx, dropDBQuery); err != nil {
		return fmt.Errorf("failed to drop database: %w", err)
	}

	// Drop user
	dropUserQuery := fmt.Sprintf("DROP USER IF EXISTS %s;", quotePostgreSQLIdentifier(userName))
	log.Info("Dropping user", "query", dropUserQuery)
	if _, err := tx.ExecContext(ctx, dropUserQuery); err != nil {
		return fmt.Errorf("failed to drop user: %w", err)
	}

	return tx.Commit()
}

// generateRandomPassword generates a random password of the given length.
// Uses only URL-safe characters to ensure the password can be used directly in PostgreSQL URLs.
func generateRandomPassword(length int) (string, error) {
	// URL-safe charset: letters, digits, and safe special characters (-, _, ., ~)
	// These characters don't require URL encoding when used in connection strings
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_~"
	b := make([]byte, length)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	for i := 0; i < length; i++ {
		b[i] = charset[b[i]%byte(len(charset))]
	}
	return string(b), nil
}

// PostgresDatabaseReconciler reconciles a PostgresDatabase object
type PostgresDatabaseReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	DBConnectionFunc func(connStr string) (*sql.DB, error)
}

// +kubebuilder:rbac:groups=postgres.homelab.mortenolsen.pro,resources=postgresdatabases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgres.homelab.mortenolsen.pro,resources=postgresdatabases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=postgres.homelab.mortenolsen.pro,resources=postgresdatabases/finalizers,verbs=update
// +kubebuilder:rbac:groups=postgres.homelab.mortenolsen.pro,resources=postgresclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *PostgresDatabaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the PostgresDatabase instance
	postgresDatabase := &postgresv1.PostgresDatabase{}
	err := r.Get(ctx, req.NamespacedName, postgresDatabase)
	if err != nil {
		if errors.IsNotFound(err) {
			// PostgresDatabase not found, could be deleted
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get PostgresDatabase")
		return ctrl.Result{}, err
	}

	// Handle deletion FIRST, before fetching dependencies
	// This ensures deletion can proceed even if cluster/secret are being deleted during undeploy
	if !postgresDatabase.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, postgresDatabase, log)
	}

	// Fetch and validate PostgresCluster
	postgresCluster, clusterNamespace, result, err := r.fetchAndValidateCluster(ctx, postgresDatabase, log)
	if result != nil {
		return *result, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Resolve connection details from PostgresCluster spec
	pgHost, pgPort, adminUser, adminPassword, result, err := r.resolveClusterConnection(ctx, postgresDatabase, postgresCluster, clusterNamespace, log)
	if result != nil {
		return *result, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Ensure finalizer
	if err := r.ensureFinalizer(ctx, postgresDatabase); err != nil {
		return ctrl.Result{}, err
	}

	// Connect to database
	db, result, err := r.connectToDatabase(ctx, postgresDatabase, pgHost, fmt.Sprintf("%d", pgPort), adminUser, adminPassword, log)
	if result != nil {
		return *result, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			log.Error(closeErr, "Failed to close database connection")
		}
	}()

	// Create/update connection secret
	dbUserPassword, result, err := r.ensureConnectionSecret(ctx, postgresDatabase, pgHost, fmt.Sprintf("%d", pgPort), log)
	if result != nil {
		return *result, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Create database and user
	result, err = r.createDatabaseAndUser(ctx, postgresDatabase, db, dbUserPassword, pgHost, fmt.Sprintf("%d", pgPort), adminUser, adminPassword, log)
	if result != nil {
		return *result, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Update PostgresDatabase status
	if err := r.updateStatus(ctx, postgresDatabase, pgHost, fmt.Sprintf("%d", pgPort)); err != nil {
		log.Error(err, "Failed to update PostgresDatabase status")
		return ctrl.Result{}, err
	}

	log.Info("PostgresDatabase status updated successfully", "PostgresDatabase.Name", postgresDatabase.Name)

	return ctrl.Result{}, nil
}

// fetchAndValidateCluster fetches and validates the PostgresCluster.
func (r *PostgresDatabaseReconciler) fetchAndValidateCluster(ctx context.Context, postgresDatabase *postgresv1.PostgresDatabase, log logr.Logger) (*postgresv1.PostgresCluster, string, *ctrl.Result, error) {
	clusterNamespace := postgresDatabase.Namespace
	if postgresDatabase.Spec.ClusterRef.Namespace != "" {
		clusterNamespace = postgresDatabase.Spec.ClusterRef.Namespace
	}
	postgresCluster := &postgresv1.PostgresCluster{}
	err := r.Get(ctx, types.NamespacedName{Name: postgresDatabase.Spec.ClusterRef.Name, Namespace: clusterNamespace}, postgresCluster)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("PostgresCluster not found, requeueing", "PostgresCluster.Name", postgresDatabase.Spec.ClusterRef.Name, "PostgresCluster.Namespace", clusterNamespace)
			r.setCondition(postgresDatabase, "ClusterReady", false, fmt.Sprintf("PostgresCluster %s/%s not found", clusterNamespace, postgresDatabase.Spec.ClusterRef.Name))
			r.updatePhase(postgresDatabase)
			if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
				log.Error(updateErr, "Failed to update status")
			}
			return nil, "", &ctrl.Result{Requeue: true}, nil
		}
		log.Error(err, "Failed to get PostgresCluster")
		r.setCondition(postgresDatabase, "ClusterReady", false, fmt.Sprintf("Failed to get PostgresCluster: %v", err))
		r.updatePhase(postgresDatabase)
		if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return nil, "", nil, err
	}

	// Check if PostgresCluster is ready
	clusterReady := r.isClusterReady(postgresCluster)
	r.setCondition(postgresDatabase, "ClusterReady", clusterReady, r.getClusterReadyMessage(postgresCluster, clusterReady))
	if !clusterReady {
		r.updatePhase(postgresDatabase)
		if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return nil, "", &ctrl.Result{Requeue: true}, nil
	}

	return postgresCluster, clusterNamespace, nil, nil
}

// resolveClusterConnection resolves connection details from PostgresCluster spec.
// Returns host, port, user, password, result (if requeue needed), and error.
func (r *PostgresDatabaseReconciler) resolveClusterConnection(ctx context.Context, postgresDatabase *postgresv1.PostgresDatabase, postgresCluster *postgresv1.PostgresCluster, clusterNamespace string, log logr.Logger) (string, int, string, string, *ctrl.Result, error) {
	// Resolve host
	host, result, err := r.resolveStringOrSecret(ctx, postgresDatabase, postgresCluster.Spec.Host, "host", clusterNamespace, log)
	if result != nil {
		return "", 0, "", "", result, err
	}
	if err != nil {
		return "", 0, "", "", nil, err
	}

	// Resolve user
	user, result, err := r.resolveStringOrSecret(ctx, postgresDatabase, postgresCluster.Spec.User, "user", clusterNamespace, log)
	if result != nil {
		return "", 0, "", "", result, err
	}
	if err != nil {
		return "", 0, "", "", nil, err
	}

	// Resolve password
	password, result, err := r.resolveStringOrSecret(ctx, postgresDatabase, postgresCluster.Spec.Password, "password", clusterNamespace, log)
	if result != nil {
		return "", 0, "", "", result, err
	}
	if err != nil {
		return "", 0, "", "", nil, err
	}

	// Get port (default to 5432)
	port := postgresCluster.Spec.Port
	if port == 0 {
		port = 5432
	}

	// Validate that all required fields are present
	if host == "" {
		r.setCondition(postgresDatabase, "SecretReady", false, "PostgresCluster host is not configured")
		r.updatePhase(postgresDatabase)
		if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return "", 0, "", "", nil, fmt.Errorf("PostgresCluster host is not configured")
	}
	if user == "" {
		r.setCondition(postgresDatabase, "SecretReady", false, "PostgresCluster user is not configured")
		r.updatePhase(postgresDatabase)
		if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return "", 0, "", "", nil, fmt.Errorf("PostgresCluster user is not configured")
	}
	if password == "" {
		r.setCondition(postgresDatabase, "SecretReady", false, "PostgresCluster password is not configured")
		r.updatePhase(postgresDatabase)
		if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return "", 0, "", "", nil, fmt.Errorf("PostgresCluster password is not configured")
	}

	r.setCondition(postgresDatabase, "SecretReady", true, "Connection details resolved from PostgresCluster")
	return host, port, user, password, nil, nil
}

// resolveStringOrSecret resolves a StringOrSecret to its actual string value.
func (r *PostgresDatabaseReconciler) resolveStringOrSecret(ctx context.Context, postgresDatabase *postgresv1.PostgresDatabase, strOrSecret *postgresv1.StringOrSecret, fieldName string, clusterNamespace string, log logr.Logger) (string, *ctrl.Result, error) {
	if strOrSecret == nil {
		return "", nil, fmt.Errorf("%s is required", fieldName)
	}

	// Check if it's a literal value
	if strOrSecret.Value != "" {
		return strOrSecret.Value, nil, nil
	}

	// Check if it's a secret reference
	if strOrSecret.ValueFrom == nil {
		return "", nil, fmt.Errorf("%s must specify either value or valueFrom", fieldName)
	}

	secretNamespace := clusterNamespace
	if strOrSecret.ValueFrom.Namespace != "" {
		secretNamespace = strOrSecret.ValueFrom.Namespace
	}

	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: strOrSecret.ValueFrom.Name, Namespace: secretNamespace}, secret)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("Secret not found, requeueing", "Secret.Name", strOrSecret.ValueFrom.Name, "Secret.Namespace", secretNamespace, "Field", fieldName)
			r.setCondition(postgresDatabase, "SecretReady", false, fmt.Sprintf("Secret %s/%s not found for %s", secretNamespace, strOrSecret.ValueFrom.Name, fieldName))
			r.updatePhase(postgresDatabase)
			if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
				log.Error(updateErr, "Failed to update status")
			}
			return "", &ctrl.Result{Requeue: true}, nil
		}
		return "", nil, fmt.Errorf("failed to get secret %s/%s: %w", secretNamespace, strOrSecret.ValueFrom.Name, err)
	}

	value, exists := secret.Data[strOrSecret.ValueFrom.Key]
	if !exists {
		return "", nil, fmt.Errorf("key %s not found in secret %s/%s", strOrSecret.ValueFrom.Key, secretNamespace, strOrSecret.ValueFrom.Name)
	}

	return string(value), nil, nil
}

// handleDeletion handles the deletion of a PostgresDatabase.
// This function is resilient to missing cluster/secret during undeploy scenarios.
func (r *PostgresDatabaseReconciler) handleDeletion(ctx context.Context, postgresDatabase *postgresv1.PostgresDatabase, log logr.Logger) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(postgresDatabase, postgresDatabaseFinalizer) {
		// No finalizer, nothing to do
		return ctrl.Result{}, nil
	}

	log.Info("Performing finalizer cleanup for PostgresDatabase", "PostgresDatabase.Name", postgresDatabase.Name)

	// Try to fetch cluster and secret for cleanup, but don't fail if they're missing
	// This allows deletion to proceed even during undeploy when resources are being deleted
	clusterNamespace := postgresDatabase.Namespace
	if postgresDatabase.Spec.ClusterRef.Namespace != "" {
		clusterNamespace = postgresDatabase.Spec.ClusterRef.Namespace
	}

	postgresCluster := &postgresv1.PostgresCluster{}
	err := r.Get(ctx, types.NamespacedName{Name: postgresDatabase.Spec.ClusterRef.Name, Namespace: clusterNamespace}, postgresCluster)
	clusterAvailable := err == nil

	var pgHost string
	var pgPort int
	var adminUser string
	var adminPassword string
	if clusterAvailable {
		// Try to resolve connection details
		var resolveErr error
		pgHost, pgPort, adminUser, adminPassword, _, resolveErr = r.resolveClusterConnection(ctx, postgresDatabase, postgresCluster, clusterNamespace, log)
		if resolveErr != nil {
			clusterAvailable = false
		}
	}

	// Only attempt cleanup if cluster and connection details are available and ReclaimPolicy is Delete
	if clusterAvailable && pgHost != "" && adminUser != "" && adminPassword != "" && postgresDatabase.Spec.ReclaimPolicy == "Delete" {
		psqlConn := fmt.Sprintf("host=%s port=%d user=%s password=%s sslmode=disable", pgHost, pgPort, adminUser, adminPassword)
		db, err := r.DBConnectionFunc(psqlConn)
		if err != nil {
			log.Error(err, "Failed to open database connection during finalization, will still remove finalizer")
		} else {
			defer func() {
				if closeErr := db.Close(); closeErr != nil {
					log.Error(closeErr, "Failed to close database connection")
				}
			}()

			if err := dropDatabaseAndUser(ctx, postgresDatabase, db, log); err != nil {
				log.Error(err, "Failed to drop database and user during finalization, will still remove finalizer")
			} else {
				log.Info("Successfully dropped database and user during finalization")
			}
		}
	} else {
		if postgresDatabase.Spec.ReclaimPolicy == "Delete" {
			log.Info("Cluster or secret not available during finalization, skipping database cleanup", "clusterAvailable", clusterAvailable)
		}
	}

	// Always remove the finalizer, even if cleanup failed or wasn't possible
	// This ensures the resource can be deleted even if dependencies are gone
	// Re-fetch the resource to get the latest version and avoid conflicts
	latestPostgresDatabase := &postgresv1.PostgresDatabase{}
	if err := r.Get(ctx, types.NamespacedName{Name: postgresDatabase.Name, Namespace: postgresDatabase.Namespace}, latestPostgresDatabase); err != nil {
		if errors.IsNotFound(err) {
			// Resource already deleted, nothing to do
			log.Info("PostgresDatabase already deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to re-fetch PostgresDatabase before removing finalizer")
		return ctrl.Result{}, err
	}

	// Check if finalizer still exists (might have been removed by another process)
	if !controllerutil.ContainsFinalizer(latestPostgresDatabase, postgresDatabaseFinalizer) {
		log.Info("Finalizer already removed")
		return ctrl.Result{}, nil
	}

	// Remove the finalizer
	controllerutil.RemoveFinalizer(latestPostgresDatabase, postgresDatabaseFinalizer)
	if err := r.Update(ctx, latestPostgresDatabase); err != nil {
		if errors.IsConflict(err) {
			// Conflict means resource was updated, requeue to retry
			log.Info("Conflict updating PostgresDatabase to remove finalizer, will requeue")
			return ctrl.Result{Requeue: true}, nil
		}
		log.Error(err, "Failed to remove finalizer")
		return ctrl.Result{}, err
	}

	log.Info("Finalizer removed successfully")
	return ctrl.Result{}, nil
}

// ensureFinalizer ensures the finalizer is set on the PostgresDatabase.
func (r *PostgresDatabaseReconciler) ensureFinalizer(ctx context.Context, postgresDatabase *postgresv1.PostgresDatabase) error {
	if !controllerutil.ContainsFinalizer(postgresDatabase, postgresDatabaseFinalizer) {
		controllerutil.AddFinalizer(postgresDatabase, postgresDatabaseFinalizer)
		return r.Update(ctx, postgresDatabase)
	}
	return nil
}

// connectToDatabase connects to the PostgreSQL database.
func (r *PostgresDatabaseReconciler) connectToDatabase(ctx context.Context, postgresDatabase *postgresv1.PostgresDatabase, pgHost, pgPort, adminUser, adminPassword string, log logr.Logger) (*sql.DB, *ctrl.Result, error) {
	psqlConn := fmt.Sprintf("host=%s port=%s user=%s password=%s sslmode=disable", pgHost, pgPort, adminUser, adminPassword)
	db, err := r.DBConnectionFunc(psqlConn)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to open database connection: %v", err)
		log.Error(err, "Failed to open database connection")
		r.setCondition(postgresDatabase, "ConnectionReady", false, errMsg)
		r.updatePhase(postgresDatabase)
		if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return nil, &ctrl.Result{}, err
	}

	err = db.PingContext(ctx)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to connect to PostgreSQL: %v", err)
		log.Error(err, "Failed to connect to PostgreSQL")
		r.setCondition(postgresDatabase, "ConnectionReady", false, errMsg)
		r.updatePhase(postgresDatabase)
		if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return nil, &ctrl.Result{Requeue: true}, err
	}
	log.Info("Successfully connected to PostgresCluster")
	r.setCondition(postgresDatabase, "ConnectionReady", true, "Successfully connected to PostgreSQL")
	return db, nil, nil
}

// ensureConnectionSecret ensures the connection secret exists.
func (r *PostgresDatabaseReconciler) ensureConnectionSecret(ctx context.Context, postgresDatabase *postgresv1.PostgresDatabase, pgHost, pgPort string, log logr.Logger) (string, *ctrl.Result, error) {
	dbConnectionSecretName := fmt.Sprintf("%s-connection", postgresDatabase.Name)
	dbConnectionSecret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: dbConnectionSecretName, Namespace: postgresDatabase.Namespace}, dbConnectionSecret)
	if err != nil && errors.IsNotFound(err) {
		log.Info("Creating connection secret for PostgresDatabase", "Secret.Namespace", postgresDatabase.Namespace, "Secret.Name", dbConnectionSecretName)

		dbUserPassword, err := generateRandomPassword(32)
		if err != nil {
			errMsg := fmt.Sprintf("Failed to generate random password: %v", err)
			log.Error(err, "Failed to generate random password for database user")
			r.setCondition(postgresDatabase, "SecretReady", false, errMsg)
			r.updatePhase(postgresDatabase)
			if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
				log.Error(updateErr, "Failed to update status")
			}
			return "", &ctrl.Result{}, err
		}

		databaseName := getDatabaseName(postgresDatabase)
		userName := getUserName(postgresDatabase)
		dbConnectionSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      dbConnectionSecretName,
				Namespace: postgresDatabase.Namespace,
			},
			Data: map[string][]byte{
				"host":     []byte(pgHost),
				"port":     []byte(pgPort),
				"database": []byte(databaseName),
				"user":     []byte(userName),
				"password": []byte(dbUserPassword),
				"url":      []byte(fmt.Sprintf("postgresql://%s:%s@%s:%s/%s?sslmode=disable", userName, dbUserPassword, pgHost, pgPort, databaseName)),
			},
		}

		if err := controllerutil.SetControllerReference(postgresDatabase, dbConnectionSecret, r.Scheme); err != nil {
			errMsg := fmt.Sprintf("Failed to set controller reference: %v", err)
			log.Error(err, "Failed to set controller reference for database connection secret")
			r.setCondition(postgresDatabase, "SecretReady", false, errMsg)
			r.updatePhase(postgresDatabase)
			if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
				log.Error(updateErr, "Failed to update status")
			}
			return "", &ctrl.Result{}, err
		}

		if err := r.Create(ctx, dbConnectionSecret); err != nil {
			errMsg := fmt.Sprintf("Failed to create connection secret: %v", err)
			log.Error(err, "Failed to create database connection secret")
			r.setCondition(postgresDatabase, "SecretReady", false, errMsg)
			r.updatePhase(postgresDatabase)
			if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
				log.Error(updateErr, "Failed to update status")
			}
			return "", &ctrl.Result{}, err
		}
		log.Info("Database connection secret created successfully")
		r.setCondition(postgresDatabase, "SecretReady", true, "Connection secret created")
		return dbUserPassword, nil, nil
	} else if err != nil {
		errMsg := fmt.Sprintf("Failed to get connection secret: %v", err)
		log.Error(err, "Failed to get database connection secret")
		r.setCondition(postgresDatabase, "SecretReady", false, errMsg)
		r.updatePhase(postgresDatabase)
		if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return "", &ctrl.Result{}, err
	}
	log.Info("Database connection secret already exists", "Secret.Namespace", postgresDatabase.Namespace, "Secret.Name", dbConnectionSecretName)
	dbUserPassword := string(dbConnectionSecret.Data["password"])
	r.setCondition(postgresDatabase, "SecretReady", true, "Connection secret exists")
	return dbUserPassword, nil, nil
}

// createDatabaseAndUser creates the database and user.
func (r *PostgresDatabaseReconciler) createDatabaseAndUser(ctx context.Context, postgresDatabase *postgresv1.PostgresDatabase, db *sql.DB, dbUserPassword string, pgHost, pgPort, adminUser, adminPassword string, log logr.Logger) (*ctrl.Result, error) {
	databaseName := getDatabaseName(postgresDatabase)
	userName := getUserName(postgresDatabase)
	quotedUserName := quotePostgreSQLIdentifier(userName)
	quotedDBName := quotePostgreSQLIdentifier(databaseName)
	// Escape single quotes in password by doubling them
	escapedPassword := strings.ReplaceAll(dbUserPassword, "'", "''")

	// Create user first (before database) so we can set them as owner
	// Execute outside transaction to avoid abort issues
	createUserQuery := fmt.Sprintf("CREATE USER %s WITH PASSWORD '%s';", quotedUserName, escapedPassword)
	userExists := false
	if _, err := db.ExecContext(ctx, createUserQuery); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			errMsg := fmt.Sprintf("Failed to create user: %v", err)
			log.Error(err, "Failed to create user", "query", createUserQuery)
			r.setCondition(postgresDatabase, "DatabaseReady", false, errMsg)
			r.updatePhase(postgresDatabase)
			if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
				log.Error(updateErr, "Failed to update status")
			}
			return &ctrl.Result{}, err
		}
		log.Info("User already exists, updating password if needed", "user", userName)
		userExists = true
		// User exists, update password to ensure it matches the secret
		alterUserQuery := fmt.Sprintf("ALTER USER %s WITH PASSWORD '%s';", quotedUserName, escapedPassword)
		if _, alterErr := db.ExecContext(ctx, alterUserQuery); alterErr != nil {
			log.Info("Failed to update user password (non-critical, may already be correct)", "error", alterErr)
			// Non-critical, continue - password might already be correct
		}
	}

	// Create database with the user as owner (must be executed outside of a transaction)
	createDBQuery := fmt.Sprintf("CREATE DATABASE %s OWNER %s;", quotedDBName, quotedUserName)
	if _, err := db.ExecContext(ctx, createDBQuery); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			errMsg := fmt.Sprintf("Failed to create database: %v", err)
			log.Error(err, "Failed to create database", "query", createDBQuery)
			r.setCondition(postgresDatabase, "DatabaseReady", false, errMsg)
			r.updatePhase(postgresDatabase)
			if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
				log.Error(updateErr, "Failed to update status")
			}
			return &ctrl.Result{}, err
		}
		log.Info("Database already exists", "database", databaseName)
		// If database exists and user existed, ensure user is the owner
		if userExists {
			alterDBOwnerQuery := fmt.Sprintf("ALTER DATABASE %s OWNER TO %s;", quotedDBName, quotedUserName)
			if _, alterErr := db.ExecContext(ctx, alterDBOwnerQuery); alterErr != nil {
				log.Info("Failed to change database owner (non-critical, may already be correct)", "error", alterErr)
				// Non-critical, continue - ownership might already be correct
			} else {
				log.Info("Changed database owner to user", "database", databaseName, "user", userName)
			}
		}
	}

	// Grant database-level privileges
	// Note: GRANT can be executed multiple times safely (idempotent)
	grantPrivilegesQuery := fmt.Sprintf("GRANT ALL PRIVILEGES ON DATABASE %s TO %s;", quotedDBName, quotedUserName)
	if _, err := db.ExecContext(ctx, grantPrivilegesQuery); err != nil {
		errMsg := fmt.Sprintf("Failed to grant database privileges: %v", err)
		log.Error(err, "Failed to grant database privileges", "query", grantPrivilegesQuery)
		r.setCondition(postgresDatabase, "DatabaseReady", false, errMsg)
		r.updatePhase(postgresDatabase)
		if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return &ctrl.Result{}, err
	}

	// Connect to the newly created database to grant schema privileges
	// We need to connect to the specific database, not the default postgres database
	dbConnStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable", pgHost, pgPort, adminUser, adminPassword, databaseName)
	targetDB, err := r.DBConnectionFunc(dbConnStr)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to connect to target database: %v", err)
		log.Error(err, "Failed to connect to target database")
		r.setCondition(postgresDatabase, "DatabaseReady", false, errMsg)
		r.updatePhase(postgresDatabase)
		if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return &ctrl.Result{}, err
	}
	defer func() {
		if closeErr := targetDB.Close(); closeErr != nil {
			log.Error(closeErr, "Failed to close target database connection")
		}
	}()

	// Verify connection to target database
	if err := targetDB.PingContext(ctx); err != nil {
		errMsg := fmt.Sprintf("Failed to ping target database: %v", err)
		log.Error(err, "Failed to ping target database")
		r.setCondition(postgresDatabase, "DatabaseReady", false, errMsg)
		r.updatePhase(postgresDatabase)
		if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return &ctrl.Result{}, err
	}

	// Grant schema-level privileges on public schema
	// This is required for the user to be able to use the database
	grantSchemaPrivilegesQuery := fmt.Sprintf("GRANT ALL PRIVILEGES ON SCHEMA public TO %s;", quotedUserName)
	if _, err := targetDB.ExecContext(ctx, grantSchemaPrivilegesQuery); err != nil {
		errMsg := fmt.Sprintf("Failed to grant schema privileges: %v", err)
		log.Error(err, "Failed to grant schema privileges", "query", grantSchemaPrivilegesQuery)
		r.setCondition(postgresDatabase, "DatabaseReady", false, errMsg)
		r.updatePhase(postgresDatabase)
		if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return &ctrl.Result{}, err
	}

	// Grant CREATE privilege on public schema so user can create objects
	grantCreateQuery := fmt.Sprintf("GRANT CREATE ON SCHEMA public TO %s;", quotedUserName)
	if _, err := targetDB.ExecContext(ctx, grantCreateQuery); err != nil {
		errMsg := fmt.Sprintf("Failed to grant CREATE privilege on schema: %v", err)
		log.Error(err, "Failed to grant CREATE privilege on schema", "query", grantCreateQuery)
		r.setCondition(postgresDatabase, "DatabaseReady", false, errMsg)
		r.updatePhase(postgresDatabase)
		if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return &ctrl.Result{}, err
	}

	// Grant default privileges on all tables, sequences, and functions in public schema
	// This ensures the user has privileges on objects created in the future
	grantDefaultPrivilegesQuery := fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO %s;", quotedUserName)
	if _, err := targetDB.ExecContext(ctx, grantDefaultPrivilegesQuery); err != nil {
		log.Info("Failed to grant default privileges on tables (non-critical)", "error", err)
		// Non-critical, continue
	}

	grantDefaultSeqQuery := fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO %s;", quotedUserName)
	if _, err := targetDB.ExecContext(ctx, grantDefaultSeqQuery); err != nil {
		log.Info("Failed to grant default privileges on sequences (non-critical)", "error", err)
		// Non-critical, continue
	}

	grantDefaultFuncQuery := fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON FUNCTIONS TO %s;", quotedUserName)
	if _, err := targetDB.ExecContext(ctx, grantDefaultFuncQuery); err != nil {
		log.Info("Failed to grant default privileges on functions (non-critical)", "error", err)
		// Non-critical, continue
	}

	// Transfer ownership of existing tables to the user
	// This is critical for applications that need to modify table structures (migrations, etc.)
	transferTableOwnershipQuery := fmt.Sprintf(`
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN (SELECT tablename FROM pg_tables WHERE schemaname = 'public') LOOP
        EXECUTE 'ALTER TABLE public.' || quote_ident(r.tablename) || ' OWNER TO %s;';
    END LOOP;
END $$;`, quotedUserName)
	if _, err := targetDB.ExecContext(ctx, transferTableOwnershipQuery); err != nil {
		log.Info("Failed to transfer table ownership (non-critical, may have no tables)", "error", err)
		// Non-critical, continue - database might not have any tables yet
	} else {
		log.Info("Transferred ownership of existing tables to user", "user", userName)
	}

	// Transfer ownership of existing sequences to the user
	transferSequenceOwnershipQuery := fmt.Sprintf(`
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN (SELECT sequence_name FROM information_schema.sequences WHERE sequence_schema = 'public') LOOP
        EXECUTE 'ALTER SEQUENCE public.' || quote_ident(r.sequence_name) || ' OWNER TO %s;';
    END LOOP;
END $$;`, quotedUserName)
	if _, err := targetDB.ExecContext(ctx, transferSequenceOwnershipQuery); err != nil {
		log.Info("Failed to transfer sequence ownership (non-critical, may have no sequences)", "error", err)
		// Non-critical, continue - database might not have any sequences yet
	} else {
		log.Info("Transferred ownership of existing sequences to user", "user", userName)
	}

	// Transfer ownership of existing views to the user
	transferViewOwnershipQuery := fmt.Sprintf(`
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN (SELECT table_name FROM information_schema.views WHERE table_schema = 'public') LOOP
        EXECUTE 'ALTER VIEW public.' || quote_ident(r.table_name) || ' OWNER TO %s;';
    END LOOP;
END $$;`, quotedUserName)
	if _, err := targetDB.ExecContext(ctx, transferViewOwnershipQuery); err != nil {
		log.Info("Failed to transfer view ownership (non-critical, may have no views)", "error", err)
		// Non-critical, continue - database might not have any views yet
	} else {
		log.Info("Transferred ownership of existing views to user", "user", userName)
	}

	// Transfer ownership of existing materialized views to the user
	transferMatViewOwnershipQuery := fmt.Sprintf(`
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN (SELECT matviewname FROM pg_matviews WHERE schemaname = 'public') LOOP
        EXECUTE 'ALTER MATERIALIZED VIEW public.' || quote_ident(r.matviewname) || ' OWNER TO %s;';
    END LOOP;
END $$;`, quotedUserName)
	if _, err := targetDB.ExecContext(ctx, transferMatViewOwnershipQuery); err != nil {
		log.Info("Failed to transfer materialized view ownership (non-critical, may have no materialized views)", "error", err)
		// Non-critical, continue - database might not have any materialized views yet
	} else {
		log.Info("Transferred ownership of existing materialized views to user", "user", userName)
	}

	// Transfer ownership of existing functions to the user
	transferFunctionOwnershipQuery := fmt.Sprintf(`
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN (SELECT p.proname, pg_get_function_identity_arguments(p.oid) as args
              FROM pg_proc p
              JOIN pg_namespace n ON p.pronamespace = n.oid
              WHERE n.nspname = 'public') LOOP
        EXECUTE 'ALTER FUNCTION public.' || quote_ident(r.proname) || '(' || r.args || ') OWNER TO %s;';
    END LOOP;
END $$;`, quotedUserName)
	if _, err := targetDB.ExecContext(ctx, transferFunctionOwnershipQuery); err != nil {
		log.Info("Failed to transfer function ownership (non-critical, may have no functions)", "error", err)
		// Non-critical, continue - database might not have any functions yet
	} else {
		log.Info("Transferred ownership of existing functions to user", "user", userName)
	}

	// Transfer ownership of existing types to the user
	transferTypeOwnershipQuery := fmt.Sprintf(`
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN (SELECT t.typname
              FROM pg_type t
              JOIN pg_namespace n ON t.typnamespace = n.oid
              WHERE n.nspname = 'public'
              AND t.typtype IN ('e', 'c', 'r', 'b')) LOOP
        EXECUTE 'ALTER TYPE public.' || quote_ident(r.typname) || ' OWNER TO %s;';
    END LOOP;
END $$;`, quotedUserName)
	if _, err := targetDB.ExecContext(ctx, transferTypeOwnershipQuery); err != nil {
		log.Info("Failed to transfer type ownership (non-critical, may have no types)", "error", err)
		// Non-critical, continue - database might not have any types yet
	} else {
		log.Info("Transferred ownership of existing types to user", "user", userName)
	}

	// Transfer ownership of existing domains to the user
	transferDomainOwnershipQuery := fmt.Sprintf(`
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN (SELECT t.typname
              FROM pg_type t
              JOIN pg_namespace n ON t.typnamespace = n.oid
              WHERE n.nspname = 'public'
              AND t.typtype = 'd') LOOP
        EXECUTE 'ALTER DOMAIN public.' || quote_ident(r.typname) || ' OWNER TO %s;';
    END LOOP;
END $$;`, quotedUserName)
	if _, err := targetDB.ExecContext(ctx, transferDomainOwnershipQuery); err != nil {
		log.Info("Failed to transfer domain ownership (non-critical, may have no domains)", "error", err)
		// Non-critical, continue - database might not have any domains yet
	} else {
		log.Info("Transferred ownership of existing domains to user", "user", userName)
	}

	log.Info("Database and user created successfully with full privileges and object ownership")
	r.setCondition(postgresDatabase, "DatabaseReady", true, "Database and user created successfully")
	return nil, nil
}

// updateStatus updates the PostgresDatabase status with conditions, phase, and connection info.
func (r *PostgresDatabaseReconciler) updateStatus(ctx context.Context, postgresDatabase *postgresv1.PostgresDatabase, pgHost, pgPort string) error {
	// Set observed generation
	postgresDatabase.Status.ObservedGeneration = postgresDatabase.Generation

	// Determine overall Ready condition
	clusterReady := r.getConditionStatus(postgresDatabase, "ClusterReady")
	secretReady := r.getConditionStatus(postgresDatabase, "SecretReady")
	connectionReady := r.getConditionStatus(postgresDatabase, "ConnectionReady")
	databaseReady := r.getConditionStatus(postgresDatabase, "DatabaseReady")

	overallReady := clusterReady && secretReady && connectionReady && databaseReady
	overallMessage := "PostgresDatabase is ready"
	if !overallReady {
		var reasons []string
		if !clusterReady {
			reasons = append(reasons, "cluster not ready")
		}
		if !secretReady {
			reasons = append(reasons, "secret not ready")
		}
		if !connectionReady {
			reasons = append(reasons, "connection not ready")
		}
		if !databaseReady {
			reasons = append(reasons, "database not ready")
		}
		overallMessage = fmt.Sprintf("PostgresDatabase not ready: %s", strings.Join(reasons, ", "))
	}
	r.setCondition(postgresDatabase, "Ready", overallReady, overallMessage)

	// Update phase based on conditions
	r.updatePhase(postgresDatabase)

	// Update connection info (without password for security)
	if overallReady {
		databaseName := getDatabaseName(postgresDatabase)
		userName := getUserName(postgresDatabase)
		portInt := 5432
		if pgPort != "" {
			if _, err := fmt.Sscanf(pgPort, "%d", &portInt); err != nil {
				// If parsing fails, use default port
				portInt = 5432
			}
		}
		postgresDatabase.Status.Connection = &postgresv1.PostgresDatabaseConnection{
			Host:     pgHost,
			Port:     portInt,
			Database: databaseName,
			User:     userName,
			URL:      fmt.Sprintf("postgresql://%s@%s:%s/%s?sslmode=disable", userName, pgHost, pgPort, databaseName),
		}
	}

	return r.Status().Update(ctx, postgresDatabase)
}

// setCondition sets or updates a condition on the PostgresDatabase.
func (r *PostgresDatabaseReconciler) setCondition(postgresDatabase *postgresv1.PostgresDatabase, conditionType string, status bool, message string) {
	conditionStatus := metav1.ConditionFalse
	reason := "NotReady"
	if status {
		conditionStatus = metav1.ConditionTrue
		reason = conditionReasonReady
	}

	now := metav1.NewTime(time.Now())
	condition := metav1.Condition{
		Type:               conditionType,
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: postgresDatabase.Generation,
	}

	// Find existing condition
	for i, c := range postgresDatabase.Status.Conditions {
		if c.Type == conditionType {
			// Only update if status or generation changed
			if c.Status != conditionStatus || c.ObservedGeneration != postgresDatabase.Generation {
				postgresDatabase.Status.Conditions[i] = condition
			} else {
				// Update message and time even if status hasn't changed
				postgresDatabase.Status.Conditions[i].Message = message
				postgresDatabase.Status.Conditions[i].LastTransitionTime = now
			}
			return
		}
	}

	// Condition doesn't exist, add it
	postgresDatabase.Status.Conditions = append(postgresDatabase.Status.Conditions, condition)
}

// updatePhase updates the phase based on the current conditions.
func (r *PostgresDatabaseReconciler) updatePhase(postgresDatabase *postgresv1.PostgresDatabase) {
	// Check if being deleted
	if !postgresDatabase.DeletionTimestamp.IsZero() {
		postgresDatabase.Status.Phase = postgresv1.PostgresDatabasePhaseDeleting
		return
	}

	// Check Ready condition
	readyCondition := r.getCondition(postgresDatabase, "Ready")
	if readyCondition == nil {
		postgresDatabase.Status.Phase = postgresv1.PostgresDatabasePhasePending
		return
	}

	switch readyCondition.Status {
	case metav1.ConditionTrue:
		postgresDatabase.Status.Phase = postgresv1.PostgresDatabasePhaseReady
	case metav1.ConditionFalse:
		// Check if there's a failure condition
		databaseCondition := r.getCondition(postgresDatabase, "DatabaseReady")
		if databaseCondition != nil && databaseCondition.Status == metav1.ConditionFalse {
			postgresDatabase.Status.Phase = postgresv1.PostgresDatabasePhaseFailed
		} else {
			postgresDatabase.Status.Phase = postgresv1.PostgresDatabasePhaseCreating
		}
	default:
		postgresDatabase.Status.Phase = postgresv1.PostgresDatabasePhasePending
	}
}

// getCondition returns the condition with the given type, or nil if not found.
func (r *PostgresDatabaseReconciler) getCondition(postgresDatabase *postgresv1.PostgresDatabase, conditionType string) *metav1.Condition {
	for i := range postgresDatabase.Status.Conditions {
		if postgresDatabase.Status.Conditions[i].Type == conditionType {
			return &postgresDatabase.Status.Conditions[i]
		}
	}
	return nil
}

// getConditionStatus returns true if the condition exists and is True, false otherwise.
func (r *PostgresDatabaseReconciler) getConditionStatus(postgresDatabase *postgresv1.PostgresDatabase, conditionType string) bool {
	condition := r.getCondition(postgresDatabase, conditionType)
	return condition != nil && condition.Status == metav1.ConditionTrue
}

// isClusterReady checks if the PostgresCluster is ready.
func (r *PostgresDatabaseReconciler) isClusterReady(cluster *postgresv1.PostgresCluster) bool {
	for _, condition := range cluster.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// getClusterReadyMessage returns a message about the cluster readiness.
func (r *PostgresDatabaseReconciler) getClusterReadyMessage(cluster *postgresv1.PostgresCluster, ready bool) string {
	if ready {
		return fmt.Sprintf("PostgresCluster %s/%s is ready", cluster.Namespace, cluster.Name)
	}
	for _, condition := range cluster.Status.Conditions {
		if condition.Type == "Ready" {
			return fmt.Sprintf("PostgresCluster %s/%s is not ready: %s", cluster.Namespace, cluster.Name, condition.Message)
		}
	}
	return fmt.Sprintf("PostgresCluster %s/%s readiness unknown", cluster.Namespace, cluster.Name)
}

// SetupWithManager sets up the controller with the Manager.
func (r *PostgresDatabaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.DBConnectionFunc == nil {
		r.DBConnectionFunc = func(connStr string) (*sql.DB, error) {
			return sql.Open("postgres", connStr)
		}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&postgresv1.PostgresDatabase{}).
		Owns(&corev1.Secret{}).
		Named("postgresdatabase").
		Complete(r)
}
