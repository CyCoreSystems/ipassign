package ipam

import (
	"context"
	"errors"

	"github.com/CyCoreSystems/ipassign/internal/annotation"

	v1 "k8s.io/api/core/v1"
)

// ErrNoIPs indicates that there were no public IP addresses available for assignment
var ErrNoIPs = errors.New("no public IP addresses available")

// An Assigner is an IP Address manager which can assign an available Public IP address to a Node
type Assigner interface {

	// Assign will assign an available public IP to the given Node
	Assign(ctx context.Context, node v1.Node, annotator annotation.Annotator) error
}
