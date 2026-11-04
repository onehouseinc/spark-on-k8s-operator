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

var _ = Describe("submission recovery from stale application reads", Serial, func() {
	DescribeTable("uses authoritative application status after submission", func(freezeAPIReader bool) {
		app := newRecoveryApplication(v1beta2.ApplicationStateNew)
		cachedApp := app.DeepCopy()
		submit := newControlledSubmit(true)
		done := startRecoveryReconcile(newRecoveryReconciler(k8sClient), app, submit)
		submit.waitForInvocation()
		pod := createRecoveryDriver(app, submit)
		submit.release()
		var outcome recoveryResult
		Eventually(done, 10*time.Second).Should(Receive(&outcome))
		Expect(outcome.err).NotTo(HaveOccurred())
		Expect(getRecoveryApplication(app).Status.AppState.State).To(Equal(v1beta2.ApplicationStateSubmitted))
		stale := recoveryIntercept(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if target, ok := obj.(*v1beta2.SparkApplication); ok && key == client.ObjectKeyFromObject(app) {
					cachedApp.DeepCopyInto(target)
					return nil
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		var reader client.Reader = k8sClient
		if freezeAPIReader {
			reader = stale
		}
		r := sparkapplication.NewReconciler(nil, k8sClient.Scheme(), stale, record.NewFakeRecorder(100), nil,
			sparkapplication.Options{APIReader: reader, Namespaces: []string{"default"}})

		_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})

		latest := getRecoveryApplication(app)
		if freezeAPIReader {
			Expect(err).To(HaveOccurred(), "negative control: a stale authoritative reader must not satisfy the recovery assertion")
			Expect(latest.Status.AppState.State).NotTo(Equal(v1beta2.ApplicationStateRunning))
		} else {
			Expect(err).NotTo(HaveOccurred())
			Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateRunning))
		}
		Expect(latest.Status.SubmissionID).To(Equal(pod.Labels[common.LabelSubmissionID]))
		Expect(submit.invocations()).To(Equal(1))
		current := &corev1.Pod{}
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), current)).To(Succeed())
		Expect(current.UID).To(Equal(pod.UID))
		Expect(current.DeletionTimestamp.IsZero()).To(BeTrue())
	},
		Entry("with the live API reader", false),
		Entry("negative control with the same frozen reader", true),
	)

	It("does not delete a same-name driver owned by another application incarnation", func() {
		app := newRecoveryApplication(v1beta2.ApplicationStateInvalidating)
		app.Status.DriverInfo.PodName = util.GetDriverPodName(app)
		app.Status.SubmissionID = "retired-run"
		Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
		pod := createDriverPod(app.Name, app.Namespace)
		pod.Labels[common.LabelSubmissionID] = app.Status.SubmissionID
		persistRecoveryDriver(app, pod)
		pod.OwnerReferences[0].UID = "other-app-uid"
		Expect(k8sClient.Update(context.Background(), pod)).To(Succeed())
		submit := newControlledSubmit(false)

		_, err := newRecoveryReconciler(k8sClient).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})

		Expect(err).To(MatchError(ContainSubstring("does not belong")))
		Expect(getRecoveryApplication(app).Status.AppState.State).To(Equal(v1beta2.ApplicationStateInvalidating))
		Expect(submit.invocations()).To(BeZero())
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), pod)).To(Succeed())
		Expect(pod.DeletionTimestamp.IsZero()).To(BeTrue())
	})

	It("does not delete a replacement driver created after the cleanup read", func() {
		app := newRecoveryApplication(v1beta2.ApplicationStateInvalidating)
		app.Status.DriverInfo.PodName = util.GetDriverPodName(app)
		app.Status.SubmissionID = "retired-run"
		Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
		old := createDriverPod(app.Name, app.Namespace)
		old.Labels[common.LabelSubmissionID] = app.Status.SubmissionID
		persistRecoveryDriver(app, old)
		var replacement *corev1.Pod
		racing := recoveryIntercept(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, pod := obj.(*corev1.Pod); pod && replacement == nil {
					if err := c.Delete(ctx, obj, client.GracePeriodSeconds(0)); err != nil {
						return err
					}
					latest := &v1beta2.SparkApplication{}
					if err := c.Get(ctx, client.ObjectKeyFromObject(app), latest); err != nil {
						return err
					}
					latest.Status.SubmissionID = "replacement-run"
					latest.Status.AppState.State = v1beta2.ApplicationStateRunning
					latest.Status.LastSubmissionAttemptTime = metav1.Now()
					if err := c.Status().Update(ctx, latest); err != nil {
						return err
					}
					replacement = createDriverPod(app.Name, app.Namespace)
					replacement.Labels[common.LabelSubmissionID] = "replacement-run"
					replacement.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(app, v1beta2.SchemeGroupVersion.WithKind("SparkApplication"))}
					if err := c.Create(ctx, replacement); err != nil {
						return err
					}
				}
				return c.Delete(ctx, obj, opts...)
			},
		})

		_, err := newRecoveryReconciler(racing).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})

		Expect(err).NotTo(HaveOccurred())
		latest := getRecoveryApplication(app)
		Expect(latest.Status.SubmissionID).To(Equal("replacement-run"))
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateRunning))
		Expect(replacement).NotTo(BeNil())
		current := &corev1.Pod{}
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(old), current)).To(Succeed())
		Expect(current.UID).To(Equal(replacement.UID))
		Expect(current.UID).NotTo(Equal(old.UID))
		Expect(current.DeletionTimestamp.IsZero()).To(BeTrue())
	})
})
