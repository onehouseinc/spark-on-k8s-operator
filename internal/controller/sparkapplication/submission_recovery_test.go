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
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kubeflow/spark-operator/api/v1beta2"
	"github.com/kubeflow/spark-operator/internal/controller/sparkapplication"
	"github.com/kubeflow/spark-operator/pkg/common"
	"github.com/kubeflow/spark-operator/pkg/util"
)

var _ = Describe("submission recovery", Serial, func() {
	DescribeTable("records the submission identity before invoking spark-submit", func(state v1beta2.ApplicationStateType) {
		app := newRecoveryApplication(state)
		submit := newControlledSubmit(true)
		done := startRecoveryReconcile(newRecoveryReconciler(k8sClient), app, submit)
		submit.waitForInvocation()

		latest := getRecoveryApplication(app)
		Expect(latest.Status.SubmissionID).To(Equal(submit.submissionID()))
		Expect(latest.Status.DriverInfo.PodName).To(Equal(util.GetDriverPodName(app)))
		Expect(latest.Status.LastSubmissionAttemptTime.IsZero()).To(BeFalse())
		Expect(latest.Status.AppState.State).NotTo(Equal(v1beta2.ApplicationStateSubmitted))

		submit.release()
		Eventually(done, 10*time.Second).Should(Receive())
	},
		Entry("NEW", v1beta2.ApplicationStateNew),
		Entry("SUBMISSION_FAILED", v1beta2.ApplicationStateFailedSubmission),
		Entry("PENDING_RERUN", v1beta2.ApplicationStatePendingRerun),
	)

	It("does not invoke spark-submit when recording intent fails", func() {
		app := newRecoveryApplication(v1beta2.ApplicationStateNew)
		submit := newControlledSubmit(false)
		failing := recoveryIntercept(interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, subresource string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
				Expect(subresource).To(Equal("status"))
				return apierrors.NewServiceUnavailable("injected status outage")
			},
		})

		_, err := newRecoveryReconciler(failing).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})

		Expect(err).To(HaveOccurred())
		Expect(submit.invocations()).To(Equal(0))
		Expect(getRecoveryApplication(app).Status.SubmissionID).To(BeEmpty())
		pod := &corev1.Pod{}
		Expect(apierrors.IsNotFound(k8sClient.Get(context.Background(), client.ObjectKey{Namespace: app.Namespace, Name: util.GetDriverPodName(app)}, pod))).To(BeTrue())
	})

	DescribeTable("retries only the outcome after a metadata conflict", func(state v1beta2.ApplicationStateType) {
		app := newRecoveryApplication(state)
		submit := newControlledSubmit(true)
		done := startRecoveryReconcile(newRecoveryReconciler(k8sClient), app, submit)
		submit.waitForInvocation()
		pod := createRecoveryDriver(app, submit)
		latest := getRecoveryApplication(app)
		latest.Annotations = map[string]string{"test.sparkoperator.k8s.io/concurrent-write": "true"}
		Expect(k8sClient.Update(context.Background(), latest)).To(Succeed())

		submit.release()
		var outcome recoveryResult
		Eventually(done, 10*time.Second).Should(Receive(&outcome))
		Expect(outcome.err).NotTo(HaveOccurred())

		Expect(submit.invocations()).To(Equal(1))
		latest = getRecoveryApplication(app)
		Expect(latest.Status.SubmissionID).To(Equal(pod.Labels[common.LabelSubmissionID]))
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateSubmitted))
		Expect(latest.Annotations).To(HaveKeyWithValue("test.sparkoperator.k8s.io/concurrent-write", "true"))
		current := &corev1.Pod{}
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), current)).To(Succeed())
		Expect(current.UID).To(Equal(pod.UID))
		Expect(current.DeletionTimestamp.IsZero()).To(BeTrue())
	},
		Entry("NEW", v1beta2.ApplicationStateNew),
		Entry("SUBMISSION_FAILED", v1beta2.ApplicationStateFailedSubmission),
		Entry("PENDING_RERUN", v1beta2.ApplicationStatePendingRerun),
	)

	DescribeTable("recovers the existing driver after all outcome writes are lost", func(state v1beta2.ApplicationStateType) {
		app := newRecoveryApplication(state)
		if state != v1beta2.ApplicationStateNew {
			app.Status.SubmissionID = "previous-run"
			if state == v1beta2.ApplicationStateFailedSubmission {
				app.Status.AppState.ErrorMessage = "previous submission failed"
			}
			Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
		}
		submit := newControlledSubmit(true)
		var loseOutcome atomic.Bool
		failing := recoveryIntercept(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if subresource == "status" && loseOutcome.Load() {
					return apierrors.NewServiceUnavailable("injected lost submission outcome")
				}
				return c.SubResource(subresource).Update(ctx, obj, opts...)
			},
		})
		done := startRecoveryReconcile(newRecoveryReconciler(failing), app, submit)
		submit.waitForInvocation()
		pod := createRecoveryDriver(app, submit)
		loseOutcome.Store(true)
		submit.release()
		Eventually(done, 10*time.Second).Should(Receive())
		loseOutcome.Store(false)

		_, err := newRecoveryReconciler(k8sClient).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})

		Expect(err).NotTo(HaveOccurred())
		latest := getRecoveryApplication(app)
		Expect(latest.Status.SubmissionID).To(Equal(pod.Labels[common.LabelSubmissionID]))
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateRunning))
		Expect(submit.invocations()).To(Equal(1))
		current := &corev1.Pod{}
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), current)).To(Succeed())
		Expect(current.UID).To(Equal(pod.UID))
		Expect(current.DeletionTimestamp.IsZero()).To(BeTrue())
		attempts := latest.Status.SubmissionAttempts
		executions := latest.Status.ExecutionAttempts
		_, err = newRecoveryReconciler(k8sClient).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})
		Expect(err).NotTo(HaveOccurred())
		latest = getRecoveryApplication(app)
		Expect(latest.Status.SubmissionAttempts).To(Equal(attempts))
		Expect(latest.Status.ExecutionAttempts).To(Equal(executions))
	},
		Entry("NEW", v1beta2.ApplicationStateNew),
		Entry("SUBMISSION_FAILED", v1beta2.ApplicationStateFailedSubmission),
		Entry("PENDING_RERUN", v1beta2.ApplicationStatePendingRerun),
	)

	It("preserves a CPU change whose invalidation status write was lost", func() {
		app := newRecoveryApplication(v1beta2.ApplicationStateNew)
		submit := newControlledSubmit(true)
		done := startRecoveryReconcile(newRecoveryReconciler(k8sClient), app, submit)
		submit.waitForInvocation()
		pod := createRecoveryDriver(app, submit)
		latest := getRecoveryApplication(app)
		latest.Spec.Driver.CoreLimit = util.StringPtr("4")
		Expect(k8sClient.Update(context.Background(), latest)).To(Succeed())
		Expect(latest.Generation).To(BeNumerically(">", app.Generation))

		submit.release()
		var outcome recoveryResult
		Eventually(done, 10*time.Second).Should(Receive(&outcome))
		Expect(outcome.err).NotTo(HaveOccurred())

		latest = getRecoveryApplication(app)
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateInvalidating))
		Expect(latest.Status.SubmissionID).To(Equal(pod.Labels[common.LabelSubmissionID]))
		Expect(latest.Spec.Driver.CoreLimit).To(Equal(util.StringPtr("4")))
		Expect(submit.invocations()).To(Equal(1))
	})

	It("does not overwrite invalidation when the old submission finishes", func() {
		app := newRecoveryApplication(v1beta2.ApplicationStateNew)
		submit := newControlledSubmit(true)
		done := startRecoveryReconcile(newRecoveryReconciler(k8sClient), app, submit)
		submit.waitForInvocation()
		createRecoveryDriver(app, submit)
		latest := getRecoveryApplication(app)
		latest.Spec.Driver.CoreLimit = util.StringPtr("4")
		Expect(k8sClient.Update(context.Background(), latest)).To(Succeed())
		latest.Status.AppState.State = v1beta2.ApplicationStateInvalidating
		Expect(k8sClient.Status().Update(context.Background(), latest)).To(Succeed())

		submit.release()
		Eventually(done, 10*time.Second).Should(Receive())

		Expect(getRecoveryApplication(app).Status.AppState.State).To(Equal(v1beta2.ApplicationStateInvalidating))
		Expect(submit.invocations()).To(Equal(1))
	})

	It("confirms a missing cached driver with a live read", func() {
		app := newRecoveryApplication(v1beta2.ApplicationStateRunning)
		app.Status.SubmissionID = "recorded-run"
		app.Status.DriverInfo.PodName = util.GetDriverPodName(app)
		app.Status.LastSubmissionAttemptTime = metav1.Now()
		Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
		pod := createDriverPod(app.Name, app.Namespace)
		pod.Labels[common.LabelSubmissionID] = app.Status.SubmissionID
		persistRecoveryDriver(app, pod)
		stale := recoveryIntercept(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, key.Name)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})

		result, err := newRecoveryReconciler(stale).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})

		Expect(err).NotTo(HaveOccurred())
		Expect(getRecoveryApplication(app).Status.AppState.State).To(Equal(v1beta2.ApplicationStateRunning))
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
	})

	It("does not mistake a retired matching driver for an interrupted new submission", func() {
		app := newRecoveryApplication(v1beta2.ApplicationStatePendingRerun)
		app.Status.SubmissionID = "retired-run"
		app.Status.DriverInfo.PodName = util.GetDriverPodName(app)
		Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
		pod := createDriverPod(app.Name, app.Namespace)
		pod.Labels[common.LabelSubmissionID] = app.Status.SubmissionID
		persistRecoveryDriver(app, pod)
		submit := newControlledSubmit(false)

		result, err := newRecoveryReconciler(k8sClient).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})

		Expect(err).NotTo(HaveOccurred())
		latest := getRecoveryApplication(app)
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStatePendingRerun))
		Expect(latest.Status.SubmissionID).To(Equal("retired-run"))
		Expect(latest.Status.LastSubmissionAttemptTime.IsZero()).To(BeTrue())
		Expect(submit.invocations()).To(Equal(0))
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
	})

	It("waits for legacy invalidation cleanup before recording and submitting the new run", func() {
		app := newRecoveryApplication(v1beta2.ApplicationStateInvalidating)
		app.Spec.Driver.CoreLimit = util.StringPtr("4")
		Expect(k8sClient.Update(context.Background(), app)).To(Succeed())
		app.Status.SubmissionID = "retired-run"
		app.Status.DriverInfo.PodName = util.GetDriverPodName(app)
		app.Status.LastSubmissionAttemptTime = metav1.NewTime(time.Now().Add(-time.Hour))
		Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
		pod := createDriverPod(app.Name, app.Namespace)
		pod.Labels[common.LabelSubmissionID] = app.Status.SubmissionID
		pod.Finalizers = []string{"test.sparkoperator.k8s.io/hold-cleanup"}
		persistRecoveryDriver(app, pod)
		submit := newControlledSubmit(false)
		request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)}

		for range 2 {
			result, err := newRecoveryReconciler(k8sClient).Reconcile(context.Background(), request)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			latest := getRecoveryApplication(app)
			Expect(latest.Status.AppState.State).To(BeElementOf(v1beta2.ApplicationStateInvalidating, v1beta2.ApplicationStatePendingRerun))
			Expect(latest.Status.SubmissionID).To(Equal("retired-run"))
			Expect(submit.invocations()).To(Equal(0))
		}
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), pod)).To(Succeed())
		Expect(pod.DeletionTimestamp.IsZero()).To(BeFalse())
		pod.Finalizers = nil
		Expect(k8sClient.Update(context.Background(), pod)).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), pod, client.GracePeriodSeconds(0)))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}))
		}, 5*time.Second).Should(BeTrue())

		Eventually(func() (int, error) {
			_, err := newRecoveryReconciler(k8sClient).Reconcile(context.Background(), request)
			return submit.invocations(), err
		}, 5*time.Second).Should(Equal(1))
		latest := getRecoveryApplication(app)
		Expect(latest.Status.SubmissionID).To(Equal(submit.submissionID()))
		Expect(latest.Status.SubmissionID).NotTo(Equal("retired-run"))
		Expect(latest.Status.LastSubmissionAttemptTime.IsZero()).To(BeFalse())
		Expect(submit.conf(common.SparkKubernetesDriverLimitCores)).To(Equal("4"))
	})

})

type controlledSubmit struct {
	dir string
}

func newControlledSubmit(block bool) *controlledSubmit {
	dir := GinkgoT().TempDir()
	Expect(os.Mkdir(filepath.Join(dir, "bin"), 0o755)).To(Succeed())
	body := `#!/bin/sh
set -eu
printf 'call\n' >> "$SPARK_HOME/calls"
if [ ! -f "$SPARK_HOME/entered" ]; then
  printf '%s\n' "$@" > "$SPARK_HOME/args"
  touch "$SPARK_HOME/entered"
  while [ ! -f "$SPARK_HOME/release" ]; do sleep 0.01; done
fi
`
	Expect(os.WriteFile(filepath.Join(dir, "bin", "spark-submit"), []byte(body), 0o755)).To(Succeed())
	GinkgoT().Setenv(common.EnvSparkHome, dir)
	GinkgoT().Setenv(common.EnvKubernetesServiceHost, "127.0.0.1")
	GinkgoT().Setenv(common.EnvKubernetesServicePort, "443")
	submit := &controlledSubmit{dir: dir}
	if !block {
		submit.release()
	}
	return submit
}

func (s *controlledSubmit) release() {
	Expect(os.WriteFile(filepath.Join(s.dir, "release"), nil, 0o644)).To(Succeed())
}

func (s *controlledSubmit) waitForInvocation() {
	Eventually(func() error {
		_, err := os.Stat(filepath.Join(s.dir, "entered"))
		return err
	}, 10*time.Second).Should(Succeed())
}

func (s *controlledSubmit) invocations() int {
	data, err := os.ReadFile(filepath.Join(s.dir, "calls"))
	if os.IsNotExist(err) {
		return 0
	}
	Expect(err).NotTo(HaveOccurred())
	return strings.Count(string(data), "call\n")
}

func (s *controlledSubmit) conf(key string) string {
	data, err := os.ReadFile(filepath.Join(s.dir, "args"))
	Expect(err).NotTo(HaveOccurred())
	for _, arg := range strings.Split(string(data), "\n") {
		if value, found := strings.CutPrefix(arg, key+"="); found {
			return value
		}
	}
	return ""
}

func (s *controlledSubmit) submissionID() string {
	id := s.conf(fmt.Sprintf(common.SparkKubernetesDriverLabelTemplate, common.LabelSubmissionID))
	Expect(id).NotTo(BeEmpty())
	return id
}

type recoveryResult struct {
	result reconcile.Result
	err    error
}

func startRecoveryReconcile(r *sparkapplication.Reconciler, app *v1beta2.SparkApplication, submit *controlledSubmit) <-chan recoveryResult {
	done := make(chan recoveryResult, 1)
	finished := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(finished)
		result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})
		done <- recoveryResult{result: result, err: err}
	}()
	DeferCleanup(func() {
		submit.release()
		Eventually(finished, 10*time.Second).Should(BeClosed())
	})
	return done
}

func newRecoveryReconciler(c client.Client) *sparkapplication.Reconciler {
	return sparkapplication.NewReconciler(nil, k8sClient.Scheme(), c, record.NewFakeRecorder(100), nil, sparkapplication.Options{
		APIReader:                    k8sClient,
		Namespaces:                   []string{"default"},
		DriverPodCreationGracePeriod: time.Second,
	})
}

func recoveryIntercept(funcs interceptor.Funcs) client.Client {
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
	Expect(err).NotTo(HaveOccurred())
	return interceptor.NewClient(c, funcs)
}

func newRecoveryApplication(state v1beta2.ApplicationStateType) *v1beta2.SparkApplication {
	app := &v1beta2.SparkApplication{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "submission-recovery-", Namespace: "default"},
		Spec: v1beta2.SparkApplicationSpec{
			Image:               util.StringPtr("spark:test"),
			MainApplicationFile: util.StringPtr("local:///test.jar"),
			SparkVersion:        "3.5.3",
			RestartPolicy: v1beta2.RestartPolicy{
				Type:                             v1beta2.RestartPolicyAlways,
				OnSubmissionFailureRetryInterval: util.Int64Ptr(1),
			},
		},
	}
	v1beta2.SetSparkApplicationDefaults(app)
	Expect(k8sClient.Create(context.Background(), app)).To(Succeed())
	app.Status.AppState.State = state
	if state == v1beta2.ApplicationStateFailedSubmission {
		app.Status.LastSubmissionAttemptTime = metav1.NewTime(time.Now().Add(-time.Minute))
		app.Status.SubmissionAttempts = 1
	}
	Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
	DeferCleanup(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), app))).To(Succeed())
	})
	return app
}

func getRecoveryApplication(app *v1beta2.SparkApplication) *v1beta2.SparkApplication {
	latest := &v1beta2.SparkApplication{}
	Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(app), latest)).To(Succeed())
	return latest
}

func createRecoveryDriver(app *v1beta2.SparkApplication, submit *controlledSubmit) *corev1.Pod {
	pod := createDriverPod(app.Name, app.Namespace)
	pod.Labels[common.LabelSubmissionID] = submit.submissionID()
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(app, v1beta2.SchemeGroupVersion.WithKind("SparkApplication"))}
	pod.Annotations = map[string]string{}
	for _, key := range []string{"sparkoperator.k8s.io/app-uid", "sparkoperator.k8s.io/app-generation"} {
		if value := submit.conf(fmt.Sprintf(common.SparkKubernetesDriverAnnotationTemplate, key)); value != "" {
			pod.Annotations[key] = value
		}
	}
	persistRecoveryDriver(app, pod)
	return pod
}

func persistRecoveryDriver(app *v1beta2.SparkApplication, pod *corev1.Pod) {
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(app, v1beta2.SchemeGroupVersion.WithKind("SparkApplication"))}
	Expect(k8sClient.Create(context.Background(), pod)).To(Succeed())
	pod.Status.Phase = corev1.PodRunning
	Expect(k8sClient.Status().Update(context.Background(), pod)).To(Succeed())
	DeferCleanup(func() {
		current := &corev1.Pod{}
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), current); err == nil {
			current.Finalizers = nil
			Expect(k8sClient.Update(context.Background(), current)).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), current, client.GracePeriodSeconds(0)))).To(Succeed())
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
	})
}
