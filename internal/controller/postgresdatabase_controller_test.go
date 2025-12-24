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

	"github.com/DATA-DOG/go-sqlmock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
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
			clusterReconciler := &PostgresClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := clusterReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: clusterName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for the admin secret to be created")
			Eventually(func() bool {
				secret := &corev1.Secret{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName + "-admin-secret", Namespace: "default"}, secret)
				return err == nil
			}, time.Second*10, time.Millisecond*250).Should(BeTrue())

			By("Reconciling the created PostgresDatabase resource with a mock database")
			db, mock, err := sqlmock.New()
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				if closeErr := db.Close(); closeErr != nil {
					Fail(fmt.Sprintf("Failed to close database connection: %v", closeErr))
				}
			}()

			// Database name and username will be computed as {namespace}_{name} = "default_test-database"
			expectedDBName := "default_test-database"
			expectedUserName := "default_test-database"
			mock.ExpectExec(fmt.Sprintf("CREATE DATABASE \"%s\";", expectedDBName)).WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectExec(fmt.Sprintf("CREATE USER \"%s\" WITH PASSWORD '.+';", expectedUserName)).WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectExec(fmt.Sprintf("GRANT ALL PRIVILEGES ON DATABASE \"%s\" TO \"%s\";", expectedDBName, expectedUserName)).WillReturnResult(sqlmock.NewResult(1, 1))

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
