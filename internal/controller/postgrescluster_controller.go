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
	"fmt" // Import fmt for string formatting

	appsv1 "k8s.io/api/apps/v1" // Import for appsv1.StatefulSet
	corev1 "k8s.io/api/core/v1" // Import for corev1.Secret
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource" // Import for resource.MustParse
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types" // Import for types.NamespacedName
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil" // Import for controllerutil.SetControllerReference
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	postgresv1 "github.com/morten-olsen/homelab-postgres-operator/api/v1"
)

// PostgresClusterReconciler reconciles a PostgresCluster object
type PostgresClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=postgres.homelab.mortenolsen.pro,resources=postgresclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgres.homelab.mortenolsen.pro,resources=postgresclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=postgres.homelab.mortenolsen.pro,resources=postgresclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the PostgresCluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
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

	// Manage the administrative secret
	adminSecretName := fmt.Sprintf("%s-admin-secret", postgresCluster.Name)
	adminSecret := &corev1.Secret{}
	password := ""

	err = r.Get(ctx, types.NamespacedName{Name: adminSecretName, Namespace: postgresCluster.Namespace}, adminSecret)
	if err != nil && errors.IsNotFound(err) {
		log.Info("Creating admin secret", "Secret.Namespace", postgresCluster.Namespace, "Secret.Name", adminSecretName)

		generatedPassword, err := generateRandomPassword(32)
		if err != nil {
			log.Error(err, "Failed to generate random password")
			return ctrl.Result{}, err
		}
		password = generatedPassword

		adminSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      adminSecretName,
				Namespace: postgresCluster.Namespace,
			},
			Data: map[string][]byte{
				"password": []byte(password),
			},
		}

		if err := controllerutil.SetControllerReference(postgresCluster, adminSecret, r.Scheme); err != nil {
			log.Error(err, "Failed to set controller reference for admin secret")
			return ctrl.Result{}, err
		}

		if err := r.Create(ctx, adminSecret); err != nil {
			log.Error(err, "Failed to create admin secret")
			return ctrl.Result{}, err
		}
		log.Info("Admin secret created successfully")
	} else if err != nil {
		log.Error(err, "Failed to get admin secret")
		return ctrl.Result{}, err
	} else {
		// Secret already exists, retrieve password
		log.Info("Admin secret already exists", "Secret.Namespace", postgresCluster.Namespace, "Secret.Name", adminSecretName)
		password = string(adminSecret.Data["password"])
	}

	// Define labels for the StatefulSet and Service
	labels := map[string]string{
		"app":        "postgres",
		"postgrescr": postgresCluster.Name,
	}

	// Manage the StatefulSet for PostgreSQL
	statefulSetName := fmt.Sprintf("%s-statefulset", postgresCluster.Name)
	statefulSet := &appsv1.StatefulSet{}

	err = r.Get(ctx, types.NamespacedName{Name: statefulSetName, Namespace: postgresCluster.Namespace}, statefulSet)
	if err != nil && errors.IsNotFound(err) {
		log.Info("Creating StatefulSet", "StatefulSet.Namespace", postgresCluster.Namespace, "StatefulSet.Name", statefulSetName)

		statefulSet = &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      statefulSetName,
				Namespace: postgresCluster.Namespace,
				Labels:    labels,
			},
			Spec: appsv1.StatefulSetSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: labels,
				},
				ServiceName: fmt.Sprintf("%s-service", postgresCluster.Name), // Headless service name
				Replicas:    func(i int32) *int32 { return &i }(1),           // Single replica for now
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: labels,
					},
					Spec: corev1.PodSpec{
						SecurityContext: &corev1.PodSecurityContext{
							RunAsNonRoot: func(b bool) *bool { return &b }(true),
							RunAsUser:    func(i int64) *int64 { return &i }(999), // Set at pod level to ensure volume ownership
							FSGroup:      func(i int64) *int64 { return &i }(999), // Ensure volumes are owned by GID 999
							SeccompProfile: &corev1.SeccompProfile{
								Type: corev1.SeccompProfileTypeRuntimeDefault,
							},
						},
						InitContainers: []corev1.Container{
							{
								Name:  "init-data-dir",
								Image: "busybox:1.36",
								SecurityContext: &corev1.SecurityContext{
									AllowPrivilegeEscalation: func(b bool) *bool { return &b }(false),
									Capabilities: &corev1.Capabilities{
										Drop: []corev1.Capability{"ALL"},
									},
									RunAsNonRoot: func(b bool) *bool { return &b }(true),
									SeccompProfile: &corev1.SeccompProfile{
										Type: corev1.SeccompProfileTypeRuntimeDefault,
									},
								},
								Command: []string{"sh", "-c", `
									# Ensure the PostgreSQL directory exists (PostgreSQL 18+ will create version-specific subdirectory)
									# If directory doesn't exist, create it (will be owned by 999:999 due to RunAsUser and FSGroup)
									if [ ! -d /var/lib/postgresql ]; then
										mkdir -p /var/lib/postgresql
									fi
									# Ensure we can write to it (should already be writable due to fsGroup)
									touch /var/lib/postgresql/.init-ready 2>/dev/null || true
									rm -f /var/lib/postgresql/.init-ready 2>/dev/null || true
								`},
								VolumeMounts: []corev1.VolumeMount{
									{
										Name:      "data",
										MountPath: "/var/lib/postgresql",
									},
								},
							},
						},
						Containers: []corev1.Container{
							{
								Name:  "postgres",
								Image: postgresCluster.Spec.Image,
								SecurityContext: &corev1.SecurityContext{
									AllowPrivilegeEscalation: func(b bool) *bool { return &b }(false),
									Capabilities: &corev1.Capabilities{
										Drop: []corev1.Capability{"ALL"},
									},
									RunAsNonRoot: func(b bool) *bool { return &b }(true),
									SeccompProfile: &corev1.SeccompProfile{
										Type: corev1.SeccompProfileTypeRuntimeDefault,
									},
								},
								Ports: []corev1.ContainerPort{
									{
										ContainerPort: 5432,
										Name:          "postgres",
									},
								},
								Env: []corev1.EnvVar{
									{
										Name:  "POSTGRES_USER",
										Value: "postgres",
									},
									{
										Name: "POSTGRES_PASSWORD",
										ValueFrom: &corev1.EnvVarSource{
											SecretKeyRef: &corev1.SecretKeySelector{
												LocalObjectReference: corev1.LocalObjectReference{
													Name: adminSecretName,
												},
												Key: "password",
											},
										},
									},
									{
										Name:  "POSTGRES_DB",
										Value: "postgres",
									},
								},
								VolumeMounts: []corev1.VolumeMount{
									{
										Name:      "data",
										MountPath: "/var/lib/postgresql",
									},
								},
							},
						},
					},
				},
				VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "data",
						},
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
							Resources: corev1.VolumeResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceStorage: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
			},
		}

		if err := controllerutil.SetControllerReference(postgresCluster, statefulSet, r.Scheme); err != nil {
			log.Error(err, "Failed to set controller reference for StatefulSet")
			return ctrl.Result{}, err
		}

		if err := r.Create(ctx, statefulSet); err != nil {
			log.Error(err, "Failed to create StatefulSet")
			return ctrl.Result{}, err
		}
		log.Info("StatefulSet created successfully")
	} else if err != nil {
		log.Error(err, "Failed to get StatefulSet")
		return ctrl.Result{}, err
	} else {
		// StatefulSet already exists, check for updates (e.g., image change)
		log.Info("StatefulSet already exists", "StatefulSet.Namespace", postgresCluster.Namespace, "StatefulSet.Name", statefulSetName)
		// TODO: Implement update logic here if needed
	}

	// Manage the Service for PostgreSQL
	serviceName := fmt.Sprintf("%s-service", postgresCluster.Name)
	service := &corev1.Service{}

	err = r.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: postgresCluster.Namespace}, service)
	if err != nil && errors.IsNotFound(err) {
		log.Info("Creating Service", "Service.Namespace", postgresCluster.Namespace, "Service.Name", serviceName)

		service = &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: postgresCluster.Namespace,
				Labels:    labels,
			},
			Spec: corev1.ServiceSpec{
				Selector: labels,
				Ports: []corev1.ServicePort{
					{
						Port: 5432,
						Name: "postgres",
					},
				},
				ClusterIP: corev1.ClusterIPNone, // Headless service
			},
		}

		if err := controllerutil.SetControllerReference(postgresCluster, service, r.Scheme); err != nil {
			log.Error(err, "Failed to set controller reference for Service")
			return ctrl.Result{}, err
		}

		if err := r.Create(ctx, service); err != nil {
			log.Error(err, "Failed to create Service")
			return ctrl.Result{}, err
		}
		log.Info("Service created successfully")
	} else if err != nil {
		log.Error(err, "Failed to get Service")
		return ctrl.Result{}, err
	} else {
		log.Info("Service already exists", "Service.Namespace", postgresCluster.Namespace, "Service.Name", serviceName)
		// TODO: Implement update logic here if needed
	}

	// Update PostgresCluster status
	postgresCluster.Status.AdminConnection = &postgresv1.PostgresClusterAdminConnection{
		Host:     fmt.Sprintf("%s-service", postgresCluster.Name), // Assuming a service will be named this
		Port:     5432,                                            // Default PostgreSQL port
		User:     "postgres",                                      // Default admin user
		Password: password,
		URL:      fmt.Sprintf("postgresql://postgres:%s@%s-service:5432/postgres", password, postgresCluster.Name),
	}
	if err := r.Status().Update(ctx, postgresCluster); err != nil {
		log.Error(err, "Failed to update PostgresCluster status")
		return ctrl.Result{}, err
	}

	log.Info("PostgresCluster status updated successfully", "PostgresCluster.Name", postgresCluster.Name)

	return ctrl.Result{}, nil

}

// SetupWithManager sets up the controller with the Manager.
func (r *PostgresClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&postgresv1.PostgresCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Named("postgrescluster").
		Complete(r)
}
