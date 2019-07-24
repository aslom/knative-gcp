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

package channel

import (
	"context"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	"go.uber.org/zap"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/tracker"

	eventingduck "github.com/knative/eventing/pkg/apis/duck/v1alpha1"

	"github.com/GoogleCloudPlatform/cloud-run-events/pkg/apis/events/v1alpha1"
	pubsubv1alpha1 "github.com/GoogleCloudPlatform/cloud-run-events/pkg/apis/pubsub/v1alpha1"
	listers "github.com/GoogleCloudPlatform/cloud-run-events/pkg/client/listers/events/v1alpha1"
	pubsublisters "github.com/GoogleCloudPlatform/cloud-run-events/pkg/client/listers/pubsub/v1alpha1"
	"github.com/GoogleCloudPlatform/cloud-run-events/pkg/reconciler"
	"github.com/GoogleCloudPlatform/cloud-run-events/pkg/reconciler/channel/resources"
)

const (
	// ReconcilerName is the name of the reconciler
	ReconcilerName = "Channels"
)

// Reconciler implements controller.Reconciler for Channel resources.
type Reconciler struct {
	*reconciler.Base

	// listers index properties about resources
	channelLister      listers.ChannelLister
	topicLister        pubsublisters.TopicLister
	subscriptionLister pubsublisters.PullSubscriptionLister

	tracker tracker.Interface // TODO: use tracker for sink.
}

// Check that our Reconciler implements controller.Reconciler
var _ controller.Reconciler = (*Reconciler)(nil)

// Reconcile compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the Service resource
// with the current status of the resource.
func (c *Reconciler) Reconcile(ctx context.Context, key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		c.Logger.Errorf("invalid resource key: %s", key)
		return nil
	}
	logger := logging.FromContext(ctx)

	// Get the Channel resource with this namespace/name
	original, err := c.channelLister.Channels(namespace).Get(name)
	if apierrs.IsNotFound(err) {
		// The resource may no longer exist, in which case we stop processing.
		logger.Errorf("Channel %q in work queue no longer exists", key)
		return nil
	} else if err != nil {
		return err
	}

	if original.GetDeletionTimestamp() != nil {
		return nil
	}

	// Don't modify the informers copy
	channel := original.DeepCopy()

	// Reconcile this copy of the channel and then write back any status
	// updates regardless of whether the reconciliation errored out.
	var reconcileErr = c.reconcile(ctx, channel)

	// If no error is returned, mark the observed generation.
	if reconcileErr == nil {
		channel.Status.ObservedGeneration = channel.Generation
	}

	if equality.Semantic.DeepEqual(original.Status, channel.Status) {
		// If we didn't change anything then don't call updateStatus.
		// This is important because the copy we loaded from the informer's
		// cache may be stale and we don't want to overwrite a prior update
		// to status with this stale state.

	} else if _, uErr := c.updateStatus(ctx, channel); uErr != nil {
		logger.Warnw("Failed to update Channel status", zap.Error(uErr))
		c.Recorder.Eventf(channel, corev1.EventTypeWarning, "UpdateFailed",
			"Failed to update status for Channel %q: %v", channel.Name, uErr)
		return uErr
	} else if reconcileErr == nil {
		// There was a difference and updateStatus did not return an error.
		c.Recorder.Eventf(channel, corev1.EventTypeNormal, "Updated", "Updated Channel %q", channel.GetName())
	}
	if reconcileErr != nil {
		c.Recorder.Event(channel, corev1.EventTypeWarning, "InternalError", reconcileErr.Error())
	}
	return reconcileErr
}

func (r *Reconciler) reconcile(ctx context.Context, channel *v1alpha1.Channel) error {
	logger := logging.FromContext(ctx)

	channel.Status.InitializeConditions()

	if channel.Status.TopicID == "" {
		channel.Status.TopicID = resources.GenerateTopicName(channel.Name, channel.UID)
	}

	// 1. Create the Topic.
	if topic, err := r.createTopic(ctx, channel); err != nil {
		channel.Status.MarkNoTopic("FailedCreate", "Error when attempting to create Topic.")
		return err
	} else {
		// Propagate Status.
		if c := topic.Status.GetCondition(pubsubv1alpha1.TopicConditionReady); c != nil {
			if c.IsTrue() {
				channel.Status.MarkTopicReady()
			} else if c.IsUnknown() {
				channel.Status.MarkTopicOperating(c.Reason, c.Message)
			} else if c.IsFalse() {
				channel.Status.MarkNoTopic(c.Reason, c.Message)
			}
		}
	}
	// 2. Sync all subscriptions.
	//   a. create all subscriptions that are in spec and not in status.
	//   b. delete all subscriptions that are in status but not in spec.
	if err := r.syncSubscribers(ctx, channel); err != nil {
		return err
	}

	_ = logger // TODO

	return nil
}

func (c *Reconciler) updateStatus(ctx context.Context, desired *v1alpha1.Channel) (*v1alpha1.Channel, error) {
	channel, err := c.channelLister.Channels(desired.Namespace).Get(desired.Name)
	if err != nil {
		return nil, err
	}
	// If there's nothing to update, just return.
	if reflect.DeepEqual(channel.Status, desired.Status) {
		return channel, nil
	}
	becomesReady := desired.Status.IsReady() && !channel.Status.IsReady()
	// Don't modify the informers copy.
	existing := channel.DeepCopy()
	existing.Status = desired.Status

	ch, err := c.RunClientSet.EventsV1alpha1().Channels(desired.Namespace).UpdateStatus(existing)
	if err == nil && becomesReady {
		duration := time.Since(ch.ObjectMeta.CreationTimestamp.Time)
		c.Logger.Infof("Channel %q became ready after %v", channel.Name, duration)

		if err := c.StatsReporter.ReportReady("Channel", channel.Namespace, channel.Name, duration); err != nil {
			logging.FromContext(ctx).Infof("failed to record ready for Channel, %v", err)
		}
	}

	return ch, err
}

func (c *Reconciler) syncSubscribers(ctx context.Context, channel *v1alpha1.Channel) error {
	subC := []eventingduck.SubscriberSpec(nil)
	subU := []eventingduck.SubscriberSpec(nil)
	subD := []eventingduck.SubscriberStatus(nil)

	exists := make(map[types.UID]eventingduck.SubscriberStatus)
	for _, s := range channel.Status.Subscribers {
		exists[s.UID] = s
	}

	if channel.Spec.Subscribable != nil {
		for _, want := range channel.Spec.Subscribable.Subscribers {
			if got, ok := exists[want.UID]; !ok {
				// If it does not exist, then update it.
				subC = append(subC, want)
			} else {
				if got.ObservedGeneration != want.Generation {
					subU = append(subU)
				}
			}
			// Remove want from exists.
			delete(exists, want.UID)
		}
	}

	// Remaining exists will be deleted.
	for _, e := range exists {
		subD = append(subD, e)
	}

	for _, s := range subC {
		genName := resources.GenerateSubscriptionName(channel.Name, s.UID)
		c.Logger.Infof("Channel %q will create subscription %s", channel.Name, genName)
	}
	for _, s := range subU {
		genName := resources.GenerateSubscriptionName(channel.Name, s.UID)
		c.Logger.Infof("Channel %q will update subscription %s", channel.Name, genName)
	}
	for _, s := range subD {
		genName := resources.GenerateSubscriptionName(channel.Name, s.UID)
		c.Logger.Infof("Channel %q will delete subscription %s", channel.Name, genName)
	}

	return nil
}

func (r *Reconciler) createTopic(ctx context.Context, channel *v1alpha1.Channel) (*pubsubv1alpha1.Topic, error) {
	topic, err := r.getTopic(ctx, channel)
	if err != nil && !apierrors.IsNotFound(err) {
		logging.FromContext(ctx).Error("Unable to get an Topic", zap.Error(err))
		return nil, err
	}
	if topic != nil {
		logging.FromContext(ctx).Desugar().Info("Reusing existing Topic", zap.Any("topic", topic))
		if topic.Status.Address != nil {
			channel.Status.SetAddress(topic.Status.Address.URL)
		} else {
			channel.Status.SetAddress(nil)
		}
		return topic, nil
	}
	topic, err = r.RunClientSet.PubsubV1alpha1().Topics(channel.Namespace).Create(resources.MakeTopic(&resources.TopicArgs{
		Owner:   channel,
		Project: channel.Spec.Project,
		Secret:  channel.Spec.Secret,
		Topic:   channel.Status.TopicID,
		Labels:  resources.GetLabels(controllerAgentName, channel.Name),
	}))
	if err != nil {
		logging.FromContext(ctx).Info("Topic created.", zap.Error(err), zap.Any("topic", topic))
		r.Recorder.Eventf(channel, corev1.EventTypeNormal, "TopicCreated", "Created Topic %q", topic.GetName())
	}
	return topic, err
}

func (r *Reconciler) getTopic(ctx context.Context, channel *v1alpha1.Channel) (*pubsubv1alpha1.Topic, error) {
	tl, err := r.RunClientSet.PubsubV1alpha1().Topics(channel.Namespace).List(metav1.ListOptions{
		LabelSelector: resources.GetLabelSelector(controllerAgentName, channel.Name).String(),
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.SchemeGroupVersion.String(),
			Kind:       "Channel",
		},
	})

	if err != nil {
		logging.FromContext(ctx).Error("Unable to list topics: %v", zap.Error(err))
		return nil, err
	}
	for _, topic := range tl.Items {
		if metav1.IsControlledBy(&topic, channel) {
			return &topic, nil
		}
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{}, "")
}

func (r *Reconciler) getPullSubscription(ctx context.Context, channel *v1alpha1.Channel) (*pubsubv1alpha1.PullSubscription, error) {
	sl, err := r.RunClientSet.PubsubV1alpha1().PullSubscriptions(channel.Namespace).List(metav1.ListOptions{
		LabelSelector: resources.GetLabelSelector(controllerAgentName, channel.Name).String(),
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.SchemeGroupVersion.String(),
			Kind:       "Channel",
		},
	})

	if err != nil {
		logging.FromContext(ctx).Error("Unable to list pullsubscriptions: %v", zap.Error(err))
		return nil, err
	}
	for _, subscription := range sl.Items {
		if metav1.IsControlledBy(&subscription, channel) {
			return &subscription, nil
		}
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{}, "")
}
