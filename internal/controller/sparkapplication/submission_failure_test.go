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
	"path/filepath"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kubeflow/spark-operator/api/v1beta2"
	"github.com/kubeflow/spark-operator/internal/controller/sparkapplication"
	"github.com/kubeflow/spark-operator/pkg/common"
	"github.com/kubeflow/spark-operator/pkg/util"
)

var _ = Describe("submission failure after driver creation", Serial, func() {
	DescribeTable("recovers a usable driver without publishing submission failure", func(timeout bool) {
		app := newRecoveryApplication(v1beta2.ApplicationStateNew)
		submit := newControlledSubmit(true)
		if !timeout {
			failControlledSubmit(submit)
		}
		var publishedFailure atomic.Bool
		writer := observeSubmissionFailure(&publishedFailure)
		options := sparkapplication.Options{APIReader: k8sClient, Namespaces: []string{"default"}}
		if timeout {
			options.SparkSubmitTimeout = 5 * time.Second
		}
		r := sparkapplication.NewReconciler(nil, k8sClient.Scheme(), writer, record.NewFakeRecorder(100), nil, options)
		done := startRecoveryReconcile(r, app, submit)
		submit.waitForInvocation()
		pod := createRecoveryDriver(app, submit)
		if !timeout {
			submit.release()
		}

		var outcome recoveryResult
		Eventually(done, 10*time.Second).Should(Receive(&outcome))

		Expect(publishedFailure.Load()).To(BeFalse(), "a live driver must not briefly appear as a failed submission to other controllers")
		Expect(outcome.err).NotTo(HaveOccurred())
		latest := getRecoveryApplication(app)
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateRunning))
		Expect(latest.Status.SubmissionID).To(Equal(pod.Labels[common.LabelSubmissionID]))
		Expect(latest.Status.SubmissionAttempts).To(Equal(int32(1)))
		Expect(latest.Status.ExecutionAttempts).To(Equal(int32(1)))
		Expect(submit.invocations()).To(Equal(1))
		current := &corev1.Pod{}
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), current)).To(Succeed())
		Expect(current.UID).To(Equal(pod.UID))
		Expect(current.DeletionTimestamp.IsZero()).To(BeTrue())
	},
		Entry("after spark-submit exits with an error", false),
		Entry("after spark-submit times out", true),
	)

	It("leaves the recorded intent recoverable when the driver read fails", func() {
		app := newRecoveryApplication(v1beta2.ApplicationStateNew)
		submit := newControlledSubmit(true)
		failControlledSubmit(submit)
		var publishedFailure, readOutage atomic.Bool
		var failedReads atomic.Int32
		reader := recoveryIntercept(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, pod := obj.(*corev1.Pod); pod && readOutage.Load() {
					failedReads.Add(1)
					return apierrors.NewServiceUnavailable("injected driver read outage")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		r := sparkapplication.NewReconciler(nil, k8sClient.Scheme(), observeSubmissionFailure(&publishedFailure), record.NewFakeRecorder(100), nil,
			sparkapplication.Options{APIReader: reader, Namespaces: []string{"default"}})
		done := startRecoveryReconcile(r, app, submit)
		submit.waitForInvocation()
		pod := createRecoveryDriver(app, submit)
		intent := getRecoveryApplication(app)
		readOutage.Store(true)
		submit.release()
		var outcome recoveryResult
		Eventually(done, 10*time.Second).Should(Receive(&outcome))

		Expect(failedReads.Load()).To(BeNumerically(">", 0))
		Expect(publishedFailure.Load()).To(BeFalse())
		latest := getRecoveryApplication(app)
		Expect(latest.Status.AppState.State).To(Equal(intent.Status.AppState.State))
		Expect(latest.Status.SubmissionID).To(Equal(intent.Status.SubmissionID))
		Expect(latest.Status.LastSubmissionAttemptTime.IsZero()).To(BeFalse())
		Expect(outcome.err != nil || outcome.result.RequeueAfter > 0).To(BeTrue())

		readOutage.Store(false)
		_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})
		Expect(err).NotTo(HaveOccurred())
		latest = getRecoveryApplication(app)
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateRunning))
		Expect(latest.Status.SubmissionID).To(Equal(pod.Labels[common.LabelSubmissionID]))
		Expect(latest.Status.SubmissionAttempts).To(Equal(int32(1)))
		Expect(latest.Status.ExecutionAttempts).To(Equal(int32(1)))
		Expect(submit.invocations()).To(Equal(1))
	})

	It("does not recount an already recorded failed submission when recovering its driver", func() {
		app := newRecoveryApplication(v1beta2.ApplicationStateFailedSubmission)
		app.Status.SubmissionID = "recorded-failed-run"
		app.Status.DriverInfo.PodName = util.GetDriverPodName(app)
		app.Status.AppState.ErrorMessage = "spark-submit lost its connection after creating the driver"
		Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
		pod := createDriverPod(app.Name, app.Namespace)
		pod.Labels[common.LabelSubmissionID] = app.Status.SubmissionID
		persistRecoveryDriver(app, pod)
		submit := newControlledSubmit(false)

		_, err := newRecoveryReconciler(k8sClient).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})

		Expect(err).NotTo(HaveOccurred())
		latest := getRecoveryApplication(app)
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateRunning))
		Expect(latest.Status.AppState.ErrorMessage).To(BeEmpty())
		Expect(latest.Status.SubmissionAttempts).To(Equal(int32(1)))
		Expect(latest.Status.ExecutionAttempts).To(Equal(int32(1)))
		Expect(submit.invocations()).To(BeZero())
	})

	It("does not delete an overdue Pending driver when reading its ConfigMap fails", func() {
		app, pod := newPartialSubmission()
		app.Status.LastSubmissionAttemptTime = metav1.NewTime(time.Now().Add(-11 * time.Minute))
		Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
		reader := recoveryIntercept(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, config := obj.(*corev1.ConfigMap); config {
					return apierrors.NewServiceUnavailable("injected ConfigMap read outage")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		var publishedFailure atomic.Bool
		r := sparkapplication.NewReconciler(nil, k8sClient.Scheme(), observeSubmissionFailure(&publishedFailure), record.NewFakeRecorder(100), nil,
			sparkapplication.Options{APIReader: reader, Namespaces: []string{"default"}})

		_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})

		Expect(err).To(MatchError(ContainSubstring("injected ConfigMap read outage")))
		Expect(publishedFailure.Load()).To(BeFalse())
		latest := getRecoveryApplication(app)
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateNew))
		Expect(latest.Status.SubmissionID).To(Equal(app.Status.SubmissionID))
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), pod)).To(Succeed())
		Expect(pod.DeletionTimestamp.IsZero()).To(BeTrue())
	})

	It("records an expired partial attempt before deleting its driver despite a status conflict", func() {
		app, pod := newPartialSubmission()
		app.Status.LastSubmissionAttemptTime = metav1.NewTime(time.Now().Add(-11 * time.Minute))
		Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
		submit := newControlledSubmit(false)
		var conflicted atomic.Bool
		writer := recoveryIntercept(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if updated, ok := obj.(*v1beta2.SparkApplication); ok && subresource == "status" &&
					updated.Status.AppState.State == v1beta2.ApplicationStateFailedSubmission && conflicted.CompareAndSwap(false, true) {
					latest := getRecoveryApplication(app)
					latest.Annotations = map[string]string{"concurrent-writer": "metadata-update"}
					Expect(k8sClient.Update(ctx, latest)).To(Succeed())
				}
				return c.SubResource(subresource).Update(ctx, obj, opts...)
			},
		})

		_, err := newRecoveryReconciler(writer).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})

		Expect(err).NotTo(HaveOccurred())
		Expect(conflicted.Load()).To(BeTrue())
		latest := getRecoveryApplication(app)
		Expect(latest.Status.SubmissionID).To(Equal(app.Status.SubmissionID))
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateFailedSubmission))
		Expect(latest.Status.SubmissionAttempts).To(Equal(int32(1)))
		Expect(latest.Status.ExecutionAttempts).To(BeZero())
		Expect(submit.invocations()).To(BeZero())
		Expect(apierrors.IsNotFound(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}))).To(BeTrue())
	})

})

func failControlledSubmit(submit *controlledSubmit) {
	path := filepath.Join(submit.dir, "bin", "spark-submit")
	script, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	script = append(script, []byte("echo 'injected error after driver creation' >&2\nexit 1\n")...)
	Expect(os.WriteFile(path, script, 0o755)).To(Succeed())
}

func observeSubmissionFailure(failed *atomic.Bool) client.Client {
	return recoveryIntercept(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if app, ok := obj.(*v1beta2.SparkApplication); ok && subresource == "status" && app.Status.AppState.State == v1beta2.ApplicationStateFailedSubmission {
				failed.Store(true)
			}
			return c.SubResource(subresource).Update(ctx, obj, opts...)
		},
	})
}
