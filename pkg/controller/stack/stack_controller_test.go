// +build integration

package stack

import (
	"fmt"
	"testing"
	"time"

	deploymentsv1alpha1 "github.com/elastic/stack-operators/pkg/apis/deployments/v1alpha1"
	"github.com/elastic/stack-operators/pkg/utils/test"
	"github.com/stretchr/testify/assert"
	"golang.org/x/net/context"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var c client.Client

var expectedRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: "foo", Namespace: "default"}}
var kibanaDeploymentKey = types.NamespacedName{Name: "foo-kibana", Namespace: "default"}
var discoveryServiceKey = types.NamespacedName{Name: "foo-es-discovery", Namespace: "default"}
var publicServiceKey = types.NamespacedName{Name: "foo-es-public", Namespace: "default"}

func checkReconcileCalled(t *testing.T, requests chan reconcile.Request) {
	select {
	case req := <-requests:
		assert.Equal(t, req, expectedRequest)
	case <-time.After(test.Timeout):
		assert.Fail(t, fmt.Sprintf("No request received after %s", test.Timeout))
	}
}

func checkResourceDeletionTriggersReconcile(t *testing.T, requests chan reconcile.Request, objKey types.NamespacedName, obj runtime.Object) {
	assert.NoError(t, c.Delete(context.TODO(), obj))
	checkReconcileCalled(t, requests)
	test.RetryUntilSuccess(t, func() error { return c.Get(context.TODO(), objKey, obj) })
}

func getESPods(t *testing.T) []corev1.Pod {
	esPods := &corev1.PodList{}
	esPodSelector := client.ListOptions{Namespace: "default"}
	err := esPodSelector.SetLabelSelector("stack.k8s.elastic.co/type=elasticsearch")
	assert.NoError(t, err)
	test.RetryUntilSuccess(t, func() error {
		return c.List(context.TODO(), &esPodSelector, esPods)
	})
	return esPods.Items
}

func TestReconcile(t *testing.T) {
	instance := &deploymentsv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "default"},
		Spec: deploymentsv1alpha1.StackSpec{
			Elasticsearch: deploymentsv1alpha1.ElasticsearchSpec{
				SetVMMaxMapCount: false,
				NodeCount:        3,
			},
		},
	}

	// Setup the Manager and Controller.  Wrap the Controller Reconcile function so it writes each request to a
	// channel when it is finished.
	mgr, err := manager.New(cfg, manager.Options{})
	assert.NoError(t, err)
	c = mgr.GetClient()

	recFn, requests := SetupTestReconcile(newReconciler(mgr))
	assert.NoError(t, add(mgr, recFn))

	stopMgr, mgrStopped := StartTestManager(mgr, t)

	defer func() {
		close(stopMgr)
		mgrStopped.Wait()
	}()

	// Create the Stack object and expect the Reconcile and Deployment to be created
	err = c.Create(context.TODO(), instance)
	// The instance object may not be a valid object because it might be missing some required fields.
	// Please modify the instance object by adding required fields and then remove the following if statement.
	if apierrors.IsInvalid(err) {
		t.Logf("failed to create object, got an invalid object error: %v", err)
		return
	}
	assert.NoError(t, err)
	defer c.Delete(context.TODO(), instance)

	checkReconcileCalled(t, requests)

	// Elasticsearch pods should be created
	esPods := getESPods(t)
	assert.Equal(t, 3, len(esPods))

	// Kibana deployment should be created
	deploy := &appsv1.Deployment{}
	test.RetryUntilSuccess(t, func() error { return c.Get(context.TODO(), kibanaDeploymentKey, deploy) })

	// Services should be created
	discoveryService := &corev1.Service{}
	test.RetryUntilSuccess(t, func() error { return c.Get(context.TODO(), discoveryServiceKey, discoveryService) })
	publicService := &corev1.Service{}
	test.RetryUntilSuccess(t, func() error { return c.Get(context.TODO(), publicServiceKey, publicService) })

	// Delete resources and expect Reconcile to be called and eventually recreate them
	// ES pod
	assert.NoError(t, c.Delete(context.TODO(), &esPods[0]))
	checkReconcileCalled(t, requests)
	test.RetryUntilSuccess(t, func() error {
		nPods := len(getESPods(t))
		if nPods != 3 {
			return fmt.Errorf("Got %d pods out of 3", nPods)
		}
		return nil
	})
	// Kibana
	checkResourceDeletionTriggersReconcile(t, requests, kibanaDeploymentKey, deploy)
	// Services
	checkResourceDeletionTriggersReconcile(t, requests, publicServiceKey, publicService)
	checkResourceDeletionTriggersReconcile(t, requests, discoveryServiceKey, discoveryService)

	// Manually delete Deployment and Services since GC might not be enabled in the test control plane
	clean(t, deploy)
	clean(t, publicService)
	clean(t, discoveryService)
}

func clean(t *testing.T, obj runtime.Object) {
	err := c.Delete(context.TODO(), obj)
	// If the resource is already deleted, we don't care, but any other error is important
	if !apierrors.IsNotFound(err) {
		assert.NoError(t, err)
	}
}
