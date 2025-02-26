//go:generate packer-sdc mapstructure-to-hcl2 -type Config
//go:generate packer-sdc struct-markdown

package update_nodepool

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/errors"
	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/requests"
	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/responses"
	"github.com/aliyun/alibaba-cloud-sdk-go/services/ecs"
	"github.com/aliyun/alibaba-cloud-sdk-go/services/ram"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/hashicorp/hcl/v2/hcldec"
	packerecs "github.com/hashicorp/packer-plugin-alicloud/builder/ecs"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/template/config"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"
)

const (
	Packer        = "HashiCorp-Packer"
	BuilderId     = "update-nodepool"
	OSSSuffix     = "oss-"
	RAWFileFormat = "raw"
	VHDFileFormat = "vhd"
)

const (
	PolicyTypeSystem        = "System"
	NoSetRoleError          = "NoSetRoletoECSServiceAcount"
	RoleNotExistError       = "EntityNotExist.Role"
	DefaultImportRoleName   = "AliyunECSImageImportDefaultRole"
	DefaultImportPolicyName = "AliyunECSImageImportRolePolicy"
	DefaultImportRolePolicy = `{
  "Statement": [
    {
      "Action": "sts:AssumeRole",
      "Effect": "Allow",
      "Principal": {
        "Service": [
          "ecs.aliyuncs.com"
        ]
      }
    }
  ],
  "Version": "1"
}`
)

// Configuration of this post processor
type Config struct {
	packerecs.Config `mapstructure:",squash"`

	// The name of the OSS bucket where the RAW or VHD file will be copied to
	// for import. If the Bucket doesn't exist, the post-process will create it for
	// you.
	OSSBucket string `mapstructure:"oss_bucket_name" required:"true"`
	// The name of the object key in `oss_bucket_name` where the RAW or VHD
	// file will be copied to for import. This is treated as a [template
	// engine](/packer/docs/templates/legacy_json_templates/engine), and you may access any of the variables
	// stored in the generated data using the [build](/packer/docs/templates/legacy_json_templates/engine)
	// template function.
	OSSKey string `mapstructure:"oss_key_name"`
	// Whether we should skip removing the RAW or VHD file uploaded to OSS
	// after the import process has completed. `true` means that we should
	// leave it in the OSS bucket, `false` means to clean it out. Defaults to
	// `false`.
	SkipClean bool              `mapstructure:"skip_clean"`
	Tags      map[string]string `mapstructure:"tags"`
	// The description of the image, with a length limit of `0` to `256`
	// characters. Leaving it blank means null, which is the default value. It
	// cannot begin with `http://` or `https://`.
	AlicloudImageDescription        string   `mapstructure:"image_description"`
	AlicloudResourceGroupId         string   `mapstructure:"resource_group_id"`
	AlicloudImageShareAccounts      []string `mapstructure:"image_share_account"`
	AlicloudImageDestinationRegions []string `mapstructure:"image_copy_regions"`
	// Type of the OS, like linux/windows
	OSType string `mapstructure:"image_os_type" required:"true"`
	// Platform such as `CentOS`
	Platform string `mapstructure:"image_platform" required:"true"`
	// Platform type of the image system: `i386` or `x86_64`
	Architecture string `mapstructure:"image_architecture" required:"true"`
	// Size of the system disk, in GB, values
	//  range:
	//   - cloud - 5 \~ 2000
	//   - cloud_efficiency - 20 \~ 2048
	//   - cloud_ssd - 20 \~ 2048
	//   - cloud_essd - 20 \~ 2048
	Size string `mapstructure:"image_system_size"`
	// The format of the image for import, now alicloud only support RAW and
	// VHD.
	Format string `mapstructure:"format" required:"true"`
	// If this value is true, when the target image name is duplicated with an
	// existing image, it will delete the existing image and then create the
	// target image, otherwise, the creation will fail. The default value is
	// false.
	AlicloudImageForceDelete bool `mapstructure:"image_force_delete"`

	ctx interpolate.Context
}

type PostProcessor struct {
	config            Config
	DiskDeviceMapping []ecs.DiskDeviceMapping

	ossClient *oss.Client
	ramClient *ram.Client
}

func (p *PostProcessor) ConfigSpec() hcldec.ObjectSpec { return p.config.FlatMapstructure().HCL2Spec() }

func (p *PostProcessor) Configure(raws ...interface{}) error {
	err := config.Decode(&p.config, &config.DecodeOpts{
		PluginType:         BuilderId,
		Interpolate:        true,
		InterpolateContext: &p.config.ctx,
		InterpolateFilter: &interpolate.RenderFilter{
			Exclude: []string{
				"oss_key_name",
			},
		},
	}, raws...)
	if err != nil {
		return err
	}

	errs := new(packersdk.MultiError)

	// Check and render oss_key_name
	if err = interpolate.Validate(p.config.OSSKey, &p.config.ctx); err != nil {
		errs = packersdk.MultiErrorAppend(
			errs, fmt.Errorf("Error parsing oss_key_name template: %s", err))
	}

	errs = packersdk.MultiErrorAppend(errs, p.config.AlicloudImageTag.CopyOn(&p.config.AlicloudImageTags)...)

	// Check we have alicloud access variables defined somewhere
	errs = packersdk.MultiErrorAppend(errs, p.config.AlicloudAccessConfig.Prepare(&p.config.ctx)...)

	// define all our required parameters
	templates := map[string]*string{
		"oss_bucket_name": &p.config.OSSBucket,
	}
	// Check out required params are defined
	for key, ptr := range templates {
		if *ptr == "" {
			errs = packersdk.MultiErrorAppend(
				errs, fmt.Errorf("%s must be set", key))
		}
	}

	// Anything which flagged return back up the stack
	if len(errs.Errors) > 0 {
		return errs
	}

	packersdk.LogSecretFilter.Set(p.config.AlicloudAccessKey, p.config.AlicloudSecretKey)
	log.Println(p.config)
	return nil
}

func (p *PostProcessor) PostProcess(ctx context.Context, ui packersdk.Ui, artifact packersdk.Artifact) (packersdk.Artifact, bool, bool, error) {
	var err error

	generatedData := artifact.State("generated_data")
	if generatedData == nil {
		// Make sure it's not a nil map so we can assign to it later.
		generatedData = make(map[string]interface{})
	}
	p.config.ctx.Data = generatedData

	// Render this key since we didn't in the configure phase
	p.config.OSSKey, err = interpolate.Render(p.config.OSSKey, &p.config.ctx)
	if err != nil {
		return nil, false, false, fmt.Errorf("Error rendering oss_key_name template: %s", err)
	}
	if p.config.OSSKey == "" {
		p.config.OSSKey = "Packer_" + strconv.Itoa(time.Now().Nanosecond())
	}

	ui.Say(fmt.Sprintf("Rendered oss_key_name as %s", p.config.OSSKey))
	ui.Say("Looking for RAW or VHD in artifact")

	// Locate the files output from the builder
	source := ""
	for _, path := range artifact.Files() {
		if strings.HasSuffix(path, VHDFileFormat) || strings.HasSuffix(path, RAWFileFormat) {
			source = path
			break
		}
	}

	// Hope we found something useful
	if source == "" {
		return nil, false, false, fmt.Errorf("No vhd or raw file found in artifact from builder")
	}

	ecsClient, err := p.config.AlicloudAccessConfig.Client()
	if err != nil {
		return nil, false, false, fmt.Errorf("Failed to connect alicloud ecs  %s", err)
	}

	endpoint := getEndPoint(p.config.AlicloudRegion, p.config.OSSBucket)

	describeImagesRequest := ecs.CreateDescribeImagesRequest()
	describeImagesRequest.RegionId = p.config.AlicloudRegion
	describeImagesRequest.ImageName = p.config.AlicloudImageName
	imagesResponse, err := ecsClient.DescribeImages(describeImagesRequest)
	if err != nil {
		return nil, false, false, fmt.Errorf("Failed to start import from %s/%s: %s", endpoint, p.config.OSSKey, err)
	}

	images := imagesResponse.Images.Image
	if len(images) > 0 && !p.config.AlicloudImageForceDelete {
		return nil, false, false, fmt.Errorf("Duplicated image exists, please delete the existing images " +
			"or set the 'image_force_delete' value as true")
	}

	bucket, err := p.queryOrCreateBucket(p.config.OSSBucket)
	if err != nil {
		return nil, false, false, fmt.Errorf("Failed to query or create bucket %s: %s", p.config.OSSBucket, err)
	}

	ui.Say(fmt.Sprintf("Waiting for uploading file %s to %s/%s...", source, endpoint, p.config.OSSKey))

	err = bucket.PutObjectFromFile(p.config.OSSKey, source)
	if err != nil {
		return nil, false, false, fmt.Errorf("Failed to upload image %s: %s", source, err)
	}

	ui.Say(fmt.Sprintf("Image file %s has been uploaded to OSS", source))

	if len(images) > 0 && p.config.AlicloudImageForceDelete {
		deleteImageRequest := ecs.CreateDeleteImageRequest()
		deleteImageRequest.RegionId = p.config.AlicloudRegion
		deleteImageRequest.ImageId = images[0].ImageId
		_, err := ecsClient.DeleteImage(deleteImageRequest)
		if err != nil {
			return nil, false, false, fmt.Errorf("Delete duplicated image %s failed", images[0].ImageName)
		}
	}

	importImageRequest := p.buildImportImageRequest()
	importImageResponse, err := ecsClient.ImportImage(importImageRequest)
	if err != nil {
		e, ok := err.(errors.Error)
		if !ok || e.ErrorCode() != NoSetRoleError {
			return nil, false, false, fmt.Errorf("Failed to start import from %s/%s: %s", endpoint, p.config.OSSKey, err)
		}

		ui.Say("initialize ram role for importing image")
		if err := p.prepareImportRole(); err != nil {
			return nil, false, false, fmt.Errorf("Failed to start import from %s/%s: %s", endpoint, p.config.OSSKey, err)
		}

		acsResponse, err := ecsClient.WaitForExpected(&packerecs.WaitForExpectArgs{
			RequestFunc: func() (responses.AcsResponse, error) {
				return ecsClient.ImportImage(importImageRequest)
			},
			EvalFunc: func(response responses.AcsResponse, err error) packerecs.WaitForExpectEvalResult {
				if err == nil {
					return packerecs.WaitForExpectSuccess
				}

				e, ok = err.(errors.Error)
				if ok && packerecs.ContainsInArray([]string{
					"ImageIsImporting",
					"InvalidImageName.Duplicated",
				}, e.ErrorCode()) {
					return packerecs.WaitForExpectSuccess
				}

				if ok && e.ErrorCode() != NoSetRoleError {
					return packerecs.WaitForExpectFailToStop
				}

				return packerecs.WaitForExpectToRetry
			},
		})

		if err != nil {
			return nil, false, false, fmt.Errorf("Failed to start import from %s/%s: %s", endpoint, p.config.OSSKey, err)
		}

		importImageResponse = acsResponse.(*ecs.ImportImageResponse)
	}

	imageId := importImageResponse.ImageId

	ui.Say(fmt.Sprintf("Waiting for importing %s/%s to alicloud...", endpoint, p.config.OSSKey))
	_, err = ecsClient.WaitForImageStatus(p.config.AlicloudRegion, imageId, packerecs.ImageStatusAvailable, time.Duration(packerecs.ALICLOUD_DEFAULT_LONG_TIMEOUT)*time.Second)
	if err != nil {
		return nil, false, false, fmt.Errorf("Import image %s failed: %s", imageId, err)
	}

	// Add the reported Alicloud image ID to the artifact list
	ui.Say(fmt.Sprintf("Importing created alicloud image ID %s in region %s Finished.", imageId, p.config.AlicloudRegion))
	artifact = &packerecs.Artifact{
		AlicloudImages: map[string]string{
			p.config.AlicloudRegion: imageId,
		},
		BuilderIdValue: BuilderId,
		Client:         ecsClient,
	}

	if !p.config.SkipClean {
		ui.Message(fmt.Sprintf("Deleting import source %s/%s/%s", endpoint, p.config.OSSBucket, p.config.OSSKey))
		if err = bucket.DeleteObject(p.config.OSSKey); err != nil {
			return nil, false, false, fmt.Errorf("Failed to delete %s/%s/%s: %s", endpoint, p.config.OSSBucket, p.config.OSSKey, err)
		}
	}

	return artifact, false, false, nil
}

func (p *PostProcessor) getOssClient() *oss.Client {
	if p.ossClient == nil {
		log.Println("Creating OSS Client")
		ossClient, _ := oss.New(getEndPoint(p.config.AlicloudRegion, ""), p.config.AlicloudAccessKey,
			p.config.AlicloudSecretKey)
		p.ossClient = ossClient
	}

	return p.ossClient
}

func (p *PostProcessor) getRamClient() *ram.Client {
	if p.ramClient == nil {
		ramClient, _ := ram.NewClientWithAccessKey(p.config.AlicloudRegion, p.config.AlicloudAccessKey, p.config.AlicloudSecretKey)
		p.ramClient = ramClient
	}

	return p.ramClient
}

func (p *PostProcessor) queryOrCreateBucket(bucketName string) (*oss.Bucket, error) {
	ossClient := p.getOssClient()

	isExist, err := ossClient.IsBucketExist(bucketName)
	if err != nil {
		return nil, err
	}
	if !isExist {
		err = ossClient.CreateBucket(bucketName)
		if err != nil {
			return nil, err
		}
	}
	bucket, err := ossClient.Bucket(bucketName)
	if err != nil {
		return nil, err
	}
	return bucket, nil

}

func (p *PostProcessor) prepareImportRole() error {
	ramClient := p.getRamClient()

	getRoleRequest := ram.CreateGetRoleRequest()
	getRoleRequest.SetScheme(requests.HTTPS)
	getRoleRequest.RoleName = DefaultImportRoleName
	_, err := ramClient.GetRole(getRoleRequest)
	if err == nil {
		if e := p.updateOrAttachPolicy(); e != nil {
			return e
		}

		return nil
	}

	e, ok := err.(errors.Error)
	if !ok || e.ErrorCode() != RoleNotExistError {
		return e
	}

	if err := p.createRoleAndAttachPolicy(); err != nil {
		return err
	}

	time.Sleep(1 * time.Minute)
	return nil
}

func (p *PostProcessor) updateOrAttachPolicy() error {
	ramClient := p.getRamClient()

	listPoliciesForRoleRequest := ram.CreateListPoliciesForRoleRequest()
	listPoliciesForRoleRequest.SetScheme(requests.HTTPS)
	listPoliciesForRoleRequest.RoleName = DefaultImportRoleName
	policyListResponse, err := p.ramClient.ListPoliciesForRole(listPoliciesForRoleRequest)
	if err != nil {
		return fmt.Errorf("Failed to list policies: %s", err)
	}

	rolePolicyExists := false
	for _, policy := range policyListResponse.Policies.Policy {
		if policy.PolicyName == DefaultImportPolicyName && policy.PolicyType == PolicyTypeSystem {
			rolePolicyExists = true
			break
		}
	}

	if rolePolicyExists {
		updateRoleRequest := ram.CreateUpdateRoleRequest()
		updateRoleRequest.SetScheme(requests.HTTPS)
		updateRoleRequest.RoleName = DefaultImportRoleName
		updateRoleRequest.NewAssumeRolePolicyDocument = DefaultImportRolePolicy
		if _, err := ramClient.UpdateRole(updateRoleRequest); err != nil {
			return fmt.Errorf("Failed to update role policy: %s", err)
		}
	} else {
		attachPolicyToRoleRequest := ram.CreateAttachPolicyToRoleRequest()
		attachPolicyToRoleRequest.SetScheme(requests.HTTPS)
		attachPolicyToRoleRequest.PolicyName = DefaultImportPolicyName
		attachPolicyToRoleRequest.PolicyType = PolicyTypeSystem
		attachPolicyToRoleRequest.RoleName = DefaultImportRoleName
		if _, err := ramClient.AttachPolicyToRole(attachPolicyToRoleRequest); err != nil {
			return fmt.Errorf("Failed to attach role policy: %s", err)
		}
	}

	return nil
}

func (p *PostProcessor) createRoleAndAttachPolicy() error {
	ramClient := p.getRamClient()

	createRoleRequest := ram.CreateCreateRoleRequest()
	createRoleRequest.SetScheme(requests.HTTPS)
	createRoleRequest.RoleName = DefaultImportRoleName
	createRoleRequest.AssumeRolePolicyDocument = DefaultImportRolePolicy
	if _, err := ramClient.CreateRole(createRoleRequest); err != nil {
		return fmt.Errorf("Failed to create role: %s", err)
	}

	attachPolicyToRoleRequest := ram.CreateAttachPolicyToRoleRequest()
	attachPolicyToRoleRequest.SetScheme(requests.HTTPS)
	attachPolicyToRoleRequest.PolicyName = DefaultImportPolicyName
	attachPolicyToRoleRequest.PolicyType = PolicyTypeSystem
	attachPolicyToRoleRequest.RoleName = DefaultImportRoleName
	if _, err := ramClient.AttachPolicyToRole(attachPolicyToRoleRequest); err != nil {
		return fmt.Errorf("Failed to attach policy: %s", err)
	}
	return nil
}

func (p *PostProcessor) buildImportImageRequest() *ecs.ImportImageRequest {
	request := ecs.CreateImportImageRequest()
	request.RegionId = p.config.AlicloudRegion
	request.ImageName = p.config.AlicloudImageName
	request.Description = p.config.AlicloudImageDescription
	request.Architecture = p.config.Architecture
	request.OSType = p.config.OSType
	request.Platform = p.config.Platform
	request.DiskDeviceMapping = &[]ecs.ImportImageDiskDeviceMapping{
		{
			DiskImageSize: p.config.Size,
			Format:        p.config.Format,
			OSSBucket:     p.config.OSSBucket,
			OSSObject:     p.config.OSSKey,
		},
	}
	request.ResourceGroupId = p.config.AlicloudResourceGroupId
	return request
}

func getEndPoint(region string, bucket string) string {
	if bucket != "" {
		return "https://" + bucket + "." + getOSSRegion(region) + ".aliyuncs.com"
	}

	return "https://" + getOSSRegion(region) + ".aliyuncs.com"
}

func getOSSRegion(region string) string {
	if strings.HasPrefix(region, OSSSuffix) {
		return region
	}
	return OSSSuffix + region
}
