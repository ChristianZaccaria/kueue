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

package e2e
package mains
import (
	"context"
	"fmt"
	"log"

	appwrappersv1beta2 "your_appwrapper_package_path" // Import your AppWrapper CRD
	rayv1alpha1 "your_raycluster_package_path"       // Import your RayCluster CRD

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	// Load Kubernetes configuration
	config, err := clientcmd.BuildConfigFromFlags("", "/path/to/kubeconfig")
	if err != nil {
		log.Fatalf("Failed to load Kubernetes config: %v", err)
	}

	// Create dynamic and typed clients
	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create dynamic client: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create clientset: %v", err)
	}

	// Define AppWrapper resource
	appWrapper := &appwrappersv1beta2.AppWrapper{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-appwrapper",
			Namespace: "regularuser",
			Labels: map[string]string{
				"kueue.x-k8s.io/queue-name": "team-a-queue",
			},
		},
		Spec: appwrappersv1beta2.AppWrapperSpec{
			Components: []appwrappersv1beta2.Component{},
		},
	}

	// Create AppWrapper
	appWrapperGVR := schema.GroupVersionResource{
		Group:    "workload.codeflare.dev",
		Version:  "v1beta2",
		Resource: "appwrappers",
	}

	_, err = dynClient.Resource(appWrapperGVR).Namespace("regularuser").Create(context.TODO(), appWrapper, metav1.CreateOptions{})
	if err != nil {
		log.Fatalf("Failed to create AppWrapper: %v", err)
	}
	fmt.Println("AppWrapper created successfully")

	// Retrieve UID of AppWrapper
	createdAppWrapper, err := dynClient.Resource(appWrapperGVR).Namespace("regularuser").Get(context.TODO(), "test-appwrapper", metav1.GetOptions{})
	if err != nil {
		log.Fatalf("Failed to get AppWrapper: %v", err)
	}

	appWrapperUID := string(createdAppWrapper.GetUID())
	fmt.Printf("AppWrapper UID: %s\n", appWrapperUID)

	// Define RayCluster resource
	rayCluster := &rayv1alpha1.RayCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-raycluster",
			Namespace: "regularuser",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "workload.codeflare.dev/v1beta2",
					Kind:       "AppWrapper",
					Name:       "test-appwrapper",
					UID:        types.UID(appWrapperUID),
				},
			},
		},
		Spec: rayv1alpha1.RayClusterSpec{
			HeadGroupSpec: rayv1alpha1.HeadGroupSpec{
				Replicas: 1,
				Template: rayv1alpha1.PodTemplateSpec{
					Spec: rayv1alpha1.PodSpec{
						Containers: []rayv1alpha1.Container{
							{
								Name:  "ray-head",
								Image: "rayproject/ray:latest",
								Ports: []rayv1alpha1.ContainerPort{
									{Name: "redis", ContainerPort: 6379},
								},
							},
						},
					},
				},
			},
			WorkerGroupSpecs: []rayv1alpha1.WorkerGroupSpec{
				{
					GroupName: "workers",
					Replicas:  1,
					Template: rayv1alpha1.PodTemplateSpec{
						Spec: rayv1alpha1.PodSpec{
							Containers: []rayv1alpha1.Container{
								{
									Name:  "ray-worker",
									Image: "rayproject/ray:latest",
								},
							},
						},
					},
				},
			},
		},
	}

	// Apply the RayCluster
	rayClusterGVR := schema.GroupVersionResource{
		Group:    "ray.io",
		Version:  "v1alpha1",
		Resource: "rayclusters",
	}

	_, err = dynClient.Resource(rayClusterGVR).Namespace("regularuser").Create(context.TODO(), rayCluster, metav1.CreateOptions{})
	if err != nil {
		log.Fatalf("Failed to create RayCluster: %v", err)
	}
	fmt.Println("RayCluster created successfully")
}
