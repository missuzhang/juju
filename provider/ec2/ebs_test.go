// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package ec2_test

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/juju/errors"
	jc "github.com/juju/testing/checkers"
	"github.com/juju/utils/clock"
	awsec2 "gopkg.in/amz.v3/ec2"
	"gopkg.in/amz.v3/ec2/ec2test"
	gc "gopkg.in/check.v1"
	"gopkg.in/juju/names.v2"

	"github.com/juju/juju/cloud"
	"github.com/juju/juju/constraints"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/config"
	"github.com/juju/juju/environs/tags"
	"github.com/juju/juju/instance"
	"github.com/juju/juju/provider/ec2"
	"github.com/juju/juju/storage"
	"github.com/juju/juju/testing"
)

type ebsSuite struct {
	testing.BaseSuite
	srv         localServer
	modelConfig *config.Config
	instanceId  string
}

var _ = gc.Suite(&ebsSuite{})

func (s *ebsSuite) SetUpTest(c *gc.C) {
	s.BaseSuite.SetUpTest(c)
	s.PatchValue(&ec2.DestroyVolumeAttempt.Delay, time.Duration(0))

	modelConfig, err := config.New(config.NoDefaults, testing.FakeConfig().Merge(
		testing.Attrs{"type": "ec2"},
	))
	c.Assert(err, jc.ErrorIsNil)
	s.modelConfig = modelConfig

	s.srv.startServer(c)
	s.AddCleanup(func(c *gc.C) { s.srv.stopServer(c) })

	restoreEC2Patching := patchEC2ForTesting(c, s.srv.region)
	s.AddCleanup(func(c *gc.C) { restoreEC2Patching() })
}

func (s *ebsSuite) ebsProvider(c *gc.C) storage.Provider {
	provider, err := environs.Provider("ec2")
	c.Assert(err, jc.ErrorIsNil)

	credential := cloud.NewCredential(
		cloud.AccessKeyAuthType,
		map[string]string{
			"access-key": "x",
			"secret-key": "x",
		},
	)
	env, err := provider.Open(environs.OpenParams{
		Cloud: environs.CloudSpec{
			Type:       "ec2",
			Name:       "ec2test",
			Region:     s.srv.region.Name,
			Endpoint:   s.srv.region.EC2Endpoint,
			Credential: &credential,
		},
		Config: s.modelConfig,
	})
	c.Assert(err, jc.ErrorIsNil)

	p, err := env.StorageProvider(ec2.EBS_ProviderType)
	c.Assert(err, jc.ErrorIsNil)
	return p
}

func (s *ebsSuite) TestValidateConfigUnknownConfig(c *gc.C) {
	p := s.ebsProvider(c)
	cfg, err := storage.NewConfig("foo", ec2.EBS_ProviderType, map[string]interface{}{
		"unknown": "config",
	})
	c.Assert(err, jc.ErrorIsNil)
	err = p.ValidateConfig(cfg)
	c.Assert(err, jc.ErrorIsNil) // unknown attrs ignored
}

func (s *ebsSuite) TestSupports(c *gc.C) {
	p := s.ebsProvider(c)
	c.Assert(p.Supports(storage.StorageKindBlock), jc.IsTrue)
	c.Assert(p.Supports(storage.StorageKindFilesystem), jc.IsFalse)
}

func (s *ebsSuite) volumeSource(c *gc.C, cfg *storage.Config) storage.VolumeSource {
	p := s.ebsProvider(c)
	vs, err := p.VolumeSource(cfg)
	c.Assert(err, jc.ErrorIsNil)
	return vs
}

func (s *ebsSuite) createVolumes(vs storage.VolumeSource, instanceId string) ([]storage.CreateVolumesResult, error) {
	if instanceId == "" {
		instanceId = s.srv.ec2srv.NewInstances(1, "m1.medium", imageId, ec2test.Running, nil)[0]
	}
	volume0 := names.NewVolumeTag("0")
	volume1 := names.NewVolumeTag("1")
	volume2 := names.NewVolumeTag("2")
	params := []storage.VolumeParams{{
		Tag:      volume0,
		Size:     10 * 1000,
		Provider: ec2.EBS_ProviderType,
		Attributes: map[string]interface{}{
			"volume-type": "io1",
			"iops":        30,
		},
		Attachment: &storage.VolumeAttachmentParams{
			AttachmentParams: storage.AttachmentParams{
				InstanceId: instance.Id(instanceId),
			},
		},
		ResourceTags: map[string]string{
			tags.JujuModel: s.modelConfig.UUID(),
		},
	}, {
		Tag:      volume1,
		Size:     20 * 1000,
		Provider: ec2.EBS_ProviderType,
		Attachment: &storage.VolumeAttachmentParams{
			AttachmentParams: storage.AttachmentParams{
				InstanceId: instance.Id(instanceId),
			},
		},
		ResourceTags: map[string]string{
			tags.JujuModel: "something-else",
		},
	}, {
		Tag:      volume2,
		Size:     30 * 1000,
		Provider: ec2.EBS_ProviderType,
		ResourceTags: map[string]string{
			"abc": "123",
		},
		Attachment: &storage.VolumeAttachmentParams{
			AttachmentParams: storage.AttachmentParams{
				InstanceId: instance.Id(instanceId),
			},
		},
	}}
	return vs.CreateVolumes(params)
}

func (s *ebsSuite) assertCreateVolumes(c *gc.C, vs storage.VolumeSource, instanceId string) {
	results, err := s.createVolumes(vs, instanceId)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(results, gc.HasLen, 3)
	c.Assert(results[0].Volume, jc.DeepEquals, &storage.Volume{
		names.NewVolumeTag("0"),
		storage.VolumeInfo{
			Size:       10240,
			VolumeId:   "vol-0",
			Persistent: true,
		},
	})
	c.Assert(results[1].Volume, jc.DeepEquals, &storage.Volume{
		names.NewVolumeTag("1"),
		storage.VolumeInfo{
			Size:       20480,
			VolumeId:   "vol-1",
			Persistent: true,
		},
	})
	c.Assert(results[2].Volume, jc.DeepEquals, &storage.Volume{
		names.NewVolumeTag("2"),
		storage.VolumeInfo{
			Size:       30720,
			VolumeId:   "vol-2",
			Persistent: true,
		},
	})
	ec2Client := ec2.StorageEC2(vs)
	ec2Vols, err := ec2Client.Volumes(nil, nil)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(ec2Vols.Volumes, gc.HasLen, 3)
	sortBySize(ec2Vols.Volumes)
	c.Assert(ec2Vols.Volumes[0].Size, gc.Equals, 10)
	c.Assert(ec2Vols.Volumes[1].Size, gc.Equals, 20)
	c.Assert(ec2Vols.Volumes[2].Size, gc.Equals, 30)
}

var deleteSecurityGroupForTestFunc = func(inst ec2.SecurityGroupCleaner, group awsec2.SecurityGroup, _ clock.Clock) error {
	// With an exponential retry for deleting security groups,
	// we never return from local live tests.
	// No need to re-try in tests anyway - just call delete.
	_, err := inst.DeleteSecurityGroup(group)
	return err
}

type volumeSorter struct {
	vols []awsec2.Volume
	less func(i, j awsec2.Volume) bool
}

func sortBySize(vols []awsec2.Volume) {
	sort.Sort(volumeSorter{vols, func(i, j awsec2.Volume) bool {
		return i.Size < j.Size
	}})
}

func (s volumeSorter) Len() int {
	return len(s.vols)
}

func (s volumeSorter) Swap(i, j int) {
	s.vols[i], s.vols[j] = s.vols[j], s.vols[i]
}

func (s volumeSorter) Less(i, j int) bool {
	return s.less(s.vols[i], s.vols[j])
}

func (s *ebsSuite) TestCreateVolumes(c *gc.C) {
	vs := s.volumeSource(c, nil)
	s.assertCreateVolumes(c, vs, "")
}

func (s *ebsSuite) TestVolumeTags(c *gc.C) {
	vs := s.volumeSource(c, nil)
	results, err := s.createVolumes(vs, "")
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(results, gc.HasLen, 3)
	c.Assert(results[0].Error, jc.ErrorIsNil)
	c.Assert(results[0].Volume, jc.DeepEquals, &storage.Volume{
		names.NewVolumeTag("0"),
		storage.VolumeInfo{
			Size:       10240,
			VolumeId:   "vol-0",
			Persistent: true,
		},
	})
	c.Assert(results[1].Error, jc.ErrorIsNil)
	c.Assert(results[1].Volume, jc.DeepEquals, &storage.Volume{
		names.NewVolumeTag("1"),
		storage.VolumeInfo{
			Size:       20480,
			VolumeId:   "vol-1",
			Persistent: true,
		},
	})
	c.Assert(results[2].Error, jc.ErrorIsNil)
	c.Assert(results[2].Volume, jc.DeepEquals, &storage.Volume{
		names.NewVolumeTag("2"),
		storage.VolumeInfo{
			Size:       30720,
			VolumeId:   "vol-2",
			Persistent: true,
		},
	})
	ec2Client := ec2.StorageEC2(vs)
	ec2Vols, err := ec2Client.Volumes(nil, nil)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(ec2Vols.Volumes, gc.HasLen, 3)
	sortBySize(ec2Vols.Volumes)
	c.Assert(ec2Vols.Volumes[0].Tags, jc.SameContents, []awsec2.Tag{
		{"juju-model-uuid", "deadbeef-0bad-400d-8000-4b1d0d06f00d"},
		{"Name", "juju-testenv-volume-0"},
	})
	c.Assert(ec2Vols.Volumes[1].Tags, jc.SameContents, []awsec2.Tag{
		{"juju-model-uuid", "something-else"},
		{"Name", "juju-testenv-volume-1"},
	})
	c.Assert(ec2Vols.Volumes[2].Tags, jc.SameContents, []awsec2.Tag{
		{"Name", "juju-testenv-volume-2"},
		{"abc", "123"},
	})
}

func (s *ebsSuite) TestVolumeTypeAliases(c *gc.C) {
	instanceIdRunning := s.srv.ec2srv.NewInstances(1, "m1.medium", imageId, ec2test.Running, nil)[0]
	vs := s.volumeSource(c, nil)
	ec2Client := ec2.StorageEC2(vs)
	aliases := [][2]string{
		{"magnetic", "standard"},
		{"ssd", "gp2"},
		{"provisioned-iops", "io1"},
	}
	for i, alias := range aliases {
		params := []storage.VolumeParams{{
			Tag:      names.NewVolumeTag("0"),
			Size:     10 * 1000,
			Provider: ec2.EBS_ProviderType,
			Attributes: map[string]interface{}{
				"volume-type": alias[0],
			},
			Attachment: &storage.VolumeAttachmentParams{
				AttachmentParams: storage.AttachmentParams{
					InstanceId: instance.Id(instanceIdRunning),
				},
			},
		}}
		if alias[1] == "io1" {
			params[0].Attributes["iops"] = 30
		}
		results, err := vs.CreateVolumes(params)
		c.Assert(err, jc.ErrorIsNil)
		c.Assert(results, gc.HasLen, 1)
		c.Assert(results[0].Error, jc.ErrorIsNil)
		c.Assert(results[0].Volume.VolumeId, gc.Equals, fmt.Sprintf("vol-%d", i))
	}
	ec2Vols, err := ec2Client.Volumes(nil, nil)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(ec2Vols.Volumes, gc.HasLen, len(aliases))
	sort.Sort(volumeSorter{ec2Vols.Volumes, func(i, j awsec2.Volume) bool {
		return i.Id < j.Id
	}})
	for i, alias := range aliases {
		c.Assert(ec2Vols.Volumes[i].VolumeType, gc.Equals, alias[1])
	}
}

func (s *ebsSuite) TestDestroyVolumesNotFoundReturnsNil(c *gc.C) {
	vs := s.volumeSource(c, nil)
	results, err := vs.DestroyVolumes([]string{"vol-42"})
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(results, gc.HasLen, 1)
	c.Assert(results[0], jc.ErrorIsNil)
}

func (s *ebsSuite) TestDestroyVolumes(c *gc.C) {
	vs := s.volumeSource(c, nil)
	params := s.setupAttachVolumesTest(c, vs, ec2test.Running)
	errs, err := vs.DetachVolumes(params)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(errs, jc.DeepEquals, []error{nil})
	errs, err = vs.DestroyVolumes([]string{"vol-0"})
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(errs, jc.DeepEquals, []error{nil})

	ec2Client := ec2.StorageEC2(vs)
	ec2Vols, err := ec2Client.Volumes(nil, nil)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(ec2Vols.Volumes, gc.HasLen, 2)
	sortBySize(ec2Vols.Volumes)
	c.Assert(ec2Vols.Volumes[0].Size, gc.Equals, 20)
}

func (s *ebsSuite) TestDestroyVolumesStillAttached(c *gc.C) {
	vs := s.volumeSource(c, nil)
	s.setupAttachVolumesTest(c, vs, ec2test.Running)
	errs, err := vs.DestroyVolumes([]string{"vol-0"})
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(errs, jc.DeepEquals, []error{nil})

	ec2Client := ec2.StorageEC2(vs)
	ec2Vols, err := ec2Client.Volumes(nil, nil)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(ec2Vols.Volumes, gc.HasLen, 2)
	sortBySize(ec2Vols.Volumes)
	c.Assert(ec2Vols.Volumes[0].Size, gc.Equals, 20)
}

func (s *ebsSuite) TestDescribeVolumes(c *gc.C) {
	vs := s.volumeSource(c, nil)
	s.assertCreateVolumes(c, vs, "")

	vols, err := vs.DescribeVolumes([]string{"vol-0", "vol-1"})
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(vols, jc.DeepEquals, []storage.DescribeVolumesResult{{
		VolumeInfo: &storage.VolumeInfo{
			Size:       10240,
			VolumeId:   "vol-0",
			Persistent: true,
		},
	}, {
		VolumeInfo: &storage.VolumeInfo{
			Size:       20480,
			VolumeId:   "vol-1",
			Persistent: true,
		},
	}})
}

func (s *ebsSuite) TestDescribeVolumesNotFound(c *gc.C) {
	vs := s.volumeSource(c, nil)
	vols, err := vs.DescribeVolumes([]string{"vol-42"})
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(vols, gc.HasLen, 1)
	c.Assert(vols[0].Error, gc.ErrorMatches, "vol-42 not found")
}

func (s *ebsSuite) TestListVolumes(c *gc.C) {
	vs := s.volumeSource(c, nil)
	s.assertCreateVolumes(c, vs, "")

	// Only one volume created by assertCreateVolumes has
	// the model-uuid tag with the expected value.
	volIds, err := vs.ListVolumes()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(volIds, jc.SameContents, []string{"vol-0"})
}

func (s *ebsSuite) TestListVolumesIgnoresRootDisks(c *gc.C) {
	s.srv.ec2srv.SetCreateRootDisks(true)
	s.srv.ec2srv.NewInstances(1, "m1.medium", imageId, ec2test.Pending, nil)

	// Tag the root disk with the model UUID.
	_, err := s.srv.client.CreateTags([]string{"vol-0"}, []awsec2.Tag{
		{tags.JujuModel, s.modelConfig.UUID()},
	})
	c.Assert(err, jc.ErrorIsNil)

	vs := s.volumeSource(c, nil)
	volIds, err := vs.ListVolumes()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(volIds, gc.HasLen, 0)
}

func (s *ebsSuite) TestCreateVolumesErrors(c *gc.C) {
	vs := s.volumeSource(c, nil)
	volume0 := names.NewVolumeTag("0")

	instanceIdPending := s.srv.ec2srv.NewInstances(1, "m1.medium", imageId, ec2test.Pending, nil)[0]
	instanceIdRunning := s.srv.ec2srv.NewInstances(1, "m1.medium", imageId, ec2test.Running, nil)[0]
	attachmentParams := storage.VolumeAttachmentParams{
		AttachmentParams: storage.AttachmentParams{
			InstanceId: instance.Id(instanceIdRunning),
		},
	}

	for _, test := range []struct {
		params storage.VolumeParams
		err    string
	}{{
		params: storage.VolumeParams{
			Size:     1024,
			Provider: ec2.EBS_ProviderType,
			Attachment: &storage.VolumeAttachmentParams{
				AttachmentParams: storage.AttachmentParams{
					InstanceId: "woat",
				},
			},
		},
		err: `querying instance details: instance "woat" not found \(InvalidInstanceID.NotFound\)`,
	}, {
		params: storage.VolumeParams{
			Size:     1024,
			Provider: ec2.EBS_ProviderType,
			Attachment: &storage.VolumeAttachmentParams{
				AttachmentParams: storage.AttachmentParams{
					InstanceId: instance.Id(instanceIdPending),
				},
			},
		},
		err: "cannot attach to non-running instance i-3",
	}, {
		params: storage.VolumeParams{
			Size:       100000000,
			Provider:   ec2.EBS_ProviderType,
			Attributes: map[string]interface{}{},
			Attachment: &attachmentParams,
		},
		err: "volume size 97657 GiB exceeds the maximum of 1024 GiB",
	}, {
		params: storage.VolumeParams{
			Size:     100000000,
			Provider: ec2.EBS_ProviderType,
			Attributes: map[string]interface{}{
				"volume-type": "gp2",
			},
			Attachment: &attachmentParams,
		},
		err: "volume size 97657 GiB exceeds the maximum of 16384 GiB",
	}, {
		params: storage.VolumeParams{
			Size:     100000000,
			Provider: ec2.EBS_ProviderType,
			Attributes: map[string]interface{}{
				"volume-type": "io1",
				"iops":        "30",
			},
			Attachment: &attachmentParams,
		},
		err: "volume size 97657 GiB exceeds the maximum of 16384 GiB",
	}, {
		params: storage.VolumeParams{
			Tag:      volume0,
			Size:     1000,
			Provider: ec2.EBS_ProviderType,
			Attributes: map[string]interface{}{
				"volume-type": "io1",
				"iops":        "30",
			},
			Attachment: &attachmentParams,
		},
		err: "volume size is 1 GiB, must be at least 4 GiB",
	}, {
		params: storage.VolumeParams{
			Tag:      volume0,
			Size:     10000,
			Provider: ec2.EBS_ProviderType,
			Attributes: map[string]interface{}{
				"volume-type": "io1",
				"iops":        "1234",
			},
			Attachment: &attachmentParams,
		},
		err: "specified IOPS ratio is 1234/GiB, maximum is 30/GiB",
	}, {
		params: storage.VolumeParams{
			Tag:      volume0,
			Size:     10000,
			Provider: ec2.EBS_ProviderType,
			Attributes: map[string]interface{}{
				"volume-type": "standard",
				"iops":        "30",
			},
			Attachment: &attachmentParams,
		},
		err: `IOPS specified, but volume type is "standard"`,
	}, {
		params: storage.VolumeParams{
			Tag:      volume0,
			Size:     10000,
			Provider: ec2.EBS_ProviderType,
			Attributes: map[string]interface{}{
				"volume-type": "what",
			},
			Attachment: &attachmentParams,
		},
		err: "validating EBS storage config: volume-type: unexpected value \"what\"",
	}} {
		results, err := vs.CreateVolumes([]storage.VolumeParams{test.params})
		c.Assert(err, jc.ErrorIsNil)
		c.Assert(results, gc.HasLen, 1)
		c.Check(results[0].Error, gc.ErrorMatches, test.err)
	}
}

var imageId = "ami-ccf405a5" // Ubuntu Maverick, i386, EBS store

func (s *ebsSuite) setupAttachVolumesTest(
	c *gc.C, vs storage.VolumeSource, state awsec2.InstanceState,
) []storage.VolumeAttachmentParams {

	instanceId := s.srv.ec2srv.NewInstances(1, "m1.medium", imageId, state, nil)[0]
	s.assertCreateVolumes(c, vs, instanceId)

	return []storage.VolumeAttachmentParams{{
		Volume:   names.NewVolumeTag("0"),
		VolumeId: "vol-0",
		AttachmentParams: storage.AttachmentParams{
			Machine:    names.NewMachineTag("1"),
			InstanceId: instance.Id(instanceId),
		},
	}}
}

func (s *ebsSuite) TestAttachVolumesNotRunning(c *gc.C) {
	vs := s.volumeSource(c, nil)
	instanceId := s.srv.ec2srv.NewInstances(1, "m1.medium", imageId, ec2test.Pending, nil)[0]
	results, err := s.createVolumes(vs, instanceId)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(results, gc.Not(gc.HasLen), 0)
	for _, result := range results {
		c.Check(errors.Cause(result.Error), gc.ErrorMatches, "cannot attach to non-running instance i-3")
	}
}

func (s *ebsSuite) TestAttachVolumes(c *gc.C) {
	vs := s.volumeSource(c, nil)
	params := s.setupAttachVolumesTest(c, vs, ec2test.Running)
	result, err := vs.AttachVolumes(params)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(result, gc.HasLen, 1)
	c.Assert(result[0].Error, jc.ErrorIsNil)
	c.Assert(result[0].VolumeAttachment, jc.DeepEquals, &storage.VolumeAttachment{
		names.NewVolumeTag("0"),
		names.NewMachineTag("1"),
		storage.VolumeAttachmentInfo{
			DeviceName: "xvdf",
			ReadOnly:   false,
		},
	})

	ec2Client := ec2.StorageEC2(vs)
	ec2Vols, err := ec2Client.Volumes(nil, nil)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(ec2Vols.Volumes, gc.HasLen, 3)
	sortBySize(ec2Vols.Volumes)
	c.Assert(ec2Vols.Volumes[0].Attachments, jc.DeepEquals, []awsec2.VolumeAttachment{{
		VolumeId:   "vol-0",
		InstanceId: "i-3",
		Device:     "/dev/sdf",
		Status:     "attached",
	}})

	// Test idempotency.
	result, err = vs.AttachVolumes(params)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(result, gc.HasLen, 1)
	c.Assert(result[0].Error, jc.ErrorIsNil)
	c.Assert(result[0].VolumeAttachment, jc.DeepEquals, &storage.VolumeAttachment{
		names.NewVolumeTag("0"),
		names.NewMachineTag("1"),
		storage.VolumeAttachmentInfo{
			DeviceName: "xvdf",
			ReadOnly:   false,
		},
	})
}

func (s *ebsSuite) TestAttachVolumesCreating(c *gc.C) {
	vs := s.volumeSource(c, nil)
	params := s.setupAttachVolumesTest(c, vs, ec2test.Running)
	var calls int
	s.srv.proxy.ModifyResponse = makeDescribeVolumesResponseModifier(func(resp *awsec2.VolumesResp) error {
		if len(resp.Volumes) != 1 {
			return errors.New("expected one volume")
		}
		calls++
		if calls == 1 {
			resp.Volumes[0].Status = "creating"
		} else {
			resp.Volumes[0].Status = "available"
		}
		return nil
	})
	result, err := vs.AttachVolumes(params)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(result, gc.HasLen, 1)
	c.Assert(result[0].Error, jc.ErrorIsNil)
	c.Assert(calls, gc.Equals, 2)
}

func (s *ebsSuite) TestAttachVolumesDetaching(c *gc.C) {
	vs := s.volumeSource(c, nil)
	params := s.setupAttachVolumesTest(c, vs, ec2test.Running)
	s.srv.proxy.ModifyResponse = makeDescribeVolumesResponseModifier(func(resp *awsec2.VolumesResp) error {
		if len(resp.Volumes) != 1 {
			return errors.New("expected one volume")
		}
		resp.Volumes[0].Status = "in-use"
		resp.Volumes[0].Attachments = append(resp.Volumes[0].Attachments, awsec2.VolumeAttachment{
			InstanceId: "something else",
		})
		return nil
	})
	result, err := vs.AttachVolumes(params)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(result, gc.HasLen, 1)
	c.Assert(result[0].Error, gc.ErrorMatches, "volume vol-0 is attached to something else")
}

func (s *ebsSuite) TestDetachVolumes(c *gc.C) {
	vs := s.volumeSource(c, nil)
	params := s.setupAttachVolumesTest(c, vs, ec2test.Running)
	_, err := vs.AttachVolumes(params)
	c.Assert(err, jc.ErrorIsNil)
	errs, err := vs.DetachVolumes(params)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(errs, jc.DeepEquals, []error{nil})

	ec2Client := ec2.StorageEC2(vs)
	ec2Vols, err := ec2Client.Volumes(nil, nil)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(ec2Vols.Volumes, gc.HasLen, 3)
	sortBySize(ec2Vols.Volumes)
	c.Assert(ec2Vols.Volumes[0].Attachments, gc.HasLen, 0)

	// Test idempotent
	errs, err = vs.DetachVolumes(params)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(errs, jc.DeepEquals, []error{nil})
}

func (s *ebsSuite) TestDetachVolumesIncorrectState(c *gc.C) {
	s.testDetachVolumesDetachedState(c, "IncorrectState")
}

func (s *ebsSuite) TestDetachVolumesAttachmentNotFound(c *gc.C) {
	s.testDetachVolumesDetachedState(c, "InvalidAttachment.NotFound")
}

func (s *ebsSuite) testDetachVolumesDetachedState(c *gc.C, errorCode string) {
	vs := s.volumeSource(c, nil)
	params := s.setupAttachVolumesTest(c, vs, ec2test.Running)
	_, err := vs.AttachVolumes(params)
	c.Assert(err, jc.ErrorIsNil)

	s.srv.proxy.ModifyResponse = func(resp *http.Response) error {
		resp.StatusCode = http.StatusBadRequest
		return replaceResponseBody(resp, ec2Errors{[]awsec2.Error{{
			Code: errorCode,
		}}})
	}
	errs, err := vs.DetachVolumes(params)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(errs, jc.DeepEquals, []error{nil})
}

type blockDeviceMappingSuite struct {
	testing.BaseSuite
}

var _ = gc.Suite(&blockDeviceMappingSuite{})

func (*blockDeviceMappingSuite) TestBlockDeviceNamer(c *gc.C) {
	var nextName func() (string, string, error)
	expect := func(expectRequest, expectActual string) {
		request, actual, err := nextName()
		c.Assert(err, jc.ErrorIsNil)
		c.Assert(request, gc.Equals, expectRequest)
		c.Assert(actual, gc.Equals, expectActual)
	}
	expectN := func(expectRequest, expectActual string) {
		for i := 1; i <= 6; i++ {
			request, actual, err := nextName()
			c.Assert(err, jc.ErrorIsNil)
			c.Assert(request, gc.Equals, expectRequest+strconv.Itoa(i))
			c.Assert(actual, gc.Equals, expectActual+strconv.Itoa(i))
		}
	}
	expectErr := func(expectErr string) {
		_, _, err := nextName()
		c.Assert(err, gc.ErrorMatches, expectErr)
	}

	// First without numbers.
	nextName = ec2.BlockDeviceNamer(false)
	expect("/dev/sdf", "xvdf")
	expect("/dev/sdg", "xvdg")
	expect("/dev/sdh", "xvdh")
	expect("/dev/sdi", "xvdi")
	expect("/dev/sdj", "xvdj")
	expect("/dev/sdk", "xvdk")
	expect("/dev/sdl", "xvdl")
	expect("/dev/sdm", "xvdm")
	expect("/dev/sdn", "xvdn")
	expect("/dev/sdo", "xvdo")
	expect("/dev/sdp", "xvdp")
	expectErr("too many EBS volumes to attach")

	// Now with numbers.
	nextName = ec2.BlockDeviceNamer(true)
	expect("/dev/sdf1", "xvdf1")
	expect("/dev/sdf2", "xvdf2")
	expect("/dev/sdf3", "xvdf3")
	expect("/dev/sdf4", "xvdf4")
	expect("/dev/sdf5", "xvdf5")
	expect("/dev/sdf6", "xvdf6")
	expectN("/dev/sdg", "xvdg")
	expectN("/dev/sdh", "xvdh")
	expectN("/dev/sdi", "xvdi")
	expectN("/dev/sdj", "xvdj")
	expectN("/dev/sdk", "xvdk")
	expectN("/dev/sdl", "xvdl")
	expectN("/dev/sdm", "xvdm")
	expectN("/dev/sdn", "xvdn")
	expectN("/dev/sdo", "xvdo")
	expectN("/dev/sdp", "xvdp")
	expectErr("too many EBS volumes to attach")
}

func (*blockDeviceMappingSuite) TestGetBlockDeviceMappings(c *gc.C) {
	mapping := ec2.GetBlockDeviceMappings(constraints.Value{}, "trusty", false)
	c.Assert(mapping, gc.DeepEquals, []awsec2.BlockDeviceMapping{{
		VolumeSize: 8,
		DeviceName: "/dev/sda1",
	}, {
		VirtualName: "ephemeral0",
		DeviceName:  "/dev/sdb",
	}, {
		VirtualName: "ephemeral1",
		DeviceName:  "/dev/sdc",
	}, {
		VirtualName: "ephemeral2",
		DeviceName:  "/dev/sdd",
	}, {
		VirtualName: "ephemeral3",
		DeviceName:  "/dev/sde",
	}})
}

func (*blockDeviceMappingSuite) TestGetBlockDeviceMappingsController(c *gc.C) {
	mapping := ec2.GetBlockDeviceMappings(constraints.Value{}, "trusty", true)
	c.Assert(mapping, gc.DeepEquals, []awsec2.BlockDeviceMapping{{
		VolumeSize: 32,
		DeviceName: "/dev/sda1",
	}, {
		VirtualName: "ephemeral0",
		DeviceName:  "/dev/sdb",
	}, {
		VirtualName: "ephemeral1",
		DeviceName:  "/dev/sdc",
	}, {
		VirtualName: "ephemeral2",
		DeviceName:  "/dev/sdd",
	}, {
		VirtualName: "ephemeral3",
		DeviceName:  "/dev/sde",
	}})
}

func makeDescribeVolumesResponseModifier(modify func(*awsec2.VolumesResp) error) func(*http.Response) error {
	return func(resp *http.Response) error {
		if resp.Request.URL.Query().Get("Action") != "DescribeVolumes" {
			return nil
		}
		var respDecoded struct {
			XMLName xml.Name
			awsec2.VolumesResp
		}
		if err := xml.NewDecoder(resp.Body).Decode(&respDecoded); err != nil {
			return err
		}
		resp.Body.Close()

		if err := modify(&respDecoded.VolumesResp); err != nil {
			return err
		}
		return replaceResponseBody(resp, &respDecoded)
	}
}

func replaceResponseBody(resp *http.Response, value interface{}) error {
	var buf bytes.Buffer
	if err := xml.NewEncoder(&buf).Encode(value); err != nil {
		return err
	}
	resp.Body = ioutil.NopCloser(&buf)
	return nil
}

type ec2Errors struct {
	Errors []awsec2.Error `xml:"Errors>Error"`
}
