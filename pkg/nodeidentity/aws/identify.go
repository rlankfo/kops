/*
Copyright 2019 The Kubernetes Authors.

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

package aws

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	corev1 "k8s.io/api/core/v1"
	expirationcache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kops/pkg/nodeidentity"
)

const (
	// CloudTagInstanceGroupName is a cloud tag that defines the instance group name
	// This is used by the aws nodeidentifier to securely identify the node instancegroup
	CloudTagInstanceGroupName = "kops.k8s.io/instancegroup"
	// ClusterAutoscalerNodeTemplateLabel is the prefix used on node labels when copying to cloud tags.
	ClusterAutoscalerNodeTemplateLabel = "k8s.io/cluster-autoscaler/node-template/label/"
	// The expiration time of nodeidentity.Info cache.
	cacheTTL = 60 * time.Minute
)

// nodeIdentifier identifies a node from EC2
type nodeIdentifier struct {
	// client is the ec2 interface
	ec2Client ec2iface.EC2API

	// cache is a cache of nodeidentity.Info
	cache expirationcache.Store
	// cacheEnabled indicates if caching should be used
	cacheEnabled bool
}

// New creates and returns a nodeidentity.Identifier for Nodes running on AWS
func New(CacheNodeidentityInfo bool) (nodeidentity.Identifier, error) {
	config := aws.NewConfig()
	config = config.WithCredentialsChainVerboseErrors(true)

	s, err := session.NewSession(config)
	if err != nil {
		return nil, fmt.Errorf("error starting new AWS session: %v", err)
	}
	s.Handlers.Send.PushFront(func(r *request.Request) {
		// Log requests
		klog.V(4).Infof("AWS API Request: %s/%s", r.ClientInfo.ServiceName, r.Operation.Name)
	})

	metadata := ec2metadata.New(s, config)

	region, err := metadata.Region()
	if err != nil {
		return nil, fmt.Errorf("error querying ec2 metadata service (for region): %v", err)
	}

	ec2Client := ec2.New(s, config.WithRegion(region))

	return &nodeIdentifier{
		ec2Client:    ec2Client,
		cache:        expirationcache.NewTTLStore(stringKeyFunc, cacheTTL),
		cacheEnabled: CacheNodeidentityInfo,
	}, nil
}

// stringKeyFunc is a string as cache key function
func stringKeyFunc(obj interface{}) (string, error) {
	key := obj.(*nodeidentity.Info).InstanceID
	return key, nil
}

// IdentifyNode queries AWS for the node identity information
func (i *nodeIdentifier) IdentifyNode(ctx context.Context, node *corev1.Node) (*nodeidentity.Info, error) {
	providerID := node.Spec.ProviderID
	if providerID == "" {
		return nil, fmt.Errorf("providerID was not set for node %s", node.Name)
	}
	if !strings.HasPrefix(providerID, "aws://") {
		return nil, fmt.Errorf("providerID %q not recognized for node %s", providerID, node.Name)
	}

	tokens := strings.Split(strings.TrimPrefix(providerID, "aws://"), "/")
	if len(tokens) != 3 {
		return nil, fmt.Errorf("providerID %q not recognized for node %s", providerID, node.Name)
	}

	// zone := tokens[1]
	instanceID := tokens[2]

	// If caching is enabled try pulling nodeidentity.Info from cache before
	// doing a EC2 API call.
	if i.cacheEnabled {
		obj, exists, err := i.cache.GetByKey(instanceID)
		if err != nil {
			klog.Warningf("Nodeidentity info cache lookup failure: %v", err)
		}
		if exists {
			return obj.(*nodeidentity.Info), nil
		}
	}

	// Based on node-authorizer code
	instance, err := i.getInstance(instanceID)
	if err != nil {
		return nil, err
	}

	instanceState := "?"
	if instance.State != nil {
		instanceState = aws.StringValue(instance.State.Name)
	}
	if instanceState != ec2.InstanceStateNameRunning {
		return nil, fmt.Errorf("found instance %q, but state is %q", instanceID, instanceState)
	}

	labels := map[string]string{}
	if instance.InstanceLifecycle != nil {
		labels[fmt.Sprintf("node-role.kubernetes.io/%s-worker", *instance.InstanceLifecycle)] = "true"
	}

	info := &nodeidentity.Info{
		InstanceID: instanceID,
		Labels:     labels,
	}

	for _, tag := range instance.Tags {
		if strings.HasPrefix(aws.StringValue(tag.Key), ClusterAutoscalerNodeTemplateLabel) {
			info.Labels[strings.TrimPrefix(aws.StringValue(tag.Key), ClusterAutoscalerNodeTemplateLabel)] = aws.StringValue(tag.Value)
		}
	}

	// If caching is enabled add the nodeidentity.Info to cache.
	if i.cacheEnabled {
		err = i.cache.Add(info)
		if err != nil {
			klog.Warningf("Failed to add node identity info to cache: %v", err)
		}
	}

	return info, nil
}

// getInstance queries EC2 for the instance with the specified ID, returning an error if not found
func (i *nodeIdentifier) getInstance(instanceID string) (*ec2.Instance, error) {
	// Based on node-authorizer code
	resp, err := i.ec2Client.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: aws.StringSlice([]string{instanceID}),
	})
	if err != nil {
		return nil, fmt.Errorf("error from ec2 DescribeInstances request: %v", err)
	}

	// @check we found some instances
	if len(resp.Reservations) <= 0 || len(resp.Reservations[0].Instances) <= 0 {
		return nil, fmt.Errorf("missing instance id: %s", instanceID)
	}
	if len(resp.Reservations[0].Instances) > 1 {
		return nil, fmt.Errorf("found multiple instances with instance id: %s", instanceID)
	}

	instance := resp.Reservations[0].Instances[0]
	return instance, nil
}
