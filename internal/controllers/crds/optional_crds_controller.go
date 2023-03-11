package crds

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/kong/kubernetes-ingress-controller/v2/internal/controllers/utils"
	"github.com/kong/kubernetes-ingress-controller/v2/internal/util"
)

// +kubebuilder:rbac:groups="apiextensions.k8s.io",resources=customresourcedefinitions,verbs=list;watch

type Controller interface {
	SetupWithManager(mgr ctrl.Manager) error
}

type OptionalCRDController struct {
	Log              logr.Logger
	Manager          ctrl.Manager
	CacheSyncTimeout time.Duration
	Controller       Controller

	neededGVRs []schema.GroupVersionResource

	startControllerOnce sync.Once
}

func (r *OptionalCRDController) SetupWithManager(mgr ctrl.Manager) error {
	if utils.CRDExists(mgr.GetClient().RESTMapper(), r.GVR) {
		r.Log.Info("CRD exists, setting up its controller")
		return r.Controller.SetupWithManager(mgr)
	}

	_, err := mgr.GetClient().RESTMapper().KindFor()
	return !meta.IsNoMatchError(err)

	r.Log.Info("CRD does not exist, setting up a watch for it")

	c, err := controller.New("OptionalCRDs", mgr, controller.Options{
		Reconciler: r,
		LogConstructor: func(_ *reconcile.Request) logr.Logger {
			return r.Log
		},
		CacheSyncTimeout: r.CacheSyncTimeout,
	})
	if err != nil {
		return err
	}

	predicateFuncs := predicate.NewPredicateFuncs(r.isTheOptionalCRD)
	predicateFuncs.DeleteFunc = func(e event.DeleteEvent) bool { return false }
	predicateFuncs.GenericFunc = func(e event.GenericEvent) bool { return false }
	predicateFuncs.UpdateFunc = func(e event.UpdateEvent) bool { return false }
	return c.Watch(
		&source.Kind{Type: &apiextensionsv1.CustomResourceDefinition{}},
		&handler.EnqueueRequestForObject{},
	)
}

func (r *OptionalCRDController) allNeededCRDsExist() bool {
	for _, gvr := range r.neededGVRs {
		if !utils.CRDExists(r.Manager.GetClient().RESTMapper(), gvr) {
			return false
		}
	}

	return true
}

func (r *OptionalCRDController) isTheOptionalCRD(obj client.Object) bool {
	crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
	if !ok {
		return false
	}

	cl := r.Manager.GetClient()
	gvk, err := cl.RESTMapper().KindFor(r.GVR)
	if err != nil {
		r.Log.Error(err, "failed to get GVK for GVR", "gvr", r.GVR)
		return false
	}

	if crd.Spec.Group == gvk.Group &&
		crd.Spec.Versions[0].Name == gvk.Version &&
		crd.Spec.Names.Kind == gvk.Kind {
		return true
	}
	return false
}

func (r *OptionalCRDController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("CustomResourceDefinition", req.NamespacedName)

	crd := new(apiextensionsv1.CustomResourceDefinition)
	if err := r.Manager.GetClient().Get(ctx, req.NamespacedName, crd); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(util.DebugLevel).Info("object enqueued no longer exists, skipping", "name", req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	log.V(util.DebugLevel).Info("processing CRD", "name", req.Name)

	var startControllerErr error
	r.startControllerOnce.Do(func() {
		log.Info("CRD created, setting up its controller")
		startControllerErr = r.Controller.SetupWithManager(r.Manager)
	})
	if startControllerErr != nil {
		return ctrl.Result{}, startControllerErr
	}

	return ctrl.Result{}, nil
}
