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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kubeflow/spark-operator/api/v1beta2"
	"github.com/kubeflow/spark-operator/pkg/common"
	"github.com/kubeflow/spark-operator/pkg/util"
)

var _ = Describe("partial submission recovery", Serial, func() {
	It("waits for Spark's driver ConfigMap before recovering a Pending driver", func() {
		app, pod := newPartialSubmission()
		submit := newControlledSubmit(false)
		r := newRecoveryReconciler(k8sClient)
		request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)}
		result, err := r.Reconcile(context.Background(), request)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(getRecoveryApplication(app).Status.AppState.State).To(Equal(v1beta2.ApplicationStateNew))
		Expect(submit.invocations()).To(BeZero())
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), pod)).To(Succeed())
		Expect(pod.DeletionTimestamp.IsZero()).To(BeTrue())

		config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: pod.Spec.Volumes[0].ConfigMap.Name, Namespace: app.Namespace}}
		Expect(k8sClient.Create(context.Background(), config)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), config))).To(Succeed()) })
		_, err = r.Reconcile(context.Background(), request)
		Expect(err).NotTo(HaveOccurred())
		latest := getRecoveryApplication(app)
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateSubmitted))
		Expect(latest.Status.SubmissionID).To(Equal("interrupted-run"))
		Expect(latest.Status.SubmissionAttempts).To(Equal(int32(1)))
		Expect(submit.invocations()).To(BeZero())
	})

	It("retires an incomplete Pending driver only after the submission deadline", func() {
		app, pod := newPartialSubmission()
		app.Status.LastSubmissionAttemptTime = metav1.NewTime(time.Now().Add(-11 * time.Minute))
		Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
		submit := newControlledSubmit(false)
		_, err := newRecoveryReconciler(k8sClient).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})
		Expect(err).NotTo(HaveOccurred())
		latest := getRecoveryApplication(app)
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateFailedSubmission))
		Expect(latest.Status.AppState.ErrorMessage).To(ContainSubstring(pod.Spec.Volumes[0].ConfigMap.Name))
		Expect(latest.Status.SubmissionAttempts).To(Equal(int32(1)))
		Expect(submit.invocations()).To(BeZero())
		Expect(apierrors.IsNotFound(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}))).To(BeTrue())
	})

	It("rejects an old application UID stamp even if the owner reference was replaced", func() {
		app, pod := newPartialSubmission()
		pod.Annotations = map[string]string{common.AnnotationAppUID: "old-app-uid"}
		Expect(k8sClient.Update(context.Background(), pod)).To(Succeed())
		submit := newControlledSubmit(false)
		_, err := newRecoveryReconciler(k8sClient).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})
		Expect(err).To(MatchError(ContainSubstring("does not belong")))
		Expect(submit.invocations()).To(BeZero())
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), pod)).To(Succeed())
		Expect(pod.DeletionTimestamp.IsZero()).To(BeTrue())
	})
})

func newPartialSubmission() (*v1beta2.SparkApplication, *corev1.Pod) {
	app := newRecoveryApplication(v1beta2.ApplicationStateNew)
	app.Status.SubmissionID = "interrupted-run"
	app.Status.LastSubmissionAttemptTime = metav1.Now()
	Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
	pod := createDriverPod(app.Name, app.Namespace)
	pod.Labels[common.LabelSubmissionID] = app.Status.SubmissionID
	pod.Spec.TerminationGracePeriodSeconds = util.Int64Ptr(0)
	pod.Spec.Volumes = []corev1.Volume{{
		Name: "spark-conf-volume-driver",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: app.Name + "-conf"},
		}},
	}}
	persistRecoveryDriver(app, pod)
	pod.Status.Phase = corev1.PodPending
	Expect(k8sClient.Status().Update(context.Background(), pod)).To(Succeed())
	return app, pod
}
