package ipam

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/CyCoreSystems/ipassign/internal/annotation"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/rotisserie/eris"
	v1 "k8s.io/api/core/v1"
)

type awsAssigner struct {
	accessKey string
	secretKey string
	group     string
	region    string

	ipTagKey string
	ipTagVal string

	// testMode bool
}

// NewAWSAssigner returns a new AWS IP address Assigner
func NewAWSAssigner() (Assigner, error) {
	if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		return nil, eris.New("AWS_ACCESS_KEY_ID must be defined")
	}
	if os.Getenv("AWS_SECRET_ACCESS_KEY") == "" {
		return nil, eris.New("AWS_SECRET_ACCESS_KEY must be defined")
	}
	if os.Getenv("AWS_REGION") == "" {
		return nil, eris.New("AWS_REGION must be defined")
	}
	if os.Getenv("GROUP") == "" {
		return nil, eris.New("GROUP must be defined")
	}

	a := &awsAssigner{
		accessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
		secretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		region:    os.Getenv("AWS_REGION"),
		group:     os.Getenv("GROUP"),
		ipTagKey:  os.Getenv("IP_TAG_KEY"),
		ipTagVal:  os.Getenv("IP_TAG_VAL"),
	}

	return a, nil
}

func (a *awsAssigner) Assign(ctx context.Context, nodeToAssign v1.Node, ann annotation.Annotator) error {
	instanceID, err := awsInstanceID(nodeToAssign)
	if err != nil {
		return eris.Wrapf(err, "failed to determine instanceID of node %q", nodeToAssign.Name)
	}

	eip, err := awsGetElasticIPs(a.ipTagKey, a.ipTagVal)
	if err != nil {
		return eris.Wrap(err, "failed to get available IPs")
	}

	if eip == nil {
		return ErrNoIPs
	}

	if ann != nil {
		if err = ann.Set(ctx, aws.StringValue(eip.PublicIp)); err != nil {
			return eris.Wrapf(err, "failed to mark node %q with IP assignment", nodeToAssign.Name)
		}
	}

	if err = awsAssociateIP(ctx, instanceID, eip.AllocationId); err != nil {
		if ann != nil {
			if err2 := ann.Set(ctx, ""); err2 != nil {
				return eris.Wrapf(err, "CRITICAL: failed to assign IP %q to node %q AND FAILED to remove annotation from that node: %s", aws.StringValue(eip.PublicIp), nodeToAssign.Name, err2.Error())
			}
		}
		return eris.Wrapf(err, "failed to assign IP %q to node %q", aws.StringValue(eip.PublicIp), nodeToAssign.Name)
	}

	// AWS is slow and does not provide any API-based feedback mechanism (found
	// so far, anyway), so we wait a full minute before rebooting the instance
	// for the IP change to take effect.
	time.Sleep(time.Minute)

	if err = awsRebootInstance(instanceID); err != nil {
		return eris.Wrapf(err, "failed to reboot instance %s", instanceID)
	}

	return nil
}

func awsAssociateIP(ctx context.Context, instanceID string, ipID *string) error {
	interfaceID, err := awsGetInterfaceID(ctx, instanceID)
	if err != nil {
		return err
	}

	/*
		if r.testMode {
			log.Debugf(fmt.Sprintf("TEST MODE: not associating node %s, interface %s with IP %s", instanceID, interfaceID, ipID))
			return nil
		}
	*/

	sess, err := session.NewSession()
	if err != nil {
		return eris.Wrap(err, "failed to create AWS session")
	}

	_, err = ec2.New(sess).AssociateAddress(&ec2.AssociateAddressInput{
		AllocationId:       ipID,
		NetworkInterfaceId: aws.String(interfaceID),
		AllowReassociation: aws.Bool(true),
	})
	return err
}

func awsGetInterfaceID(ctx context.Context, instanceID string) (string, error) {
	sess, err := session.NewSession()
	if err != nil {
		return "", eris.Wrap(err, "failed to create AWS session")
	}

	res, err := ec2.New(sess).DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("attachment.instance-id"),
				Values: []*string{aws.String(instanceID)},
			},
		},
	})
	if err != nil {
		return "", eris.Wrapf(err, "failed to get interfaces for instanceID %s", instanceID)
	}
	if len(res.NetworkInterfaces) < 1 {
		return "", eris.New("instance has no interfaces")
	}

	// As of 2019-02-04, EKS instances have three interfaces.  Only one of these
	// is useful to us:  it is the one which has a non-nil Association (of a
	// public IP address).
	// NOTE: this is an UNDOCUMENTED EKS internal which may be subject to change!
	for _, i := range res.NetworkInterfaces {
		if i.Association == nil {
			continue
		}
		return aws.StringValue(i.NetworkInterfaceId), nil
	}
	return "", eris.New("no valid interface found")
}

func awsGetElasticIPs(key, val string) (*ec2.Address, error) {
	var res *ec2.DescribeAddressesOutput

	sess, err := session.NewSession()
	if err != nil {
		return nil, eris.Wrap(err, "failed to create AWS session")
	}

	res, err = ec2.New(sess).DescribeAddresses(&ec2.DescribeAddressesInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String(fmt.Sprintf("tag:%s", key)),
				Values: []*string{
					aws.String(val),
				},
			},
			{
				Name: aws.String("tag:group"),
				Values: []*string{
					aws.String(os.Getenv("GROUP")),
				},
			},
		},
	})
	if err != nil {
		return nil, err
	}

	for _, eip := range res.Addresses {
		if eip.InstanceId == nil || *eip.InstanceId == "" {
			return eip, nil
		}
	}

	return nil, nil
}

func awsRebootInstance(id string) error {
	sess, err := session.NewSession()
	if err != nil {
		return eris.Wrap(err, "failed to create AWS session")
	}

	_, err = ec2.New(sess).RebootInstances(&ec2.RebootInstancesInput{
		InstanceIds: []*string{
			aws.String(id),
		},
	})
	return err
}

func awsInstanceID(n v1.Node) (string, error) {
	provID := n.Spec.ProviderID

	u, err := url.Parse(provID)
	if err != nil {
		return "", eris.Wrapf(err, "failed to parse ProviderID (%s) as an URL", provID)
	}

	pathPieces := strings.Split(u.Path, "/")
	if len(pathPieces) < 1 {
		return "", eris.Wrapf(err, "unexpected ProviderID URL format: %s", u.Path)
	}

	return pathPieces[len(pathPieces)-1], nil
}
