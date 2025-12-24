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

	// Drop database
	dropDBQuery := fmt.Sprintf("DROP DATABASE IF EXISTS %s;", quotePostgreSQLIdentifier(postgresDatabase.Spec.DatabaseName))
	log.Info("Dropping database", "query", dropDBQuery)
	if _, err := tx.ExecContext(ctx, dropDBQuery); err != nil {
		return fmt.Errorf("failed to drop database: %w", err)
	}

	// Drop user
	dropUserQuery := fmt.Sprintf("DROP USER IF EXISTS %s;", quotePostgreSQLIdentifier(postgresDatabase.Spec.UserName))
	log.Info("Dropping user", "query", dropUserQuery)
	if _, err := tx.ExecContext(ctx, dropUserQuery); err != nil {
		return fmt.Errorf("failed to drop user: %w", err)
	}

	return tx.Commit()
}

// generateRandomPassword generates a random password of the given length.
func generateRandomPassword(length int) (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%^&*()-_+="
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

	// Fetch and validate PostgresCluster
	postgresCluster, clusterNamespace, result, err := r.fetchAndValidateCluster(ctx, postgresDatabase, log)
	if result != nil {
		return *result, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Get admin secret
	adminSecret, result, err := r.fetchAdminSecret(ctx, postgresDatabase, postgresCluster, clusterNamespace, log)
	if result != nil {
		return *result, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	adminUser := "postgres" // Default admin user
	adminPassword := string(adminSecret.Data["password"])
	pgHost := fmt.Sprintf("%s-service", postgresCluster.Name)
	pgPort := "5432"

	// Handle deletion
	if !postgresDatabase.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, postgresDatabase, pgHost, pgPort, adminUser, adminPassword, log)
	}

	// Ensure finalizer
	if err := r.ensureFinalizer(ctx, postgresDatabase); err != nil {
		return ctrl.Result{}, err
	}

	// Connect to database
	db, result, err := r.connectToDatabase(ctx, postgresDatabase, pgHost, pgPort, adminUser, adminPassword, log)
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
	dbUserPassword, result, err := r.ensureConnectionSecret(ctx, postgresDatabase, pgHost, pgPort, log)
	if result != nil {
		return *result, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Create database and user
	result, err = r.createDatabaseAndUser(ctx, postgresDatabase, db, dbUserPassword, log)
	if result != nil {
		return *result, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Update PostgresDatabase status
	if err := r.updateStatus(ctx, postgresDatabase, pgHost, pgPort); err != nil {
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

// fetchAdminSecret fetches the admin secret for the PostgresCluster.
func (r *PostgresDatabaseReconciler) fetchAdminSecret(ctx context.Context, postgresDatabase *postgresv1.PostgresDatabase, postgresCluster *postgresv1.PostgresCluster, clusterNamespace string, log logr.Logger) (*corev1.Secret, *ctrl.Result, error) {
	adminSecretName := fmt.Sprintf("%s-admin-secret", postgresCluster.Name)
	adminSecret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: adminSecretName, Namespace: clusterNamespace}, adminSecret)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("Admin secret for PostgresCluster not found, requeueing", "Secret.Name", adminSecretName)
			r.setCondition(postgresDatabase, "SecretReady", false, fmt.Sprintf("Admin secret %s/%s not found", clusterNamespace, adminSecretName))
			r.updatePhase(postgresDatabase)
			if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
				log.Error(updateErr, "Failed to update status")
			}
			return nil, &ctrl.Result{Requeue: true}, nil
		}
		log.Error(err, "Failed to get admin secret for PostgresCluster")
		r.setCondition(postgresDatabase, "SecretReady", false, fmt.Sprintf("Failed to get admin secret: %v", err))
		r.updatePhase(postgresDatabase)
		if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return nil, nil, err
	}
	r.setCondition(postgresDatabase, "SecretReady", true, "Admin secret found")
	return adminSecret, nil, nil
}

// handleDeletion handles the deletion of a PostgresDatabase.
func (r *PostgresDatabaseReconciler) handleDeletion(ctx context.Context, postgresDatabase *postgresv1.PostgresDatabase, pgHost, pgPort, adminUser, adminPassword string, log logr.Logger) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(postgresDatabase, postgresDatabaseFinalizer) {
		log.Info("Performing finalizer cleanup for PostgresDatabase", "PostgresDatabase.Name", postgresDatabase.Name)

		if postgresDatabase.Spec.ReclaimPolicy == "Delete" {
			psqlConn := fmt.Sprintf("host=%s port=%s user=%s password=%s sslmode=disable", pgHost, pgPort, adminUser, adminPassword)
			db, err := r.DBConnectionFunc(psqlConn)
			if err != nil {
				log.Error(err, "Failed to open database connection during finalization")
				return ctrl.Result{}, err
			}
			defer func() {
				if closeErr := db.Close(); closeErr != nil {
					log.Error(closeErr, "Failed to close database connection")
				}
			}()

			if err := dropDatabaseAndUser(ctx, postgresDatabase, db, log); err != nil {
				log.Error(err, "Failed to drop database and user during finalization")
				return ctrl.Result{}, err
			}
		}

		// remove our finalizer from the list and update it.
		controllerutil.RemoveFinalizer(postgresDatabase, postgresDatabaseFinalizer)
		if err := r.Update(ctx, postgresDatabase); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Stop reconciliation as the object is being deleted
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

		dbConnectionSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      dbConnectionSecretName,
				Namespace: postgresDatabase.Namespace,
			},
			Data: map[string][]byte{
				"host":     []byte(pgHost),
				"port":     []byte(pgPort),
				"database": []byte(postgresDatabase.Spec.DatabaseName),
				"user":     []byte(postgresDatabase.Spec.UserName),
				"password": []byte(dbUserPassword),
				"url":      []byte(fmt.Sprintf("postgresql://%s:%s@%s:%s/%s?sslmode=disable", postgresDatabase.Spec.UserName, dbUserPassword, pgHost, pgPort, postgresDatabase.Spec.DatabaseName)),
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
func (r *PostgresDatabaseReconciler) createDatabaseAndUser(ctx context.Context, postgresDatabase *postgresv1.PostgresDatabase, db *sql.DB, dbUserPassword string, log logr.Logger) (*ctrl.Result, error) {
	// Create database (must be executed outside of a transaction)
	quotedDBName := quotePostgreSQLIdentifier(postgresDatabase.Spec.DatabaseName)
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
		log.Info("Database already exists", "database", postgresDatabase.Spec.DatabaseName)
	}

	// Create user and grant privileges
	quotedUserName := quotePostgreSQLIdentifier(postgresDatabase.Spec.UserName)
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
		log.Info("User already exists", "user", postgresDatabase.Spec.UserName)
	}

	// Grant privileges (can be done in a transaction, but we'll do it directly for simplicity)
	// Note: GRANT can be executed multiple times safely (idempotent)
	grantPrivilegesQuery := fmt.Sprintf("GRANT ALL PRIVILEGES ON DATABASE %s TO %s;", quotedDBName, quotedUserName)
	if _, err := db.ExecContext(ctx, grantPrivilegesQuery); err != nil {
		errMsg := fmt.Sprintf("Failed to grant privileges: %v", err)
		log.Error(err, "Failed to grant privileges", "query", grantPrivilegesQuery)
		r.setCondition(postgresDatabase, "DatabaseReady", false, errMsg)
		r.updatePhase(postgresDatabase)
		if updateErr := r.Status().Update(ctx, postgresDatabase); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return &ctrl.Result{}, err
	}
	log.Info("Database and user created successfully")
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
		postgresDatabase.Status.Connection = &postgresv1.PostgresDatabaseConnection{
			Host:     pgHost,
			Port:     5432,
			Database: postgresDatabase.Spec.DatabaseName,
			User:     postgresDatabase.Spec.UserName,
			URL:      fmt.Sprintf("postgresql://%s@%s:%s/%s?sslmode=disable", postgresDatabase.Spec.UserName, pgHost, pgPort, postgresDatabase.Spec.DatabaseName),
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
