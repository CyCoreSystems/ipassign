package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/ericchiang/k8s"
	corev1 "github.com/ericchiang/k8s/apis/core/v1"
	"github.com/inconshreveable/log15"
	"github.com/pkg/errors"
)

var ipTagKey = "voice"
var ipTagVal = "proxy"
var nodeKey = "voice"
var nodeVal = "proxy"

var testMode bool

var log log15.Logger

func init() {
	flag.BoolVar(&testMode, "test", false, "test mode will not update associations")
	flag.StringVar(&ipTagKey, "ipTagKey", "voice", "key name by which the potential Elastic IPs will be tagged")
	flag.StringVar(&ipTagVal, "ipTagVal", "proxy", "key value by which potential Elastic IPs will be tagged")
	flag.StringVar(&nodeKey, "nodeKey", "voice", "key name by which potential Nodes will be tagged")
	flag.StringVar(&nodeVal, "nodeVal", "proxy", "key value by which potential Nodes will be tagged")

	log = log15.New()
}

func main() {
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		log.Crit("AWS_ACCESS_KEY_ID must be defined")
		os.Exit(1)
	}
	if os.Getenv("AWS_SECRET_ACCESS_KEY") == "" {
		log.Crit("AWS_SECRET_ACCESS_KEY must be defined")
		os.Exit(1)
	}
	if os.Getenv("AWS_REGION") == "" {
		log.Crit("AWS_REGION must be defined")
		os.Exit(1)
	}
	if os.Getenv("GROUP") == "" {
		log.Crit("GROUP must be defined")
		os.Exit(1)
	}
	if os.Getenv("IP_TAG_KEY") != "" {
		ipTagKey = os.Getenv("IP_TAG_KEY")
	}
	if os.Getenv("IP_TAG_VAL") != "" {
		ipTagVal = os.Getenv("IP_TAG_VAL")
	}
	if os.Getenv("NODE_KEY") != "" {
		nodeKey = os.Getenv("NODE_KEY")
	}
	if os.Getenv("NODE_VAL") != "" {
		nodeVal = os.Getenv("NODE_VAL")
	}

	for ctx.Err() == nil {
		kc, err := k8s.NewInClusterClient()
		if err != nil {
			log.Crit("failed to connect to kubernetes API server", "error", err)
			os.Exit(1)
		}

		m := &Manager{
			kc: kc,
		}

		err = m.watch(ctx)
		if errors.Cause(err) == io.EOF {
			log.Debug("gRPC connection timed out")
		} else {
			log.Warn("watch exited", "error", err)
		}
	}
	os.Exit(1)
}

// Manager manages IP assignments for proxy nodes
type Manager struct {
	kc *k8s.Client
}

func (m *Manager) watch(ctx context.Context) error {
	resourceModel := new(corev1.Node)
	w, err := m.kc.Watch(ctx, "", resourceModel)
	if err != nil {
		return errors.Wrap(err, "failed to watch nodes")
	}
	defer w.Close() // nolint

	// Run an initial reconciliation
	if err = m.reconcile(ctx); err != nil {
		return errors.Wrap(err, "initial reconciliation failed")
	}

	// Process node changes
	for {
		ref := new(corev1.Node)
		if _, err = w.Next(ref); err != nil {
			return errors.Wrap(err, "error during watch of nodes")
		}

		if err = m.reconcile(ctx); err != nil {
			return errors.Wrap(err, "failed to reconcile nodes with IPs")
		}
	}
}

func (m *Manager) getProxyNodes(ctx context.Context) (ret []*corev1.Node, err error) {
	list := new(corev1.NodeList)
	if err = m.kc.List(ctx, "", list); err != nil {
		return
	}

	for _, n := range list.GetItems() {
		if val, ok := n.GetMetadata().GetLabels()[nodeKey]; ok && val == nodeVal {
			ret = append(ret, n)
		}
	}
	if len(ret) < 1 {
		err = errors.New("no proxy nodes found")
	}
	return
}

func (m *Manager) associateIP(ctx context.Context, instanceID string, ipID string) error {
	interfaceID, err := m.getInterfaceID(ctx, instanceID)
	if err != nil {
		return err
	}

	if testMode {
		log.Debug(fmt.Sprintf("TEST MODE: not associating node %s, interface %s with IP %s", instanceID, interfaceID, ipID))
		return nil
	}

	sess, err := session.NewSession()
	if err != nil {
		return errors.Wrap(err, "failed to create AWS session")
	}

	_, err = ec2.New(sess).AssociateAddress(&ec2.AssociateAddressInput{
		AllocationId:       aws.String(ipID),
		NetworkInterfaceId: aws.String(interfaceID),
		AllowReassociation: aws.Bool(true),
	})
	return err
}

func (m *Manager) getInterfaceID(ctx context.Context, instanceID string) (string, error) {
	sess, err := session.NewSession()
	if err != nil {
		return "", errors.Wrap(err, "failed to create AWS session")
	}

	res, err := ec2.New(sess).DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{&ec2.Filter{
			Name:   aws.String("attachment.instance-id"),
			Values: []*string{aws.String(instanceID)},
		}},
	})
	if err != nil {
		return "", errors.Wrapf(err, "failed to get interfaces for instanceID %s", instanceID)
	}
	if len(res.NetworkInterfaces) < 1 {
		return "", errors.New("instance has no interfaces")
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
	return "", errors.New("no valid interface found")
}

func (m *Manager) getElasticIPs(ctx context.Context) (ret []*ec2.Address, err error) {
	var res *ec2.DescribeAddressesOutput

	sess, err := session.NewSession()
	if err != nil {
		return nil, errors.Wrap(err, "failed to create AWS session")
	}

	res, err = ec2.New(sess).DescribeAddresses(&ec2.DescribeAddressesInput{
		Filters: []*ec2.Filter{
			&ec2.Filter{
				Name: aws.String(fmt.Sprintf("tag:%s", ipTagKey)),
				Values: []*string{
					aws.String(ipTagVal),
				},
			},
			&ec2.Filter{
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
	if len(res.Addresses) < 1 {
		return nil, errors.New("no ElasticIPs found")
	}
	return res.Addresses, nil
}

func (m *Manager) rebootNode(ctx context.Context, id string) error {
	sess, err := session.NewSession()
	if err != nil {
		return errors.Wrap(err, "failed to create AWS session")
	}

	_, err = ec2.New(sess).RebootInstances(&ec2.RebootInstancesInput{
		InstanceIds: []*string{
			aws.String(id),
		},
	})
	return err
}

func (m *Manager) reconcile(ctx context.Context) error {
	nodeList, err := m.getProxyNodes(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to get list of proxy nodes")
	}

	ipList, err := m.getElasticIPs(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to get list of Elastic IPs")
	}

	var availableIPs []*ec2.Address
	for _, ip := range ipList {
		var matched bool
		for _, n := range nodeList {
			id, err := instanceID(n)
			if err != nil {
				log.Warn("failed to get instanceID for node", "node", n.String())
				continue
			}
			if aws.StringValue(ip.InstanceId) == id {
				matched = true
				break
			}
		}
		if !matched {
			log.Debug(fmt.Sprintf("IP address %s is available", aws.StringValue(ip.PublicIp)))
			availableIPs = append(availableIPs, ip)
		}
	}

	var availableNodes []*corev1.Node
	for _, n := range nodeList {
		id, err := instanceID(n)
		if err != nil {
			log.Warn("failed to get instanceID for node", "node", n.String())
			continue
		}

		var matched bool
		for _, ip := range ipList {
			if id == aws.StringValue(ip.InstanceId) {
				matched = true
				break
			}
		}
		if !matched {
			log.Debug(fmt.Sprintf("Node %s is available", n.GetMetadata().GetName()))
			availableNodes = append(availableNodes, n)
		}
	}

	// Pair available IPs to available nodes
	if len(availableNodes) > 0 {
		id, err := instanceID(availableNodes[0])
		if err != nil {
			return errors.Wrapf(err, "failed to get instance ID of first available node %s", availableNodes[0].String())
		}

		if len(availableIPs) < 1 {
			return errors.Errorf("no IP addresses available for %d nodes", len(availableNodes))
		}

		log.Info(fmt.Sprintf("associating node %s with IP %s", id, aws.StringValue(availableIPs[0].PublicIp)))
		if err = m.associateIP(ctx, id, aws.StringValue(availableIPs[0].AllocationId)); err != nil {
			return errors.Wrapf(err, "failed to associate IP %s with node %s", aws.StringValue(availableIPs[0].PublicIp), id)
		}

		log.Info("waiting 1 minute following association")
		time.Sleep(time.Minute)

		log.Info(fmt.Sprintf("rebooting node %s after assigning IP %s", id, aws.StringValue(availableIPs[0].PublicIp)))
		return errors.Wrapf(m.rebootNode(ctx, id), "failed to reboot node %s", id)
	}

	if len(availableIPs) > 0 {
		log.Debug("all nodes have IPs, but there are spare IP addresses available")
	}
	return nil
}

func instanceID(n *corev1.Node) (string, error) {
	provID := n.GetSpec().GetProviderID()
	u, err := url.Parse(provID)
	if err != nil {
		return "", errors.Wrapf(err, "failed to parse ProviderID (%s) as an URL", provID)
	}
	pathPieces := strings.Split(u.Path, "/")
	if len(pathPieces) < 1 {
		return "", errors.Wrapf(err, "unexpected ProviderID URL format: %s", u.Path)
	}
	return pathPieces[len(pathPieces)-1], nil
}
