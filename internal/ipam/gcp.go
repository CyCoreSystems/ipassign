package ipam

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/CyCoreSystems/ipassign/internal/annotation"

	"cloud.google.com/go/compute/metadata"
	"github.com/rotisserie/eris"
	gcpv1 "google.golang.org/api/compute/v1"
	v1 "k8s.io/api/core/v1"
)

var apiPollInterval = time.Second

type gcpAssigner struct {
	project string
	zone    string

	ipTagKey string
	ipTagVal string

	// testMode bool
}

// NewGCPAssigner returns a new GCP IP address Assigner
func NewGCPAssigner(zone string) (Assigner, error) {
	mc := metadata.NewClient(http.DefaultClient)

	project, err := mc.ProjectID()
	if err != nil {
		return nil, eris.Wrap(err, "failed to get project ID from metadata server")
	}

	if zone == "" {
		zone, err = mc.Zone()
		if err != nil {
			return nil, eris.Wrap(err, "failed to retrieve zone from metadata server")
		}
	}

	return &gcpAssigner{
		project:  project,
		zone:     zone,
		ipTagKey: os.Getenv("IP_TAG_KEY"),
		ipTagVal: os.Getenv("IP_TAG_VAL"),
	}, nil
}

func (a *gcpAssigner) Assign(ctx context.Context, nodeToAssign v1.Node, ann annotation.Annotator) error {
	svc, err := gcpv1.NewService(ctx)
	if err != nil {
		return eris.Wrap(err, "failed to create GCP compute instance API client")
	}

	ip, err := a.nextAvailableIP(svc)
	if err != nil {
		return eris.Wrap(err, "failed to get next available IP")
	}

	if ip == "" {
		return ErrNoIPs
	}

	if ann != nil {
		if err = ann.Set(ctx, ip); err != nil {
			return eris.Wrapf(err, "failed to annotate selected node %q", nodeToAssign.Name)
		}
	}

	if err = a.assignIP(ctx, svc, ip, nodeToAssign.Name); err != nil {
		if ann != nil {
			if err2 := ann.Set(ctx, ""); err2 != nil {
				return eris.Wrapf(err, "CRITICAL: failed to assign IP %q to node %q AND FAILED to remove annotation from that node: %s", ip, nodeToAssign.Name, err2.Error())
			}
		}
		return eris.Wrapf(err, "failed to assign IP %q to node %q", ip, nodeToAssign.Name)
	}

	return nil
}

func (a *gcpAssigner) nextAvailableIP(svc *gcpv1.Service) (string, error) {
	list, err := svc.Addresses.List(a.project, a.zone).Filter(fmt.Sprintf(`%s = %s`, a.ipTagKey, a.ipTagVal)).Do()
	if err != nil {
		return "", eris.Wrapf(err, "failed to get IP list from project %q, zone %q matching labels %q = %q", a.project, a.zone, a.ipTagKey, a.ipTagVal)
	}

	for _, addr := range list.Items {
		if addr.Status == "RESERVED" {
			return addr.Address, nil
		}
	}

	return "", nil
}

func (a *gcpAssigner) assignIP(ctx context.Context, svc *gcpv1.Service, ip string, instance string) error {
	// Fist, see if we need to remove an existing AccessConfig (almost certainly so)
	i, err := svc.Instances.Get(a.project, a.zone, instance).Do()
	if err != nil {
		return eris.Wrap(err, "failed to load instance")
	}

	if len(i.NetworkInterfaces) != 1 {
		return eris.Errorf("unhandled interfaces count (%d) for instance %q", len(i.NetworkInterfaces), instance)
	}

	if len(i.NetworkInterfaces[0].AccessConfigs) > 1 {
		return eris.Errorf("unhandled accessConfig count (%d) for instance %q", len(i.NetworkInterfaces[0].AccessConfigs), instance)
	}

	if len(i.NetworkInterfaces[0].AccessConfigs) > 0 {
		op, err := svc.Instances.DeleteAccessConfig(a.project, a.zone, instance, i.NetworkInterfaces[0].AccessConfigs[0].Name, i.NetworkInterfaces[0].Name).Context(ctx).Do()
		if err != nil {
			return eris.Wrapf(err, "failed to delete existing accessConfig from instance %q", instance)
		}

		for op.Status != "DONE" {
			select {
			case <-ctx.Done():
				return eris.Wrapf(err, "IP deletion from %q cancelled", instance)
			case <-time.After(apiPollInterval):
			}
		}

		if op.Error != nil {
			return eris.Wrapf(opError(op.Error), "failed to delete existing accessConfig from instance %q", instance)
		}
	}

	// Now Add the new AccessConfig
	op, err := svc.Instances.AddAccessConfig(a.project, a.zone, instance, i.NetworkInterfaces[0].Name, &gcpv1.AccessConfig{
		Name:        "External NAT",
		NatIP:       ip,
		NetworkTier: "PREMIUM",
		Type:        "ONE_TO_ONE_NAT",
	}).Context(ctx).Do()
	if err != nil {
		return eris.Wrapf(err, "failed to assign %q to %q", ip, instance)
	}

	for op.Status != "DONE" {
		select {
		case <-ctx.Done():
			return eris.Wrapf(err, "IP assignment of %q to %q cancelled", ip, instance)
		case <-time.After(apiPollInterval):
		}
	}

	if op.Error != nil {
		return eris.Wrapf(opError(op.Error), "failed to assign IP %q to %q", ip, instance)
	}

	return nil
}

func opError(errList *gcpv1.OperationError) error {
	if errList == nil {
		return nil
	}

	if len(errList.Errors) < 1 {
		return nil
	}

	var outList []string
	for _, e := range errList.Errors {
		outList = append(outList, e.Message)
	}

	return eris.Errorf("%s", strings.Join(outList, ","))
}
