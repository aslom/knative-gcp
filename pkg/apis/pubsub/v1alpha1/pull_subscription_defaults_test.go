/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPullSubscriptionDefaults(t *testing.T) {
	tests := []struct {
		name  string
		start *PullSubscription
		want  *PullSubscription
	}{{
		name: "non-nil",
		start: &PullSubscription{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{},
			},
			Spec: PullSubscriptionSpec{},
		},
		want: &PullSubscription{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					PubSubModeAnnotation: PubSubModeCloudEventsBinary,
				},
			},
			Spec: PullSubscriptionSpec{},
		},
	}, {
		name: "nil annotations",
		start: &PullSubscription{
			ObjectMeta: metav1.ObjectMeta{},
			Spec:       PullSubscriptionSpec{},
		},
		want: &PullSubscription{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					PubSubModeAnnotation: PubSubModeCloudEventsBinary,
				},
			},
			Spec: PullSubscriptionSpec{},
		},
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := test.start
			got.SetDefaults(context.Background())

			if diff := cmp.Diff(test.want, got); diff != "" {
				t.Errorf("failed to get expected (-want, +got) = %v", diff)
			}
		})
	}
}