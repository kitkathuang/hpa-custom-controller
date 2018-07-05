package main

import (
	"os"
	"time"

	"github.com/golang/glog"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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
	sharedInformers := informers.NewSharedInformerFactory(client, 60*time.Minute)
	hpaController := NewHpaController(
		client,
		sharedInformers.Extensions().V1beta1().ReplicaSets(),
		sharedInformers.Autoscaling().V2beta1().HorizontalPodAutoscalers(),
	)
	sharedInformers.Start(stopCh)
	hpaController.Run(stopCh)
}
