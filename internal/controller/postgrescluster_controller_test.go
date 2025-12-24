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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	postgresv1 "github.com/morten-olsen/homelab-postgres-operator/api/v1"
)

var _ = Describe("PostgresCluster Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		postgrescluster := &postgresv1.PostgresCluster{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind PostgresCluster")
			err := k8sClient.Get(ctx, typeNamespacedName, postgrescluster)
			if err != nil && errors.IsNotFound(err) {
				resource := &postgresv1.PostgresCluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					// TODO(user): Specify other spec details if needed.
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &postgresv1.PostgresCluster{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance PostgresCluster")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &PostgresClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking if the StatefulSet was created")
			var statefulSet *appsv1.StatefulSet
			Eventually(func() bool {
				statefulSet = &appsv1.StatefulSet{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-statefulset", Namespace: "default"}, statefulSet)
				return err == nil
			}, time.Second*10, time.Millisecond*250).Should(BeTrue())

			By("Updating StatefulSet status to have ready replicas")
			replicas := int32(1)
			statefulSet.Status.ReadyReplicas = replicas
			statefulSet.Status.Replicas = replicas
			Expect(k8sClient.Status().Update(ctx, statefulSet)).To(Succeed())

			By("Checking if the Service was created")
			Eventually(func() bool {
				service := &corev1.Service{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-service", Namespace: "default"}, service)
				return err == nil
			}, time.Second*10, time.Millisecond*250).Should(BeTrue())

			By("Checking if the admin Secret was created")
			Eventually(func() bool {
				secret := &corev1.Secret{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-admin-secret", Namespace: "default"}, secret)
				if err != nil {
					return false
				}
				// Check for password key
				_, ok := secret.Data["password"]
				return ok
			}, time.Second*10, time.Millisecond*250).Should(BeTrue())

			By("Reconciling again to update status with ready StatefulSet")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking if the PostgresCluster status was updated")
			Eventually(func() bool {
				updatedPostgresCluster := &postgresv1.PostgresCluster{}
				err := k8sClient.Get(ctx, typeNamespacedName, updatedPostgresCluster)
				if err != nil {
					return false
				}
				return updatedPostgresCluster.Status.AdminConnection != nil
			}, time.Second*10, time.Millisecond*250).Should(BeTrue())
		})
	})
})
