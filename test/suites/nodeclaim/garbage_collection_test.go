/*
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

package nodeclaim_test

import (
	"encoding/base64"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	coretest "sigs.k8s.io/karpenter/pkg/test"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	awserrors "github.com/aws/karpenter-provider-aws/pkg/errors"
	"github.com/aws/karpenter-provider-aws/pkg/utils"
	environmentaws "github.com/aws/karpenter-provider-aws/test/pkg/environment/aws"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("GarbageCollection", func() {
	var customAMI string
	var instanceInput *ec2.RunInstancesInput
	var instanceProfileName string
	var roleName string

	BeforeEach(func() {
		securityGroups := env.GetSecurityGroups(map[string]string{"karpenter.sh/discovery": env.ClusterName})
		subnets := env.GetSubnetInfo(map[string]string{"karpenter.sh/discovery": env.ClusterName})
		Expect(securityGroups).ToNot(HaveLen(0))
		Expect(subnets).ToNot(HaveLen(0))

		customAMI = env.GetAMIBySSMPath(fmt.Sprintf("/aws/service/eks/optimized-ami/%s/amazon-linux-2023/x86_64/standard/recommended/image_id", env.K8sVersion()))
		instanceProfileName = fmt.Sprintf("KarpenterNodeInstanceProfile-%s", env.ClusterName)
		roleName = fmt.Sprintf("KarpenterNodeRole-%s", env.ClusterName)
		instanceInput = &ec2.RunInstancesInput{
			InstanceType: "c5.large",
			IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{
				Name: aws.String(instanceProfileName),
			},
			SecurityGroupIds: lo.Map(securityGroups, func(s environmentaws.SecurityGroup, _ int) string {
				return *s.GroupIdentifier.GroupId
			}),
			SubnetId: aws.String(subnets[0].ID),
			BlockDeviceMappings: []ec2types.BlockDeviceMapping{
				{
					DeviceName: aws.String("/dev/xvda"),
					Ebs: &ec2types.EbsBlockDevice{
						Encrypted:           aws.Bool(true),
						DeleteOnTermination: aws.Bool(true),
						VolumeType:          ec2types.VolumeTypeGp3,
						VolumeSize:          aws.Int32(20),
					},
				},
			},
			ImageId: aws.String(customAMI), // EKS AL2-based AMI
			TagSpecifications: []ec2types.TagSpecification{
				{
					ResourceType: ec2types.ResourceTypeInstance,
					Tags: []ec2types.Tag{
						{
							Key:   aws.String(fmt.Sprintf("kubernetes.io/cluster/%s", env.ClusterName)),
							Value: aws.String("owned"),
						},
						{
							Key:   aws.String(karpv1.NodePoolLabelKey),
							Value: aws.String(nodePool.Name),
						},
						{
							Key:   aws.String(v1.LabelNodeClass),
							Value: aws.String(nodeClass.Name),
						},
					},
				},
			},
			MinCount: aws.Int32(1),
			MaxCount: aws.Int32(1),
		}
	})
	It("should succeed to garbage collect an Instance that was launched by a NodeClaim but has no Instance mapping", func() {
		// Update the userData for the instance input with the correct NodePool
		rawContent, err := os.ReadFile("testdata/al2023_userdata_input.yaml")
		Expect(err).ToNot(HaveOccurred())
		instanceInput.UserData = lo.ToPtr(base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(string(rawContent), env.ClusterName,
			env.ClusterEndpoint, env.ExpectCABundle()))))
		env.ExpectInstanceProfileCreated(instanceProfileName, roleName)
		DeferCleanup(func() {
			env.ExpectInstanceProfileDeleted(instanceProfileName, roleName)
		})
		// Create an instance manually to mock Karpenter launching an instance
		out := env.EventuallyExpectRunInstances(instanceInput)
		Expect(out.Instances).To(HaveLen(1))

		DeferCleanup(func() {
			_, err := env.EC2API.TerminateInstances(env.Context, &ec2.TerminateInstancesInput{
				InstanceIds: []string{*out.Instances[0].InstanceId},
			})
			Expect(awserrors.IgnoreNotFound(err)).ToNot(HaveOccurred())
		})

		// Wait for the node to register with the cluster
		node := env.EventuallyExpectCreatedNodeCount("==", 1)[0]

		// Update the tags to add the EKSClusterNameTagKey tag
		_, err = env.EC2API.CreateTags(env.Context, &ec2.CreateTagsInput{
			Resources: []string{*out.Instances[0].InstanceId},
			Tags: []ec2types.Tag{
				{
					Key:   aws.String(v1.EKSClusterNameTagKey),
					Value: aws.String(env.ClusterName),
				},
			},
		})
		Expect(err).ToNot(HaveOccurred())
		// Eventually expect the node and the instance to be removed (shutting-down)
		env.EventuallyExpectNotFound(node)
		Eventually(func(g Gomega) {
			g.Expect(string(env.GetInstanceByID(aws.ToString(out.Instances[0].InstanceId)).State.Name)).To(BeElementOf("terminated", "shutting-down"))
		}, time.Second*10).Should(Succeed())
	})
	It("should succeed to garbage collect an Instance that was deleted without the cluster's knowledge", func() {
		// Disable the interruption queue for the garbage collection coretest
		env.ExpectSettingsOverridden(corev1.EnvVar{Name: "INTERRUPTION_QUEUE", Value: ""})
		pod := coretest.Pod()
		env.ExpectCreated(nodeClass, nodePool, pod)
		env.EventuallyExpectHealthy(pod)
		node := env.ExpectCreatedNodeCount("==", 1)[0]

		_, err := env.EC2API.TerminateInstances(env.Context, &ec2.TerminateInstancesInput{
			InstanceIds: []string{lo.Must(utils.ParseInstanceID(node.Spec.ProviderID))},
		})
		Expect(err).ToNot(HaveOccurred())

		// The garbage collection mechanism should eventually delete this NodeClaim and Node
		env.EventuallyExpectNotFound(node)
	})
})
