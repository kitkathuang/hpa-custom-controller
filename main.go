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
	namespace = "default"
)

func main() {
	// TODO: Add support for passing in a kubeconfig

	config, err := rest.InClusterConfig()

	if err != nil {
		glog.Error("Failed to get in cluster config")
		os.Exit(1)
	}

	client := kubernetes.NewForConfigOrDie(config)
	stopCh := make(chan struct{})
	// resync time
	sharedInformers := informers.NewSharedInformerFactoryWithOptions(client, 60*time.Minute, WithNamespace(namespace))
	hpaController := NewHpaController(
		client,
		sharedInformers.Extensions().V1beta1().ReplicaSets(namespace),
		sharedInformers.Autoscaling().V2beta1().HorizontalPodAutoscalers(namespace),
	)
	sharedInformers.Start(stopCh)
	hpaController.Run(stopCh)
}
