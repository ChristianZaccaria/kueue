/*
Copyright 2024 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package jobframework

import (
	"context"
	"errors"
	"fmt"
	"os"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"k8s.io/apimachinery/pkg/watch"
	retrywatch "k8s.io/client-go/tools/watch"

	"sigs.k8s.io/kueue/pkg/controller/jobs/noop"
)

const (
	RayCluster  = "RayCluster"
	RayJob      = "RayJob"
	RayService  = "RayService"
	MPIJobs     = "MPIJob"
	MXJobs      = "MXJob"
	PaddleJobs  = "PaddleJob"
	PyTorchJobs = "PyTorchJob"
	TFJobs      = "TFJob"
	XGBoostJobs = "XGBoostJob"
)

var (
	errFailedMappingResource = errors.New("restMapper failed mapping resource")
)

// SetupControllers setups all controllers and webhooks for integrations.
// When the platform developers implement a separate kueue-manager to manage the in-house custom jobs,
// they can easily setup controllers and webhooks for the in-house custom jobs.
//
// Note that the first argument, "mgr" must be initialized on the outside of this function.
// In addition, if the manager uses the kueue's internal cert management for the webhooks,
// this function needs to be called after the certs get ready because the controllers won't work
// until the webhooks are operating, and the webhook won't work until the
// certs are all in place.
func SetupControllers(mgr ctrl.Manager, log logr.Logger, opts ...Option) error {
	options := ProcessOptions(opts...)

	for fwkName := range options.EnabledExternalFrameworks {
		if err := RegisterExternalJobType(fwkName); err != nil {
			return err
		}
	}
	return ForEachIntegration(func(name string, cb IntegrationCallbacks) error {
		logger := log.WithValues("jobFrameworkName", name)
		fwkNamePrefix := fmt.Sprintf("jobFrameworkName %q", name)

		if options.EnabledFrameworks.Has(name) {
			if cb.CanSupportIntegration != nil {
				if canSupport, err := cb.CanSupportIntegration(opts...); !canSupport || err != nil {
					log.Error(err, "Failed to configure reconcilers")
					os.Exit(1)
				}
			}
			gvk, err := apiutil.GVKForObject(cb.JobType, mgr.GetScheme())
			if err != nil {
				return fmt.Errorf("%s: %w: %w", fwkNamePrefix, errFailedMappingResource, err)
			}
			if !isAPIAvailable(mgr, RayCRDsName(), TrainingOperatorCRDsName()) {
				logger.Info("API not available, waiting for it to become available... - Skipping setup of controller and webhook")
				waitForAPIs(context.Background(), logger, mgr, gvk, RayCRDsName(), TrainingOperatorCRDsName(), func() {
					setupComponents(mgr, logger, gvk, fwkNamePrefix, cb, opts...)
				})
			} else {
				logger.Info("API is available, setting up components...")
				setupComponents(mgr, logger, gvk, fwkNamePrefix, cb, opts...)
			}
		}
		if err := noop.SetupWebhook(mgr, cb.JobType); err != nil {
			return fmt.Errorf("%s: unable to create noop webhook: %w", fwkNamePrefix, err)
		}
		return nil
	})
}

func setupComponents(mgr ctrl.Manager, log logr.Logger, gvk schema.GroupVersionKind, fwkNamePrefix string, cb IntegrationCallbacks, opts ...Option) {
	// Attempt to get the REST mapping for the GVK
	if _, err := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		if !meta.IsNoMatchError(err) {
			log.Error(err, fmt.Sprintf("%s: unable to get REST mapping", fwkNamePrefix))
			return
		}
		log.Info("No matching API in the server for job framework, skipped setup of controller and webhook")
	} else {
		if err := setupControllerAndWebhook(mgr, gvk, fwkNamePrefix, cb, opts...); err != nil {
			log.Error(err, "Failed to set up controller and webhook")
		} else {
			log.Info("Set up controller and webhook for job framework")
		}
	}
}

func setupControllerAndWebhook(mgr ctrl.Manager, gvk schema.GroupVersionKind, fwkNamePrefix string, cb IntegrationCallbacks, opts ...Option) error {
	if err := cb.NewReconciler(
		mgr.GetClient(),
		mgr.GetEventRecorderFor(fmt.Sprintf("%s-%s-controller", gvk.Kind, "managerName")), // Ensure managerName is defined or fetched
		opts...,
	).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("%s: %w", fwkNamePrefix, err)
	}

	if err := cb.SetupWebhook(mgr, opts...); err != nil {
		return fmt.Errorf("%s: unable to create webhook: %w", fwkNamePrefix, err)
	}

	return nil
}

// SetupIndexes setups the indexers for integrations.
// When the platform developers implement a separate kueue-manager to manage the in-house custom jobs,
// they can easily setup indexers for the in-house custom jobs.
//
// Note that the second argument, "indexer" needs to be the fieldIndexer obtained from the Manager.
func SetupIndexes(ctx context.Context, indexer client.FieldIndexer, opts ...Option) error {
	options := ProcessOptions(opts...)
	return ForEachIntegration(func(name string, cb IntegrationCallbacks) error {
		if options.EnabledFrameworks.Has(name) {
			if err := cb.SetupIndexes(ctx, indexer); err != nil {
				return fmt.Errorf("jobFrameworkName %q: %w", name, err)
			}
		}
		return nil
	})
}

func isAPIAvailable(mgr ctrl.Manager, rcApiNames []string, toApiNames []string) bool {
	discoveryClient := discovery.NewDiscoveryClientForConfigOrDie(mgr.GetConfig())
	apiGroupResources, err := restmapper.GetAPIGroupResources(discoveryClient)
	exitOnError(err, "unable to get API group resources")

	restMapper := restmapper.NewDiscoveryRESTMapper(apiGroupResources)
	checkAPIs := func(apiNames []string) bool {
		for _, apiName := range apiNames {
			_, err = restMapper.KindFor(schema.GroupVersionResource{Resource: apiName})
			if err != nil {
				fmt.Printf("API not available: %s, error: %v\n", apiName, err)
				return false
			} else {
				fmt.Printf("API available: %s\n", apiName)
			}
		}
		return true
	}

	return checkAPIs(rcApiNames) || checkAPIs(toApiNames)
}

func waitForAPIs(ctx context.Context, log logr.Logger, mgr ctrl.Manager, gvk schema.GroupVersionKind, rcApiNames []string, toApiNames []string, action func()) {
	if isAPIAvailable(mgr, rcApiNames, toApiNames) {
		log.Info("At least one of the required APIs is available, invoking action")
		action()
		return
	}

	// Wait for the API to become available then invoke action
	log.Info("Required APIs not available, setting up retry watcher")
	dynamicClient := dynamic.NewForConfigOrDie(mgr.GetConfig())
	resource := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: gvk.Version, Resource: "customresourcedefinitions"}
	listWatch := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return dynamicClient.Resource(resource).List(ctx, options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return dynamicClient.Resource(resource).Watch(ctx, options)
		},
	}

	// Perform an initial list to get the resource version
	list, err := dynamicClient.Resource(resource).List(ctx, metav1.ListOptions{})
	exitOnError(err, "unable to list resources")

	retryWatcher, err := retrywatch.NewRetryWatcher(list.GetResourceVersion(), listWatch)
	exitOnError(err, "unable to create retry watcher")

	defer retryWatcher.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-retryWatcher.ResultChan():
			switch event.Type {
			case watch.Error:
				exitOnError(apierrors.FromObject(event.Object), "error watching for APIs")

			case watch.Added, watch.Modified:
				if isAPIAvailable(mgr, rcApiNames, toApiNames) {
					log.Info("Required APIs installed, invoking deferred action")
					action()
					return
				}
			}
		}
	}
}

func RayCRDsName() []string {
	return []string{RayCluster, RayJob, RayService}
}

func TrainingOperatorCRDsName() []string {
	return []string{MPIJobs, MXJobs, PaddleJobs, PyTorchJobs, TFJobs, XGBoostJobs}
}

func exitOnError(err error, msg string) {
	if err != nil {
		fmt.Print(err, msg)
		os.Exit(1)
	}
}
