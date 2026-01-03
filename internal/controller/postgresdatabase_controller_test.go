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

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	postgresv1 "github.com/morten-olsen/homelab-postgres-operator/api/v1"
)

var _ = Describe("PostgresDatabase Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-database"
		const clusterName = "test-cluster"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			By("creating the custom resource for the Kind PostgresCluster")
			cluster := &postgresv1.PostgresCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName,
					Namespace: "default",
				},
				Spec: postgresv1.PostgresClusterSpec{
					Host: &postgresv1.StringOrSecret{
						Value: "localhost",
					},
					Port: 5432,
					User: &postgresv1.StringOrSecret{
						Value: "postgres",
					},
					Password: &postgresv1.StringOrSecret{
						Value: "testpassword",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			By("creating the custom resource for the Kind PostgresDatabase")
			resource := &postgresv1.PostgresDatabase{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: postgresv1.PostgresDatabaseSpec{
					ClusterRef: postgresv1.ClusterReference{
						Name: clusterName,
					},
					// DatabaseName and UserName are optional - will be computed as {namespace}_{name}
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(func() {
			By("deleting the custom resource for the Kind PostgresDatabase")
			resource := &postgresv1.PostgresDatabase{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("deleting the custom resource for the Kind PostgresCluster")
			cluster := &postgresv1.PostgresCluster{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: "default"}, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created PostgresCluster resource")
			clusterDB, clusterMock, err := sqlmock.New()
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				_ = clusterDB.Close() // Ignore close error in test cleanup
			}()
			clusterMock.ExpectPing()

			clusterReconciler := &PostgresClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				TestConnection: func(ctx context.Context, host string, port int, user, password string, log logr.Logger) (bool, string) {
					if pingErr := clusterDB.PingContext(ctx); pingErr != nil {
						return false, pingErr.Error()
					}
					return true, "Successfully connected to PostgreSQL"
				},
			}
			_, err = clusterReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: clusterName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Updating PostgresCluster status to Ready")
			cluster := &postgresv1.PostgresCluster{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: "default"}, cluster)).To(Succeed())
			cluster.Status.Conditions = []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "Ready",
					Message:            "PostgresCluster is ready",
					LastTransitionTime: metav1.Now(),
					ObservedGeneration: cluster.Generation,
				},
			}
			Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())
			Expect(clusterMock.ExpectationsWereMet()).Should(Succeed())

			By("Reconciling the created PostgresDatabase resource with a mock database")
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				_ = db.Close() // Ignore close error in test cleanup
			}()

			// Database name and username will be computed as {namespace}_{name} = "default_test-database"
			expectedDBName := "default_test-database"
			expectedUserName := "default_test-database"
			mock.ExpectExec(fmt.Sprintf(`CREATE DATABASE "%s";`, expectedDBName)).WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectExec(fmt.Sprintf(`CREATE USER "%s" WITH PASSWORD '.+';`, expectedUserName)).WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectExec(fmt.Sprintf(`GRANT ALL PRIVILEGES ON DATABASE "%s" TO "%s";`, expectedDBName, expectedUserName)).WillReturnResult(sqlmock.NewResult(1, 1))
			// Additional queries for schema privileges
			mock.ExpectPing() // Ping when connecting to the target database
			mock.ExpectExec(fmt.Sprintf(`GRANT ALL PRIVILEGES ON SCHEMA public TO "%s";`, expectedUserName)).WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectExec(fmt.Sprintf(`GRANT CREATE ON SCHEMA public TO "%s";`, expectedUserName)).WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectExec(fmt.Sprintf(`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO "%s";`, expectedUserName)).WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectExec(fmt.Sprintf(`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO "%s";`, expectedUserName)).WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectExec(fmt.Sprintf(`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON FUNCTIONS TO "%s";`, expectedUserName)).WillReturnResult(sqlmock.NewResult(1, 1))

			databaseReconciler := &PostgresDatabaseReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				DBConnectionFunc: func(connStr string) (*sql.DB, error) {
					return db, nil
				},
			}

			_, err = databaseReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mock.ExpectationsWereMet()).Should(Succeed())
		})
	})
})
