/*
Copyright 2022 The KCP Authors.

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

package namespace

import (
	"context"
	"encoding/json"
	"math/rand"
	"strings"
	"time"

	"github.com/kcp-dev/logicalcluster"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"

	schedulingv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/scheduling/v1alpha1"
	workloadv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/workload/v1alpha1"
	locationreconciler "github.com/kcp-dev/kcp/pkg/reconciler/scheduling/location"
)

const removingGracePeriod = 5 * time.Second

// placementSchedulingReconciler schedules a workload for this ns. It checks the current placement annotation on the ns,
// and find all valid workloadclusters.
type placementSchedulingReconciler struct {
	listWorkloadCluster func(clusterName logicalcluster.Name) ([]*workloadv1alpha1.WorkloadCluster, error)
	listPlacement       func(clusterName logicalcluster.Name) ([]*schedulingv1alpha1.Placement, error)
	getLocation         func(clusterName logicalcluster.Name, name string) (*schedulingv1alpha1.Location, error)

	patchNamespace func(ctx context.Context, clusterName logicalcluster.Name, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*corev1.Namespace, error)

	enqueueAfter func(logicalcluster.Name, *corev1.Namespace, time.Duration)
}

type locationClusters struct {
	candidates       map[string]*workloadv1alpha1.WorkloadCluster
	scheduledCluster *workloadv1alpha1.WorkloadCluster
}

func newLocationClusters(clusters []*workloadv1alpha1.WorkloadCluster) *locationClusters {
	l := &locationClusters{
		candidates: map[string]*workloadv1alpha1.WorkloadCluster{},
	}

	for _, cluster := range clusters {
		l.candidates[cluster.Name] = cluster
	}

	return l
}

func (l *locationClusters) scheduled() bool {
	return l.scheduledCluster != nil
}

func (l *locationClusters) exclude(workloadClusterName string) {
	delete(l.candidates, workloadClusterName)
}

// potentiallySchedule sets a workloadCluster as a scheduled cluster for this location if
// this workloadCluster is a valid candidate and this location is not scheduled yet, and
// return true.
func (l *locationClusters) potentiallySchedule(workloadClusterName string) bool {
	cluster, found := l.candidates[workloadClusterName]
	if !found {
		return false
	}

	if l.scheduled() {
		return false
	}

	l.scheduledCluster = cluster
	return true
}

func (l *locationClusters) schedule() *workloadv1alpha1.WorkloadCluster {
	if len(l.candidates) == 0 {
		return nil
	}

	var candidates []*workloadv1alpha1.WorkloadCluster
	for _, cluster := range l.candidates {
		candidates = append(candidates, cluster)
	}

	l.scheduledCluster = candidates[rand.Intn(len(candidates))]
	return l.scheduledCluster
}

func (r *placementSchedulingReconciler) reconcile(ctx context.Context, ns *corev1.Namespace) (reconcileStatus, *corev1.Namespace, error) {
	clusterName := logicalcluster.From(ns)

	validPlacements := []*schedulingv1alpha1.Placement{}
	_, foundPlacement := ns.Annotations[schedulingv1alpha1.PlacementAnnotationKey]

	if foundPlacement {
		placements, err := r.listPlacement(clusterName)
		if err != nil {
			return reconcileStatusStop, ns, err
		}

		validPlacements = filterValidPlacements(ns, placements)
	}

	// 1. pick all workload cluster in all bound placements
	validLocationClusters := map[schedulingv1alpha1.LocationReference]*locationClusters{}
	var errs []error
	for _, placement := range validPlacements {
		clusters, err := r.getAllValidWorkloadClustersForPlacement(clusterName, placement, ns)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		if len(clusters) > 0 {
			validLocationClusters[*placement.Status.SelectedLocation] = newLocationClusters(clusters)
		}
	}

	if len(errs) > 0 {
		return reconcileStatusStop, ns, utilerrors.NewAggregate(errs)
	}

	// 2. find the scheduled workload cluster to the ns, including synced, removing
	synced, removing := syncedRemovingCluster(ns)

	// 3. if the synced cluster is in the valid clusters, stop scheduling
	expectedAnnotations := map[string]interface{}{} // nil means to remove the key
	expectedLabels := map[string]interface{}{}      // nil means to remove the key

	for _, cluster := range synced {
		clusterScheduledByLocation := false
		for _, locationClusters := range validLocationClusters {
			//this is non deterministic when the same workload clusters are selected in multiple locations.
			// TODO(qiujian16): consider if we need to save the location/workloadcluster mappings in the ns.
			if locationClusters.potentiallySchedule(cluster) {
				clusterScheduledByLocation = true
			}

			// exclude synced cluster from candidates.
			locationClusters.exclude(cluster)
		}
		if !clusterScheduledByLocation {
			// it is no longer a synced cluster, mark it as removing.
			now := time.Now().UTC().Format(time.RFC3339)
			expectedAnnotations[workloadv1alpha1.InternalClusterDeletionTimestampAnnotationPrefix+cluster] = now
			klog.V(4).Infof("set cluster %s removing for ns %s|%s since it is not a valid cluster anymore", cluster, clusterName, ns.Name)
		}
	}

	// 4. if removing cluster is in the valid cluster, exclude it from the candidates, also, check if the removing cluster
	// should be removed.
	minEnqueueDuration := removingGracePeriod + 1
	for cluster, removingTime := range removing {
		for _, locationClusters := range validLocationClusters {
			// exclude removing cluster from candidates
			locationClusters.exclude(cluster)
		}

		if removingTime.Add(removingGracePeriod).Before(time.Now()) {
			expectedLabels[workloadv1alpha1.InternalClusterResourceStateLabelPrefix+cluster] = nil
			expectedAnnotations[workloadv1alpha1.InternalClusterDeletionTimestampAnnotationPrefix+cluster] = nil
			klog.V(4).Infof("remove cluster %s for ns %s|%s", cluster, clusterName, ns.Name)
		} else {
			enqueuDuration := time.Until(removingTime.Add(removingGracePeriod))
			if enqueuDuration < minEnqueueDuration {
				minEnqueueDuration = enqueuDuration
			}
		}
	}

	// 5. randomly select a cluster if there is no cluster syncing currently.
	// TODO(qiujian16): we currently schedule each in each location independently. It cannot guarantee 1 cluster is schedule per location
	// when the same workloadclusters are in multiple locations, we need to rethink whether we need a better algorithm or we need location
	// to be exclusive.
	for _, locationClusters := range validLocationClusters {
		if locationClusters.scheduled() {
			continue
		}

		chosenCluster := locationClusters.schedule()
		if chosenCluster == nil {
			continue
		}

		expectedLabels[workloadv1alpha1.InternalClusterResourceStateLabelPrefix+chosenCluster.Name] = string(workloadv1alpha1.ResourceStateSync)
		klog.V(4).Infof("set cluster %s sync for ns %s|%s", chosenCluster.Name, clusterName, ns.Name)
	}

	if len(expectedLabels) > 0 || len(expectedAnnotations) > 0 {
		ns, err := r.patchNamespaceLabelAnnotation(ctx, clusterName, ns, expectedLabels, expectedAnnotations)
		return reconcileStatusContinue, ns, err
	}

	//6. Requeue at last to check if removing cluster should be removed later.
	if minEnqueueDuration <= removingGracePeriod {
		klog.V(2).Infof("enqueue ns %s|%s after %s", clusterName, ns.Name, minEnqueueDuration)
		r.enqueueAfter(clusterName, ns, minEnqueueDuration)
	}

	return reconcileStatusContinue, ns, nil
}

func (r *placementSchedulingReconciler) getAllValidWorkloadClustersForPlacement(clusterName logicalcluster.Name, placement *schedulingv1alpha1.Placement, ns *corev1.Namespace) ([]*workloadv1alpha1.WorkloadCluster, error) {
	if placement.Status.Phase == schedulingv1alpha1.PlacementPending || placement.Status.SelectedLocation == nil {
		return nil, nil
	}

	locationWorkspace := logicalcluster.New(placement.Status.SelectedLocation.Path)
	location, err := r.getLocation(
		locationWorkspace,
		placement.Status.SelectedLocation.LocationName)
	switch {
	case errors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	}

	// find all workloadclusters in the location workspace
	workloadClusters, err := r.listWorkloadCluster(locationWorkspace)
	if err != nil {
		return nil, err
	}

	// filter the workload clusters by location
	locationClusters, err := locationreconciler.LocationWorkloadClusters(workloadClusters, location)
	if err != nil {
		return nil, err
	}

	// find all the valid workload clusters.
	validClusters := locationreconciler.FilterNonEvicting(locationreconciler.FilterReady(locationClusters))

	return validClusters, nil
}

func (r *placementSchedulingReconciler) patchNamespaceLabelAnnotation(ctx context.Context, clusterName logicalcluster.Name, ns *corev1.Namespace, labels, annotations map[string]interface{}) (*corev1.Namespace, error) {
	patch := map[string]interface{}{}
	if len(annotations) > 0 {
		if err := unstructured.SetNestedField(patch, annotations, "metadata", "annotations"); err != nil {
			return ns, err
		}
	}
	if len(labels) > 0 {
		if err := unstructured.SetNestedField(patch, labels, "metadata", "labels"); err != nil {
			return ns, err
		}
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return ns, err
	}
	klog.V(3).Infof("Patching to update workload cluster information on namespace %s|%s: %s",
		clusterName, ns.Name, string(patchBytes))
	updated, err := r.patchNamespace(ctx, clusterName, ns.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return ns, err
	}
	return updated, nil
}

// syncedRemovingCluster finds synced and removing clusters for this ns.
func syncedRemovingCluster(ns *corev1.Namespace) ([]string, map[string]time.Time) {
	synced := []string{}
	removing := map[string]time.Time{}
	for k := range ns.Labels {
		if !strings.HasPrefix(k, workloadv1alpha1.InternalClusterResourceStateLabelPrefix) {
			continue
		}

		workloadCluster := strings.TrimPrefix(k, workloadv1alpha1.InternalClusterResourceStateLabelPrefix)

		deletionAnnotationKey := workloadv1alpha1.InternalClusterDeletionTimestampAnnotationPrefix + workloadCluster

		if value, ok := ns.Annotations[deletionAnnotationKey]; ok {
			removingTime, _ := time.Parse(time.RFC3339, value)
			removing[workloadCluster] = removingTime
			continue
		}

		synced = append(synced, workloadCluster)
	}

	return synced, removing
}
