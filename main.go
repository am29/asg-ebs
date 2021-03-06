package main // import "github.com/Jimdo/asg-ebs"

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"

	"gopkg.in/alecthomas/kingpin.v2"

	log "github.com/Sirupsen/logrus"
)

type createFileSystemOnVolumeTimeout struct{}

func (e createFileSystemOnVolumeTimeout) Error() string {
	return "Volume Timeout"
}

type ByStartTime []*ec2.Snapshot

func (s ByStartTime) Len() int           { return len(s) }
func (s ByStartTime) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s ByStartTime) Less(i, j int) bool { return (*s[i].StartTime).Before(*s[j].StartTime) }

func waitForFile(file string, timeout time.Duration) error {
	startTime := time.Now()
	if _, err := os.Stat(file); err == nil {
		return nil
	}
	newTimeout := timeout - time.Since(startTime)
	if newTimeout > 0 {
		return waitForFile(file, newTimeout)
	} else {
		return errors.New("File " + file + " not found")
	}
}

func run(cmd string, args ...string) error {
	log.WithFields(log.Fields{"cmd": cmd, "args": args}).Info("Running command")
	out, err := exec.Command(cmd, args...).CombinedOutput()
	if err != nil {
		log.WithFields(log.Fields{"cmd": cmd, "args": args, "err": err, "out": string(out)}).Info("Error running command")
		return err
	}
	return nil
}

func slurpFile(file string) string {
	v, err := ioutil.ReadFile(file)
	if err != nil {
		log.WithFields(log.Fields{"err": err, "file": file}).Info("Failed to read file")
	}
	return string(v)
}

type AsgEbs interface {
	checkDevice(device string) error
	checkMountPoint(mountPoint string) error
	findVolume(tagKey string, tagValue string) (*string, error)
	attachVolume(volumeId string, attachAs string, deleteOnTermination bool) error
	findSnapshot(tagKey string, tagValue string) (*string, error)
	createVolume(createSize int64, createName string, createVolumeType string, createTags map[string]string, snapshotId *string) (*string, error)
	mountVolume(device string, mountPoint string) error
	makeFileSystem(device string, mkfsInodeRatio int64, volumeId string) error
	waitUntilVolumeAvailable(volumeId string) error
}

type AwsAsgEbs struct {
	AwsConfig        *aws.Config
	Region           string
	AvailabilityZone string
	InstanceId       string
}

func NewAwsAsgEbs(maxRetries int) *AwsAsgEbs {
	awsAsgEbs := &AwsAsgEbs{}

	metadata := ec2metadata.New(session.New())

	region, err := metadata.Region()
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Fatal("Failed to get region from instance metadata")
	}
	log.WithFields(log.Fields{"region": region}).Info("Setting region")
	awsAsgEbs.Region = region

	availabilityZone, err := metadata.GetMetadata("placement/availability-zone")
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Fatal("Failed to get availability zone from instance metadata")
	}
	log.WithFields(log.Fields{"az": availabilityZone}).Info("Setting availability zone")
	awsAsgEbs.AvailabilityZone = availabilityZone

	instanceId, err := metadata.GetMetadata("instance-id")
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Fatal("Failed to get instance id from instance metadata")
	}
	log.WithFields(log.Fields{"instance_id": instanceId}).Info("Setting instance id")
	awsAsgEbs.InstanceId = instanceId

	awsAsgEbs.AwsConfig = aws.NewConfig().
		WithRegion(region).
		WithCredentials(ec2rolecreds.NewCredentials(session.New())).
		WithMaxRetries(maxRetries)

	return awsAsgEbs
}

func (awsAsgEbs *AwsAsgEbs) findVolume(tagKey string, tagValue string) (*string, error) {
	svc := ec2.New(session.New(awsAsgEbs.AwsConfig))

	params := &ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("tag:" + tagKey),
				Values: []*string{
					aws.String(tagValue),
				},
			},
			{
				Name: aws.String("tag:filesystem"),
				Values: []*string{
					aws.String("true"),
				},
			},
			{
				Name: aws.String("status"),
				Values: []*string{
					aws.String("available"),
				},
			},
			{
				Name: aws.String("availability-zone"),
				Values: []*string{
					aws.String(awsAsgEbs.AvailabilityZone),
				},
			},
		},
	}

	describeVolumesOutput, err := svc.DescribeVolumes(params)
	if err != nil {
		return nil, err
	}
	if len(describeVolumesOutput.Volumes) == 0 {
		return nil, nil
	}
	return describeVolumesOutput.Volumes[0].VolumeId, nil
}

func (awsAsgEbs *AwsAsgEbs) findSnapshot(tagKey string, tagValue string) (*string, error) {
	svc := ec2.New(session.New(awsAsgEbs.AwsConfig))

	describeSnapshotsInput := &ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("tag:" + tagKey),
				Values: []*string{
					aws.String(tagValue),
				},
			},
			{
				Name: aws.String("status"),
				Values: []*string{
					aws.String("completed"),
				},
			},
		},
	}
	describeSnapshotsOutput, err := svc.DescribeSnapshots(describeSnapshotsInput)
	if err != nil {
		return nil, err
	}
	snapshots := describeSnapshotsOutput.Snapshots
	sort.Sort(sort.Reverse(ByStartTime(snapshots)))

	if len(snapshots) == 0 {
		return nil, nil
	}

	return snapshots[0].SnapshotId, nil
}

func (awsAsgEbs *AwsAsgEbs) createVolume(createSize int64, createName string, createVolumeType string, createTags map[string]string, snapshotId *string) (*string, error) {
	svc := ec2.New(session.New(awsAsgEbs.AwsConfig))

	filesystem := "false"

	createVolumeInput := &ec2.CreateVolumeInput{
		AvailabilityZone: &awsAsgEbs.AvailabilityZone,
		Size:             aws.Int64(createSize),
		VolumeType:       aws.String(createVolumeType),
	}

	if snapshotId != nil {
		createVolumeInput.SnapshotId = aws.String(*snapshotId)
		filesystem = "true"
	}

	vol, err := svc.CreateVolume(createVolumeInput)
	if err != nil {
		return nil, err
	}
	tags := []*ec2.Tag{
		{
			Key:   aws.String("Name"),
			Value: aws.String(createName),
		},
		{
			Key:   aws.String("filesystem"),
			Value: aws.String(filesystem),
		},
	}
	for k, v := range createTags {
		tags = append(tags,
			&ec2.Tag{
				Key:   aws.String(k),
				Value: aws.String(v),
			},
		)
	}

	createTagsInput := &ec2.CreateTagsInput{
		Resources: []*string{vol.VolumeId},
		Tags:      tags,
	}
	_, err = svc.CreateTags(createTagsInput)
	if err != nil {
		return vol.VolumeId, err
	}

	return vol.VolumeId, nil
}

func (awsAsgEbs *AwsAsgEbs) waitUntilVolumeAvailable(volumeId string) error {
	svc := ec2.New(session.New(awsAsgEbs.AwsConfig))

	describeVolumeInput := &ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String(volumeId)},
	}
	err := svc.WaitUntilVolumeAvailable(describeVolumeInput)
	if err != nil {
		return &createFileSystemOnVolumeTimeout{}
	}
	return nil
}

func (awsAsgEbs *AwsAsgEbs) attachVolume(volumeId string, attachAs string, deleteOnTermination bool) error {
	svc := ec2.New(session.New(awsAsgEbs.AwsConfig))

	attachVolumeInput := &ec2.AttachVolumeInput{
		VolumeId:   aws.String(volumeId),
		Device:     aws.String(attachAs),
		InstanceId: aws.String(awsAsgEbs.InstanceId),
	}
	_, err := svc.AttachVolume(attachVolumeInput)
	if err != nil {
		return err
	}

	describeVolumeInput := &ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String(volumeId)},
	}
	err = svc.WaitUntilVolumeInUse(describeVolumeInput)
	if err != nil {
		return err
	}

	if deleteOnTermination {
		modifyInstanceAttributeInput := &ec2.ModifyInstanceAttributeInput{
			Attribute:  aws.String("blockDeviceMapping"),
			InstanceId: aws.String(awsAsgEbs.InstanceId),
			BlockDeviceMappings: []*ec2.InstanceBlockDeviceMappingSpecification{
				{
					DeviceName: aws.String(attachAs),
					Ebs: &ec2.EbsInstanceBlockDeviceSpecification{
						DeleteOnTermination: aws.Bool(true),
						VolumeId:            aws.String(volumeId),
					},
				},
			},
		}
		_, err = svc.ModifyInstanceAttribute(modifyInstanceAttributeInput)
		if err != nil {
			return err
		}
	}

	err = waitForFile("/dev/"+attachAs, 60*time.Second)
	if err != nil {
		return err
	}

	return nil
}

func (awsAsgEbs *AwsAsgEbs) makeFileSystem(device string, mkfsInodeRatio int64, volumeId string) error {
	svc := ec2.New(session.New(awsAsgEbs.AwsConfig))

	err := run("/usr/sbin/mkfs.ext4", "-i", fmt.Sprintf("%d", mkfsInodeRatio), device)
	if err != nil {
		return err
	}
	tags := []*ec2.Tag{
		{
			Key:   aws.String("filesystem"),
			Value: aws.String("true"),
		},
	}
	createTagsInput := &ec2.CreateTagsInput{
		Resources: []*string{aws.String(volumeId)},
		Tags:      tags,
	}
	_, err = svc.CreateTags(createTagsInput)
	if err != nil {
		return err
	}
	return nil
}

func (awsAsgEbs *AwsAsgEbs) mountVolume(device string, mountPoint string) error {
	err := os.MkdirAll(mountPoint, 0755)
	if err != nil {
		return err
	}
	return run("/bin/mount", device, mountPoint)
}

func (awsAsgEbs *AwsAsgEbs) checkDevice(device string) error {
	if _, err := os.Stat(device); !os.IsNotExist(err) {
		return errors.New("Device exists")
	}
	return nil
}

func (awsAsgEbs *AwsAsgEbs) checkMountPoint(mountPoint string) error {
	if strings.Contains(slurpFile("/proc/mounts"), mountPoint) {
		return errors.New("Already mounted")
	}
	return nil
}

type CreateTagsValue map[string]string

func (v CreateTagsValue) Set(str string) error {
	parts := strings.SplitN(str, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("expected KEY=VALUE got '%s'", str)
	}
	key := parts[0]
	value := parts[1]
	v[key] = value
	return nil
}

func (v CreateTagsValue) String() string {
	return ""
}

func CreateTags(s kingpin.Settings) (target *map[string]string) {
	newMap := make(map[string]string)
	target = &newMap
	s.SetValue((*CreateTagsValue)(target))
	return
}

func runAsgEbs(asgEbs AsgEbs, cfg Config) {

	createFileSystemOnVolume := false
	var volumeId *string
	var snapshotId *string
	attachAsDevice := "/dev/" + *cfg.attachAs

	// Precondition checks
	err := asgEbs.checkDevice(attachAsDevice)
	if err != nil {
		log.WithFields(log.Fields{"device": attachAsDevice}).Fatal("Device already exists")
	}

	err = asgEbs.checkMountPoint(*cfg.mountPoint)
	if err != nil {
		log.WithFields(log.Fields{"mount_point": *cfg.mountPoint}).Fatal("Already mounted")
	}

	if *cfg.snapshotName == "" {
		for i := 1; i <= 10; i++ {
			volumeId, err = asgEbs.findVolume(*cfg.tagKey, *cfg.tagValue)
			if err != nil {
				log.WithFields(log.Fields{"error": err}).Fatal("Failed to find volume")
			}
			if volumeId == nil {
				break
			} else {
				log.WithFields(log.Fields{"volume": *volumeId, "device": attachAsDevice, "attempt": i}).Info("Trying to attach existing volume")
				err = asgEbs.attachVolume(*volumeId, *cfg.attachAs, *cfg.deleteOnTermination)
				if err != nil {
					log.WithFields(log.Fields{"error": err}).Warn("Failed to attach volume")
				} else {
					break
				}
			}
		}
	} else {
		snapshotId, err = asgEbs.findSnapshot("Name", *cfg.snapshotName)
		if err != nil {
			log.WithFields(log.Fields{"error": err, "snapshot_name": *cfg.snapshotName}).Fatal("Failed to find snapshot")
		}
	}

	if volumeId == nil {
		log.Info("Creating new volume")
		volumeId, err = asgEbs.createVolume(*cfg.createSize, *cfg.createName, *cfg.createVolumeType, *cfg.createTags, snapshotId)
		if err != nil {
			log.WithFields(log.Fields{"error": err}).Fatal("Failed to create new volume")
		}
		log.WithFields(log.Fields{"volume": *volumeId}).Info("Waiting until new volume is available")
		err = asgEbs.waitUntilVolumeAvailable(*volumeId)
		if err != nil {
			log.WithFields(log.Fields{"error": err, "volume": *volumeId}).Fatal("Waiting for volume timed out")
		}
		if snapshotId == nil {
			createFileSystemOnVolume = true
		}
		log.WithFields(log.Fields{"volume": *volumeId, "device": attachAsDevice}).Info("Attaching volume")
		err = asgEbs.attachVolume(*volumeId, *cfg.attachAs, *cfg.deleteOnTermination)
		if err != nil {
			log.WithFields(log.Fields{"error": err}).Fatal("Failed to attach volume")
		}
	}

	if createFileSystemOnVolume {
		log.WithFields(log.Fields{"device": attachAsDevice}).Info("Creating file system on new volume")
		err = asgEbs.makeFileSystem(attachAsDevice, *cfg.mkfsInodeRatio, *volumeId)
		if err != nil {
			log.WithFields(log.Fields{"error": err}).Fatal("Failed to create file system")
		}
	}

	log.WithFields(log.Fields{"device": attachAsDevice, "mount_point": *cfg.mountPoint}).Info("Mounting volume")
	err = asgEbs.mountVolume(attachAsDevice, *cfg.mountPoint)
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Fatal("Failed to mount volume")
	}

}

type Config struct {
	tagKey              *string
	tagValue            *string
	attachAs            *string
	mountPoint          *string
	createSize          *int64
	mkfsInodeRatio      *int64
	createName          *string
	createVolumeType    *string
	createTags          *map[string]string
	deleteOnTermination *bool
	snapshotName        *string
	maxRetries          *int
}

func main() {
	cfg := &Config{
		tagKey:              kingpin.Flag("tag-key", "The tag key to search for").Required().PlaceHolder("KEY").String(),
		tagValue:            kingpin.Flag("tag-value", "The tag value to search for").Required().PlaceHolder("VALUE").String(),
		attachAs:            kingpin.Flag("attach-as", "device name e.g. xvdb").Required().PlaceHolder("DEVICE").String(),
		mountPoint:          kingpin.Flag("mount-point", "Directory where the volume will be mounted").Required().PlaceHolder("DIR").String(),
		createSize:          kingpin.Flag("create-size", "The size of the created volume, in GiBs").Required().PlaceHolder("SIZE").Int64(),
		mkfsInodeRatio:      kingpin.Flag("mkfs-inode-ratio", "mkfs.ext4 inode ratio (-i)").Default("16384").Int64(),
		createName:          kingpin.Flag("create-name", "The name of the created volume").Required().PlaceHolder("NAME").String(),
		createVolumeType:    kingpin.Flag("create-volume-type", "The volume type of the created volume. This can be `gp2` for General Purpose (SSD) volumes or `standard` for Magnetic volumes").Required().PlaceHolder("TYPE").Enum("standard", "gp2"),
		createTags:          CreateTags(kingpin.Flag("create-tags", "Tag to use for the new volume, can be specified multiple times").PlaceHolder("KEY=VALUE")),
		deleteOnTermination: kingpin.Flag("delete-on-termination", "Delete volume when instance is terminated").Bool(),
		snapshotName:        kingpin.Flag("snapshot-name", "Name of snapshot to use for new volume").String(),
		maxRetries:          kingpin.Flag("max-retries", "Maximum number of retries for AWS requests").Default("20").Int(),
	}

	kingpin.UsageTemplate(kingpin.CompactUsageTemplate)
	kingpin.CommandLine.Help = "Script to create, attach, format and mount an EBS Volume to an EC2 instance"
	kingpin.Parse()

	awsAsgEbs := NewAwsAsgEbs(*cfg.maxRetries)

	runAsgEbs(awsAsgEbs, *cfg)

}
