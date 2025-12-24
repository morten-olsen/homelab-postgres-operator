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

const postgresDatabaseFinalizer = "postgresdatabase.finalizers.homelab.mortenolsen.pro"

// dropDatabaseAndUser drops the database and user from the PostgreSQL instance.
func dropDatabaseAndUser(ctx context.Context, postgresDatabase *postgresv1.PostgresDatabase, db *sql.DB, log logr.Logger) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck

	// Drop database
	dropDBQuery := fmt.Sprintf("DROP DATABASE IF EXISTS %s;", postgresDatabase.Spec.DatabaseName)
	log.Info("Dropping database", "query", dropDBQuery)
	if _, err := tx.ExecContext(ctx, dropDBQuery); err != nil {
		return fmt.Errorf("failed to drop database: %w", err)
	}

	// Drop user
	dropUserQuery := fmt.Sprintf("DROP USER IF EXISTS %s;", postgresDatabase.Spec.UserName)
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

//+kubebuilder:rbac:groups=postgres.homelab.mortenolsen.pro,resources=postgresdatabases,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=postgres.homelab.mortenolsen.pro,resources=postgresdatabases/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=postgres.homelab.mortenolsen.pro,resources=postgresdatabases/finalizers,verbs=update
//+kubebuilder:rbac:groups=postgres.homelab.mortenolsen.pro,resources=postgresclusters,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

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

	// Fetch the PostgresCluster that this database belongs to
	clusterNamespace := postgresDatabase.Namespace
	if postgresDatabase.Spec.ClusterRef.Namespace != "" {
		clusterNamespace = postgresDatabase.Spec.ClusterRef.Namespace
	}
	postgresCluster := &postgresv1.PostgresCluster{}
	err = r.Get(ctx, types.NamespacedName{Name: postgresDatabase.Spec.ClusterRef.Name, Namespace: clusterNamespace}, postgresCluster)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("PostgresCluster not found, requeueing", "PostgresCluster.Name", postgresDatabase.Spec.ClusterRef.Name, "PostgresCluster.Namespace", clusterNamespace)
			return ctrl.Result{Requeue: true}, nil // Requeue until cluster exists
		}
		log.Error(err, "Failed to get PostgresCluster")
		return ctrl.Result{}, err
	}

	// Get the admin secret for the PostgresCluster
	adminSecretName := fmt.Sprintf("%s-admin-secret", postgresCluster.Name)
	adminSecret := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Name: adminSecretName, Namespace: clusterNamespace}, adminSecret)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("Admin secret for PostgresCluster not found, requeueing", "Secret.Name", adminSecretName)
			return ctrl.Result{Requeue: true}, nil // Requeue until secret exists
		}
		log.Error(err, "Failed to get admin secret for PostgresCluster")
		return ctrl.Result{}, err
	}

	adminUser := "postgres" // Default admin user
	adminPassword := string(adminSecret.Data["password"])
	pgHost := fmt.Sprintf("%s-service", postgresCluster.Name)
	pgPort := "5432"

	// Examine if the object is being deleted
	if !postgresDatabase.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(postgresDatabase, postgresDatabaseFinalizer) {
			log.Info("Performing finalizer cleanup for PostgresDatabase", "PostgresDatabase.Name", postgresDatabase.Name)

			if postgresDatabase.Spec.ReclaimPolicy == "Delete" {
				psqlConn := fmt.Sprintf("host=%s port=%s user=%s password=%s sslmode=disable", pgHost, pgPort, adminUser, adminPassword)
				db, err := r.DBConnectionFunc(psqlConn)
				if err != nil {
					log.Error(err, "Failed to open database connection during finalization")
					return ctrl.Result{}, err
				}
				defer db.Close()

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

	// The object is not being deleted, so if it does not have our finalizer,
	// then lets add it.
	if !controllerutil.ContainsFinalizer(postgresDatabase, postgresDatabaseFinalizer) {
		controllerutil.AddFinalizer(postgresDatabase, postgresDatabaseFinalizer)
		if err := r.Update(ctx, postgresDatabase); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Construct connection string for admin
	psqlConn := fmt.Sprintf("host=%s port=%s user=%s password=%s sslmode=disable", pgHost, pgPort, adminUser, adminPassword)
	db, err := r.DBConnectionFunc(psqlConn)
	if err != nil {
		log.Error(err, "Failed to open database connection")
		return ctrl.Result{}, err
	}
	defer db.Close()

	err = db.PingContext(ctx)
	if err != nil {
		log.Error(err, "Failed to connect to PostgreSQL")
		return ctrl.Result{Requeue: true}, err
	}
	log.Info("Successfully connected to PostgresCluster", "PostgresCluster.Name", postgresCluster.Name)

	// Create/Update connection secret for PostgresDatabase
	dbConnectionSecretName := fmt.Sprintf("%s-connection", postgresDatabase.Name)
	dbConnectionSecret := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Name: dbConnectionSecretName, Namespace: postgresDatabase.Namespace}, dbConnectionSecret)
	var dbUserPassword string
	if err != nil && errors.IsNotFound(err) {
		log.Info("Creating connection secret for PostgresDatabase", "Secret.Namespace", postgresDatabase.Namespace, "Secret.Name", dbConnectionSecretName)

		var err error
		dbUserPassword, err = generateRandomPassword(32)
		if err != nil {
			log.Error(err, "Failed to generate random password for database user")
			return ctrl.Result{}, err
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
			log.Error(err, "Failed to set controller reference for database connection secret")
			return ctrl.Result{}, err
		}

		if err := r.Create(ctx, dbConnectionSecret); err != nil {
			log.Error(err, "Failed to create database connection secret")
			return ctrl.Result{}, err
		}
		log.Info("Database connection secret created successfully")
	} else if err != nil {
		log.Error(err, "Failed to get database connection secret")
		return ctrl.Result{}, err
	} else {
		log.Info("Database connection secret already exists", "Secret.Namespace", postgresDatabase.Namespace, "Secret.Name", dbConnectionSecretName)
		dbUserPassword = string(dbConnectionSecret.Data["password"])
	}

	// Create database and user
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		log.Error(err, "Failed to begin transaction for database/user creation")
		return ctrl.Result{}, err
	}
	defer tx.Rollback() // nolint:errcheck

	// Create database
	createDBQuery := fmt.Sprintf("CREATE DATABASE %s;", postgresDatabase.Spec.DatabaseName)
	if _, err := tx.ExecContext(ctx, createDBQuery); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			log.Error(err, "Failed to create database")
			return ctrl.Result{}, err
		}
	}

	// Create user
	createUserQuery := fmt.Sprintf("CREATE USER %s WITH PASSWORD '%s';", postgresDatabase.Spec.UserName, dbUserPassword)
	if _, err := tx.ExecContext(ctx, createUserQuery); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			log.Error(err, "Failed to create user")
			return ctrl.Result{}, err
		}
	}

	// Grant privileges
	grantPrivilegesQuery := fmt.Sprintf("GRANT ALL PRIVILEGES ON DATABASE %s TO %s;", postgresDatabase.Spec.DatabaseName, postgresDatabase.Spec.UserName)
	if _, err := tx.ExecContext(ctx, grantPrivilegesQuery); err != nil {
		log.Error(err, "Failed to grant privileges")
		return ctrl.Result{}, err
	}

	if err := tx.Commit(); err != nil {
		log.Error(err, "Failed to commit transaction for database/user creation")
		return ctrl.Result{}, err
	}
	log.Info("Database and user created successfully")

	// Update PostgresDatabase status
	postgresDatabase.Status.Connection = &postgresv1.PostgresDatabaseConnection{
		Host:     pgHost,
		Port:     5432,
		Database: postgresDatabase.Spec.DatabaseName,
		User:     postgresDatabase.Spec.UserName,
		Password: dbUserPassword,
		URL:      fmt.Sprintf("postgresql://%s:%s@%s:%s/%s?sslmode=disable", postgresDatabase.Spec.UserName, dbUserPassword, pgHost, pgPort, postgresDatabase.Spec.DatabaseName),
	}
	if err := r.Status().Update(ctx, postgresDatabase); err != nil {
		log.Error(err, "Failed to update PostgresDatabase status")
		return ctrl.Result{}, err
	}

	log.Info("PostgresDatabase status updated successfully", "PostgresDatabase.Name", postgresDatabase.Name)

	return ctrl.Result{}, nil
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
