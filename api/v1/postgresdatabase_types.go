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

// PostgresDatabaseSpec defines the desired state of PostgresDatabase
type PostgresDatabaseSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// ClusterRef is a reference to the PostgresCluster that this database belongs to.
	ClusterRef ClusterReference `json:"clusterRef"`

	// DatabaseName is the name of the database to create.
	// If not specified, it will be automatically set to {namespace}_{name}.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	DatabaseName string `json:"databaseName,omitempty"`

	// UserName is the name of the user (role) to create for this database.
	// If not specified, it will be automatically set to {namespace}_{name}.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	UserName string `json:"userName,omitempty"`

	// ReclaimPolicy defines what happens to the database and user when this resource is deleted.
	// Can be "Retain" (default) or "Delete".
	// +kubebuilder:default:="Retain"
	// +kubebuilder:validation:Enum=Retain;Delete
	// +optional
	ReclaimPolicy string `json:"reclaimPolicy,omitempty"`
}

// ClusterReference contains enough information to let you locate the
// PostgresCluster this database should be created in.
type ClusterReference struct {
	// Name of the PostgresCluster.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace of the PostgresCluster. If empty, defaults to the current namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// PostgresDatabaseStatus defines the observed state of PostgresDatabase.
type PostgresDatabaseStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// observedGeneration is the most recent generation observed for this PostgresDatabase.
	// It corresponds to the PostgresDatabase's generation, which is updated on mutation by the API server.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the latest available observations of the PostgresDatabase's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// phase represents the current phase of the PostgresDatabase.
	// +optional
	Phase PostgresDatabasePhase `json:"phase,omitempty"`

	// connection contains the connection details for the created database and user.
	// Note: Password is not included in status for security reasons.
	// +optional
	Connection *PostgresDatabaseConnection `json:"connection,omitempty"`
}

// PostgresDatabasePhase represents the phase of a PostgresDatabase.
// +kubebuilder:validation:Enum=Pending;Creating;Ready;Failed;Deleting
type PostgresDatabasePhase string

const (
	// PostgresDatabasePhasePending indicates that the PostgresDatabase is pending creation.
	PostgresDatabasePhasePending PostgresDatabasePhase = "Pending"

	// PostgresDatabasePhaseCreating indicates that the PostgresDatabase is being created.
	PostgresDatabasePhaseCreating PostgresDatabasePhase = "Creating"

	// PostgresDatabasePhaseReady indicates that the PostgresDatabase is ready and operational.
	PostgresDatabasePhaseReady PostgresDatabasePhase = "Ready"

	// PostgresDatabasePhaseFailed indicates that the PostgresDatabase has failed.
	PostgresDatabasePhaseFailed PostgresDatabasePhase = "Failed"

	// PostgresDatabasePhaseDeleting indicates that the PostgresDatabase is being deleted.
	PostgresDatabasePhaseDeleting PostgresDatabasePhase = "Deleting"
)

// PostgresDatabaseConnection defines the connection details for a specific database and user.
type PostgresDatabaseConnection struct {
	// host is the hostname of the postgres cluster.
	Host string `json:"host"`

	// port is the port of the postgres cluster.
	Port int `json:"port"`

	// database is the name of the created database.
	Database string `json:"database"`

	// user is the username for the created user.
	User string `json:"user"`

	// url is the connection url for the created user and database (without password for security).
	URL string `json:"url"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// PostgresDatabase is the Schema for the postgresdatabases API
type PostgresDatabase struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of PostgresDatabase
	// +required
	Spec PostgresDatabaseSpec `json:"spec"`

	// status defines the observed state of PostgresDatabase
	// +optional
	Status PostgresDatabaseStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PostgresDatabaseList contains a list of PostgresDatabase
type PostgresDatabaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PostgresDatabase `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PostgresDatabase{}, &PostgresDatabaseList{})
}
