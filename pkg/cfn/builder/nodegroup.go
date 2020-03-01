package builder

import (
	"fmt"

	cfn "github.com/aws/aws-sdk-go/service/cloudformation"
	gfn "github.com/awslabs/goformation/cloudformation"

	"github.com/kris-nova/logger"

	api "github.com/weaveworks/eksctl/pkg/apis/eksctl.io/v1alpha5"
	"github.com/weaveworks/eksctl/pkg/cfn/outputs"
	"github.com/weaveworks/eksctl/pkg/nodebootstrap"
)

// NodeGroupResourceSet stores the resource information of the nodegroup
type NodeGroupResourceSet struct {
	rs                   *resourceSet
	clusterSpec          *api.ClusterConfig
	spec                 *api.NodeGroup
	supportsManagedNodes bool
	provider             api.ClusterProvider
	clusterStackName     string
	nodeGroupName        string
	instanceProfileARN   *gfn.Value
	securityGroups       []*gfn.Value
	vpc                  *gfn.Value
	userData             *gfn.Value
	sharedTags           []*cfn.Tag
}

// NewNodeGroupResourceSet returns a resource set for a nodegroup embedded in a cluster config
func NewNodeGroupResourceSet(provider api.ClusterProvider, spec *api.ClusterConfig,
	clusterStackName string, sharedTags []*cfn.Tag, ng *api.NodeGroup,
	supportsManagedNodes bool) *NodeGroupResourceSet {
	return &NodeGroupResourceSet{
		rs:                   newResourceSet(),
		clusterStackName:     clusterStackName,
		nodeGroupName:        ng.Name,
		supportsManagedNodes: supportsManagedNodes,
		clusterSpec:          spec,
		spec:                 ng,
		provider:             provider,
		sharedTags:           sharedTags,
	}
}

// AddAllResources adds all the information about the nodegroup to the resource set
func (n *NodeGroupResourceSet) AddAllResources() error {
	n.rs.template.Description = fmt.Sprintf(
		"%s (AMI family: %s, SSH access: %v, private networking: %v) %s",
		nodeGroupTemplateDescription,
		n.spec.AMIFamily, api.IsEnabled(n.spec.SSH.Allow), n.spec.PrivateNetworking,
		templateDescriptionSuffix)

	n.rs.defineOutputWithoutCollector(outputs.NodeGroupFeaturePrivateNetworking, n.spec.PrivateNetworking, false)
	n.rs.defineOutputWithoutCollector(outputs.NodeGroupFeatureSharedSecurityGroup, n.spec.SecurityGroups.WithShared, false)
	n.rs.defineOutputWithoutCollector(outputs.NodeGroupFeatureLocalSecurityGroup, n.spec.SecurityGroups.WithLocal, false)

	n.vpc = makeImportValue(n.clusterStackName, outputs.ClusterVPC)

	userData, err := nodebootstrap.NewUserData(n.clusterSpec, n.spec)
	if err != nil {
		return err
	}
	n.userData = gfn.NewString(userData)

	// Ensure MinSize is set, as it is required by the ASG cfn resource
	if n.spec.MinSize == nil {
		if n.spec.DesiredCapacity == nil {
			defaultNodeCount := api.DefaultNodeCount
			n.spec.MinSize = &defaultNodeCount
		} else {
			n.spec.MinSize = n.spec.DesiredCapacity
		}
		logger.Info("--nodes-min=%d was set automatically for nodegroup %s", *n.spec.MinSize, n.nodeGroupName)
	} else if n.spec.DesiredCapacity != nil && *n.spec.DesiredCapacity < *n.spec.MinSize {
		return fmt.Errorf("cannot use --nodes-min=%d and --nodes=%d at the same time", *n.spec.MinSize, *n.spec.DesiredCapacity)
	}

	// Ensure MaxSize is set, as it is required by the ASG cfn resource
	if n.spec.MaxSize == nil {
		if n.spec.DesiredCapacity == nil {
			n.spec.MaxSize = n.spec.MinSize
		} else {
			n.spec.MaxSize = n.spec.DesiredCapacity
		}
		logger.Info("--nodes-max=%d was set automatically for nodegroup %s", *n.spec.MaxSize, n.nodeGroupName)
	} else if n.spec.DesiredCapacity != nil && *n.spec.DesiredCapacity > *n.spec.MaxSize {
		return fmt.Errorf("cannot use --nodes-max=%d and --nodes=%d at the same time", *n.spec.MaxSize, *n.spec.DesiredCapacity)
	} else if *n.spec.MaxSize < *n.spec.MinSize {
		return fmt.Errorf("cannot use --nodes-min=%d and --nodes-max=%d at the same time", *n.spec.MinSize, *n.spec.MaxSize)
	}

	n.addResourcesForIAM()
	n.addResourcesForSecurityGroups()

	return n.addResourcesForNodeGroup()
}

// RenderJSON returns the rendered JSON
func (n *NodeGroupResourceSet) RenderJSON() ([]byte, error) {
	return n.rs.renderJSON()
}

// Template returns the CloudFormation template
func (n *NodeGroupResourceSet) Template() gfn.Template {
	return *n.rs.template
}

func (n *NodeGroupResourceSet) newResource(name string, resource interface{}) *gfn.Value {
	return n.rs.newResource(name, resource)
}

func (n *NodeGroupResourceSet) addResourcesForNodeGroup() error {
	launchTemplateName := gfn.MakeFnSubString(fmt.Sprintf("${%s}", gfn.StackName))
	launchTemplateData := n.newLaunchTemplateData()

	if n.spec.SSH != nil && api.IsSetAndNonEmptyString(n.spec.SSH.PublicKeyName) {
		launchTemplateData.KeyName = gfn.NewString(*n.spec.SSH.PublicKeyName)
	}

	if volumeSize := n.spec.VolumeSize; volumeSize != nil && *volumeSize > 0 {
		var (
			kmsKeyID   *gfn.Value
			volumeIOPS *gfn.Value
		)
		if api.IsSetAndNonEmptyString(n.spec.VolumeKmsKeyID) {
			kmsKeyID = gfn.NewString(*n.spec.VolumeKmsKeyID)
		}

		if *n.spec.VolumeType == api.NodeVolumeTypeIO1 {
			volumeIOPS = gfn.NewInteger(*n.spec.VolumeIOPS)
		}

		launchTemplateData.BlockDeviceMappings = []gfn.AWSEC2LaunchTemplate_BlockDeviceMapping{{
			DeviceName: gfn.NewString(*n.spec.VolumeName),
			Ebs: &gfn.AWSEC2LaunchTemplate_Ebs{
				VolumeSize: gfn.NewInteger(*volumeSize),
				VolumeType: gfn.NewString(*n.spec.VolumeType),
				Encrypted:  gfn.NewBoolean(*n.spec.VolumeEncrypted),
				KmsKeyId:   kmsKeyID,
				Iops:       volumeIOPS,
			},
		}}
	}

	launchTemplate := &gfn.AWSEC2LaunchTemplate{
		LaunchTemplateName: launchTemplateName,
		LaunchTemplateData: launchTemplateData,
	}

	// Do not create a Launch Template resource for Spot-managed nodegroups.
	if n.spec.SpotOcean == nil {
		n.newResource("NodeGroupLaunchTemplate", launchTemplate)
	}

	vpcZoneIdentifier, err := AssignSubnets(n.spec.AvailabilityZones, n.clusterStackName, n.clusterSpec, n.spec.PrivateNetworking)
	if err != nil {
		return err
	}

	tags := []map[string]interface{}{
		{
			"Key":               "Name",
			"Value":             fmt.Sprintf("%s-%s-Node", n.clusterSpec.Metadata.Name, n.nodeGroupName),
			"PropagateAtLaunch": "true",
		},
		{
			"Key":               "kubernetes.io/cluster/" + n.clusterSpec.Metadata.Name,
			"Value":             "owned",
			"PropagateAtLaunch": "true",
		},
	}
	if api.IsEnabled(n.spec.IAM.WithAddonPolicies.AutoScaler) {
		tags = append(tags,
			map[string]interface{}{
				"Key":               "k8s.io/cluster-autoscaler/enabled",
				"Value":             "true",
				"PropagateAtLaunch": "true",
			},
			map[string]interface{}{
				"Key":               "k8s.io/cluster-autoscaler/" + n.clusterSpec.Metadata.Name,
				"Value":             "owned",
				"PropagateAtLaunch": "true",
			},
		)
	}

	g, err := n.newNodeGroupResource(launchTemplate, &vpcZoneIdentifier, tags)
	if err != nil {
		return fmt.Errorf("failed to build nodegroup resource: %v", err)
	}
	n.newResource("NodeGroup", g)

	return nil
}

// AssignSubnets subnets based on the specified availability zones
func AssignSubnets(availabilityZones []string, clusterStackName string, clusterSpec *api.ClusterConfig, privateNetworking bool) (interface{}, error) {
	// currently goformation type system doesn't allow specifying `VPCZoneIdentifier: { "Fn::ImportValue": ... }`,
	// and tags don't have `PropagateAtLaunch` field, so we have a custom method here until this gets resolved

	if numNodeGroupsAZs := len(availabilityZones); numNodeGroupsAZs > 0 {
		subnets := clusterSpec.VPC.Subnets.Private
		if !privateNetworking {
			subnets = clusterSpec.VPC.Subnets.Public
		}
		makeErrorDesc := func() string {
			return fmt.Sprintf("(subnets=%#v AZs=%#v)", subnets, availabilityZones)
		}
		if len(subnets) < numNodeGroupsAZs {
			return nil, fmt.Errorf("VPC doesn't have enough subnets for nodegroup AZs %s", makeErrorDesc())
		}
		subnetIDs := make([]string, numNodeGroupsAZs)
		for i, az := range availabilityZones {
			subnet, ok := subnets[az]
			if !ok {
				return nil, fmt.Errorf("VPC doesn't have subnets in %s %s", az, makeErrorDesc())
			}

			subnetIDs[i] = subnet.ID
		}
		return subnetIDs, nil
	}

	var subnets *gfn.Value
	if privateNetworking {
		subnets = makeImportValue(clusterStackName, outputs.ClusterSubnetsPrivate)
	} else {
		subnets = makeImportValue(clusterStackName, outputs.ClusterSubnetsPublic)
	}

	return map[string][]interface{}{
		gfn.FnSplit: {",", subnets},
	}, nil
}

// GetAllOutputs collects all outputs of the nodegroup
func (n *NodeGroupResourceSet) GetAllOutputs(stack cfn.Stack) error {
	return n.rs.GetAllOutputs(stack)
}

func (n *NodeGroupResourceSet) newLaunchTemplateData() *gfn.AWSEC2LaunchTemplate_LaunchTemplateData {
	launchTemplateData := &gfn.AWSEC2LaunchTemplate_LaunchTemplateData{
		IamInstanceProfile: &gfn.AWSEC2LaunchTemplate_IamInstanceProfile{
			Arn: n.instanceProfileARN,
		},
		ImageId:  gfn.NewString(n.spec.AMI),
		UserData: n.userData,
		NetworkInterfaces: []gfn.AWSEC2LaunchTemplate_NetworkInterface{{
			AssociatePublicIpAddress: gfn.NewBoolean(!n.spec.PrivateNetworking),
			DeviceIndex:              gfn.NewInteger(0),
			Groups:                   n.securityGroups,
		}},
	}
	if !api.HasMixedInstances(n.spec) {
		launchTemplateData.InstanceType = gfn.NewString(n.spec.InstanceType)
	} else {
		launchTemplateData.InstanceType = gfn.NewString(n.spec.InstancesDistribution.InstanceTypes[0])
	}
	if n.spec.EBSOptimized != nil {
		launchTemplateData.EbsOptimized = gfn.NewBoolean(*n.spec.EBSOptimized)
	}

	return launchTemplateData
}

func (n *NodeGroupResourceSet) newNodeGroupResource(launchTemplate *gfn.AWSEC2LaunchTemplate,
	vpcZoneIdentifier interface{}, tags []map[string]interface{}) (*awsCloudFormationResource, error) {

	if n.spec.SpotOcean != nil {
		logger.Debug("creating nodegroup using spot ocean")
		return n.newNodeGroupSpotOceanResource(launchTemplate, vpcZoneIdentifier, tags)
	}

	logger.Debug("creating nodegroup using aws auto scaling group")
	return n.newNodeGroupAutoScalingGroupResource(launchTemplate, vpcZoneIdentifier, tags)
}

func (n *NodeGroupResourceSet) newNodeGroupAutoScalingGroupResource(launchTemplate *gfn.AWSEC2LaunchTemplate,
	vpcZoneIdentifier interface{}, tags []map[string]interface{}) (*awsCloudFormationResource, error) {

	ng := n.spec
	ngProps := map[string]interface{}{
		"VPCZoneIdentifier": vpcZoneIdentifier,
		"Tags":              tags,
	}
	if ng.DesiredCapacity != nil {
		ngProps["DesiredCapacity"] = fmt.Sprintf("%d", *ng.DesiredCapacity)
	}
	if ng.MinSize != nil {
		ngProps["MinSize"] = fmt.Sprintf("%d", *ng.MinSize)
	}
	if ng.MaxSize != nil {
		ngProps["MaxSize"] = fmt.Sprintf("%d", *ng.MaxSize)
	}
	if len(ng.TargetGroupARNs) > 0 {
		ngProps["TargetGroupARNs"] = ng.TargetGroupARNs
	}
	if api.HasMixedInstances(ng) {
		ngProps["MixedInstancesPolicy"] = n.newMixedInstancesPolicy(launchTemplate.LaunchTemplateName)
	} else {
		ngProps["LaunchTemplate"] = map[string]interface{}{
			"LaunchTemplateName": launchTemplate.LaunchTemplateName,
			"Version":            gfn.MakeFnGetAttString("NodeGroupLaunchTemplate.LatestVersionNumber"),
		}
	}

	return &awsCloudFormationResource{
		Type:       "AWS::AutoScaling::AutoScalingGroup",
		Properties: ngProps,
		UpdatePolicy: map[string]map[string]string{
			"AutoScalingRollingUpdate": {
				"MinInstancesInService": "0",
				"MaxBatchSize":          "1",
			},
		},
	}, nil
}

func (n *NodeGroupResourceSet) newMixedInstancesPolicy(launchTemplateName *gfn.Value) map[string]interface{} {
	ng := n.spec
	instanceTypes := ng.InstancesDistribution.InstanceTypes
	overrides := make([]map[string]string, len(instanceTypes))

	for i, instanceType := range instanceTypes {
		overrides[i] = map[string]string{
			"InstanceType": instanceType,
		}
	}
	policy := map[string]interface{}{
		"LaunchTemplate": map[string]interface{}{
			"LaunchTemplateSpecification": map[string]interface{}{
				"LaunchTemplateName": launchTemplateName,
				"Version":            gfn.MakeFnGetAttString("NodeGroupLaunchTemplate.LatestVersionNumber"),
			},

			"Overrides": overrides,
		},
	}

	instancesDistribution := map[string]string{}

	// Only set the price if it was specified so otherwise AWS picks "on-demand price" as the default
	if ng.InstancesDistribution.MaxPrice != nil {
		instancesDistribution["SpotMaxPrice"] = fmt.Sprintf("%f", *ng.InstancesDistribution.MaxPrice)
	}
	if ng.InstancesDistribution.OnDemandBaseCapacity != nil {
		instancesDistribution["OnDemandBaseCapacity"] = fmt.Sprintf("%d", *ng.InstancesDistribution.OnDemandBaseCapacity)
	}
	if ng.InstancesDistribution.OnDemandPercentageAboveBaseCapacity != nil {
		instancesDistribution["OnDemandPercentageAboveBaseCapacity"] = fmt.Sprintf("%d", *ng.InstancesDistribution.OnDemandPercentageAboveBaseCapacity)
	}
	if ng.InstancesDistribution.SpotInstancePools != nil {
		instancesDistribution["SpotInstancePools"] = fmt.Sprintf("%d", *ng.InstancesDistribution.SpotInstancePools)
	}

	policy["InstancesDistribution"] = instancesDistribution

	return policy
}
