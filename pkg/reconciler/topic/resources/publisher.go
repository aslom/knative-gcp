/*
Copyright 2019 Google LLC

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

package resources

import (
	"fmt"

	servingv1beta1 "github.com/knative/serving/pkg/apis/serving/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/kmeta"

	"github.com/GoogleCloudPlatform/cloud-run-events/pkg/apis/pubsub/v1alpha1"
)

// PublisherArgs are the arguments needed to create a Topic publisher.
// Every field is required.
type PublisherArgs struct {
	Image  string
	Topic  *v1alpha1.Topic
	Labels map[string]string
}

const (
	credsVolume    = "google-cloud-key"
	credsMountPath = "/var/secrets/google"
)

// DefaultSecretSelector is the default secret selector used to load the creds
// for the publisher to auth with Google Cloud.
func DefaultSecretSelector() *corev1.SecretKeySelector {
	return &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{
			Name: "google-cloud-key",
		},
		Key: "key.json",
	}
}

// MakePublisher generates (but does not insert into K8s) the Invoker Deployment for
// Channels.
func MakePublisher(args *PublisherArgs) *servingv1beta1.Service {
	secret := args.Topic.Spec.Secret
	if secret == nil {
		secret = DefaultSecretSelector()
	}

	credsFile := fmt.Sprintf("%s/%s", credsMountPath, secret.Key)

	podSpec := corev1.PodSpec{
		ServiceAccountName: args.Topic.Spec.ServiceAccountName,
		Containers: []corev1.Container{{
			Name:  "publisher",
			Image: args.Image,
			Env: []corev1.EnvVar{{
				Name:  "GOOGLE_APPLICATION_CREDENTIALS",
				Value: credsFile,
			}, {
				Name:  "PROJECT_ID",
				Value: args.Topic.Spec.Project,
			}, {
				Name:  "PUBSUB_TOPIC_ID",
				Value: args.Topic.Spec.Topic,
			}},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      credsVolume,
				MountPath: credsMountPath,
			}}},
		},
		Volumes: []corev1.Volume{{
			Name: credsVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: secret.Name,
				},
			},
		}},
	}

	return &servingv1beta1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    args.Topic.Namespace,
			GenerateName: fmt.Sprintf("pubsub-publisher-%s-", args.Topic.Name),
			// TODO: not sure we should use labels like this. It depends on the
			//  upstream caller to use the correct label creation method.
			Labels:          args.Labels,
			OwnerReferences: []metav1.OwnerReference{*kmeta.NewControllerRef(args.Topic)},
		},
		Spec: servingv1beta1.ServiceSpec{
			ConfigurationSpec: servingv1beta1.ConfigurationSpec{
				Template: servingv1beta1.RevisionTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: args.Labels,
					},
					Spec: servingv1beta1.RevisionSpec{
						PodSpec: podSpec,
					},
				},
			},
		},
	}
}