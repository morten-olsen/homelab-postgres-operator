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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// PostgresClusterSpec defines the desired state of PostgresCluster
type PostgresClusterSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// host is the hostname or IP address of the PostgreSQL server.
	// Either host or hostFrom must be specified.
	// +optional
	Host *StringOrSecret `json:"host,omitempty"`

	// port is the port number of the PostgreSQL server.
	// Defaults to 5432 if not specified.
	// +kubebuilder:default:=5432
	// +optional
	Port int `json:"port,omitempty"`

	// user is the username for connecting to the PostgreSQL server.
	// Either user or userFrom must be specified.
	// +optional
	User *StringOrSecret `json:"user,omitempty"`

	// password is the password for connecting to the PostgreSQL server.
	// Either password or passwordFrom must be specified.
	// +optional
	Password *StringOrSecret `json:"password,omitempty"`
}

// StringOrSecret represents a value that can either be a literal string or a reference to a secret.
// Exactly one of Value or ValueFrom must be specified.
type StringOrSecret struct {
	// value is a literal string value.
	// +optional
	Value string `json:"value,omitempty"`

	// valueFrom is a reference to a secret key.
	// +optional
	ValueFrom *SecretKeySelector `json:"valueFrom,omitempty"`
}

// SecretKeySelector selects a key from a secret.
type SecretKeySelector struct {
	// name is the name of the secret in the same namespace as the PostgresCluster.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// key is the key in the secret to select.
	// +kubebuilder:validation:Required
	Key string `json:"key"`

	// namespace is the namespace of the secret. If not specified, defaults to the PostgresCluster namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// PostgresClusterStatus defines the observed state of PostgresCluster.
type PostgresClusterStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// observedGeneration is the most recent generation observed for this PostgresCluster.
	// It corresponds to the PostgresCluster's generation, which is updated on mutation by the API server.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the latest available observations of the PostgresCluster's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// phase represents the current phase of the PostgresCluster.
	// +optional
	Phase PostgresClusterPhase `json:"phase,omitempty"`

	// adminConnection contains the connection details for the admin user.
	// Note: Password is not included in status for security reasons.
	// +optional
	AdminConnection *PostgresClusterAdminConnection `json:"adminConnection,omitempty"`
}

// PostgresClusterPhase represents the phase of a PostgresCluster.
// +kubebuilder:validation:Enum=Pending;Creating;Ready;Failed;Deleting
type PostgresClusterPhase string

const (
	// PostgresClusterPhasePending indicates that the PostgresCluster is pending creation.
	PostgresClusterPhasePending PostgresClusterPhase = "Pending"

	// PostgresClusterPhaseCreating indicates that the PostgresCluster is being created.
	PostgresClusterPhaseCreating PostgresClusterPhase = "Creating"

	// PostgresClusterPhaseReady indicates that the PostgresCluster is ready and operational.
	PostgresClusterPhaseReady PostgresClusterPhase = "Ready"

	// PostgresClusterPhaseFailed indicates that the PostgresCluster has failed.
	PostgresClusterPhaseFailed PostgresClusterPhase = "Failed"

	// PostgresClusterPhaseDeleting indicates that the PostgresCluster is being deleted.
	PostgresClusterPhaseDeleting PostgresClusterPhase = "Deleting"
)

// PostgresClusterAdminConnection defines the connection details for the admin user.
type PostgresClusterAdminConnection struct {
	// host is the hostname of the postgres cluster.
	Host string `json:"host"`

	// port is the port of the postgres cluster.
	Port int `json:"port"`

	// user is the username for the admin user.
	User string `json:"user"`

	// url is the connection url for the admin user (without password for security).
	URL string `json:"url"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// PostgresCluster is the Schema for the postgresclusters API
type PostgresCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of PostgresCluster
	// +required
	Spec PostgresClusterSpec `json:"spec"`

	// status defines the observed state of PostgresCluster
	// +optional
	Status PostgresClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PostgresClusterList contains a list of PostgresCluster
type PostgresClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PostgresCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PostgresCluster{}, &PostgresClusterList{})
}
