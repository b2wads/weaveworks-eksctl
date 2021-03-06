package builder

import (
	"fmt"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/pkg/errors"
	api "github.com/weaveworks/eksctl/pkg/apis/eksctl.io/v1alpha5"
	"github.com/weaveworks/eksctl/pkg/utils"
	gfnec2 "github.com/weaveworks/goformation/v4/cloudformation/ec2"
	gfneks "github.com/weaveworks/goformation/v4/cloudformation/eks"
	gfnt "github.com/weaveworks/goformation/v4/cloudformation/types"
)

// ManagedNodeGroupResourceSet defines the CloudFormation resources required for a managed nodegroup
type ManagedNodeGroupResourceSet struct {
	clusterConfig         *api.ClusterConfig
	clusterStackName      string
	nodeGroup             *api.ManagedNodeGroup
	launchTemplateFetcher *LaunchTemplateFetcher
	*resourceSet

	// UserDataMimeBoundary sets the MIME boundary for user data
	UserDataMimeBoundary string
}

const ManagedNodeGroupResourceName = "ManagedNodeGroup"

// NewManagedNodeGroup creates a new ManagedNodeGroupResourceSet
func NewManagedNodeGroup(cluster *api.ClusterConfig, nodeGroup *api.ManagedNodeGroup, launchTemplateFetcher *LaunchTemplateFetcher, clusterStackName string) *ManagedNodeGroupResourceSet {
	return &ManagedNodeGroupResourceSet{
		clusterConfig:         cluster,
		clusterStackName:      clusterStackName,
		nodeGroup:             nodeGroup,
		launchTemplateFetcher: launchTemplateFetcher,
		resourceSet:           newResourceSet(),
	}
}

// AddAllResources adds all required CloudFormation resources
func (m *ManagedNodeGroupResourceSet) AddAllResources() error {
	m.resourceSet.template.Description = fmt.Sprintf(
		"%s (SSH access: %v) %s",
		"EKS Managed Nodes",
		api.IsEnabled(m.nodeGroup.SSH.Allow),
		"[created by eksctl]")

	m.template.Mappings[servicePrincipalPartitionMapName] = servicePrincipalPartitionMappings

	var nodeRole *gfnt.Value
	if m.nodeGroup.IAM.InstanceRoleARN == "" {
		enableSSM := m.nodeGroup.SSH != nil && api.IsEnabled(m.nodeGroup.SSH.EnableSSM)

		if err := createRole(m.resourceSet, m.clusterConfig.IAM, m.nodeGroup.IAM, true, enableSSM); err != nil {
			return err
		}
		nodeRole = gfnt.MakeFnGetAttString(cfnIAMInstanceRoleName, "Arn")
	} else {
		nodeRole = gfnt.NewString(NormalizeARN(m.nodeGroup.IAM.InstanceRoleARN))
	}

	subnets, err := AssignSubnets(m.nodeGroup.AvailabilityZones, m.clusterStackName, m.clusterConfig, false, m.nodeGroup.CustomSubnets)
	if err != nil {
		return err
	}

	scalingConfig := gfneks.Nodegroup_ScalingConfig{}
	if m.nodeGroup.MinSize != nil {
		scalingConfig.MinSize = gfnt.NewInteger(*m.nodeGroup.MinSize)
	}
	if m.nodeGroup.MaxSize != nil {
		scalingConfig.MaxSize = gfnt.NewInteger(*m.nodeGroup.MaxSize)
	}
	if m.nodeGroup.DesiredCapacity != nil {
		scalingConfig.DesiredSize = gfnt.NewInteger(*m.nodeGroup.DesiredCapacity)
	}
	managedResource := &gfneks.Nodegroup{
		ClusterName:   gfnt.NewString(m.clusterConfig.Metadata.Name),
		NodegroupName: gfnt.NewString(m.nodeGroup.Name),
		ScalingConfig: &scalingConfig,
		Subnets:       subnets,
		NodeRole:      nodeRole,
		Labels:        m.nodeGroup.Labels,
		Tags:          m.nodeGroup.Tags,
	}

	var launchTemplate *gfneks.Nodegroup_LaunchTemplateSpecification

	if m.nodeGroup.LaunchTemplate != nil {
		launchTemplateData, err := m.launchTemplateFetcher.Fetch(m.nodeGroup.LaunchTemplate)
		if err != nil {
			return err
		}
		if err := validateLaunchTemplate(launchTemplateData, m.nodeGroup); err != nil {
			return err
		}

		launchTemplate = &gfneks.Nodegroup_LaunchTemplateSpecification{
			Id: gfnt.NewString(m.nodeGroup.LaunchTemplate.ID),
		}
		if version := m.nodeGroup.LaunchTemplate.Version; version != nil {
			launchTemplate.Version = gfnt.NewString(*version)
		}

		if launchTemplateData.ImageId == nil {
			managedResource.AmiType = gfnt.NewString(getAMIType(*launchTemplateData.InstanceType))
		}
	} else {
		launchTemplateData, err := m.makeLaunchTemplateData()
		if err != nil {
			return err
		}
		if launchTemplateData.ImageId == nil {
			managedResource.AmiType = gfnt.NewString(getAMIType(m.nodeGroup.InstanceType))
		}

		ltRef := m.newResource("LaunchTemplate", &gfnec2.LaunchTemplate{
			LaunchTemplateName: gfnt.MakeFnSubString(fmt.Sprintf("${%s}", gfnt.StackName)),
			LaunchTemplateData: launchTemplateData,
		})
		launchTemplate = &gfneks.Nodegroup_LaunchTemplateSpecification{
			Id: ltRef,
		}
	}

	managedResource.LaunchTemplate = launchTemplate
	m.newResource(ManagedNodeGroupResourceName, managedResource)
	return nil
}

func validateLaunchTemplate(launchTemplateData *ec2.ResponseLaunchTemplateData, ng *api.ManagedNodeGroup) error {
	if launchTemplateData.InstanceType == nil {
		return errors.New("instance type must be set in the launch template")
	}

	// Custom AMI
	if launchTemplateData.ImageId != nil {
		if launchTemplateData.UserData == nil {
			return errors.New("node bootstrapping script (UserData) must be set when using a custom AMI")
		}
		if ng.AMI != "" {
			return errors.New("cannot set managedNodegroup.AMI when launchTemplate.ImageId is set")
		}
	}

	if launchTemplateData.IamInstanceProfile != nil && launchTemplateData.IamInstanceProfile.Arn != nil {
		return errors.New("IAM instance profile must not be set in the launch template")
	}

	return nil
}

func getAMIType(instanceType string) string {
	if utils.IsGPUInstanceType(instanceType) {
		return eks.AMITypesAl2X8664Gpu
	}
	if utils.IsARMInstanceType(instanceType) {
		// TODO Upgrade SDK and use constant from the eks library
		return "AL2_ARM_64"
	}
	return eks.AMITypesAl2X8664
}

// RenderJSON implements the ResourceSet interface
func (m *ManagedNodeGroupResourceSet) RenderJSON() ([]byte, error) {
	return m.resourceSet.renderJSON()
}

// WithIAM implements the ResourceSet interface
func (m *ManagedNodeGroupResourceSet) WithIAM() bool {
	// eksctl does not support passing pre-created IAM instance roles to Managed Nodes,
	// so the IAM capability is always required
	return true
}

// WithNamedIAM implements the ResourceSet interface
func (m *ManagedNodeGroupResourceSet) WithNamedIAM() bool {
	return m.nodeGroup.IAM.InstanceRoleName != ""
}
