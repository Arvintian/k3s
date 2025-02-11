package secretsencrypt

import (
	"context"
	"fmt"
	"strings"

	"github.com/k3s-io/k3s/pkg/cluster"
	"github.com/k3s-io/k3s/pkg/daemons/config"
	"github.com/k3s-io/k3s/pkg/util"
	coreclient "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/pager"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
)

const (
	controllerAgentName        string = "reencrypt-controller"
	secretsUpdateStartEvent    string = "SecretsUpdateStart"
	secretsProgressEvent       string = "SecretsProgress"
	secretsUpdateCompleteEvent string = "SecretsUpdateComplete"
	secretsUpdateErrorEvent    string = "SecretsUpdateError"
	controlPlaneRoleLabelKey   string = "node-role.kubernetes.io/control-plane"
)

type handler struct {
	ctx           context.Context
	controlConfig *config.Control
	nodes         coreclient.NodeController
	secrets       coreclient.SecretController
	recorder      record.EventRecorder
}

func Register(
	ctx context.Context,
	k8s kubernetes.Interface,
	controlConfig *config.Control,
	nodes coreclient.NodeController,
	secrets coreclient.SecretController,
) error {
	h := &handler{
		ctx:           ctx,
		controlConfig: controlConfig,
		nodes:         nodes,
		secrets:       secrets,
		recorder:      util.BuildControllerEventRecorder(k8s, controllerAgentName, metav1.NamespaceDefault),
	}

	nodes.OnChange(ctx, "reencrypt-controller", h.onChangeNode)
	return nil
}

// onChangeNode handles changes to Nodes. We are looking for a specific annotation change
func (h *handler) onChangeNode(nodeName string, node *corev1.Node) (*corev1.Node, error) {
	if node == nil {
		return nil, nil
	}

	ann, ok := node.Annotations[EncryptionHashAnnotation]
	if !ok {
		return node, nil
	}

	if valid, err := h.validateReencryptStage(node, ann); err != nil {
		h.recorder.Event(node, corev1.EventTypeWarning, secretsUpdateErrorEvent, err.Error())
		return node, err
	} else if !valid {
		return node, nil
	}

	reencryptHash, err := GenReencryptHash(h.controlConfig.Runtime, EncryptionReencryptActive)
	if err != nil {
		h.recorder.Event(node, corev1.EventTypeWarning, secretsUpdateErrorEvent, err.Error())
		return node, err
	}
	ann = EncryptionReencryptActive + "-" + reencryptHash

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err = h.nodes.Get(nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		node.Annotations[EncryptionHashAnnotation] = ann
		_, err = h.nodes.Update(node)
		return err
	})
	if err != nil {
		h.recorder.Event(node, corev1.EventTypeWarning, secretsUpdateErrorEvent, err.Error())
		return node, err
	}

	if err := h.updateSecrets(node); err != nil {
		h.recorder.Event(node, corev1.EventTypeWarning, secretsUpdateErrorEvent, err.Error())
		return node, err
	}

	// If skipping, revert back to the previous stage
	if h.controlConfig.EncryptSkip {
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			node, err = h.nodes.Get(nodeName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			BootstrapEncryptionHashAnnotation(node, h.controlConfig.Runtime)
			_, err = h.nodes.Update(node)
			return err
		})
		return node, err
	}

	// Remove last key
	curKeys, err := GetEncryptionKeys(h.controlConfig.Runtime)
	if err != nil {
		h.recorder.Event(node, corev1.EventTypeWarning, secretsUpdateErrorEvent, err.Error())
		return node, err
	}

	curKeys = curKeys[:len(curKeys)-1]
	if err = WriteEncryptionConfig(h.controlConfig.Runtime, curKeys, true); err != nil {
		h.recorder.Event(node, corev1.EventTypeWarning, secretsUpdateErrorEvent, err.Error())
		return node, err
	}
	logrus.Infoln("Removed key: ", curKeys[len(curKeys)-1])
	if err != nil {
		h.recorder.Event(node, corev1.EventTypeWarning, secretsUpdateErrorEvent, err.Error())
		return node, err
	}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err = h.nodes.Get(nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		return WriteEncryptionHashAnnotation(h.controlConfig.Runtime, node, EncryptionReencryptFinished)
	})
	if err != nil {
		h.recorder.Event(node, corev1.EventTypeWarning, secretsUpdateErrorEvent, err.Error())
		return node, err
	}
	if err := cluster.Save(h.ctx, h.controlConfig, true); err != nil {
		h.recorder.Event(node, corev1.EventTypeWarning, secretsUpdateErrorEvent, err.Error())
		return node, err
	}
	return node, nil
}

// validateReencryptStage ensures that the request for reencryption is valid and
// that there is only one active reencryption at a time
func (h *handler) validateReencryptStage(node *corev1.Node, annotation string) (bool, error) {
	split := strings.Split(annotation, "-")
	if len(split) != 2 {
		err := fmt.Errorf("invalid annotation %s found on node %s", annotation, node.ObjectMeta.Name)
		return false, err
	}
	stage := split[0]
	hash := split[1]

	// Validate the specific stage and the request via sha256 hash
	if stage != EncryptionReencryptRequest {
		return false, nil
	}
	if reencryptRequestHash, err := GenReencryptHash(h.controlConfig.Runtime, EncryptionReencryptRequest); err != nil {
		return false, err
	} else if reencryptRequestHash != hash {
		err = fmt.Errorf("invalid hash: %s found on node %s", hash, node.ObjectMeta.Name)
		return false, err
	}
	reencryptActiveHash, err := GenReencryptHash(h.controlConfig.Runtime, EncryptionReencryptActive)
	if err != nil {
		return false, err
	}
	labelSelector := labels.Set{controlPlaneRoleLabelKey: "true"}.String()
	nodes, err := h.nodes.List(metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return false, err
	}
	for _, node := range nodes.Items {
		if ann, ok := node.Annotations[EncryptionHashAnnotation]; ok {
			split := strings.Split(ann, "-")
			if len(split) != 2 {
				return false, fmt.Errorf("invalid annotation %s found on node %s", ann, node.ObjectMeta.Name)
			}
			stage := split[0]
			hash := split[1]
			if stage == EncryptionReencryptActive && hash == reencryptActiveHash {
				return false, fmt.Errorf("another reencrypt is already active")
			}
		}
	}
	return true, nil
}

func (h *handler) updateSecrets(node *corev1.Node) error {
	secretPager := pager.New(pager.SimplePageFunc(func(opts metav1.ListOptions) (runtime.Object, error) {
		return h.secrets.List("", opts)
	}))
	i := 0
	secretPager.EachListItem(h.ctx, metav1.ListOptions{}, func(obj runtime.Object) error {
		if secret, ok := obj.(*corev1.Secret); ok {
			if _, err := h.secrets.Update(secret); err != nil {
				return fmt.Errorf("failed to reencrypted secret: %v", err)
			}
			if i != 0 && i%10 == 0 {
				h.recorder.Eventf(node, corev1.EventTypeNormal, secretsProgressEvent, "reencrypted %d secrets", i)
			}
			i++
		}
		return nil
	})
	h.recorder.Eventf(node, corev1.EventTypeNormal, secretsUpdateCompleteEvent, "completed reencrypt of %d secrets", i)
	return nil
}
