// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package watchers

import (
	"errors"
	"sync"

	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"

	"github.com/cilium/cilium/pkg/k8s"
	ciliumio "github.com/cilium/cilium/pkg/k8s/apis/cilium.io"
	"github.com/cilium/cilium/pkg/k8s/informer"
	slim_corev1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/core/v1"
	slim_metav1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/apis/meta/v1"
	slimclientset "github.com/cilium/cilium/pkg/k8s/slim/k8s/client/clientset/versioned"
	"github.com/cilium/cilium/pkg/k8s/utils"
	"github.com/cilium/cilium/pkg/k8s/watchers/resources"
	"github.com/cilium/cilium/pkg/labels"
	"github.com/cilium/cilium/pkg/labelsfilter"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/policy"
)

func (k *K8sWatcher) namespacesInit(slimClient slimclientset.Interface, asyncControllers *sync.WaitGroup) {
	apiGroup := k8sAPIGroupNamespaceV1Core
	namespaceStore, namespaceController := informer.NewInformer(
		utils.ListerWatcherFromTyped[*slim_corev1.NamespaceList](slimClient.CoreV1().Namespaces()),
		&slim_corev1.Namespace{},
		0,
		cache.ResourceEventHandlerFuncs{
			// AddFunc does not matter since the endpoint will fetch
			// namespace labels when the endpoint is created
			// DelFunc does not matter since, when a namespace is deleted, all
			// pods belonging to that namespace are also deleted.
			UpdateFunc: func(oldObj, newObj interface{}) {
				var valid, equal bool
				defer k.K8sEventReceived(apiGroup, metricNS, resources.MetricUpdate, valid, equal)
				if oldNS := k8s.ObjToV1Namespace(oldObj); oldNS != nil {
					if newNS := k8s.ObjToV1Namespace(newObj); newNS != nil {
						valid = true
						if oldNS.DeepEqual(newNS) {
							equal = true
							return
						}

						err := k.updateK8sV1Namespace(oldNS, newNS)
						k.K8sEventProcessed(metricNS, resources.MetricUpdate, err == nil)
					}
				}
			},
		},
		nil,
	)

	k.namespaceStore = namespaceStore
	k.blockWaitGroupToSyncResources(k.stop, nil, namespaceController.HasSynced, k8sAPIGroupNamespaceV1Core)
	k.k8sAPIGroups.AddAPI(apiGroup)
	asyncControllers.Done()
	namespaceController.Run(k.stop)
}

func (k *K8sWatcher) updateK8sV1Namespace(oldNS, newNS *slim_corev1.Namespace) error {
	oldNSLabels := map[string]string{}
	newNSLabels := map[string]string{}

	for k, v := range oldNS.GetLabels() {
		oldNSLabels[policy.JoinPath(ciliumio.PodNamespaceMetaLabels, k)] = v
	}
	for k, v := range newNS.GetLabels() {
		newNSLabels[policy.JoinPath(ciliumio.PodNamespaceMetaLabels, k)] = v
	}

	oldLabels := labels.Map2Labels(oldNSLabels, labels.LabelSourceK8s)
	newLabels := labels.Map2Labels(newNSLabels, labels.LabelSourceK8s)

	oldIdtyLabels, _ := labelsfilter.Filter(oldLabels)
	newIdtyLabels, _ := labelsfilter.Filter(newLabels)

	// Do not perform any other operations the the old labels are the same as
	// the new labels
	if oldIdtyLabels.DeepEqual(&newIdtyLabels) {
		return nil
	}

	eps := k.endpointManager.GetEndpoints()
	failed := false
	for _, ep := range eps {
		epNS := ep.GetK8sNamespace()
		if oldNS.Name == epNS {
			err := ep.ModifyIdentityLabels(newIdtyLabels, oldIdtyLabels)
			if err != nil {
				log.WithError(err).WithField(logfields.EndpointID, ep.ID).
					Warningf("unable to update endpoint with new namespace labels")
				failed = true
			}
		}
	}
	if failed {
		return errors.New("unable to update some endpoints with new namespace labels")
	}
	return nil
}

// GetCachedNamespace returns a namespace from the local store.
func (k *K8sWatcher) GetCachedNamespace(namespace string) (*slim_corev1.Namespace, error) {
	<-k.controllersStarted
	k.WaitForCacheSync(k8sAPIGroupNamespaceV1Core)
	nsName := &slim_corev1.Namespace{
		ObjectMeta: slim_metav1.ObjectMeta{
			Name: namespace,
		},
	}
	namespaceInterface, exists, err := k.namespaceStore.Get(nsName)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, k8s_errors.NewNotFound(schema.GroupResource{
			Group:    "core",
			Resource: "namespace",
		}, namespace)
	}
	return namespaceInterface.(*slim_corev1.Namespace).DeepCopy(), nil
}
