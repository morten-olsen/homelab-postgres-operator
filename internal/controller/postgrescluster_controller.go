/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq" // PostgreSQL driver
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-logr/logr"
	postgresv1 "github.com/morten-olsen/homelab-postgres-operator/api/v1"
)

const (
	connectionSuccessMessage = "Successfully connected to PostgreSQL"
)

// PostgresClusterReconciler reconciles a PostgresCluster object
type PostgresClusterReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	TestConnection func(ctx context.Context, host string, port int, user, password string, log logr.Logger) (bool, string)
}

// +kubebuilder:rbac:groups=postgres.homelab.mortenolsen.pro,resources=postgresclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgres.homelab.mortenolsen.pro,resources=postgresclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=postgres.homelab.mortenolsen.pro,resources=postgresclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *PostgresClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the PostgresCluster instance
	postgresCluster := &postgresv1.PostgresCluster{}
	err := r.Get(ctx, req.NamespacedName, postgresCluster)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			log.Error(err, "Failed to get PostgresCluster")
			return ctrl.Result{}, err
		}
		// PostgresCluster not found, could be deleted
		return ctrl.Result{}, nil
	}

	// Resolve connection details from spec
	host, result, err := r.resolveStringOrSecret(ctx, postgresCluster, postgresCluster.Spec.Host, "host", log)
	if result != nil {
		return *result, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	user, result, err := r.resolveStringOrSecret(ctx, postgresCluster, postgresCluster.Spec.User, "user", log)
	if result != nil {
		return *result, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	password, result, err := r.resolveStringOrSecret(ctx, postgresCluster, postgresCluster.Spec.Password, "password", log)
	if result != nil {
		return *result, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	port := postgresCluster.Spec.Port
	if port == 0 {
		port = 5432
	}

	// Validate that all required fields are present
	if host == "" {
		r.setCondition(postgresCluster, "ConfigurationValid", false, "host is required")
		r.updatePhase(postgresCluster)
		if updateErr := r.Status().Update(ctx, postgresCluster); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return ctrl.Result{}, fmt.Errorf("host is required")
	}
	if user == "" {
		r.setCondition(postgresCluster, "ConfigurationValid", false, "user is required")
		r.updatePhase(postgresCluster)
		if updateErr := r.Status().Update(ctx, postgresCluster); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return ctrl.Result{}, fmt.Errorf("user is required")
	}
	if password == "" {
		r.setCondition(postgresCluster, "ConfigurationValid", false, "password is required")
		r.updatePhase(postgresCluster)
		if updateErr := r.Status().Update(ctx, postgresCluster); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return ctrl.Result{}, fmt.Errorf("password is required")
	}

	// Test connection to PostgreSQL
	testConnFunc := r.TestConnection
	if testConnFunc == nil {
		testConnFunc = r.testConnection
	}
	connectionReady, connectionMessage := testConnFunc(ctx, host, port, user, password, log)

	// Update status
	r.setCondition(postgresCluster, "ConfigurationValid", true, "Configuration is valid")
	r.setCondition(postgresCluster, "ConnectionReady", connectionReady, connectionMessage)

	// Update PostgresCluster status
	if err := r.updateStatus(ctx, postgresCluster, host, port, user); err != nil {
		log.Error(err, "Failed to update PostgresCluster status")
		return ctrl.Result{}, err
	}

	log.Info("PostgresCluster status updated successfully", "PostgresCluster.Name", postgresCluster.Name)

	// Requeue if connection is not ready
	if !connectionReady {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

// resolveStringOrSecret resolves a StringOrSecret to its actual string value.
// It returns the resolved value, a result (if requeue is needed), and an error.
func (r *PostgresClusterReconciler) resolveStringOrSecret(ctx context.Context, postgresCluster *postgresv1.PostgresCluster, strOrSecret *postgresv1.StringOrSecret, fieldName string, log logr.Logger) (string, *ctrl.Result, error) {
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

	secretNamespace := postgresCluster.Namespace
	if strOrSecret.ValueFrom.Namespace != "" {
		secretNamespace = strOrSecret.ValueFrom.Namespace
	}

	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: strOrSecret.ValueFrom.Name, Namespace: secretNamespace}, secret)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("Secret not found, requeueing", "Secret.Name", strOrSecret.ValueFrom.Name, "Secret.Namespace", secretNamespace, "Field", fieldName)
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

// testConnection tests the connection to PostgreSQL.
func (r *PostgresClusterReconciler) testConnection(ctx context.Context, host string, port int, user, password string, log logr.Logger) (bool, string) {
	connStr := fmt.Sprintf("host=%s port=%d user=%s password=%s sslmode=disable dbname=postgres", host, port, user, password)
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return false, fmt.Sprintf("Failed to open database connection: %v", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			log.Error(closeErr, "Failed to close database connection")
		}
	}()

	if err := db.PingContext(ctx); err != nil {
		return false, fmt.Sprintf("Failed to connect to PostgreSQL: %v", err)
	}

	return true, connectionSuccessMessage
}

// updateStatus updates the PostgresCluster status with conditions, phase, and connection info.
func (r *PostgresClusterReconciler) updateStatus(ctx context.Context, postgresCluster *postgresv1.PostgresCluster, host string, port int, user string) error {
	// Set observed generation
	postgresCluster.Status.ObservedGeneration = postgresCluster.Generation

	// Get condition statuses
	configValid := r.getConditionStatus(postgresCluster, "ConfigurationValid")
	connectionReady := r.getConditionStatus(postgresCluster, "ConnectionReady")

	// Determine overall Ready condition
	overallReady := configValid && connectionReady
	overallMessage := "PostgresCluster is ready"
	if !overallReady {
		var reasons []string
		if !configValid {
			reasons = append(reasons, "configuration invalid")
		}
		if !connectionReady {
			reasons = append(reasons, "connection not ready")
		}
		overallMessage = fmt.Sprintf("PostgresCluster not ready: %s", fmt.Sprint(reasons))
	}
	r.setCondition(postgresCluster, "Ready", overallReady, overallMessage)

	// Update phase based on conditions
	r.updatePhase(postgresCluster)

	// Update admin connection (without password for security)
	if overallReady {
		postgresCluster.Status.AdminConnection = &postgresv1.PostgresClusterAdminConnection{
			Host: host,
			Port: port,
			User: user,
			URL:  fmt.Sprintf("postgresql://%s@%s:%d/postgres", user, host, port),
		}
	}

	return r.Status().Update(ctx, postgresCluster)
}

// setCondition sets or updates a condition on the PostgresCluster.
func (r *PostgresClusterReconciler) setCondition(postgresCluster *postgresv1.PostgresCluster, conditionType string, status bool, message string) {
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
		ObservedGeneration: postgresCluster.Generation,
	}

	// Find existing condition
	for i, c := range postgresCluster.Status.Conditions {
		if c.Type == conditionType {
			// Only update if status or generation changed
			if c.Status != conditionStatus || c.ObservedGeneration != postgresCluster.Generation {
				postgresCluster.Status.Conditions[i] = condition
			} else {
				// Update message and time even if status hasn't changed
				postgresCluster.Status.Conditions[i].Message = message
				postgresCluster.Status.Conditions[i].LastTransitionTime = now
			}
			return
		}
	}

	// Condition doesn't exist, add it
	postgresCluster.Status.Conditions = append(postgresCluster.Status.Conditions, condition)
}

// updatePhase updates the phase based on the current conditions.
func (r *PostgresClusterReconciler) updatePhase(postgresCluster *postgresv1.PostgresCluster) {
	// Check if being deleted
	if !postgresCluster.DeletionTimestamp.IsZero() {
		postgresCluster.Status.Phase = postgresv1.PostgresClusterPhaseDeleting
		return
	}

	// Check Ready condition
	readyCondition := r.getCondition(postgresCluster, "Ready")
	if readyCondition == nil {
		postgresCluster.Status.Phase = postgresv1.PostgresClusterPhasePending
		return
	}

	switch readyCondition.Status {
	case metav1.ConditionTrue:
		postgresCluster.Status.Phase = postgresv1.PostgresClusterPhaseReady
	case metav1.ConditionFalse:
		// Check if there's a failure condition
		configCondition := r.getCondition(postgresCluster, "ConfigurationValid")
		connectionCondition := r.getCondition(postgresCluster, "ConnectionReady")
		if configCondition != nil && configCondition.Status == metav1.ConditionFalse {
			postgresCluster.Status.Phase = postgresv1.PostgresClusterPhaseFailed
		} else if connectionCondition != nil && connectionCondition.Status == metav1.ConditionFalse {
			postgresCluster.Status.Phase = postgresv1.PostgresClusterPhaseCreating
		} else {
			postgresCluster.Status.Phase = postgresv1.PostgresClusterPhaseCreating
		}
	default:
		postgresCluster.Status.Phase = postgresv1.PostgresClusterPhasePending
	}
}

// getCondition returns the condition with the given type, or nil if not found.
func (r *PostgresClusterReconciler) getCondition(postgresCluster *postgresv1.PostgresCluster, conditionType string) *metav1.Condition {
	for i := range postgresCluster.Status.Conditions {
		if postgresCluster.Status.Conditions[i].Type == conditionType {
			return &postgresCluster.Status.Conditions[i]
		}
	}
	return nil
}

// getConditionStatus returns true if the condition exists and is True, false otherwise.
func (r *PostgresClusterReconciler) getConditionStatus(postgresCluster *postgresv1.PostgresCluster, conditionType string) bool {
	condition := r.getCondition(postgresCluster, conditionType)
	return condition != nil && condition.Status == metav1.ConditionTrue
}

// SetupWithManager sets up the controller with the Manager.
func (r *PostgresClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&postgresv1.PostgresCluster{}).
		Named("postgrescluster").
		Complete(r)
}
