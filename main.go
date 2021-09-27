package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/CyCoreSystems/ipassign/internal/annotation"
	"github.com/CyCoreSystems/ipassign/internal/constants"
	"github.com/CyCoreSystems/ipassign/internal/ipam"

	"github.com/pkg/errors"
	"github.com/rotisserie/eris"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var ipTagKey = "voice"
var ipTagVal = "proxy"
var nodeKey = "voice"
var nodeVal = "proxy"
var zone = ""

var cloud = "aws"

var testMode bool

var log *zap.SugaredLogger

var watchLoopShortCycleBuffer = 2 * time.Second

func init() {
	flag.BoolVar(&testMode, "test", false, "test mode will not update associations")
	flag.StringVar(&ipTagKey, "ipTagKey", "voice", "key name by which the potential Elastic IPs will be tagged")
	flag.StringVar(&ipTagVal, "ipTagVal", "proxy", "key value by which potential Elastic IPs will be tagged")
	flag.StringVar(&nodeKey, "nodeKey", "voice", "key name by which potential Nodes will be tagged")
	flag.StringVar(&nodeVal, "nodeVal", "proxy", "key value by which potential Nodes will be tagged")

	flag.StringVar(&cloud, "cloud", "aws", "cloud platform: aws, gcp")
	flag.StringVar(&zone, "zone", "", "override zone setting (e.g. for GCP global zone)")
}

func main() {
	flag.Parse()

	logger, err := zap.NewProduction()
	if err != nil {
		panic("failed to instantiate logger: " + err.Error())
	}
	log = logger.Sugar()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
	if os.Getenv("CLOUD") != "" {
		cloud = os.Getenv("CLOUD")
	}
	if os.Getenv("ZONE") != "" {
		zone = os.Getenv("ZONE")
	}

	var cloudAssigner ipam.Assigner
	switch cloud {
	case "aws":
		cloudAssigner, err = ipam.NewAWSAssigner()
		if err != nil {
			log.Fatalw("failed to create AWS IP assigner", "error", err)
		}
	case "gcp":
		cloudAssigner, err = ipam.NewGCPAssigner(zone)
		if err != nil {
			log.Fatalw("failed to create GCP IP assigner")
		}
	default:
		log.Fatalf("unhandled cloud type %q", cloud)
	}

	for ctx.Err() == nil {
		config, err := rest.InClusterConfig()
		if err != nil {
			log.Fatalw("failed to create kubernetes config", "error", err)
		}

		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			log.Fatalw("failed to create kubernetes clientset", "error", err)
		}

		m := &Manager{
			clientset: clientset,
			assigner:  cloudAssigner,
		}

		err = m.watch(ctx)
		if errors.Cause(err) == io.EOF {
			log.Debugw("gRPC connection timed out")
		} else if kerr, ok := errors.Cause(err).(*kerrors.StatusError); ok  {
			errString, errData := kerr.DebugError()

			log.Warnf("watch exited" + errString, errData)
		} else {
			log.Warnw("watch exited", "error", err)
		}

		if ctx.Err() == nil {
			time.Sleep(watchLoopShortCycleBuffer)
		}
	}
	os.Exit(1)
}

// Manager manages IP assignments for proxy nodes
type Manager struct {
	clientset *kubernetes.Clientset
	assigner  ipam.Assigner
}

func nodeListOptions() metav1.ListOptions {
	return metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", nodeKey, nodeVal),
	}
}

func (m *Manager) watch(ctx context.Context) error {
	w, err := m.clientset.CoreV1().Nodes().Watch(ctx, nodeListOptions())
	if err != nil {
		return eris.Wrap(err, "failed to create node watcher")
	}
	defer w.Stop()

	for {
		ev, ok := <-w.ResultChan()
		if !ok {
			return eris.New("watcher exited")
		}

		if ev.Type == watch.Added || ev.Type == watch.Deleted || ev.Type == watch.Modified {
			if err := m.reconcile(ctx); err != nil {
				return eris.Wrap(err, "failed to reconcile")
			}
		}
	}
}

func (m *Manager) getNextNode(ctx context.Context) (n v1.Node, err error) {
	nodes, err := m.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return n, eris.Wrap(err, "failed to get list of Nodes")
	}

	for _, n = range nodes.Items {
		if val, ok := n.Labels[nodeKey]; ok {
			if val == nodeVal {
				// This is a node we are looking for

				if _, ok = n.Annotations[constants.AnnotationAssignment]; ok {
					// node already assigned an IP
					continue
				}

				return n, nil
			}
		}
	}

	// No available nodes
	return
}

func (m *Manager) reconcile(ctx context.Context) error {
	node, err := m.getNextNode(ctx)
	if err != nil {
		return eris.Wrap(err, "failed to get next available node")
	}

	if node.Name == "" {
		// No nodes awaiting IP assignment
		return nil
	}

	// Mark the node for allocation
	err = m.assigner.Assign(ctx, node, annotation.NodeAnnotator(m.clientset.CoreV1().Nodes(), node.Name, constants.AnnotationAssignment))
	if err == ipam.ErrNoIPs {
		return nil
	}
	if err != nil {
		return eris.Wrapf(err, "failed to assign IP to node %q", node.Name)
	}

	return nil
}
