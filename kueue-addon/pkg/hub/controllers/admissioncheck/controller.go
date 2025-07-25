package admissioncheck

import (
	"context"
	"fmt"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	kueuev1beta1 "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	kueueclient "sigs.k8s.io/kueue/client-go/clientset/versioned"
	kueueinformerv1beta1 "sigs.k8s.io/kueue/client-go/informers/externalversions/kueue/v1beta1"
	kueuelisterv1beta1 "sigs.k8s.io/kueue/client-go/listers/kueue/v1beta1"

	"open-cluster-management.io/addon-contrib/kueue-addon/pkg/hub/controllers/common"
	clusterclient "open-cluster-management.io/api/client/cluster/clientset/versioned"
	clusterinformerv1beta1 "open-cluster-management.io/api/client/cluster/informers/externalversions/cluster/v1beta1"
	clusterlisterv1beta1 "open-cluster-management.io/api/client/cluster/listers/cluster/v1beta1"
	clusterapiv1beta1 "open-cluster-management.io/api/cluster/v1beta1"
	sdkv1beta1 "open-cluster-management.io/sdk-go/pkg/apis/cluster/v1beta1"
	"open-cluster-management.io/sdk-go/pkg/patcher"

	"k8s.io/apimachinery/pkg/util/sets"
	"open-cluster-management.io/ocm/pkg/common/helpers"
	"open-cluster-management.io/ocm/pkg/common/queue"
)

const (
	admissioncheckControllerName = "AdmissionCheckController"
)

// AdmissioncheckController manages MultiKueueConfig and MultiKueueCluster resources based on PlacementDecisions.
type admissioncheckController struct {
	clusterClient           clusterclient.Interface
	kueueClient             kueueclient.Interface
	placementLister         clusterlisterv1beta1.PlacementLister
	placementDecisionGetter helpers.PlacementDecisionGetter
	admissioncheckLister    kueuelisterv1beta1.AdmissionCheckLister
	eventRecorder           events.Recorder
}

// NewAdmissionCheckController returns a controller that reconciles MultiKueueConfig and MultiKueueCluster resources
// for each AdmissionCheck, based on Placement and PlacementDecision changes.
func NewAdmissionCheckController(
	ctx context.Context,
	clusterClient clusterclient.Interface,
	kueueClient kueueclient.Interface,
	placementInformer clusterinformerv1beta1.PlacementInformer,
	placementDecisionInformer clusterinformerv1beta1.PlacementDecisionInformer,
	admissionCheckInformer kueueinformerv1beta1.AdmissionCheckInformer,
	recorder events.Recorder,
) factory.Controller {
	c := &admissioncheckController{
		clusterClient:           clusterClient,
		kueueClient:             kueueClient,
		placementLister:         placementInformer.Lister(),
		placementDecisionGetter: helpers.PlacementDecisionGetter{Client: placementDecisionInformer.Lister()},
		admissioncheckLister:    admissionCheckInformer.Lister(),
		eventRecorder:           recorder.WithComponentSuffix("admission-check-controller"),
	}

	return factory.New().
		WithFilteredEventsInformersQueueKeysFunc(
			queue.QueueKeyByMetaName,
			func(obj interface{}) bool {
				accessor, _ := meta.Accessor(obj)
				admissionCheck, _ := accessor.(*kueuev1beta1.AdmissionCheck)
				// Filter OCM admission check controller
				return admissionCheck.Spec.ControllerName == common.AdmissionCheckControllerName
			},
			admissionCheckInformer.Informer()).
		WithInformersQueueKeysFunc(
			AdmissionCheckByPlacementQueueKey(admissionCheckInformer), placementInformer.Informer()).
		WithInformersQueueKeysFunc(
			AdmissionCheckByPlacementDecisionQueueKey(admissionCheckInformer), placementDecisionInformer.Informer()).
		WithSync(c.sync).
		ToController(admissioncheckControllerName, recorder)
}

// Sync ensures that MultiKueueConfig and MultiKueueCluster resources match the current PlacementDecision state
// for the given AdmissionCheck. It creates, updates, or deletes resources as needed.
func (c *admissioncheckController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	key := syncCtx.QueueKey()
	logger := klog.FromContext(ctx)
	logger.Info("Reconciling AdmissionCheck", "key", key)

	admissionCheck, err := c.admissioncheckLister.Get(key)
	if errors.IsNotFound(err) {
		logger.Info("AdmissionCheck not found", "key", key)
		return nil
	}
	if err != nil {
		return err
	}

	// Init placement tracker
	placementName := admissionCheck.Spec.Parameters.Name
	placement := &clusterapiv1beta1.Placement{
		ObjectMeta: metav1.ObjectMeta{Name: placementName, Namespace: common.KueueNamespace},
		Spec:       clusterapiv1beta1.PlacementSpec{},
	}

	// New decision tracker
	pdTracker := sdkv1beta1.NewPlacementDecisionClustersTracker(placement, c.placementDecisionGetter, nil)

	// Refresh and get existing decision clusters
	if err := pdTracker.Refresh(); err != nil {
		return fmt.Errorf("failed to refresh placement decision tracker: %v", err)
	}
	clusters := pdTracker.ExistingClusterGroupsBesides().GetClusters()

	// Build desired MultiKueueConfig and MultiKueueCluster set
	multiKueueConfigName := placementName
	mkconfig := &kueuev1beta1.MultiKueueConfig{
		ObjectMeta: metav1.ObjectMeta{Name: multiKueueConfigName},
		Spec: kueuev1beta1.MultiKueueConfigSpec{
			Clusters: []string{},
		},
	}

	// Record MultiKueueConfig clusters and update MultiKueueCluster
	expectedMKClusterNames := sets.New[string]()
	for cn := range clusters {
		mkclusterName := placementName + "-" + cn
		mkcluster := &kueuev1beta1.MultiKueueCluster{
			ObjectMeta: metav1.ObjectMeta{Name: mkclusterName},
			Spec: kueuev1beta1.MultiKueueClusterSpec{
				KubeConfig: kueuev1beta1.KubeConfig{
					LocationType: kueuev1beta1.SecretLocationType,
					Location:     common.GetMultiKueueSecretName(cn),
				},
			},
		}
		if err := c.createOrUpdateMultiKueueCluster(ctx, mkcluster); err != nil {
			return fmt.Errorf("failed to create/update multi kueue cluster %s: %v", mkclusterName, err)
		}
		mkconfig.Spec.Clusters = append(mkconfig.Spec.Clusters, mkclusterName)
		expectedMKClusterNames.Insert(mkclusterName)
	}

	// Clean up MultiKueueClusters
	if err := c.cleanupMultiKueueClusters(ctx, multiKueueConfigName, expectedMKClusterNames); err != nil {
		return err
	}

	// Update MultiKueueConfig
	if err := c.createOrUpdateMultiKueueConfig(ctx, mkconfig); err != nil {
		return fmt.Errorf("failed to create/update multi kueue config %s: %v", mkconfig.Name, err)
	}

	// Update AdmissionCheck status
	newadmissioncheck := admissionCheck.DeepCopy()
	meta.SetStatusCondition(&newadmissioncheck.Status.Conditions, metav1.Condition{
		Type:    kueuev1beta1.MultiKueueClusterActive,
		Status:  metav1.ConditionTrue,
		Reason:  "Active",
		Message: fmt.Sprintf("MultiKueueConfig %s and MultiKueueClusters are generated successfully", placementName),
	})
	admissioncheckPatcher := patcher.NewPatcher[
		*kueuev1beta1.AdmissionCheck, kueuev1beta1.AdmissionCheckSpec, kueuev1beta1.AdmissionCheckStatus](
		c.kueueClient.KueueV1beta1().AdmissionChecks())
	_, err = admissioncheckPatcher.PatchStatus(ctx, newadmissioncheck, newadmissioncheck.Status, admissionCheck.Status)
	return err
}

// CreateOrUpdateMultiKueueConfig creates or updates the MultiKueueConfig resource to match the desired cluster list.
func (c *admissioncheckController) createOrUpdateMultiKueueConfig(ctx context.Context, mkconfig *kueuev1beta1.MultiKueueConfig) error {
	oldmkconfig, err := c.kueueClient.KueueV1beta1().MultiKueueConfigs().Get(ctx, mkconfig.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = c.kueueClient.KueueV1beta1().MultiKueueConfigs().Create(ctx, mkconfig, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}

	newmkconfig := oldmkconfig.DeepCopy()
	newmkconfig.Spec.Clusters = mkconfig.Spec.Clusters
	_, err = c.kueueClient.KueueV1beta1().MultiKueueConfigs().Update(ctx, newmkconfig, metav1.UpdateOptions{})
	return err
}

// CreateOrUpdateMultiKueueCluster creates or updates a MultiKueueCluster resource for a specific cluster.
func (c *admissioncheckController) createOrUpdateMultiKueueCluster(ctx context.Context, mkc *kueuev1beta1.MultiKueueCluster) error {
	oldmkcluster, err := c.kueueClient.KueueV1beta1().MultiKueueClusters().Get(ctx, mkc.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = c.kueueClient.KueueV1beta1().MultiKueueClusters().Create(ctx, mkc, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}

	newmkc := oldmkcluster.DeepCopy()
	newmkc.Spec.KubeConfig = *mkc.Spec.KubeConfig.DeepCopy()
	_, err = c.kueueClient.KueueV1beta1().MultiKueueClusters().Update(ctx, newmkc, metav1.UpdateOptions{})
	return err
}

// CleanupMultiKueueClusters deletes MultiKueueCluster resources that are no longer referenced in the MultiKueueConfig.
// It ensures that only the expected clusters remain for the given Placement.
func (c *admissioncheckController) cleanupMultiKueueClusters(ctx context.Context, placementName string, expectedMKClusterNames sets.Set[string]) error {
	existingConfig, err := c.kueueClient.KueueV1beta1().MultiKueueConfigs().Get(ctx, placementName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get existing multi kueue config %s: %v", placementName, err)
	}
	for _, oldClusterName := range existingConfig.Spec.Clusters {
		if !expectedMKClusterNames.Has(oldClusterName) {
			if err := c.kueueClient.KueueV1beta1().MultiKueueClusters().Delete(ctx, oldClusterName, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("failed to delete multi kueue cluster %s: %v", oldClusterName, err)
			}
		}
	}
	return nil
}
