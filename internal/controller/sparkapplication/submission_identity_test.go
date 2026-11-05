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
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubeflow/spark-operator/api/v1beta2"
	"github.com/kubeflow/spark-operator/pkg/common"
	"github.com/kubeflow/spark-operator/pkg/util"
)

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
			if err != nil {
				t.Fatal(err)
			}
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
				if got := confs[key]; !reflect.DeepEqual(got, []string{value}) {
					t.Errorf("%s = %v, want exactly [%s]", key, got, value)
				}
			}
		})
	}
}
