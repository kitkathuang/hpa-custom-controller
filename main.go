package main

import (
	"os"
	"time"

	"github.com/golang/glog"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	namespace         = "default"
	cacheResyncPeriod = 10 // resync caches every 10 minutes
)

func main() {

	// TODO: Add support for passing in a kubeconfig.
	// As of right now, using InClusterConfig, the controller expects to be run
	// in a pod within the cluster. It will use the service account passed into
	// the container for auth.
	config, err := rest.InClusterConfig()

	if err != nil {
		glog.Error("Failed to get in cluster config")
		os.Exit(1)
	}

	client := kubernetes.NewForConfigOrDie(config)

	// Limit informers so they only get events about
	// replica sets and hpas in the "default" namespace
	sharedInformers := informers.NewFilteredSharedInformerFactory(client, cacheResyncPeriod*time.Minute, namespace, nil)

	hpaController := NewHpaController(
		client,
		sharedInformers.Extensions().V1beta1().ReplicaSets(),
		sharedInformers.Autoscaling().V2beta1().HorizontalPodAutoscalers(),
	)

	stopCh := make(chan struct{})

	sharedInformers.Start(stopCh)
	hpaController.Run(stopCh)
}
