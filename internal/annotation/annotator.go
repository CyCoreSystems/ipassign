package annotation

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// Annotator describes somewhich which can add or remove a particular annotation to a kubernetes object.
type Annotator interface {
	// Set the annotation to the object
	Set(ctx context.Context, val string) error
}

type nodeAnnotator struct {
	key      string
	nodeName string
	k        v1.NodeInterface
}

// NodeAnnotator returns a new Annotator for a given node and annotation
func NodeAnnotator(k v1.NodeInterface, nodeName, key string) Annotator {
	return &nodeAnnotator{
		key:      key,
		nodeName: nodeName,
		k:        k,
	}
}

func (a *nodeAnnotator) Set(ctx context.Context, val string) error {
	_, err := a.k.Patch(ctx, a.nodeName, types.StrategicMergePatchType, []byte(fmt.Sprintf(`
metadata:
  annotations:
	 %s: %q
`, a.key, val)), metav1.PatchOptions{})

	return err
}
