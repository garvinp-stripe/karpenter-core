/*
Copyright 2023 The Kubernetes Authors.

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

package scheduling

import (
	"context"
	"fmt"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	csitranslation "k8s.io/csi-translation-lib"
	"k8s.io/csi-translation-lib/plugins"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:generate controller-gen object:headerFile="../../hack/boilerplate.go.txt" paths="."

// translator is a CSI Translator that translates in-tree plugin names to their out-of-tree CSI driver names
var translator = csitranslation.New()

// +k8s:deepcopy-gen=true
type Volumes map[string]sets.Set[string]

func (u Volumes) Add(provisioner string, pvcID string) {
	existing, ok := u[provisioner]
	if !ok {
		existing = sets.New[string]()
		u[provisioner] = existing
	}
	existing.Insert(pvcID)
}

func (u Volumes) Union(vol Volumes) Volumes {
	cp := Volumes{}
	for k, v := range u {
		cp[k] = sets.New(sets.List(v)...)
	}
	for k, v := range vol {
		existing, ok := cp[k]
		if !ok {
			existing = sets.New[string]()
			cp[k] = existing
		}
		existing.Insert(sets.List(v)...)
	}
	return cp
}

func (u Volumes) Insert(volumes Volumes) {
	for k, v := range volumes {
		existing, ok := u[k]
		if !ok {
			existing = sets.New[string]()
			u[k] = existing
		}
		existing.Insert(sets.List(v)...)
	}
}

//nolint:gocyclo
func GetVolumes(ctx context.Context, kubeClient client.Client, pod *v1.Pod) (Volumes, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("pod", pod.Name))
	podPVCs := Volumes{}
	defaultStorageClassName, err := DiscoverDefaultStorageClassName(ctx, kubeClient)
	if err != nil {
		return nil, fmt.Errorf("discovering default storage class, %w", err)
	}
	for _, volume := range pod.Spec.Volumes {
		ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("volume", volume.Name))
		var pvcID, storageClassName, volumeName string
		var pvc v1.PersistentVolumeClaim
		if volume.PersistentVolumeClaim != nil {
			if err = kubeClient.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: volume.PersistentVolumeClaim.ClaimName}, &pvc); err != nil {
				return nil, err
			}
			pvcID = fmt.Sprintf("%s/%s", pod.Namespace, volume.PersistentVolumeClaim.ClaimName)
			storageClassName = lo.FromPtr(pvc.Spec.StorageClassName)
			volumeName = pvc.Spec.VolumeName
		} else if volume.Ephemeral != nil {
			// generated name per https://kubernetes.io/docs/concepts/storage/ephemeral-volumes/#persistentvolumeclaim-naming
			pvcID = fmt.Sprintf("%s/%s-%s", pod.Namespace, pod.Name, volume.Name)
			storageClassName = lo.FromPtr(volume.Ephemeral.VolumeClaimTemplate.Spec.StorageClassName)
			volumeName = volume.Ephemeral.VolumeClaimTemplate.Spec.VolumeName
		} else {
			continue
		}
		if storageClassName == "" {
			storageClassName = defaultStorageClassName
		}
		driverName, err := resolveDriver(ctx, kubeClient, volumeName, storageClassName)
		if err != nil {
			return nil, err
		}
		// might be a non-CSI driver, something we don't currently handle
		if driverName != "" {
			podPVCs.Add(driverName, pvcID)
		}
	}
	return podPVCs, nil
}

// resolveDriver resolves the storage driver name in the following order:
//  1. If the PV associated with the pod volume is using CSI.driver in its spec, then use that name
//  2. If the StorageClass associated with the PV has a Provisioner
func resolveDriver(ctx context.Context, kubeClient client.Client, volumeName string, storageClassName string) (string, error) {
	// We can track the volume usage by the CSI Driver name which is pulled from the storage class for dynamic
	// volumes, or if it's bound/static we can pull the volume name
	if volumeName != "" {
		driverName, err := driverFromVolume(ctx, kubeClient, volumeName)
		if err != nil {
			return "", err
		}
		if driverName != "" {
			return driverName, nil
		}
	}
	if storageClassName != "" {
		driverName, err := driverFromSC(ctx, kubeClient, storageClassName)
		if err != nil {
			return "", err
		}
		if driverName != "" {
			return driverName, nil
		}
	}
	// Driver name wasn't able to resolve for this volume. In this case, we just ignore the
	// volume and move on to the other volumes that the pod has
	return "", nil
}

// driverFromSC resolves the storage driver name by getting the Provisioner name from the StorageClass
func driverFromSC(ctx context.Context, kubeClient client.Client, storageClassName string) (string, error) {
	var sc storagev1.StorageClass
	if err := kubeClient.Get(ctx, client.ObjectKey{Name: storageClassName}, &sc); err != nil {
		return "", err
	}
	// Check if the provisioner name is an in-tree plugin name
	if csiName, err := translator.GetCSINameFromInTreeName(sc.Provisioner); err == nil {
		return csiName, nil
	}
	return sc.Provisioner, nil
}

// driverFromVolume resolves the storage driver name by getting the CSI spec from inside the PersistentVolume
func driverFromVolume(ctx context.Context, kubeClient client.Client, volumeName string) (string, error) {
	var pv v1.PersistentVolume
	if err := kubeClient.Get(ctx, client.ObjectKey{Name: volumeName}, &pv); err != nil {
		return "", err
	}
	if pv.Spec.CSI != nil {
		return pv.Spec.CSI.Driver, nil
	} else if pv.Spec.AWSElasticBlockStore != nil {
		return plugins.AWSEBSDriverName, nil
	}
	return "", nil
}

// VolumeUsage tracks volume limits on a per node basis.  The number of volumes that can be mounted varies by instance
// type. We need to be aware and track the mounted volume usage to inform our awareness of which pods can schedule to
// which nodes.
// +k8s:deepcopy-gen=true
type VolumeUsage struct {
	volumes    Volumes
	podVolumes map[types.NamespacedName]Volumes
	limits     map[string]int
}

func NewVolumeUsage() *VolumeUsage {
	return &VolumeUsage{
		volumes:    Volumes{},
		podVolumes: map[types.NamespacedName]Volumes{},
		limits:     map[string]int{},
	}
}

func (v *VolumeUsage) ExceedsLimits(vols Volumes) error {
	for k, volumes := range v.volumes.Union(vols) {
		if limit, hasLimit := v.limits[k]; hasLimit && len(volumes) > limit {
			return fmt.Errorf("would exceed volume limit for %s, %d > %d", k, len(volumes), limit)
		}
	}
	return nil
}

func (v *VolumeUsage) AddLimit(storageDriver string, value int) {
	v.limits[storageDriver] = value
}

func (v *VolumeUsage) Add(pod *v1.Pod, volumes Volumes) {
	v.podVolumes[client.ObjectKeyFromObject(pod)] = volumes
	v.volumes = v.volumes.Union(volumes)
}

func (v *VolumeUsage) DeletePod(key types.NamespacedName) {
	delete(v.podVolumes, key)
	// volume names could be duplicated, so we re-create our volumes
	v.volumes = Volumes{}
	for _, c := range v.podVolumes {
		v.volumes.Insert(c)
	}
}
