//go:build linux || darwin

/*
Copyright 2026 The Kubeflow authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sparkapplication_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kubeflow/spark-operator/api/v1beta2"
	"github.com/kubeflow/spark-operator/internal/controller/sparkapplication"
)

var _ = Describe("submission recovery after process death", Serial, func() {
	It("recovers the same driver after SIGKILL interrupts submission", func() {
		app := newRecoveryApplication(v1beta2.ApplicationStateNew)
		submit := newControlledSubmit(true)
		configPath := filepath.Join(GinkgoT().TempDir(), "kubeconfig")
		kubeconfig := clientcmdapi.Config{
			Clusters: map[string]*clientcmdapi.Cluster{"envtest": {
				Server:                   cfg.Host,
				CertificateAuthorityData: cfg.CAData,
			}},
			AuthInfos: map[string]*clientcmdapi.AuthInfo{"envtest": {
				ClientCertificateData: cfg.CertData,
				ClientKeyData:         cfg.KeyData,
			}},
			Contexts:       map[string]*clientcmdapi.Context{"envtest": {Cluster: "envtest", AuthInfo: "envtest"}},
			CurrentContext: "envtest",
		}
		Expect(clientcmd.WriteToFile(kubeconfig, configPath)).To(Succeed())
		executable, err := os.Executable()
		Expect(err).NotTo(HaveOccurred())
		cmd := exec.Command(executable, "-test.run=^TestSubmissionCrashHelper$")
		cmd.Env = append(os.Environ(),
			"SPARK_OPERATOR_CRASH_HELPER=1",
			"SPARK_OPERATOR_TEST_KUBECONFIG="+configPath,
			"SPARK_OPERATOR_TEST_APP="+app.Name,
			"SPARK_OPERATOR_TEST_NAMESPACE="+app.Namespace,
		)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdout = GinkgoWriter
		cmd.Stderr = GinkgoWriter
		Expect(cmd.Start()).To(Succeed())
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		stopped := false
		DeferCleanup(func() {
			if !stopped {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				Eventually(done, 10*time.Second).Should(Receive())
			}
		})
		submit.waitForInvocation()
		driver := createRecoveryDriver(app, submit)
		Expect(getRecoveryApplication(app).Status.SubmissionID).To(Equal(submit.submissionID()))

		// Kill the whole operator process group, including its blocked spark-submit.
		Expect(syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)).To(Succeed())
		var processError error
		Eventually(done, 10*time.Second).Should(Receive(&processError))
		stopped = true
		Expect(processError).To(HaveOccurred())
		Expect(getRecoveryApplication(app).Status.AppState.State).To(Equal(v1beta2.ApplicationStateNew))

		_, err = newRecoveryReconciler(k8sClient).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})
		Expect(err).NotTo(HaveOccurred())
		latest := getRecoveryApplication(app)
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateRunning))
		Expect(latest.Status.SubmissionID).To(Equal(submit.submissionID()))
		current := &corev1.Pod{}
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(driver), current)).To(Succeed())
		Expect(current.UID).To(Equal(driver.UID))
		Expect(current.DeletionTimestamp.IsZero()).To(BeTrue())
		Expect(submit.invocations()).To(Equal(1))
	})
})

func TestSubmissionCrashHelper(t *testing.T) {
	if os.Getenv("SPARK_OPERATOR_CRASH_HELPER") != "1" {
		return
	}
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("SPARK_OPERATOR_TEST_KUBECONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1beta2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	namespace := os.Getenv("SPARK_OPERATOR_TEST_NAMESPACE")
	r := sparkapplication.NewReconciler(nil, scheme, c, record.NewFakeRecorder(100), nil, sparkapplication.Options{
		APIReader: c, Namespaces: []string{namespace}, SparkSubmitTimeout: 20 * time.Second,
	})
	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{
		Namespace: namespace,
		Name:      os.Getenv("SPARK_OPERATOR_TEST_APP"),
	}})
	if err != nil {
		t.Fatal(err)
	}
}
