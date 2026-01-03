//go:build e2e
// +build e2e

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

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/morten-olsen/homelab-postgres-operator/test/utils"
)

// namespace where the project is deployed in
const namespace = "postgres-operator-system"

// serviceAccountName created for the project
const serviceAccountName = "postgres-operator-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "postgres-operator-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "postgres-operator-metrics-binding"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deleting old rbac resources")
		cmd = exec.Command("kubectl", "delete", "clusterrole", "manager-role", "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "clusterrolebinding", "manager-rolebinding", "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("cleaning up the metrics role binding")
		cmd = exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=postgres-operator-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": ["curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics"],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks

		It("should create a PostgresCluster and a PostgresDatabase", func() {
			const clusterName = "e2e-test-cluster"
			const dbName = "e2e-test-db"
			const userName = "e2e-test-user"
			const dbResourceName = "e2e-test-database"
			const postgresServerName = "e2e-postgres-server"
			const postgresPassword = "e2e-test-password"

			By("deploying a PostgreSQL server for testing")
			// Create a secret for PostgreSQL password
			secret := fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata:
  name: %s-secret
  namespace: %s
type: Opaque
stringData:
  password: %s
`, postgresServerName, namespace, postgresPassword)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(secret)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create PostgreSQL secret")

			// Deploy PostgreSQL StatefulSet
			postgresStatefulSet := fmt.Sprintf(`
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: %s
  namespace: %s
spec:
  serviceName: %s
  replicas: 1
  selector:
    matchLabels:
      app: postgres
      name: %s
  template:
    metadata:
      labels:
        app: postgres
        name: %s
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 999
        fsGroup: 999
        seccompProfile:
          type: RuntimeDefault
      containers:
      - name: postgres
        image: postgres:16
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop: ["ALL"]
          runAsNonRoot: true
          seccompProfile:
            type: RuntimeDefault
        ports:
        - containerPort: 5432
          name: postgres
        env:
        - name: POSTGRES_USER
          value: postgres
        - name: POSTGRES_PASSWORD
          valueFrom:
            secretKeyRef:
              name: %s-secret
              key: password
        - name: POSTGRES_DB
          value: postgres
        volumeMounts:
        - name: data
          mountPath: /var/lib/postgresql/data
  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: ["ReadWriteOnce"]
      resources:
        requests:
          storage: 1Gi
`, postgresServerName, namespace, postgresServerName, postgresServerName, postgresServerName, postgresServerName)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(postgresStatefulSet)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create PostgreSQL StatefulSet")

			// Create Service for PostgreSQL
			postgresService := fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
spec:
  selector:
    app: postgres
    name: %s
  ports:
  - port: 5432
    targetPort: 5432
    name: postgres
  clusterIP: None
`, postgresServerName, namespace, postgresServerName)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(postgresService)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create PostgreSQL Service")

			By("waiting for PostgreSQL server to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulset", postgresServerName, "-n", namespace, "-o", "jsonpath={.status.readyReplicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"), "PostgreSQL StatefulSet should have 1 ready replica")
			}, "5m", "5s").Should(Succeed())

			By("creating a PostgresCluster resource with connection details")
			cluster := fmt.Sprintf(`
apiVersion: postgres.homelab.mortenolsen.pro/v1
kind: PostgresCluster
metadata:
  name: %s
  namespace: %s
spec:
  host:
    value: %s.%s.svc.cluster.local
  port: 5432
  user:
    value: postgres
  password:
    valueFrom:
      name: %s-secret
      key: password
`, clusterName, namespace, postgresServerName, namespace, postgresServerName)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cluster)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the PostgresCluster to be Ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "postgrescluster", clusterName, "-n", namespace, "-o", "json")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				var cluster struct {
					Status struct {
						Phase      string `json:"phase"`
						Conditions []struct {
							Type    string `json:"type"`
							Status  string `json:"status"`
							Message string `json:"message"`
						} `json:"conditions"`
					} `json:"status"`
				}
				err = json.Unmarshal([]byte(output), &cluster)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cluster.Status.Phase).To(Equal("Ready"), "PostgresCluster should be in Ready phase")

				// Check Ready condition
				readyFound := false
				for _, condition := range cluster.Status.Conditions {
					if condition.Type == "Ready" {
						readyFound = true
						g.Expect(condition.Status).To(Equal("True"), fmt.Sprintf("Ready condition should be True, but got: %s", condition.Message))
						break
					}
				}
				g.Expect(readyFound).To(BeTrue(), "Ready condition should exist")
			}, "5m", "5s").Should(Succeed())

			By("creating a PostgresDatabase resource")
			database := fmt.Sprintf(`
apiVersion: postgres.homelab.mortenolsen.pro/v1
kind: PostgresDatabase
metadata:
  name: %s
  namespace: %s
spec:
  clusterRef:
    name: %s
  databaseName: %s
  userName: %s
`, dbResourceName, namespace, clusterName, dbName, userName)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(database)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the PostgresDatabase to be Ready")
			var dbStatus struct {
				Phase      string `json:"phase"`
				Conditions []struct {
					Type    string `json:"type"`
					Status  string `json:"status"`
					Message string `json:"message"`
				} `json:"conditions"`
				Connection struct {
					Host     string `json:"host"`
					Port     int    `json:"port"`
					Database string `json:"database"`
					User     string `json:"user"`
					URL      string `json:"url"`
				} `json:"connection"`
			}
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "postgresdatabase", dbResourceName, "-n", namespace, "-o", "json")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				var db struct {
					Status struct {
						Phase      string `json:"phase"`
						Conditions []struct {
							Type    string `json:"type"`
							Status  string `json:"status"`
							Message string `json:"message"`
						} `json:"conditions"`
						Connection struct {
							Host     string `json:"host"`
							Port     int    `json:"port"`
							Database string `json:"database"`
							User     string `json:"user"`
							URL      string `json:"url"`
						} `json:"connection"`
					} `json:"status"`
				}
				err = json.Unmarshal([]byte(output), &db)
				g.Expect(err).NotTo(HaveOccurred())

				// Check phase
				g.Expect(db.Status.Phase).To(Equal("Ready"), "PostgresDatabase should be in Ready phase")

				// Check Ready condition
				readyFound := false
				var readyMessage string
				for _, condition := range db.Status.Conditions {
					if condition.Type == "Ready" {
						readyFound = true
						g.Expect(condition.Status).To(Equal("True"), fmt.Sprintf("Ready condition should be True, but got: %s", condition.Message))
						readyMessage = condition.Message
						break
					}
				}
				g.Expect(readyFound).To(BeTrue(), "Ready condition should exist")
				g.Expect(readyMessage).To(Equal("PostgresDatabase is ready"), "Ready condition message should indicate readiness")

				// Check for any failed conditions
				for _, condition := range db.Status.Conditions {
					if condition.Status == "False" && condition.Type != "Ready" {
						g.Expect(condition.Status).To(Equal("True"), fmt.Sprintf("Condition %s should not be False: %s", condition.Type, condition.Message))
					}
				}

				// Verify connection info is set
				g.Expect(db.Status.Connection.Host).NotTo(BeEmpty(), "Connection host should be set")
				g.Expect(db.Status.Connection.Database).To(Equal(dbName), "Connection database should match")
				g.Expect(db.Status.Connection.User).To(Equal(userName), "Connection user should match")
				g.Expect(db.Status.Connection.Port).To(Equal(5432), "Connection port should be 5432")

				dbStatus = db.Status
			}, "5m", "5s").Should(Succeed())

			By("verifying the connection secret was created")
			var secretData map[string][]byte
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "secret", fmt.Sprintf("%s-connection", dbResourceName), "-n", namespace, "-o", "json")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				var secret struct {
					Data map[string][]byte `json:"data"`
				}
				err = json.Unmarshal([]byte(output), &secret)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(secret.Data).Should(HaveKey("host"))
				g.Expect(secret.Data).Should(HaveKey("port"))
				g.Expect(secret.Data).Should(HaveKey("database"))
				g.Expect(secret.Data).Should(HaveKey("user"))
				g.Expect(secret.Data).Should(HaveKey("password"))
				g.Expect(secret.Data).Should(HaveKey("url"))
				secretData = secret.Data
			}, "1m", "2s").Should(Succeed())

			By("verifying the database and user were created correctly")
			Expect(string(secretData["database"])).To(Equal(dbName))
			Expect(string(secretData["user"])).To(Equal(userName))
			Expect(dbStatus.Connection.Database).To(Equal(dbName))
			Expect(dbStatus.Connection.User).To(Equal(userName))

			By("cleaning up PostgreSQL server resources")
			cmd = exec.Command("kubectl", "delete", "statefulset", postgresServerName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "service", postgresServerName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "secret", fmt.Sprintf("%s-secret", postgresServerName), "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
	})
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
