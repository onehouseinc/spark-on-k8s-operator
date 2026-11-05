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

package sparkapplication

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kubeflow/spark-operator/api/v1beta2"
	"github.com/kubeflow/spark-operator/pkg/common"
	"github.com/kubeflow/spark-operator/pkg/util"
)

func TestSubmissionProcessTimeout(t *testing.T) {
	installSparkSubmit(t, "exec sleep 30\n")
	start := time.Now()
	err := runSparkSubmit(&submission{attemptStartTime: start}, 50*time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 5*time.Second)
}

func TestSubmissionProcessResults(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want string
	}{
		{name: "success", body: "exit 0\n"},
		{name: "failure", body: "echo 'invalid Spark configuration' >&2\nexit 1\n", want: "invalid Spark configuration"},
		{name: "existing driver", body: "echo 'code=409' >&2\nexit 1\n", want: "driver pod already exist"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			installSparkSubmit(t, tt.body)
			err := runSparkSubmit(&submission{attemptStartTime: time.Now()}, 5*time.Second)
			if tt.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.want)
			}
		})
	}
}

func TestSubmissionProcessArguments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "arguments")
	t.Setenv("SUBMISSION_TEST_ARGUMENTS", path)
	installSparkSubmit(t, "printf '%s\\n' \"$@\" > \"$SUBMISSION_TEST_ARGUMENTS\"\n")
	args := []string{"--conf", "spark.test=value with spaces", "local:///app.jar"}
	require.NoError(t, runSparkSubmit(&submission{args: args, attemptStartTime: time.Now()}, 5*time.Second))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, strings.Join(args, "\n")+"\n", string(got))
}

// installSparkSubmit points SPARK_HOME at a temporary executable so tests exercise
// real subprocess deadlines, argument passing and stderr without launching Spark.
func installSparkSubmit(t *testing.T, body string) {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	require.NoError(t, os.Mkdir(bin, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "spark-submit"), []byte("#!/bin/sh\n"+body), 0755))
	t.Setenv(common.EnvSparkHome, home)
}

func TestSubmissionArgumentsPreserveIdentity(t *testing.T) {
	t.Setenv(common.EnvKubernetesServiceHost, "localhost")
	t.Setenv(common.EnvKubernetesServicePort, "443")

	for _, override := range []bool{false, true} {
		name := "defaults"
		if override {
			name = "user overrides"
		}
		t.Run(name, func(t *testing.T) {
			app := &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test",
					Namespace:  "default",
					UID:        "application-uid",
					Generation: 7,
				},
				Spec: v1beta2.SparkApplicationSpec{
					Image:        util.StringPtr("spark:test"),
					SparkVersion: "3.5.3",
					SparkConf:    map[string]string{"spark.sql.shuffle.partitions": "8"},
					Driver: v1beta2.DriverSpec{SparkPodSpec: v1beta2.SparkPodSpec{
						Annotations: map[string]string{"team": "data"},
					}},
				},
				Status: v1beta2.SparkApplicationStatus{SubmissionID: "submission-id"},
			}
			if override {
				app.Labels = map[string]string{
					common.LabelSparkAppName: "wrong-app",
					common.LabelSubmissionID: "wrong-submission",
				}
				app.Spec.Driver.Labels = map[string]string{
					common.LabelSparkAppName: "another-app",
					common.LabelSubmissionID: "another-submission",
				}
				app.Spec.Executor.Labels = app.Spec.Driver.Labels
				for _, role := range []string{"driver", "executor"} {
					app.Spec.SparkConf["spark.kubernetes."+role+".label."+common.LabelSparkAppName] = "spark-conf-app"
					app.Spec.SparkConf["spark.kubernetes."+role+".label."+common.LabelSubmissionID] = "spark-conf-submission"
				}
				app.Spec.SparkConf[common.SparkKubernetesSubmissionWaitAppCompletion] = "true"
				app.Spec.SparkConf["spark.kubernetes.driver.annotation.sparkoperator.k8s.io/app-uid"] = "wrong-uid"
				app.Spec.SparkConf["spark.kubernetes.driver.annotation.sparkoperator.k8s.io/app-generation"] = "1"
				app.Spec.Driver.Annotations["sparkoperator.k8s.io/app-uid"] = "another-uid"
				app.Spec.Driver.Annotations["sparkoperator.k8s.io/app-generation"] = "2"
			}
			v1beta2.SetSparkApplicationDefaults(app)

			args, err := buildSparkSubmitArgs(app)
			require.NoError(t, err)
			confs := make(map[string][]string)
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--conf" {
					key, value, _ := strings.Cut(args[i+1], "=")
					confs[key] = append(confs[key], value)
					i++
				}
			}
			for key, value := range map[string]string{
				common.SparkKubernetesSubmissionWaitAppCompletion:                        "false",
				"spark.kubernetes.driver.annotation.sparkoperator.k8s.io/app-uid":        "application-uid",
				"spark.kubernetes.driver.annotation.sparkoperator.k8s.io/app-generation": "7",
				"spark.kubernetes.driver.annotation.team":                                "data",
				"spark.sql.shuffle.partitions":                                           "8",
				"spark.kubernetes.driver.label.sparkoperator.k8s.io/app-name":            "test",
				"spark.kubernetes.driver.label.sparkoperator.k8s.io/submission-id":       "submission-id",
				"spark.kubernetes.executor.label.sparkoperator.k8s.io/app-name":          "test",
				"spark.kubernetes.executor.label.sparkoperator.k8s.io/submission-id":     "submission-id",
			} {
				require.Equal(t, []string{value}, confs[key], key)
			}
		})
	}
}

func TestSubmissionDeadlineIncludesPreparation(t *testing.T) {
	t.Setenv(common.EnvKubernetesServiceHost, "localhost")
	t.Setenv(common.EnvKubernetesServicePort, "443")
	invocation := filepath.Join(t.TempDir(), "invoked")
	t.Setenv("SUBMISSION_TEST_INVOCATION", invocation)
	installSparkSubmit(t, "touch \"$SUBMISSION_TEST_INVOCATION\"\n")

	app := &v1beta2.SparkApplication{
		ObjectMeta: metav1.ObjectMeta{Name: "deadline", Namespace: "default", UID: "app-uid", Generation: 1},
		Spec: v1beta2.SparkApplicationSpec{
			Image:        util.StringPtr("spark:test"),
			SparkVersion: "3.5.3",
		},
		Status: v1beta2.SparkApplicationStatus{SubmissionID: "recorded-attempt", LastSubmissionAttemptTime: metav1.Now()},
	}
	v1beta2.SetSparkApplicationDefaults(app)
	timeout := 5 * time.Second
	deadline, cancel := context.WithDeadline(context.Background(), app.Status.LastSubmissionAttemptTime.Add(timeout))
	defer cancel()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	prepared := false
	slow := interceptor.NewClient(c, interceptor.Funcs{
		Create: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*corev1.Service); ok {
				<-deadline.Done()
				prepared = true
			}
			return underlying.Create(ctx, obj, opts...)
		},
	})
	r := NewReconciler(nil, scheme, slow, record.NewFakeRecorder(10), nil, Options{
		APIReader: c, EnableUIService: true, SubmissionTimeout: timeout,
	})

	err := r.submitSparkApplication(app)

	require.True(t, prepared)
	require.ErrorContains(t, err, "spark-submit timed out after 5s")
	_, err = os.Stat(invocation)
	require.ErrorIs(t, err, os.ErrNotExist, "an expired intent must not launch a new submit process")
}
