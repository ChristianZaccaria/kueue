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
	"strings"
	"time"
	// "unsafe"

	"github.com/jinzhu/inflection"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"sigs.k8s.io/kueue/pkg/controller/jobs/noop"
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
func SetupControllers(mgr ctrl.Manager, log logr.Logger, cancel context.CancelFunc, opts ...Option) error {
	options := ProcessOptions(opts...)

	for fwkName := range options.EnabledExternalFrameworks {
		if err := RegisterExternalJobType(fwkName); err != nil {
			return err
		}
	}
	// Create a dynamic client to interact with the Kubernetes API
	dynClient, err := dynamic.NewForConfig(mgr.GetConfig())
	if err != nil {
		log.Info("unable to create dynamic client")
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
			if _, err = mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
				if !meta.IsNoMatchError(err) {
					return fmt.Errorf("%s: %w", fwkNamePrefix, err)
				}
				logger.Info("No matching API in the server for job framework, skipped setup of controller and webhook")
				go waitForAPI(context.Background(), dynClient, logger, gvk, func() {
					log.Info("API now available, triggering restart of the Kueue pod")
					cancel()
				})
			} else {
				if err = cb.NewReconciler(
					mgr.GetClient(),
					mgr.GetEventRecorderFor(fmt.Sprintf("%s-%s-controller", name, options.ManagerName)),
					opts...,
				).SetupWithManager(mgr); err != nil {
					return fmt.Errorf("%s: %w", fwkNamePrefix, err)
				}
				if err = cb.SetupWebhook(mgr, opts...); err != nil {
					return fmt.Errorf("%s: unable to create webhook: %w", fwkNamePrefix, err)
				}
				logger.Info("Set up controller and webhook for job framework")
				return nil
			}
		}
		if err := noop.SetupWebhook(mgr, cb.JobType); err != nil {
			return fmt.Errorf("%s: unable to create noop webhook: %w", fwkNamePrefix, err)
		}
		return nil
	})
}

func waitForAPI(ctx context.Context, dynClient *dynamic.DynamicClient, log logr.Logger, gvk schema.GroupVersionKind, action func()) {
	// Determine the resource GVR (GroupVersionResource) from the GVK
	gvr := schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: strings.ToLower(inflection.Plural(gvk.Kind))}
	resourceInterface := dynClient.Resource(gvr).Namespace(metav1.NamespaceAll)

	log.Info(fmt.Sprintf("API %v not available, setting up retry watcher", gvk))

	setupWatch := func() (watch.Interface, error) {
		return resourceInterface.Watch(ctx, metav1.ListOptions{})
	}

	var watchInterface watch.Interface
	var err error
	for {
		watchInterface, err = setupWatch()
		if err != nil {
			select {
			case <-ctx.Done():
				log.Info(fmt.Sprint("Context cancelled! - Stopping watcher for API ", "gvk", gvk))
				return
			case <-time.After(time.Second * 5):
				continue
			}
		}
		break
	}

	defer watchInterface.Stop()
	log.Info(fmt.Sprint("API has been found!", "gvk ", gvk))
	action()
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
