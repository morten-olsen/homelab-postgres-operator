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
	"net/url"
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

	// Resolve connection URL from PostgresCluster spec
	pgURL, result, err := r.resolveClusterConnection(ctx, postgresDatabase, postgresCluster, clusterNamespace, log)
	if result != nil {
		return *result, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Parse connection details from URL for status updates
	pgHost, pgPort, _, _, err := r.parsePostgresURL(pgURL)
	if err != nil {
		log.Error(err, "Failed to parse PostgreSQL URL")
		r.setCondition(postgresDatabase, "SecretReady", false, fmt.Sprintf("Failed to parse URL: %v", err))
		r.updatePhase(postgresDatabase)
		if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return ctrl.Result{}, err
	}

	// Ensure finalizer
	if err := r.ensureFinalizer(ctx, postgresDatabase); err != nil {
		return ctrl.Result{}, err
	}

	// Connect to database using the URL
	db, result, err := r.connectToDatabase(ctx, postgresDatabase, pgURL, log)
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
	result, err = r.createDatabaseAndUser(ctx, postgresDatabase, db, dbUserPassword, pgURL, log)
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

// resolveClusterConnection resolves connection URL from PostgresCluster spec.
// Returns URL string, result (if requeue needed), and error.
func (r *PostgresDatabaseReconciler) resolveClusterConnection(ctx context.Context, postgresDatabase *postgresv1.PostgresDatabase, postgresCluster *postgresv1.PostgresCluster, clusterNamespace string, log logr.Logger) (string, *ctrl.Result, error) {
	// Resolve URL
	urlStr, result, err := r.resolveStringOrSecret(ctx, postgresDatabase, postgresCluster.Spec.URL, "url", clusterNamespace, log)
	if result != nil {
		return "", result, err
	}
	if err != nil {
		return "", nil, err
	}

	// Validate that URL is present
	if urlStr == "" {
		r.setCondition(postgresDatabase, "SecretReady", false, "PostgresCluster url is not configured")
		r.updatePhase(postgresDatabase)
		if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return "", nil, fmt.Errorf("PostgresCluster url is not configured")
	}

	r.setCondition(postgresDatabase, "SecretReady", true, "Connection URL resolved from PostgresCluster")
	return urlStr, nil, nil
}

// parsePostgresURL parses a PostgreSQL URL or connection string and returns connection details.
func (r *PostgresDatabaseReconciler) parsePostgresURL(urlStr string) (host string, port int, user string, password string, err error) {
	// Try parsing as URL first
	if strings.HasPrefix(urlStr, "postgresql://") || strings.HasPrefix(urlStr, "postgres://") {
		parsedURL, err := url.Parse(urlStr)
		if err != nil {
			return "", 0, "", "", fmt.Errorf("failed to parse URL: %w", err)
		}

		host = parsedURL.Hostname()
		if host == "" {
			return "", 0, "", "", fmt.Errorf("host is required in URL")
		}

		if parsedURL.Port() != "" {
			if p, err := fmt.Sscanf(parsedURL.Port(), "%d", &port); err != nil || p != 1 {
				return "", 0, "", "", fmt.Errorf("invalid port in URL: %s", parsedURL.Port())
			}
		} else {
			port = 5432
		}

		if parsedURL.User != nil {
			user = parsedURL.User.Username()
			password, _ = parsedURL.User.Password()
		}

		return host, port, user, password, nil
	}

	// Try parsing as connection string (key=value format)
	// Parse the connection string manually, handling quoted values
	parseConnectionStringPart := func(part, key string) string {
		prefix := key + "="
		if !strings.HasPrefix(part, prefix) {
			return ""
		}
		value := strings.TrimPrefix(part, prefix)
		// Remove quotes if present
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		return value
	}

	parts := strings.Fields(urlStr)
	for _, part := range parts {
		if h := parseConnectionStringPart(part, "host"); h != "" {
			host = h
		} else if p := parseConnectionStringPart(part, "port"); p != "" {
			if portVal, err := fmt.Sscanf(p, "%d", &port); err != nil || portVal != 1 {
				return "", 0, "", "", fmt.Errorf("invalid port in connection string: %s", p)
			}
		} else if u := parseConnectionStringPart(part, "user"); u != "" {
			user = u
		} else if pw := parseConnectionStringPart(part, "password"); pw != "" {
			password = pw
		}
	}

	if host == "" {
		host = "localhost"
	}
	if port == 0 {
		port = 5432
	}

	return host, port, user, password, nil
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

	var pgURL string
	if clusterAvailable {
		// Try to resolve connection URL
		var resolveErr error
		pgURL, _, resolveErr = r.resolveClusterConnection(ctx, postgresDatabase, postgresCluster, clusterNamespace, log)
		if resolveErr != nil {
			clusterAvailable = false
		}
	}

	// Only attempt cleanup if cluster and connection URL are available and ReclaimPolicy is Delete
	if clusterAvailable && pgURL != "" && postgresDatabase.Spec.ReclaimPolicy == "Delete" {
		// Ensure we connect to the postgres database for cleanup
		cleanupURL := r.ensureDatabaseInURL(pgURL, "postgres")
		db, err := r.DBConnectionFunc(cleanupURL)
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

// connectToDatabase connects to the PostgreSQL database using the provided URL.
func (r *PostgresDatabaseReconciler) connectToDatabase(ctx context.Context, postgresDatabase *postgresv1.PostgresDatabase, pgURL string, log logr.Logger) (*sql.DB, *ctrl.Result, error) {
	// Ensure we connect to the postgres database (not a specific database)
	testURL := r.ensureDatabaseInURL(pgURL, "postgres")
	db, err := r.DBConnectionFunc(testURL)
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

// ensureDatabaseInURL ensures the database name is set in the URL/connection string.
func (r *PostgresDatabaseReconciler) ensureDatabaseInURL(urlStr, dbName string) string {
	if strings.HasPrefix(urlStr, "postgresql://") || strings.HasPrefix(urlStr, "postgres://") {
		// URL format - ensure it has the database in the path
		parsedURL, err := url.Parse(urlStr)
		if err == nil {
			// Replace path with /{dbName} if empty or different
			if parsedURL.Path == "" || parsedURL.Path == "/" {
				parsedURL.Path = "/" + dbName
			}
			return parsedURL.String()
		}
		// If parsing fails, return original
		return urlStr
	}

	// Connection string format - add dbname if not present
	if !strings.Contains(urlStr, "dbname=") {
		if strings.Contains(urlStr, "?") {
			return strings.Replace(urlStr, "?", fmt.Sprintf(" dbname=%s?", dbName), 1)
		}
		return urlStr + fmt.Sprintf(" dbname=%s", dbName)
	}

	return urlStr
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
func (r *PostgresDatabaseReconciler) createDatabaseAndUser(ctx context.Context, postgresDatabase *postgresv1.PostgresDatabase, db *sql.DB, dbUserPassword string, pgURL string, log logr.Logger) (*ctrl.Result, error) {
	databaseName := getDatabaseName(postgresDatabase)
	userName := getUserName(postgresDatabase)

	// Create database (must be executed outside of a transaction)
	quotedDBName := quotePostgreSQLIdentifier(databaseName)
	createDBQuery := fmt.Sprintf("CREATE DATABASE %s;", quotedDBName)
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
	}

	// Create user and grant privileges
	quotedUserName := quotePostgreSQLIdentifier(userName)
	// Escape single quotes in password by doubling them
	escapedPassword := strings.ReplaceAll(dbUserPassword, "'", "''")

	// Create user (execute outside transaction to avoid abort issues)
	createUserQuery := fmt.Sprintf("CREATE USER %s WITH PASSWORD '%s';", quotedUserName, escapedPassword)
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
		// User exists, update password to ensure it matches the secret
		alterUserQuery := fmt.Sprintf("ALTER USER %s WITH PASSWORD '%s';", quotedUserName, escapedPassword)
		if _, alterErr := db.ExecContext(ctx, alterUserQuery); alterErr != nil {
			log.Info("Failed to update user password (non-critical, may already be correct)", "error", alterErr)
			// Non-critical, continue - password might already be correct
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
	targetDBURL := r.ensureDatabaseInURL(pgURL, databaseName)
	targetDB, err := r.DBConnectionFunc(targetDBURL)
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

	log.Info("Database and user created successfully with full privileges")
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
