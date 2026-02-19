package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/autoscaling"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/ebs"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/iam"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		cfg := config.New(ctx, "openclaw-vps")

		instanceType := cfg.Get("instanceType")
		if instanceType == "" {
			instanceType = "t3.medium"
		}

		tailscaleAuthKey := cfg.RequireSecret("tailscaleAuthKey")
		anthropicApiKey := cfg.RequireSecret("anthropicApiKey")
		snapshotId := cfg.Get("snapshotId")

		// Look up Ubuntu 24.04 AMI
		ami, err := ec2.LookupAmi(ctx, &ec2.LookupAmiArgs{
			MostRecent: pulumi.BoolRef(true),
			Owners:     []string{"099720109477"}, // Canonical
			Filters: []ec2.GetAmiFilter{
				{
					Name:   "name",
					Values: []string{"ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"},
				},
				{
					Name:   "virtualization-type",
					Values: []string{"hvm"},
				},
			},
		})
		if err != nil {
			return err
		}

		// Security Group: no inbound, Tailscale uses outbound NAT traversal + DERP relays
		sg, err := ec2.NewSecurityGroup(ctx, "openclaw-sg", &ec2.SecurityGroupArgs{
			Description: pulumi.String("OpenClaw VPS - no inbound traffic"),
			Egress: ec2.SecurityGroupEgressArray{
				&ec2.SecurityGroupEgressArgs{
					Protocol:    pulumi.String("-1"),
					FromPort:    pulumi.Int(0),
					ToPort:      pulumi.Int(0),
					CidrBlocks:  pulumi.StringArray{pulumi.String("0.0.0.0/0")},
					Description: pulumi.String("Allow all outbound"),
				},
			},
		})
		if err != nil {
			return err
		}

		// Get AZ for the EBS volume
		azs, err := aws.GetAvailabilityZones(ctx, &aws.GetAvailabilityZonesArgs{
			State: pulumi.StringRef("available"),
		})
		if err != nil {
			return err
		}
		az := azs.Names[0]

		// EBS Volume (persistent, optionally restored from snapshot)
		volumeArgs := &ebs.VolumeArgs{
			AvailabilityZone: pulumi.String(az),
			Size:             pulumi.Int(20),
			Type:             pulumi.String("gp3"),
			Tags: pulumi.StringMap{
				"Name": pulumi.String("openclaw-data"),
			},
		}
		if snapshotId != "" {
			volumeArgs.SnapshotId = pulumi.String(snapshotId)
		}
		volume, err := ebs.NewVolume(ctx, "openclaw-data", volumeArgs)
		if err != nil {
			return err
		}

		// IAM Role for EC2
		assumeRolePolicy := `{
			"Version": "2012-10-17",
			"Statement": [{
				"Effect": "Allow",
				"Principal": {"Service": "ec2.amazonaws.com"},
				"Action": "sts:AssumeRole"
			}]
		}`

		role, err := iam.NewRole(ctx, "openclaw-role", &iam.RoleArgs{
			AssumeRolePolicy: pulumi.String(assumeRolePolicy),
		})
		if err != nil {
			return err
		}

		// Policy: attach EBS + create/manage snapshots for daily backups
		_, err = iam.NewRolePolicy(ctx, "openclaw-ebs-policy", &iam.RolePolicyArgs{
			Role: role.Name,
			Policy: pulumi.String(`{
				"Version": "2012-10-17",
				"Statement": [{
					"Effect": "Allow",
					"Action": [
						"ec2:AttachVolume",
						"ec2:DetachVolume",
						"ec2:DescribeVolumes",
						"ec2:CreateSnapshot",
						"ec2:DeleteSnapshot",
						"ec2:DescribeSnapshots",
						"ec2:CreateTags"
					],
					"Resource": "*"
				}]
			}`),
		})
		if err != nil {
			return err
		}

		instanceProfile, err := iam.NewInstanceProfile(
			ctx,
			"openclaw-profile",
			&iam.InstanceProfileArgs{
				Role: role.Name,
			},
		)
		if err != nil {
			return err
		}

		// Read and encode docker-compose for user-data injection
		dockerCompose, err := os.ReadFile("docker-compose.yml")
		if err != nil {
			return fmt.Errorf("reading docker-compose.yml: %w", err)
		}
		dockerComposeB64 := base64.StdEncoding.EncodeToString(dockerCompose)

		userDataTemplate, err := os.ReadFile("user-data.sh")
		if err != nil {
			return fmt.Errorf("reading user-data.sh: %w", err)
		}

		// Build user-data by interpolating template variables
		userData := pulumi.All(
			volume.ID(),
			tailscaleAuthKey,
			anthropicApiKey,
		).ApplyT(func(args []interface{}) string {
			volID := fmt.Sprintf("%v", args[0])
			tsKey := fmt.Sprintf("%v", args[1])
			anthropic := fmt.Sprintf("%v", args[2])

			script := string(userDataTemplate)
			script = strings.ReplaceAll(script, "{{.EBSVolumeID}}", volID)
			script = strings.ReplaceAll(script, "{{.TailscaleAuthKey}}", tsKey)
			script = strings.ReplaceAll(script, "{{.AnthropicApiKey}}", anthropic)
			script = strings.ReplaceAll(script, "{{.DockerComposeB64}}", dockerComposeB64)

			return script
		}).(pulumi.StringOutput)

		userDataB64 := userData.ApplyT(func(s string) string {
			return base64.StdEncoding.EncodeToString([]byte(s))
		}).(pulumi.StringOutput)

		// Launch Template (no spot config — ASG handles that)
		lt, err := ec2.NewLaunchTemplate(ctx, "openclaw-lt", &ec2.LaunchTemplateArgs{
			ImageId:      pulumi.String(ami.Id),
			InstanceType: pulumi.String(instanceType),
			UserData:     userDataB64,
			VpcSecurityGroupIds: pulumi.StringArray{
				sg.ID(),
			},
			IamInstanceProfile: &ec2.LaunchTemplateIamInstanceProfileArgs{
				Arn: instanceProfile.Arn,
			},
			BlockDeviceMappings: ec2.LaunchTemplateBlockDeviceMappingArray{
				&ec2.LaunchTemplateBlockDeviceMappingArgs{
					DeviceName: pulumi.String("/dev/sda1"),
					Ebs: &ec2.LaunchTemplateBlockDeviceMappingEbsArgs{
						VolumeSize: pulumi.Int(20),
						VolumeType: pulumi.String("gp3"),
					},
				},
			},
			Tags: pulumi.StringMap{
				"Name": pulumi.String("openclaw-vps"),
			},
		})
		if err != nil {
			return err
		}

		// Auto Scaling Group: always 1 instance, spot with on-demand fallback
		asg, err := autoscaling.NewGroup(ctx, "openclaw-asg", &autoscaling.GroupArgs{
			MinSize:         pulumi.Int(0),
			MaxSize:         pulumi.Int(1),
			DesiredCapacity: pulumi.Int(1),
			AvailabilityZones: pulumi.StringArray{
				pulumi.String(az),
			},
			CapacityRebalance: pulumi.Bool(true),
			MixedInstancesPolicy: &autoscaling.GroupMixedInstancesPolicyArgs{
				InstancesDistribution: &autoscaling.GroupMixedInstancesPolicyInstancesDistributionArgs{
					OnDemandBaseCapacity:                pulumi.Int(0),
					OnDemandPercentageAboveBaseCapacity: pulumi.Int(0),
					SpotAllocationStrategy:              pulumi.String("price-capacity-optimized"),
				},
				LaunchTemplate: &autoscaling.GroupMixedInstancesPolicyLaunchTemplateArgs{
					LaunchTemplateSpecification: &autoscaling.GroupMixedInstancesPolicyLaunchTemplateLaunchTemplateSpecificationArgs{
						LaunchTemplateId: lt.ID(),
						Version:          pulumi.String("$Latest"),
					},
				},
			},
			Tags: autoscaling.GroupTagArray{
				&autoscaling.GroupTagArgs{
					Key:               pulumi.String("Name"),
					Value:             pulumi.String("openclaw-vps"),
					PropagateAtLaunch: pulumi.Bool(true),
				},
			},
		})
		if err != nil {
			return err
		}

		ctx.Export("asgName", asg.Name)
		ctx.Export("volumeId", volume.ID())
		ctx.Export("securityGroupId", sg.ID())
		ctx.Export("availabilityZone", pulumi.String(az))

		return nil
	})
}
