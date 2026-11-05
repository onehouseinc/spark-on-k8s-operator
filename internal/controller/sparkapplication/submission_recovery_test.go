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
	"strconv"
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
	for _, state := range []v1beta2.ApplicationStateType{v1beta2.ApplicationStateNew, v1beta2.ApplicationStateFailedSubmission, v1beta2.ApplicationStatePendingRerun} {
		Context(fmt.Sprintf("from %q", state), func() {
			DescribeTable("persists identity before launch and never repeats a launch for its outcome", func(outcome string) {
				app := newRecoveryApplication(state)
				submit := newControlledSubmit(true)
				var writes, conflicts atomic.Int32
				writer := recoveryIntercept(interceptor.Funcs{
					SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
						updated := obj.(*v1beta2.SparkApplication)
						if updated.Status.AppState.State == v1beta2.ApplicationStateSubmitted {
							if writes.Add(1) == 1 && outcome == "conflict" {
								latest := getRecoveryApplication(app)
								latest.Annotations = map[string]string{"external-write": "preserved"}
								Expect(k8sClient.Update(ctx, latest)).To(Succeed())
							}
							if outcome == "lost" {
								return apierrors.NewServiceUnavailable("lost outcome")
							}
						}
						err := c.SubResource(subresource).Update(ctx, obj, opts...)
						if apierrors.IsConflict(err) {
							conflicts.Add(1)
						}
						return err
					},
				})
				done := startRecoveryReconcile(newRecoveryReconciler(writer), app, submit)
				submit.waitForInvocation()
				intent := getRecoveryApplication(app)
				Expect(intent.Status.SubmissionID).To(Equal(submit.submissionID()))
				Expect(intent.Status.DriverInfo.PodName).To(Equal(util.GetDriverPodName(app)))
				Expect(intent.Status.LastSubmissionAttemptTime.IsZero()).To(BeFalse())
				Expect(intent.Status.AppState.State).To(Equal(state))
				pod := createRecoveryDriver(app, submit)
				submit.release()
				var result recoveryResult
				Eventually(done, 10*time.Second).Should(Receive(&result))
				Expect(result.err).NotTo(HaveOccurred())
				if outcome == "lost" {
					Expect(getRecoveryApplication(app).Status).To(Equal(intent.Status))
				} else {
					Expect(getRecoveryApplication(app).Status.AppState.State).To(Equal(v1beta2.ApplicationStateSubmitted))
				}
				if outcome == "conflict" {
					Expect(conflicts.Load()).To(Equal(int32(1)))
					Expect(writes.Load()).To(Equal(int32(2)))
					Expect(getRecoveryApplication(app).Annotations).To(HaveKeyWithValue("external-write", "preserved"))
				}
				for range 2 {
					_, err := reconcileRecovery(newRecoveryReconciler(k8sClient), app)
					Expect(err).NotTo(HaveOccurred())
					latest := getRecoveryApplication(app)
					Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateRunning))
					Expect(latest.Status.SubmissionID).To(Equal(intent.Status.SubmissionID))
					Expect(latest.Status.SubmissionAttempts).To(Equal(intent.Status.SubmissionAttempts + 1))
					Expect(latest.Status.ExecutionAttempts).To(Equal(intent.Status.ExecutionAttempts + 1))
					Expect(submit.invocations()).To(Equal(1))
					assertRecoveryDriverUnchanged(pod)
				}
			}, Entry("normal outcome", "success"), Entry("metadata conflict", "conflict"), Entry("operator loses every outcome write", "lost"))

			It("creates nothing if persisting intent fails", func() {
				app := newRecoveryApplication(state)
				submit := newControlledSubmit(false)
				writer := recoveryIntercept(interceptor.Funcs{SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
					return apierrors.NewServiceUnavailable("intent unavailable")
				}})
				_, err := reconcileRecovery(newRecoveryReconciler(writer), app)
				Expect(err).To(HaveOccurred())
				Expect(submit.invocations()).To(BeZero())
				Expect(getRecoveryApplication(app).Status.SubmissionID).To(BeEmpty())
				Expect(apierrors.IsNotFound(k8sClient.Get(context.Background(), client.ObjectKey{Namespace: app.Namespace, Name: util.GetDriverPodName(app)}, &corev1.Pod{}))).To(BeTrue())
			})

			It("waits for an in-flight driver and recovers it without submitting", func() {
				app := newRecoveryApplication(state)
				submit := newControlledSubmit(false)
				for _, age := range []time.Duration{0, 9 * time.Minute} {
					recordRecoveryIntent(app, age)
					intent := app.Status.DeepCopy()
					result, err := reconcileRecovery(newRecoveryReconciler(k8sClient), app)
					Expect(err).NotTo(HaveOccurred())
					Expect(result.RequeueAfter).To(BeNumerically(">", 0))
					Expect(getRecoveryApplication(app).Status).To(Equal(*intent))
					Expect(submit.invocations()).To(BeZero())
				}
				pod := recoveryDriverForIntent(app, corev1.PodRunning)
				_, err := reconcileRecovery(newRecoveryReconciler(k8sClient), app)
				Expect(err).NotTo(HaveOccurred())
				Expect(getRecoveryApplication(app).Status.AppState.State).To(Equal(v1beta2.ApplicationStateRunning))
				Expect(getRecoveryApplication(app).Status.SubmissionID).To(Equal(app.Status.SubmissionID))
				Expect(submit.invocations()).To(BeZero())
				assertRecoveryDriverUnchanged(pod)
			})

			It("counts an expired driverless attempt before retrying", func() {
				app := newRecoveryApplication(state)
				recordRecoveryIntent(app, 11*time.Minute)
				submit := newControlledSubmit(false)
				_, err := reconcileRecovery(newRecoveryReconciler(k8sClient), app)
				Expect(err).NotTo(HaveOccurred())
				failed := getRecoveryApplication(app)
				Expect(failed.Status.AppState.State).To(Equal(v1beta2.ApplicationStateFailedSubmission))
				Expect(failed.Status.SubmissionAttempts).To(Equal(app.Status.SubmissionAttempts + 1))
				Expect(failed.Status.SubmissionID).To(Equal(app.Status.SubmissionID))
				Expect(submit.invocations()).To(BeZero())
				_, err = reconcileRecovery(newRecoveryReconciler(k8sClient), app)
				Expect(err).NotTo(HaveOccurred())
				latest := getRecoveryApplication(app)
				Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateSubmitted))
				Expect(latest.Status.SubmissionAttempts).To(Equal(failed.Status.SubmissionAttempts + 1))
				Expect(latest.Status.SubmissionID).To(Equal(submit.submissionID()))
				Expect(latest.Status.SubmissionID).NotTo(Equal(app.Status.SubmissionID))
				Expect(submit.invocations()).To(Equal(1))
			})
		})
	}

	DescribeTable("preserves newer work when an old submission returns", func(change string) {
		app := newRecoveryApplication(v1beta2.ApplicationStateNew)
		submit := newControlledSubmit(true)
		done := startRecoveryReconcile(newRecoveryReconciler(k8sClient), app, submit)
		submit.waitForInvocation()
		var pod *corev1.Pod
		if change == "CPU" || change == "invalidating" {
			pod = createRecoveryDriver(app, submit)
		}
		latest := getRecoveryApplication(app)
		if change == "recreated" {
			status := latest.Status
			Expect(k8sClient.Delete(context.Background(), latest)).To(Succeed())
			latest = &v1beta2.SparkApplication{ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace}, Spec: app.Spec}
			Expect(k8sClient.Create(context.Background(), latest)).To(Succeed())
			latest.Status = status
		}
		if change == "CPU" || change == "invalidating" {
			latest.Spec.Driver.CoreLimit = util.StringPtr("4")
			Expect(k8sClient.Update(context.Background(), latest)).To(Succeed())
		}
		if change == "invalidating" {
			latest.Status.AppState.State = v1beta2.ApplicationStateInvalidating
		}
		if change == "new ID" {
			latest.Status.SubmissionID = "replacement-run"
		}
		Expect(k8sClient.Status().Update(context.Background(), latest)).To(Succeed())
		expected := latest.DeepCopy()
		if pod == nil {
			pod = recoveryDriverForIntent(expected, corev1.PodRunning)
		}
		submit.release()
		var result recoveryResult
		Eventually(done, 10*time.Second).Should(Receive(&result))
		Expect(result.err).NotTo(HaveOccurred())
		latest = getRecoveryApplication(app)
		if change == "CPU" {
			Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateInvalidating))
			Expect(latest.Spec.Driver.CoreLimit).To(Equal(util.StringPtr("4")))
		} else {
			Expect(latest.Status).To(Equal(expected.Status))
		}
		Expect(latest.UID).To(Equal(expected.UID))
		Expect(latest.Status.SubmissionID).To(Equal(expected.Status.SubmissionID))
		Expect(submit.invocations()).To(Equal(1))
		assertRecoveryDriverUnchanged(pod)
	}, Entry("CPU change with lost predicate write", "CPU"), Entry("invalidation already saved", "invalidating"), Entry("recreated application", "recreated"), Entry("replacement submission", "new ID"))

	DescribeTable("respects the retry budget after driverless expiry", func(policy v1beta2.RestartPolicyType, attempts int32, interval int64) {
		app := newRecoveryApplication(v1beta2.ApplicationStateNew)
		app.Spec.RestartPolicy.Type = policy
		app.Spec.RestartPolicy.OnSubmissionFailureRetries = util.Int32Ptr(1)
		app.Spec.RestartPolicy.OnSubmissionFailureRetryInterval = util.Int64Ptr(interval)
		Expect(k8sClient.Update(context.Background(), app)).To(Succeed())
		app.Status.SubmissionAttempts = attempts
		recordRecoveryIntent(app, 11*time.Minute)
		submit := newControlledSubmit(false)
		for pass := range 3 {
			result, err := reconcileRecovery(newRecoveryReconciler(k8sClient), app)
			Expect(err).NotTo(HaveOccurred())
			latest := getRecoveryApplication(app)
			expected := v1beta2.ApplicationStateFailedSubmission
			if pass > 0 && policy != v1beta2.RestartPolicyAlways {
				expected = v1beta2.ApplicationStateFailed
			}
			Expect(latest.Status.AppState.State).To(Equal(expected))
			Expect(latest.Status.SubmissionAttempts).To(Equal(attempts + 1))
			Expect(latest.Status.SubmissionID).To(Equal(app.Status.SubmissionID))
			Expect(submit.invocations()).To(BeZero())
			if policy == v1beta2.RestartPolicyAlways {
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			}
		}
	}, Entry("Never", v1beta2.RestartPolicyNever, int32(0), int64(1)), Entry("exhausted OnFailure", v1beta2.RestartPolicyOnFailure, int32(1), int64(1)), Entry("waiting for backoff counts only once", v1beta2.RestartPolicyAlways, int32(0), int64(3600)))

	It("retries a recorded failure without waiting for the interrupted-submission deadline", func() {
		app := newRecoveryApplication(v1beta2.ApplicationStateFailedSubmission)
		app.Status.AppState.ErrorMessage = "spark-submit failed before driver creation"
		recordRecoveryIntent(app, time.Minute)
		submit := newControlledSubmit(false)
		_, err := reconcileRecovery(newRecoveryReconciler(k8sClient), app)
		Expect(err).NotTo(HaveOccurred())
		latest := getRecoveryApplication(app)
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateSubmitted))
		Expect(latest.Status.SubmissionAttempts).To(Equal(app.Status.SubmissionAttempts + 1))
		Expect(latest.Status.SubmissionID).To(Equal(submit.submissionID()))
		Expect(latest.Status.SubmissionID).NotTo(Equal(app.Status.SubmissionID))
		Expect(latest.Status.AppState.ErrorMessage).To(BeEmpty())
		Expect(submit.invocations()).To(Equal(1))
	})

	DescribeTable("recovers usable running and terminal drivers without recounting failures", func(phase corev1.PodPhase, savedFailure bool, expected v1beta2.ApplicationStateType) {
		app := newRecoveryApplication(v1beta2.ApplicationStateNew)
		if savedFailure {
			app.Status.AppState = v1beta2.ApplicationState{State: v1beta2.ApplicationStateFailedSubmission, ErrorMessage: "saved failure"}
			app.Status.SubmissionAttempts = 1
		}
		recordRecoveryIntent(app, time.Minute)
		pod := recoveryDriverForIntent(app, phase)
		submit := newControlledSubmit(false)
		_, err := reconcileRecovery(newRecoveryReconciler(k8sClient), app)
		Expect(err).NotTo(HaveOccurred())
		latest := getRecoveryApplication(app)
		Expect(latest.Status.AppState.State).To(Equal(expected))
		Expect(latest.Status.SubmissionAttempts).To(Equal(int32(1)))
		Expect(latest.Status.ExecutionAttempts).To(Equal(int32(1)))
		Expect(submit.invocations()).To(BeZero())
		if phase == corev1.PodSucceeded || phase == corev1.PodFailed {
			Expect(latest.Status.TerminationTime.IsZero()).To(BeFalse())
		}
		if phase == corev1.PodFailed {
			Expect(latest.Status.AppState.ErrorMessage).To(ContainSubstring("ExitCode: 42"))
		}
		assertRecoveryDriverUnchanged(pod)
	}, Entry("saved failure with running driver", corev1.PodRunning, true, v1beta2.ApplicationStateRunning), Entry("finished successfully", corev1.PodSucceeded, false, v1beta2.ApplicationStateSucceeding), Entry("finished with failure", corev1.PodFailed, false, v1beta2.ApplicationStateFailing))

	DescribeTable("recovers a driver after spark-submit fails without publishing a false failure", func(mode string) {
		app := newRecoveryApplication(v1beta2.ApplicationStateNew)
		submit := newControlledSubmit(true)
		path := filepath.Join(submit.dir, "bin", "spark-submit")
		body, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(path, append(body, []byte("\nexit 1\n")...), 0o755)).To(Succeed())
		var unavailable, publishedFailure atomic.Bool
		reader := recoveryIntercept(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, pod := obj.(*corev1.Pod); pod && unavailable.Load() {
				return apierrors.NewServiceUnavailable("driver read unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		}})
		writer := recoveryIntercept(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if obj.(*v1beta2.SparkApplication).Status.AppState.State == v1beta2.ApplicationStateFailedSubmission {
				publishedFailure.Store(true)
			}
			return c.SubResource(subresource).Update(ctx, obj, opts...)
		}})
		r := sparkapplication.NewReconciler(nil, k8sClient.Scheme(), writer, record.NewFakeRecorder(100), nil,
			sparkapplication.Options{APIReader: reader, SubmissionTimeout: 5 * time.Second})
		done := startRecoveryReconcile(r, app, submit)
		submit.waitForInvocation()
		pod := createRecoveryDriver(app, submit)
		intent := getRecoveryApplication(app)
		unavailable.Store(mode == "read outage")
		if mode != "timeout" {
			submit.release()
		}
		Eventually(done, 10*time.Second).Should(Receive())
		Expect(publishedFailure.Load()).To(BeFalse())
		if mode == "read outage" {
			Expect(getRecoveryApplication(app).Status).To(Equal(intent.Status))
			unavailable.Store(false)
			_, err = reconcileRecovery(r, app)
			Expect(err).NotTo(HaveOccurred())
		}
		latest := getRecoveryApplication(app)
		Expect(latest.Status.AppState.State).To(Equal(v1beta2.ApplicationStateRunning))
		Expect(latest.Status.SubmissionAttempts).To(Equal(int32(1)))
		Expect(latest.Status.ExecutionAttempts).To(Equal(int32(1)))
		Expect(submit.invocations()).To(Equal(1))
		assertRecoveryDriverUnchanged(pod)
	}, Entry("nonzero exit", "exit"), Entry("process timeout", "timeout"), Entry("driver read outage preserves intent", "read outage"))

	DescribeTable("never revives a retiring legacy run", func(invalidating bool) {
		app := newRecoveryApplication(v1beta2.ApplicationStatePendingRerun)
		app.Spec.Driver.CoreLimit = util.StringPtr("4")
		Expect(k8sClient.Update(context.Background(), app)).To(Succeed())
		app.Status.SubmissionID = "retired-run"
		app.Status.DriverInfo.PodName = util.GetDriverPodName(app)
		if invalidating {
			app.Status.AppState.State = v1beta2.ApplicationStateInvalidating
			app.Status.LastSubmissionAttemptTime = metav1.Now()
		}
		Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
		pod := createDriverPod(app.Name, app.Namespace)
		pod.Labels[common.LabelSubmissionID] = app.Status.SubmissionID
		pod.Finalizers = []string{"test.sparkoperator.k8s.io/hold-cleanup"}
		pod.Spec.TerminationGracePeriodSeconds = util.Int64Ptr(0)
		persistRecoveryDriver(app, pod)
		submit := newControlledSubmit(false)
		for range 2 {
			result, err := reconcileRecovery(newRecoveryReconciler(k8sClient), app)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			latest := getRecoveryApplication(app)
			Expect(latest.Status.AppState.State).To(BeElementOf(v1beta2.ApplicationStateInvalidating, v1beta2.ApplicationStatePendingRerun))
			Expect(latest.Status.LastSubmissionAttemptTime.IsZero()).To(BeTrue())
			Expect(latest.Status.SubmissionID).To(Equal("retired-run"))
			Expect(submit.invocations()).To(BeZero())
		}
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), pod)).To(Succeed())
		Expect(pod.DeletionTimestamp.IsZero()).To(BeFalse())
		pod.Finalizers = nil
		Expect(k8sClient.Update(context.Background(), pod)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}))
		}).Should(BeTrue())
		Eventually(func() int {
			_, err := reconcileRecovery(newRecoveryReconciler(k8sClient), app)
			Expect(err).NotTo(HaveOccurred())
			return submit.invocations()
		}).Should(Equal(1))
		Expect(getRecoveryApplication(app).Status.SubmissionID).To(Equal(submit.submissionID()))
		Expect(submit.conf(common.SparkKubernetesDriverLimitCores)).To(Equal("4"))
	}, Entry("already INVALIDATING", true), Entry("retired matching ID in PENDING_RERUN", false))

	DescribeTable("treats missing generated ConfigMaps differently from API errors", func(mode string) {
		app, pod := newPartialSubmission()
		if mode != "arrives" {
			app.Status.LastSubmissionAttemptTime = metav1.NewTime(time.Now().Add(-11 * time.Minute))
			Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
		}
		reader := recoveryIntercept(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.ConfigMap); ok && mode == "read outage" {
				return apierrors.NewServiceUnavailable("ConfigMap unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		}})
		var writes int
		writer := recoveryIntercept(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if obj.(*v1beta2.SparkApplication).Status.AppState.State == v1beta2.ApplicationStateFailedSubmission {
				writes++
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{})).To(Succeed(), "failure must be saved before deleting the partial driver")
				if mode == "write outage" {
					return apierrors.NewServiceUnavailable("status unavailable")
				}
				if mode == "write conflict" && writes == 1 {
					latest := getRecoveryApplication(app)
					latest.Annotations = map[string]string{"external-update": "preserved"}
					Expect(k8sClient.Update(ctx, latest)).To(Succeed())
				}
			}
			return c.SubResource(subresource).Update(ctx, obj, opts...)
		}})
		submit := newControlledSubmit(false)
		result, err := reconcileRecovery(newRecoveryReconciler(writer, reader), app)
		if strings.Contains(mode, "outage") {
			Expect(err).To(HaveOccurred())
			Expect(getRecoveryApplication(app).Status).To(Equal(app.Status))
			assertRecoveryDriverUnchanged(pod)
		} else if mode == "expires" || mode == "write conflict" {
			Expect(err).NotTo(HaveOccurred())
			expectedWrites := 1
			if mode == "write conflict" {
				expectedWrites = 2
			}
			Expect(writes).To(Equal(expectedWrites))
			Expect(getRecoveryApplication(app).Status.SubmissionAttempts).To(Equal(int32(1)))
			Expect(getRecoveryApplication(app).Status.AppState.ErrorMessage).To(ContainSubstring(pod.Spec.Volumes[0].ConfigMap.Name))
			Expect(apierrors.IsNotFound(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}))).To(BeTrue())
		} else {
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			Expect(getRecoveryApplication(app).Status).To(Equal(app.Status))
			assertRecoveryDriverUnchanged(pod)
			config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: pod.Spec.Volumes[0].ConfigMap.Name, Namespace: app.Namespace}}
			Expect(k8sClient.Create(context.Background(), config)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), config))).To(Succeed()) })
			_, err = reconcileRecovery(newRecoveryReconciler(k8sClient), app)
			Expect(err).NotTo(HaveOccurred())
			Expect(getRecoveryApplication(app).Status.AppState.State).To(Equal(v1beta2.ApplicationStateSubmitted))
		}
		Expect(submit.invocations()).To(BeZero())
	}, Entry("ConfigMap arrives before deadline", "arrives"), Entry("deadline expires", "expires"), Entry("ConfigMap read outage", "read outage"), Entry("failure write outage", "write outage"), Entry("failure write conflict", "write conflict"))

	DescribeTable("rejects ownership conflicts during recovery and cleanup", func(cleanup bool, conflictingStamp bool) {
		app := newRecoveryApplication(v1beta2.ApplicationStateNew)
		recordRecoveryIntent(app, time.Minute)
		pod := recoveryDriverForIntent(app, corev1.PodRunning)
		if cleanup {
			app.Status.AppState.State = v1beta2.ApplicationStateInvalidating
			Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
		}
		if conflictingStamp {
			pod.Annotations[common.AnnotationAppUID] = "other-app"
		} else {
			pod.Annotations = nil
			pod.OwnerReferences[0].UID = "other-app"
		}
		Expect(k8sClient.Update(context.Background(), pod)).To(Succeed())
		submit := newControlledSubmit(false)
		_, err := reconcileRecovery(newRecoveryReconciler(k8sClient), app)
		Expect(err).To(MatchError(ContainSubstring("does not belong")))
		Expect(getRecoveryApplication(app).Status).To(Equal(app.Status))
		Expect(submit.invocations()).To(BeZero())
		assertRecoveryDriverUnchanged(pod)
	}, Entry("conflicting stamp", false, true), Entry("foreign owner during recovery", false, false), Entry("foreign owner during cleanup", true, false))

	It("does not delete a replacement driver created after the cleanup read", func() {
		app := newRecoveryApplication(v1beta2.ApplicationStateInvalidating)
		recordRecoveryIntent(app, time.Minute)
		old := recoveryDriverForIntent(app, corev1.PodRunning)
		var replacement *corev1.Pod
		writer := recoveryIntercept(interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, pod := obj.(*corev1.Pod); pod && replacement == nil {
				Expect(c.Delete(ctx, obj, client.GracePeriodSeconds(0))).To(Succeed())
				latest := getRecoveryApplication(app)
				latest.Status.SubmissionID = "replacement-run"
				latest.Status.AppState.State = v1beta2.ApplicationStateRunning
				Expect(c.Status().Update(ctx, latest)).To(Succeed())
				replacement = createDriverPod(app.Name, app.Namespace)
				replacement.Labels[common.LabelSubmissionID] = latest.Status.SubmissionID
				replacement.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(app, v1beta2.SchemeGroupVersion.WithKind("SparkApplication"))}
				Expect(c.Create(ctx, replacement)).To(Succeed())
			}
			return c.Delete(ctx, obj, opts...)
		}})
		_, err := reconcileRecovery(newRecoveryReconciler(writer), app)
		Expect(err).NotTo(HaveOccurred())
		Expect(getRecoveryApplication(app).Status.SubmissionID).To(Equal("replacement-run"))
		Expect(getRecoveryApplication(app).Status.AppState.State).To(Equal(v1beta2.ApplicationStateRunning))
		Expect(replacement).NotTo(BeNil())
		Expect(replacement.UID).NotTo(Equal(old.UID))
		assertRecoveryDriverUnchanged(replacement)
	})

	DescribeTable("uses authoritative state despite stale cache reads", func(staleApp bool, staleAuthority bool) {
		app := newRecoveryApplication(v1beta2.ApplicationStateNew)
		cached := app.DeepCopy()
		recordRecoveryIntent(app, time.Minute)
		if !staleApp {
			app.Status.AppState.State = v1beta2.ApplicationStateRunning
			Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
		}
		pod := recoveryDriverForIntent(app, corev1.PodRunning)
		stale := recoveryIntercept(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if target, ok := obj.(*v1beta2.SparkApplication); ok && staleApp {
				cached.DeepCopyInto(target)
				return nil
			}
			if _, ok := obj.(*corev1.Pod); ok && !staleApp {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		}})
		var reader client.Reader = k8sClient
		if staleAuthority {
			reader = stale
		}
		submit := newControlledSubmit(false)
		_, err := reconcileRecovery(newRecoveryReconciler(stale, reader), app)
		if staleAuthority {
			Expect(err).To(HaveOccurred(), "negative control must not pass with stale authoritative state")
		} else {
			Expect(err).NotTo(HaveOccurred())
			Expect(getRecoveryApplication(app).Status.AppState.State).To(Equal(v1beta2.ApplicationStateRunning))
		}
		Expect(getRecoveryApplication(app).Status.SubmissionID).To(Equal(app.Status.SubmissionID))
		Expect(submit.invocations()).To(BeZero())
		assertRecoveryDriverUnchanged(pod)
	}, Entry("stale application", true, false), Entry("cached pod miss", false, false), Entry("negative control", true, true))
})

func reconcileRecovery(r *sparkapplication.Reconciler, app *v1beta2.SparkApplication) (reconcile.Result, error) {
	return r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})
}

func recordRecoveryIntent(app *v1beta2.SparkApplication, age time.Duration) {
	app.Status.SubmissionID = "recorded-run"
	app.Status.DriverInfo.PodName = util.GetDriverPodName(app)
	app.Status.LastSubmissionAttemptTime = metav1.NewTime(time.Now().Add(-age))
	Expect(k8sClient.Status().Update(context.Background(), app)).To(Succeed())
}

func recoveryDriverForIntent(app *v1beta2.SparkApplication, phase corev1.PodPhase) *corev1.Pod {
	pod := createDriverPod(app.Name, app.Namespace)
	pod.Labels[common.LabelSubmissionID] = app.Status.SubmissionID
	pod.Annotations = map[string]string{common.AnnotationAppUID: string(app.UID), common.AnnotationAppGeneration: strconv.FormatInt(app.Generation, 10)}
	persistRecoveryDriver(app, pod)
	pod.Status.Phase = phase
	if phase == corev1.PodSucceeded || phase == corev1.PodFailed {
		exitCode := int32(0)
		if phase == corev1.PodFailed {
			exitCode = 42
		}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: common.SparkDriverContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode, Reason: "completed"}}}}
	}
	Expect(k8sClient.Status().Update(context.Background(), pod)).To(Succeed())
	return pod
}

func assertRecoveryDriverUnchanged(pod *corev1.Pod) {
	current := &corev1.Pod{}
	Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), current)).To(Succeed())
	Expect(current.UID).To(Equal(pod.UID))
	Expect(current.Status.Phase).To(Equal(pod.Status.Phase))
	Expect(current.DeletionTimestamp.IsZero()).To(BeTrue())
}

func newPartialSubmission() (*v1beta2.SparkApplication, *corev1.Pod) {
	app := newRecoveryApplication(v1beta2.ApplicationStateNew)
	recordRecoveryIntent(app, 0)
	pod := createDriverPod(app.Name, app.Namespace)
	pod.Labels[common.LabelSubmissionID] = app.Status.SubmissionID
	pod.Spec.TerminationGracePeriodSeconds = util.Int64Ptr(0)
	pod.Spec.Volumes = []corev1.Volume{{Name: "spark-conf-volume-driver", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: app.Name + "-conf"}}}}}
	persistRecoveryDriver(app, pod)
	pod.Status.Phase = corev1.PodPending
	Expect(k8sClient.Status().Update(context.Background(), pod)).To(Succeed())
	return app, pod
}

// controlledSubmit records subprocess launches and can hold the first launch open
// while a test creates a driver, changes the application, or injects an API failure.
type controlledSubmit struct {
	dir string
}

// newControlledSubmit installs a fake spark-submit; block keeps its first call at
// a filesystem barrier until release is called, including across helper processes.
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

// release lets the first submission return so the reconciler can save its outcome.
func (s *controlledSubmit) release() {
	Expect(os.WriteFile(filepath.Join(s.dir, "release"), nil, 0o644)).To(Succeed())
}

// waitForInvocation places test mutations after process launch but before its outcome.
func (s *controlledSubmit) waitForInvocation() {
	Eventually(func() error {
		_, err := os.Stat(filepath.Join(s.dir, "entered"))
		return err
	}, 10*time.Second).Should(Succeed())
}

// invocations counts actual process starts to detect duplicate submission side effects.
func (s *controlledSubmit) invocations() int {
	data, err := os.ReadFile(filepath.Join(s.dir, "calls"))
	if os.IsNotExist(err) {
		return 0
	}
	Expect(err).NotTo(HaveOccurred())
	return strings.Count(string(data), "call\n")
}

// conf reads a Spark configuration value from the first process's recorded arguments.
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

// submissionID reads the identity passed to Spark, independently of application status.
func (s *controlledSubmit) submissionID() string {
	id := s.conf(fmt.Sprintf(common.SparkKubernetesDriverLabelTemplate, common.LabelSubmissionID))
	Expect(id).NotTo(BeEmpty())
	return id
}

// recoveryResult carries the reconcile result back across the submission barrier.
type recoveryResult struct {
	result reconcile.Result
	err    error
}

// startRecoveryReconcile runs reconciliation concurrently with test mutations and
// releases the submit barrier during cleanup so a failed assertion cannot strand it.
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

// newRecoveryReconciler allows write failures or cache lag on c while keeping
// authoritative reads connected to the real envtest API server.
func newRecoveryReconciler(c client.Client, readers ...client.Reader) *sparkapplication.Reconciler {
	var reader client.Reader = k8sClient
	if len(readers) != 0 {
		reader = readers[0]
	}
	return sparkapplication.NewReconciler(nil, k8sClient.Scheme(), c, record.NewFakeRecorder(100), nil, sparkapplication.Options{
		APIReader:                    reader,
		Namespaces:                   []string{"default"},
		DriverPodCreationGracePeriod: time.Second,
	})
}

// recoveryIntercept injects failures around a real API client, preserving server-side
// resource versions and status updates instead of simulating them in memory.
func recoveryIntercept(funcs interceptor.Funcs) client.Client {
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
	Expect(err).NotTo(HaveOccurred())
	return interceptor.NewClient(c, funcs)
}

// newRecoveryApplication creates an isolated application in the requested state;
// failed-submission fixtures start with one prior attempt already due for retry.
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

// getRecoveryApplication reads persisted status directly, avoiding the reader whose
// stale data or failures a test may have injected into the reconciler.
func getRecoveryApplication(app *v1beta2.SparkApplication) *v1beta2.SparkApplication {
	latest := &v1beta2.SparkApplication{}
	Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(app), latest)).To(Succeed())
	return latest
}

// createRecoveryDriver emulates Spark's driver creation using identity arguments
// captured from the fake process, with the owner reference normally added by the webhook.
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

// persistRecoveryDriver creates an owned, running driver without a kubelet; cleanup
// removes any test finalizer because envtest has no controller to release it.
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
