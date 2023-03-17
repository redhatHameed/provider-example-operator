package controllers

import (
	"context"
	"fmt"
	dbaasv1beta1 "github.com/RHEcosystemAppEng/dbaas-operator/api/v1beta1"
	"github.com/go-logr/logr"
	v1 "k8s.io/api/apps/v1"
	rbac "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	label "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/pointer"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"time"
)

const (
	Dbaasproviderkind    = "DBaaSProvider"
	providerResourceName = "provider-example-registration"
	provisionDocUrl      = "https://www.exmample.com/docs/provider/quickstart.html"
	provisionDescription = "Follow the guide to start a free Provider Serverless (beta) cluster"
)

type DBaaSProviderReconciler struct {
	client.Client
	*runtime.Scheme
	Log                      logr.Logger
	Clientset                *kubernetes.Clientset
	operatorNameVersion      string
	operatorInstallNamespace string
}

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=dbaas.redhat.com,resources=dbaasproviders,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dbaas.redhat.com,resources=dbaasproviders/status,verbs=get;update;patch

func (r *DBaaSProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx, "dbaasproviders", req.NamespacedName)

	// due to predicate filtering, we'll only reconcile this operator's own deployment when it's seen the first time
	// meaning we have a reconcile entry-point on operator start-up, so now we can create a cluster-scoped resource
	// owned by the operator's ClusterRole to ensure cleanup on uninstall

	dep := &v1.Deployment{}
	if err := r.Get(ctx, req.NamespacedName, dep); err != nil {
		if errors.IsNotFound(err) {
			// CR deleted since request queued, child objects getting GC'd, no requeue
			log.Info("deployment not found, deleted, no requeue")
			return ctrl.Result{}, nil
		}
		// error fetching deployment, requeue and try again
		log.Error(err, "error fetching Deployment CR")
		return ctrl.Result{}, err
	}

	isCrdInstalled, err := r.checkCrdInstalled(dbaasv1beta1.GroupVersion.String(), Dbaasproviderkind)
	if err != nil {
		log.Error(err, "error discovering GVK")
		return ctrl.Result{}, err
	}
	if !isCrdInstalled {
		log.Info("CRD not found, requeueing with rate limiter")
		// returning with 'Requeue: true' will invoke our custom rate limiter seen in SetupWithManager below
		return ctrl.Result{Requeue: true}, nil
	}

	registrationCR := &dbaasv1beta1.DBaaSProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name: providerResourceName,
		},
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(registrationCR), registrationCR); err != nil {
		if errors.IsNotFound(err) {
			// CR deleted since request queued, child objects getting GC'd, no requeue
			log.Info("resource not found, creating now")

			// crunchy bridge registration custom resource isn't present,so create now with ClusterRole owner for GC
			opts := &client.ListOptions{
				LabelSelector: label.SelectorFromSet(map[string]string{
					"olm.owner":      r.operatorNameVersion,
					"olm.owner.kind": "ClusterServiceVersion",
				}),
			}
			clusterRoleList := &rbac.ClusterRoleList{}
			if err := r.List(context.Background(), clusterRoleList, opts); err != nil {
				log.Error(err, "unable to list ClusterRoles to seek potential operand owners")
				return ctrl.Result{}, err
			}

			if len(clusterRoleList.Items) < 1 {
				err := errors.NewNotFound(
					schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "ClusterRole"}, "potentialOwner")
				log.Error(err, "could not find ClusterRole owned by CSV to inherit operand")
				return ctrl.Result{}, err
			}

			registrationCR = buildProviderCR(clusterRoleList)
			if err := r.Create(ctx, registrationCR); err != nil {
				log.Error(err, "error while creating new cluster-scoped resource")
				return ctrl.Result{}, err
			} else {
				log.Info("cluster-scoped resource created")
				return ctrl.Result{}, nil
			}
		}
		// error fetching the resource, requeue and try again
		log.Error(err, "error fetching the resource")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DBaaSProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {

	// envVar set in controller-manager's Deployment YAML
	if operatorInstallNamespace, found := os.LookupEnv("INSTALL_NAMESPACE"); !found {
		err := fmt.Errorf("INSTALL_NAMESPACE must be set")
		return err
	} else {
		r.operatorInstallNamespace = operatorInstallNamespace
	}

	// envVar set for all operators
	if operatorNameEnvVar, found := os.LookupEnv("OPERATOR_CONDITION_NAME"); !found {
		err := fmt.Errorf("OPERATOR_CONDITION_NAME must be set")
		return err
	} else {
		r.operatorNameVersion = operatorNameEnvVar
	}

	customRateLimiter := workqueue.NewItemExponentialFailureRateLimiter(30*time.Second, 30*time.Minute)

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{RateLimiter: customRateLimiter}).
		For(
			&v1.Deployment{},
			builder.WithPredicates(r.ignoreOtherDeployments()),
			builder.OnlyMetadata,
		).
		Complete(r)
}

//ignoreOtherDeployments  only on a 'create' event is issued for the deployment
func (r *DBaaSProviderReconciler) ignoreOtherDeployments() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return r.evaluatePredicateObject(e.Object)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

func (r *DBaaSProviderReconciler) evaluatePredicateObject(obj client.Object) bool {
	lbls := obj.GetLabels()
	if obj.GetNamespace() == r.operatorInstallNamespace {
		if val, keyFound := lbls["olm.owner.kind"]; keyFound {
			if val == "ClusterServiceVersion" {
				if val, keyFound := lbls["olm.owner"]; keyFound {
					return val == r.operatorNameVersion
				}
			}
		}
	}
	return false
}

// CheckCrdInstalled checks whether dbaas provider CRD, has been created yet
func (r *DBaaSProviderReconciler) checkCrdInstalled(groupVersion, kind string) (bool, error) {
	resources, err := r.Clientset.Discovery().ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, r := range resources.APIResources {
		if r.Kind == kind {
			return true, nil
		}
	}
	return false, nil
}

func buildProviderCR(clusterRoleList *rbac.ClusterRoleList) *dbaasv1beta1.DBaaSProvider {
	instance := &dbaasv1beta1.DBaaSProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name: providerResourceName,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "rbac.authorization.k8s.io/v1",
					Kind:               "ClusterRole",
					UID:                clusterRoleList.Items[0].GetUID(),
					Name:               clusterRoleList.Items[0].Name,
					Controller:         pointer.BoolPtr(true),
					BlockOwnerDeletion: pointer.BoolPtr(false),
				},
			},
			Labels: map[string]string{"related-to": "dbaas-operator", "type": "dbaas-provider-registration"},
		},
		Spec: buildProviderSpec(dbaasv1beta1.ProvisioningParameter{}, dbaasv1beta1.ProvisioningParameter{}),
	}
	return instance
}

// providerRegistrationCR CR for crunchy bridge registration
func buildProviderSpec(regions, nodes dbaasv1beta1.ProvisioningParameter) dbaasv1beta1.DBaaSProviderSpec {

	testCloudRegion := "us-west-2"
	return dbaasv1beta1.DBaaSProviderSpec{
		GroupVersion: "dbaas.redhat.com/v1alpha1",
		Provider: dbaasv1beta1.DatabaseProviderInfo{
			Name:               "Provider Example",
			DisplayName:        "DBaaS Provider Example ",
			DisplayDescription: "This is an example for providers on how to implement their operator to integrate with DBaaS.",
			Icon: dbaasv1beta1.ProviderIcon{
				Data:      "SGVsbG8sIHdvcmxkLg==",
				MediaType: "image/png",
			},
		},
		InventoryKind:  "ProviderInventory",
		ConnectionKind: "ProviderConnection",
		InstanceKind:   "ProviderInstance",
		CredentialFields: []dbaasv1beta1.CredentialField{
			{
				Key:         "APIKey",
				DisplayName: "ProviderAPIKeyD",
				Type:        "maskedstring",
				Required:    true,
				HelpText:    "This is the API Key for the example provider to show on UI",
			},
		},
		AllowsFreeTrial:              true,
		ExternalProvisionURL:         provisionDocUrl,
		ExternalProvisionDescription: provisionDescription,
		ProvisioningParameters: map[dbaasv1beta1.ProvisioningParameterType]dbaasv1beta1.ProvisioningParameter{
			dbaasv1beta1.ProvisioningName: {
				DisplayName: "Cluster name",
			},
			dbaasv1beta1.ProvisioningPlan: {
				DisplayName: "Hosting plan",
				ConditionalData: []dbaasv1beta1.ConditionalProvisioningParameterData{
					{
						Options: []dbaasv1beta1.Option{
							{
								Value:        dbaasv1beta1.ProvisioningPlanFreeTrial,
								DisplayValue: "Free trial",
							},
							{
								Value:        dbaasv1beta1.ProvisioningPlanServerless,
								DisplayValue: "Serverless",
							},
							{
								Value:        dbaasv1beta1.ProvisioningPlanDedicated,
								DisplayValue: "Dedicated",
							},
						},
						DefaultValue: dbaasv1beta1.ProvisioningPlanServerless,
					},
				},
			},
			dbaasv1beta1.ProvisioningCloudProvider: {
				DisplayName: "Cloud Provider",
				ConditionalData: []dbaasv1beta1.ConditionalProvisioningParameterData{
					{
						Dependencies: []dbaasv1beta1.FieldDependency{
							{
								Field: dbaasv1beta1.ProvisioningPlan,
								Value: dbaasv1beta1.ProvisioningPlanFreeTrial,
							},
						},
						Options: []dbaasv1beta1.Option{
							{
								Value:        "GCP",
								DisplayValue: "Google Cloud Platform",
							},
						},
						DefaultValue: "GCP",
					},
					{
						Dependencies: []dbaasv1beta1.FieldDependency{
							{
								Field: dbaasv1beta1.ProvisioningPlan,
								Value: dbaasv1beta1.ProvisioningPlanServerless,
							},
						},
						Options: []dbaasv1beta1.Option{
							{
								Value:        "AWS",
								DisplayValue: "Amazon Web Services",
							},
							{
								Value:        "GCP",
								DisplayValue: "Google Cloud Platform",
							},
						},
						DefaultValue: "AWS",
					},
					{
						Dependencies: []dbaasv1beta1.FieldDependency{
							{
								Field: dbaasv1beta1.ProvisioningPlan,
								Value: dbaasv1beta1.ProvisioningPlanDedicated,
							},
						},
						Options: []dbaasv1beta1.Option{
							{
								Value:        "AWS",
								DisplayValue: "Amazon Web Services",
							},
							{
								Value:        "GCP",
								DisplayValue: "Google Cloud Platform",
							},
						},
						DefaultValue: "AWS",
					}},
			},
			dbaasv1beta1.ProvisioningRegions: {
				DisplayName: testCloudRegion,
			},
			dbaasv1beta1.ProvisioningNodes: {
				DisplayName: "nodes",
			},
			dbaasv1beta1.ProvisioningMachineType: {
				DisplayName: "Compute",
				ConditionalData: []dbaasv1beta1.ConditionalProvisioningParameterData{
					{
						Dependencies: []dbaasv1beta1.FieldDependency{
							{
								Field: dbaasv1beta1.ProvisioningPlan,
								Value: dbaasv1beta1.ProvisioningPlanDedicated,
							},
							{
								Field: dbaasv1beta1.ProvisioningCloudProvider,
								Value: string("AWS"),
							},
						},
						Options: []dbaasv1beta1.Option{
							{
								Value:        "m5.large",
								DisplayValue: "2 vCPU, 8 GiB RAM",
							},
							{
								Value:        "m5.xlarge",
								DisplayValue: "4 vCPU, 16 GiB RAM",
							},
							{
								Value:        "m5.2xlarge",
								DisplayValue: "8 vCPU, 32 GiB RAM",
							},
							{
								Value:        "m5.4xlarge",
								DisplayValue: "16 vCPU, 64 GiB RAM",
							},
							{
								Value:        "m5.8xlarge",
								DisplayValue: "32 vCPU, 128 GiB RAM",
							},
						},
						DefaultValue: "m5.large",
					},
					{
						Dependencies: []dbaasv1beta1.FieldDependency{
							{
								Field: dbaasv1beta1.ProvisioningPlan,
								Value: dbaasv1beta1.ProvisioningPlanDedicated,
							},
							{
								Field: dbaasv1beta1.ProvisioningCloudProvider,
								Value: string("GCP"),
							},
						},
						Options: []dbaasv1beta1.Option{
							{
								Value:        "n1-standard-2",
								DisplayValue: "2 vCPU, 7.5 GiB RAM",
							},
							{
								Value:        "n1-standard-4",
								DisplayValue: "4 vCPU, 15 GiB RAM",
							},
							{
								Value:        "n1-standard-8",
								DisplayValue: "8 vCPU, 30 GiB RAM",
							},
							{
								Value:        "n1-standard-16",
								DisplayValue: "16 vCPU, 60 GiB RAM",
							},
							{
								Value:        "n1-standard-32",
								DisplayValue: "32 vCPU, 120 GiB RAM",
							},
						},
						DefaultValue: "n1-standard-2",
					},
				},
			},
			dbaasv1beta1.ProvisioningStorageGib: {
				DisplayName: "Storage",
				ConditionalData: []dbaasv1beta1.ConditionalProvisioningParameterData{
					{
						Dependencies: []dbaasv1beta1.FieldDependency{
							{
								Field: dbaasv1beta1.ProvisioningPlan,
								Value: dbaasv1beta1.ProvisioningPlanDedicated,
							},
							{
								Field: dbaasv1beta1.ProvisioningCloudProvider,
								Value: string("AWS"),
							},
						},
						Options: []dbaasv1beta1.Option{
							{
								Value:        "15",
								DisplayValue: "15 GiB",
							},
							{
								Value:        "35",
								DisplayValue: "35 GiB",
							},
							{
								Value:        "75",
								DisplayValue: "75 GiB",
							},
							{
								Value:        "150",
								DisplayValue: "150 GiB",
							},
							{
								Value:        "300",
								DisplayValue: "300 GiB",
							},
							{
								Value:        "600",
								DisplayValue: "600 GiB",
							},
						},
						DefaultValue: "15",
					},
					{
						Dependencies: []dbaasv1beta1.FieldDependency{
							{
								Field: dbaasv1beta1.ProvisioningPlan,
								Value: dbaasv1beta1.ProvisioningPlanDedicated,
							},
							{
								Field: dbaasv1beta1.ProvisioningCloudProvider,
								Value: string("GCP"),
							},
						},
						Options: []dbaasv1beta1.Option{
							{
								Value:        "15",
								DisplayValue: "15 GiB",
							},
							{
								Value:        "35",
								DisplayValue: "35 GiB",
							},
							{
								Value:        "75",
								DisplayValue: "75 GiB",
							},
							{
								Value:        "150",
								DisplayValue: "150 GiB",
							},
							{
								Value:        "300",
								DisplayValue: "300 GiB",
							},
							{
								Value:        "600",
								DisplayValue: "600 GiB",
							},
						},
						DefaultValue: "15",
					},
				},
			},
			dbaasv1beta1.ProvisioningSpendLimit: {
				DisplayName: "Spend limit",
				ConditionalData: []dbaasv1beta1.ConditionalProvisioningParameterData{
					{
						Dependencies: []dbaasv1beta1.FieldDependency{
							{
								Field: dbaasv1beta1.ProvisioningPlan,
								Value: dbaasv1beta1.ProvisioningPlanServerless,
							},
						},
						DefaultValue: "0",
					},
				},
			},
			dbaasv1beta1.ProvisioningPlanLabel: {
				DisplayName: "Select a plan",
			},
			dbaasv1beta1.ProvisioningServerlessLocationLabel: {
				DisplayName: "Select regions",
				HelpText:    "Select the geographical region where you want the database instance to run.",
			},
			dbaasv1beta1.ProvisioningDedicatedLocationLabel: {
				DisplayName: "Select regions & nodes",
				HelpText:    "Select the geographical region where you want the database instance to run, and set the number of nodes you want running in this dedicated cluster.",
			},
			dbaasv1beta1.ProvisioningHardwareLabel: {
				DisplayName: "Hardware per node",
				HelpText:    "Select the compute and storage requirements for this database instance.",
			},
			dbaasv1beta1.ProvisioningSpendLimitLabel: {
				DisplayName: "Spend limit",
				HelpText:    "Set a spending limit on resources for this database instance.",
			},
		},
	}
}
