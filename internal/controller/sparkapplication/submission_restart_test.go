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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kubeflow/spark-operator/api/v1beta2"
	"github.com/kubeflow/spark-operator/internal/controller/sparkapplication"
	"github.com/kubeflow/spark-operator/pkg/common"
	"github.com/kubeflow/spark-operator/pkg/util"
)

var _ = Describe("submission recovery with a manager cache", Serial, func() {
	It("applies a watched CPU change and recovers the replacement driver after manager restart", func() {
		ctx := context.Background()
		app := newRecoveryApplication(v1beta2.ApplicationStateNew)
		app.Spec.Driver.CoreLimit = util.StringPtr("2")
		Expect(k8sClient.Update(ctx, app)).To(Succeed())
		submit := newControlledSubmit(true)
		body := `#!/bin/sh
set -eu
printf 'call\n' >> "$SPARK_HOME/calls"
call=$(wc -l < "$SPARK_HOME/calls" | tr -d ' ')
dir="$SPARK_HOME/$call"
mkdir "$dir"
printf '%s\n' "$@" > "$dir/args"
touch "$dir/entered"
while [ ! -f "$dir/release" ] && [ ! -f "$SPARK_HOME/release-all" ]; do sleep 0.01; done
`
		Expect(os.WriteFile(filepath.Join(submit.dir, "bin", "spark-submit"), []byte(body), 0o755)).To(Succeed())
		var manager *recoveryManager
		DeferCleanup(func() {
			if manager != nil {
				manager.cancel()
				Expect(os.WriteFile(filepath.Join(submit.dir, "release-all"), nil, 0o644)).To(Succeed())
				Eventually(manager.done, 10*time.Second).Should(Receive(Succeed()))
			}
			pod := &corev1.Pod{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: app.Namespace, Name: util.GetDriverPodName(app)}, pod); err == nil {
				pod.Finalizers = nil
				Expect(k8sClient.Update(ctx, pod)).To(Succeed())
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)))).To(Succeed())
			} else {
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}
		})

		manager = startRecoveryManager(app)
		first := &controlledSubmit{dir: filepath.Join(submit.dir, "1")}
		first.waitForInvocation()
		Expect(getRecoveryApplication(app).Status.SubmissionID).To(Equal(first.submissionID()))
		oldDriver := createManagerDriver(app, first, true)

		By("changing metadata and CPU while the first submission is still running")
		latest := getRecoveryApplication(app)
		latest.Annotations = map[string]string{"test.sparkoperator.k8s.io/metadata": "changed"}
		Expect(k8sClient.Update(ctx, latest)).To(Succeed())
		latest = getRecoveryApplication(app)
		latest.Spec.Driver.CoreLimit = util.StringPtr("4")
		Expect(k8sClient.Update(ctx, latest)).To(Succeed())
		Eventually(func() v1beta2.ApplicationStateType {
			return getRecoveryApplication(app).Status.AppState.State
		}, 10*time.Second).Should(Equal(v1beta2.ApplicationStateInvalidating))
		first.release()

		By("waiting for the old driver to disappear before recording the replacement")
		Eventually(func() bool {
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(oldDriver), pod)).To(Succeed())
			return !pod.DeletionTimestamp.IsZero()
		}, 10*time.Second).Should(BeTrue())
		Eventually(func() v1beta2.ApplicationStateType {
			return getRecoveryApplication(app).Status.AppState.State
		}, 10*time.Second).Should(Equal(v1beta2.ApplicationStatePendingRerun))
		latest = getRecoveryApplication(app)
		Expect(latest.Status.SubmissionID).To(Equal(first.submissionID()))
		Expect(latest.Status.LastSubmissionAttemptTime.IsZero()).To(BeTrue())
		Expect(submit.invocations()).To(Equal(1))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(oldDriver), oldDriver)).To(Succeed())
		oldDriver.Finalizers = nil
		Expect(k8sClient.Update(ctx, oldDriver)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(oldDriver), &corev1.Pod{}))
		}, 10*time.Second).Should(BeTrue())

		second := &controlledSubmit{dir: filepath.Join(submit.dir, "2")}
		second.waitForInvocation()
		Expect(second.submissionID()).NotTo(Equal(first.submissionID()))
		Expect(second.conf(common.SparkKubernetesDriverLimitCores)).To(Equal("4"))
		Expect(getRecoveryApplication(app).Status.SubmissionID).To(Equal(second.submissionID()))
		driver := createManagerDriver(app, second, false)

		By("cancelling the manager before the replacement submission saves its outcome")
		manager.cancel()
		second.release()
		Eventually(manager.done, 10*time.Second).Should(Receive(Succeed()))
		manager = nil
		latest = getRecoveryApplication(app)
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStatePendingRerun))
		Expect(latest.Status.SubmissionID).To(Equal(second.submissionID()))

		By("recovering the same driver through a fresh manager and informer cache")
		manager = startRecoveryManager(app)
		Eventually(func() v1beta2.ApplicationStateType {
			return getRecoveryApplication(app).Status.AppState.State
		}, 10*time.Second).Should(Equal(v1beta2.ApplicationStateRunning))
		latest = getRecoveryApplication(app)
		Expect(latest.Status.SubmissionID).To(Equal(second.submissionID()))
		Expect(latest.Spec.Driver.CoreLimit).To(Equal(util.StringPtr("4")))
		Expect(latest.Annotations).To(HaveKeyWithValue("test.sparkoperator.k8s.io/metadata", "changed"))
		current := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(driver), current)).To(Succeed())
		Expect(current.UID).To(Equal(driver.UID))
		Expect(current.DeletionTimestamp.IsZero()).To(BeTrue())
		Expect(submit.invocations()).To(Equal(2))
	})
})

// recoveryManager exposes shutdown completion so tests restart the controller only
// after the preceding manager and its reconcile workers have stopped.
type recoveryManager struct {
	cancel context.CancelFunc
	done   chan error
}

// startRecoveryManager exercises informer delivery and the controller workqueue
// against envtest, restricting watches to one application to isolate each restart.
func startRecoveryManager(app *v1beta2.SparkApplication) *recoveryManager {
	shutdown := 5 * time.Second
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: k8sClient.Scheme(),
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{app.Namespace: {}},
			ByObject: map[client.Object]cache.ByObject{
				&v1beta2.SparkApplication{}: {Field: fields.OneTermEqualSelector("metadata.name", app.Name)},
				&corev1.Pod{}:               {Label: labels.SelectorFromSet(labels.Set{common.LabelSparkAppName: app.Name})},
			},
		},
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:  "0",
		GracefulShutdownTimeout: &shutdown,
	})
	Expect(err).NotTo(HaveOccurred())
	r := sparkapplication.NewReconciler(mgr, mgr.GetScheme(), mgr.GetClient(), record.NewFakeRecorder(100), nil, sparkapplication.Options{
		APIReader:                    mgr.GetAPIReader(),
		Namespaces:                   []string{app.Namespace},
		DriverPodCreationGracePeriod: time.Second,
		SubmissionTimeout:            20 * time.Second,
	})
	Expect(r.SetupWithManager(mgr, controller.Options{MaxConcurrentReconciles: 1})).To(Succeed())
	ctx, cancel := context.WithCancel(context.Background())
	manager := &recoveryManager{cancel: cancel, done: make(chan error, 1)}
	go func() { manager.done <- mgr.Start(ctx) }()
	cacheCtx, cacheCancel := context.WithTimeout(ctx, 10*time.Second)
	defer cacheCancel()
	Expect(mgr.GetCache().WaitForCacheSync(cacheCtx)).To(BeTrue())
	return manager
}

// createManagerDriver emulates a running driver with the process's recorded identity.
// holdDeletion adds a finalizer so the test can observe cleanup waiting across restart.
func createManagerDriver(app *v1beta2.SparkApplication, submit *controlledSubmit, holdDeletion bool) *corev1.Pod {
	pod := createDriverPod(app.Name, app.Namespace)
	pod.Labels[common.LabelSubmissionID] = submit.submissionID()
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(app, v1beta2.SchemeGroupVersion.WithKind("SparkApplication"))}
	pod.Annotations = make(map[string]string)
	for _, key := range []string{common.AnnotationAppUID, common.AnnotationAppGeneration} {
		pod.Annotations[key] = submit.conf(fmt.Sprintf(common.SparkKubernetesDriverAnnotationTemplate, key))
	}
	if holdDeletion {
		pod.Finalizers = []string{"test.sparkoperator.k8s.io/hold-driver"}
	}
	pod.Spec.TerminationGracePeriodSeconds = util.Int64Ptr(0)
	Expect(k8sClient.Create(context.Background(), pod)).To(Succeed())
	pod.Status.Phase = corev1.PodRunning
	Expect(k8sClient.Status().Update(context.Background(), pod)).To(Succeed())
	return pod
}

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

// TestSubmissionCrashHelper is the child-process entry point killed by the crash
// scenario; ordinary test runs leave it inactive unless the helper environment is set.
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
		APIReader: c, Namespaces: []string{namespace}, SubmissionTimeout: 20 * time.Second,
	})
	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{
		Namespace: namespace,
		Name:      os.Getenv("SPARK_OPERATOR_TEST_APP"),
	}})
	if err != nil {
		t.Fatal(err)
	}
}
