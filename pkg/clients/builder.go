package clients

import (
	"context"
	"time"

	snapshotclientset "github.com/kubernetes-csi/external-snapshotter/client/v6/clientset/versioned"
	opv1 "github.com/openshift/api/operator/v1"
	cfgclientset "github.com/openshift/client-go/config/clientset/versioned"
	cfginformers "github.com/openshift/client-go/config/informers/externalversions"
	opclient "github.com/openshift/client-go/operator/clientset/versioned"
	opinformers "github.com/openshift/client-go/operator/informers/externalversions"
	hypextclient "github.com/openshift/hypershift/client/clientset/clientset"
	hypextinformers "github.com/openshift/hypershift/client/informers/externalversions"
	"github.com/openshift/library-go/pkg/config/client"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/events"
	goc "github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

// Builder is a helper to create Clients.
type Builder struct {
	userAgwent             string
	csiDriverName          string
	guestNamespace         string
	resync                 time.Duration
	controllerConfig       *controllercmd.ControllerContext
	guestKubeConfig        *rest.Config
	guestKubeConfigFile    string
	controlPlaneNamespaces []string
	guestNamespaces        []string

	client *Clients
}

// NewBuilder creates a new Builder.
func NewBuilder(userAgent string, csiDriverName, guestNamespace string, controlPlaneNamespaces []string, controllerConfig *controllercmd.ControllerContext, resync time.Duration) *Builder {
	return &Builder{
		userAgwent:             userAgent,
		csiDriverName:          csiDriverName,
		guestNamespace:         guestNamespace,
		resync:                 resync,
		controllerConfig:       controllerConfig,
		controlPlaneNamespaces: controlPlaneNamespaces,
		guestNamespaces: []string{
			"",
			guestNamespace,
		},
	}
}

// WithHyperShiftGuest sets the kubeconfig file for the guest cluster. Usable only on HyperShift.
func (b *Builder) WithHyperShiftGuest(kubeConfigFile string, cloudConfigNamespace string) *Builder {
	b.guestKubeConfigFile = kubeConfigFile
	if cloudConfigNamespace != "" {
		b.guestNamespaces = append(b.guestNamespaces, cloudConfigNamespace)
	} else {
		b.guestNamespaces = append(b.guestNamespaces, ManagedConfigNamespace)
	}
	return b
}

// BuildOrDie creates new Kubernetes clients.
func (b *Builder) BuildOrDie(ctx context.Context) *Clients {
	controlPlaneRestConfig := rest.AddUserAgent(b.controllerConfig.KubeConfig, b.userAgwent)
	controlPlaneKubeClient := kubeclient.NewForConfigOrDie(controlPlaneRestConfig)
	controlPlaneKubeInformers := v1helpers.NewKubeInformersForNamespaces(controlPlaneKubeClient, b.controlPlaneNamespaces...)

	controlPlaneDynamicClient := dynamic.NewForConfigOrDie(controlPlaneRestConfig)
	controlPlaneDynamicInformers := dynamicinformer.NewFilteredDynamicSharedInformerFactory(controlPlaneDynamicClient, b.resync, b.controllerConfig.OperatorNamespace, nil)

	isHyperShift := b.guestKubeConfigFile != ""
	b.client = &Clients{
		ControlPlaneNamespace:     b.controllerConfig.OperatorNamespace,
		EventRecorder:             b.controllerConfig.EventRecorder,
		ControlPlaneKubeClient:    controlPlaneKubeClient,
		ControlPlaneKubeInformers: controlPlaneKubeInformers,

		ControlPlaneDynamicClient:   controlPlaneDynamicClient,
		ControlPlaneDynamicInformer: controlPlaneDynamicInformers,
		GuestNamespace:              b.guestNamespace,
	}

	guestKubeClient := controlPlaneKubeClient
	guestKubeConfig := controlPlaneRestConfig
	if isHyperShift {
		var err error
		guestKubeConfig, err = client.GetKubeConfigOrInClusterConfig(b.guestKubeConfigFile, nil)
		if err != nil {
			klog.Fatalf("error reading guesKubeConfig from %s: %v", b.guestKubeConfigFile, err)
		}
		guestKubeConfig = rest.AddUserAgent(guestKubeConfig, b.userAgwent)
		guestKubeClient = kubeclient.NewForConfigOrDie(guestKubeConfig)

		// Create all events in the GUEST cluster.
		// Use name of the operator Deployment in the management cluster + namespace
		// in the guest cluster as the closest approximation of the real involvedObject.
		controllerRef, err := events.GetControllerReferenceForCurrentPod(ctx, controlPlaneKubeClient, b.controllerConfig.OperatorNamespace, nil)
		controllerRef.Namespace = b.guestNamespace
		if err != nil {
			klog.Warningf("unable to get owner reference (falling back to namespace): %v", err)
		}
		b.client.EventRecorder = events.NewKubeRecorder(guestKubeClient.CoreV1().Events(b.guestNamespace), b.userAgwent, controllerRef)

		b.client.ControlPlaneHypeClient = hypextclient.NewForConfigOrDie(controlPlaneRestConfig)
		b.client.ControlPlaneHypeInformer = hypextinformers.NewFilteredSharedInformerFactory(b.client.ControlPlaneHypeClient, b.resync, b.controllerConfig.OperatorNamespace, nil)
	}
	// store guestKubeConfig in case we need it later for running
	b.guestKubeConfig = guestKubeConfig
	b.client.KubeClient = guestKubeClient
	b.client.KubeInformers = v1helpers.NewKubeInformersForNamespaces(guestKubeClient, b.guestNamespaces...)

	gvr := opv1.SchemeGroupVersion.WithResource("clustercsidrivers")
	guestOperatorClient, guestOperatorDynamicInformers, err := goc.NewClusterScopedOperatorClientWithConfigName(guestKubeConfig, gvr, b.csiDriverName)
	if err != nil {
		klog.Fatalf("error building clustercsidriver informers: %v", err)
	}
	b.client.OperatorClient = guestOperatorClient
	b.client.operatorDynamicInformers = guestOperatorDynamicInformers

	guestAPIExtClient, err := apiextclient.NewForConfig(guestKubeConfig)
	if err != nil {
		klog.Fatalf("error building api extension client in target cluster: %v", err)
	}
	b.client.APIExtClient = guestAPIExtClient
	b.client.APIExtInformer = apiextinformers.NewSharedInformerFactory(guestAPIExtClient, b.resync)

	guestDynamicClient, err := dynamic.NewForConfig(guestKubeConfig)
	if err != nil {
		klog.Fatalf("error building dynamic api client in target cluster: %v", err)
	}
	b.client.DynamicClient = guestDynamicClient

	// TODO: non-filtered one for VolumeSnapshots?
	b.client.DynamicInformer = dynamicinformer.NewFilteredDynamicSharedInformerFactory(guestDynamicClient, b.resync, b.guestNamespace, nil)

	b.client.OperatorClientSet = opclient.NewForConfigOrDie(guestKubeConfig)
	b.client.OperatorInformers = opinformers.NewSharedInformerFactory(b.client.OperatorClientSet, b.resync)

	b.client.ConfigClientSet = cfgclientset.NewForConfigOrDie(guestKubeConfig)
	b.client.ConfigInformers = cfginformers.NewSharedInformerFactory(b.client.ConfigClientSet, b.resync)

	return b.client
}

func (b *Builder) GetClient() *Clients {
	return b.client
}

func (b *Builder) AddSnapshotClient(ctx context.Context) (snapshotclientset.Interface, error) {
	var err error
	b.client.SnapshotClient, err = snapshotclientset.NewForConfig(b.guestKubeConfig)
	if err != nil {
		return nil, err
	}
	return b.client.SnapshotClient, nil
}
