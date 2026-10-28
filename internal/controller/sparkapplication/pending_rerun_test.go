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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kubeflow/spark-operator/api/v1beta2"
	"github.com/kubeflow/spark-operator/internal/controller/sparkapplication"
	"github.com/kubeflow/spark-operator/pkg/common"
	"github.com/kubeflow/spark-operator/pkg/util"
)

var _ = Describe("SubmissionID repair and PENDING_RERUN requeue", func() {
	ctx := context.Background()
	ns := "default"
	interval := sparkapplication.PendingRerunRequeueInterval

	BeforeEach(func() {
		// An empty SPARK_HOME makes every resubmit fail deterministically as FAILED_SUBMISSION.
		GinkgoT().Setenv(common.EnvSparkHome, GinkgoT().TempDir())
	})

	reconcileOnce := func(key types.NamespacedName) reconcile.Result {
		r := sparkapplication.NewReconciler(nil, k8sClient.Scheme(), k8sClient, record.NewFakeRecorder(10), nil,
			sparkapplication.Options{Namespaces: []string{ns}})
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		return result
	}
	expectRecheck := func(result reconcile.Result) {
		Expect(result.RequeueAfter).To(BeNumerically(">=", interval))
		Expect(result.RequeueAfter).To(BeNumerically("<=", interval*12/10)) // up to 20% jitter
	}
	getApp := func(key types.NamespacedName) *v1beta2.SparkApplication {
		app := &v1beta2.SparkApplication{}
		Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		return app
	}
	newApp := func(name string, mutate func(*v1beta2.SparkApplicationStatus)) types.NamespacedName {
		app := &v1beta2.SparkApplication{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       v1beta2.SparkApplicationSpec{MainApplicationFile: util.StringPtr("local:///dummy.jar")},
		}
		v1beta2.SetSparkApplicationDefaults(app)
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		mutate(&app.Status)
		Expect(k8sClient.Status().Update(ctx, app)).To(Succeed())
		key := types.NamespacedName{Name: name, Namespace: ns}
		DeferCleanup(func() {
			pod := &corev1.Pod{}
			if k8sClient.Get(ctx, getDriverNamespacedName(name, ns), pod) == nil {
				pod.Finalizers = nil
				_ = k8sClient.Update(ctx, pod)
				_ = k8sClient.Delete(ctx, pod)
			}
			_ = k8sClient.Delete(ctx, getApp(key))
		})
		return key
	}
	newDriver := func(appName, submissionID string, phase corev1.PodPhase, finalizers ...string) *corev1.Pod {
		pod := createDriverPod(appName, ns)
		pod.Labels[common.LabelSubmissionID] = submissionID
		pod.Finalizers = finalizers
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.Phase = phase
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		return pod
	}
	driverGone := func(appName string) bool {
		return errors.IsNotFound(k8sClient.Get(ctx, getDriverNamespacedName(appName, ns), &corev1.Pod{}))
	}

	It("PENDING_RERUN adopts the live driver's SubmissionID and resets the previous run's status", func() {
		key := newApp("adopt-stale-id", func(s *v1beta2.SparkApplicationStatus) {
			s.AppState.State = v1beta2.ApplicationStatePendingRerun
			s.SubmissionID = "old-id"
			s.ExecutorState = map[string]v1beta2.ExecutorState{"old-run-exec-1": v1beta2.ExecutorStateRunning}
		})
		pod := newDriver(key.Name, "new-id", corev1.PodPending)

		reconcileOnce(key)

		app := getApp(key)
		Expect(app.Status.SubmissionID).To(Equal("new-id"))
		Expect(app.Status.AppState.State).To(Equal(v1beta2.ApplicationStateSubmitted))
		Expect(app.Status.SubmissionAttempts).To(Equal(int32(1)))
		Expect(app.Status.ExecutionAttempts).To(Equal(int32(1)))
		Expect(app.Status.LastSubmissionAttemptTime.Time).To(BeTemporally("~", pod.CreationTimestamp.Time, time.Second))
		Expect(app.Status.ExecutorState).NotTo(HaveKey("old-run-exec-1"))
	})

	It("adopts a finished driver from an unsaved submission instead of deleting it", func() {
		key := newApp("adopt-finished-driver", func(s *v1beta2.SparkApplicationStatus) {
			s.AppState.State = v1beta2.ApplicationStatePendingRerun
			s.SubmissionID = "old-id"
		})
		newDriver(key.Name, "new-id", corev1.PodSucceeded)

		Expect(reconcileOnce(key).RequeueAfter).To(BeZero())

		app := getApp(key)
		Expect(app.Status.SubmissionID).To(Equal("new-id"))
		Expect(app.Status.AppState.State).To(Equal(v1beta2.ApplicationStateSucceeding))
		Expect(driverGone(key.Name)).To(BeFalse())
	})

	It("repairs a RUNNING app's stale SubmissionID on reconcile", func() {
		key := newApp("running-stale-id", func(s *v1beta2.SparkApplicationStatus) {
			s.AppState.State = v1beta2.ApplicationStateRunning
			s.SubmissionID = "old-id"
			s.DriverInfo.PodName = getDriverNamespacedName("running-stale-id", ns).Name
		})
		newDriver(key.Name, "new-id", corev1.PodRunning)

		reconcileOnce(key)

		app := getApp(key)
		Expect(app.Status.SubmissionID).To(Equal("new-id"))
		Expect(app.Status.AppState.State).To(Equal(v1beta2.ApplicationStateRunning))
	})

	It("does not adopt the SubmissionID of a driver controlled by another application", func() {
		key := newApp("foreign-controller", func(s *v1beta2.SparkApplicationStatus) {
			s.AppState.State = v1beta2.ApplicationStateRunning
			s.SubmissionID = "old-id"
			s.DriverInfo.PodName = getDriverNamespacedName("foreign-controller", ns).Name
		})
		pod := createDriverPod(key.Name, ns)
		pod.Labels[common.LabelSubmissionID] = "new-id"
		pod.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: v1beta2.SchemeGroupVersion.String(),
			Kind:       "SparkApplication",
			Name:       key.Name,
			UID:        "uid-of-an-earlier-app",
			Controller: util.BoolPtr(true),
		}}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())

		reconcileOnce(key)

		Expect(getApp(key).Status.SubmissionID).To(Equal("old-id"))
	})

	It("rechecks while the old driver terminates, then resubmits without any event", func() {
		key := newApp("rerun-terminating-driver", func(s *v1beta2.SparkApplicationStatus) {
			s.AppState.State = v1beta2.ApplicationStatePendingRerun
			s.SubmissionID = "old-id"
		})
		pod := newDriver(key.Name, "old-id", corev1.PodRunning, "test/hold")
		Expect(k8sClient.Delete(ctx, pod)).To(Succeed()) // the finalizer holds the pod in Terminating

		expectRecheck(reconcileOnce(key))

		Expect(k8sClient.Get(ctx, getDriverNamespacedName(key.Name, ns), pod)).To(Succeed())
		pod.Finalizers = nil
		Expect(k8sClient.Update(ctx, pod)).To(Succeed())
		Eventually(func() bool { return driverGone(key.Name) }).Should(BeTrue())

		// The recheck, not a pod event, triggers this run.
		Expect(reconcileOnce(key).RequeueAfter).To(BeZero())
		app := getApp(key)
		Expect(app.Status.AppState.State).To(Equal(v1beta2.ApplicationStateFailedSubmission))
		Expect(app.Status.SubmissionAttempts).To(Equal(int32(1)))
	})

	It("deletes a lingering web UI Service again, then resubmits once it is gone", func() {
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "rerun-lingering-svc-ui-svc", Namespace: ns},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 4040}}},
		}
		Expect(k8sClient.Create(ctx, svc)).To(Succeed())
		key := newApp("rerun-lingering-svc", func(s *v1beta2.SparkApplicationStatus) {
			s.AppState.State = v1beta2.ApplicationStatePendingRerun
			s.DriverInfo.WebUIServiceName = svc.Name
		})

		expectRecheck(reconcileOnce(key))
		Expect(getApp(key).Status.AppState.State).To(Equal(v1beta2.ApplicationStatePendingRerun))
		Eventually(func() bool {
			return errors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: svc.Name, Namespace: ns}, &corev1.Service{}))
		}).Should(BeTrue())

		Expect(reconcileOnce(key).RequeueAfter).To(BeZero())
		Expect(getApp(key).Status.AppState.State).To(Equal(v1beta2.ApplicationStateFailedSubmission))
	})

	It("deletes a finished driver of the current submission, and rechecks", func() {
		key := newApp("rerun-leftover-driver", func(s *v1beta2.SparkApplicationStatus) {
			s.AppState.State = v1beta2.ApplicationStatePendingRerun
			s.SubmissionID = "old-id"
		})
		newDriver(key.Name, "old-id", corev1.PodFailed)

		expectRecheck(reconcileOnce(key))
		Eventually(func() bool { return driverGone(key.Name) }).Should(BeTrue())
	})
})
