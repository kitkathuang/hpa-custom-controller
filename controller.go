package main

import (
	"fmt"
	"time"

	"github.com/golang/glog"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	autoscaling_v2beta1 "k8s.io/api/autoscaling/v2beta1"
	corev1 "k8s.io/api/core/v1"
	ext_v1beta1 "k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	autoscaling_informers "k8s.io/client-go/informers/autoscaling/v2beta1"
	extensions_informers "k8s.io/client-go/informers/extensions/v1beta1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	extlisters "k8s.io/client-go/listers/extensions/v1beta1"
)

const (
	controllerName = "hpa-controller"
	// namespace         = "default"
	maxRetries        = 3
	cacheResyncPeriod = 10
)

// Event is the type that we will add to our queues
type Event struct {
	key       string
	eventType string
}

// HpaController is a custom controller that monitors
// events related to replica sets and
// creates, updates, or deletes HPAs accordingly.
type HpaController struct {
	// client is a standard kubernetes clientset
	client kubernetes.Interface

	// *Listers will hit the cache in order to list k8s resources.
	// *Synced are used to ensure that we wait for caches to sync
	//   before we start processing work.
	rsLister extlisters.ReplicaSetLister
	rsSynced cache.InformerSynced

	//hpaLister autoscalingListers.HorizontalPodAutoscalerLister
	hpaSynced cache.InformerSynced

	// queue will store events as they are received by the informer.
	// It is a rate limiting queue, so that events will be processed
	// at a certain rate instead of immediately after being received.
	// queue operations are atomic so multiple workers won't process the same item.
	queue workqueue.RateLimitingInterface

	// recorder allows us to write Events to k8s objects
	recorder record.EventRecorder
}

// NewHpaController returns a new HpaController
func NewHpaController(
	client kubernetes.Interface,
	rsInformer extensions_informers.ReplicaSetInformer,
	hpaInformer autoscaling_informers.HorizontalPodAutoscalerInformer) (c *HpaController) {
	var event Event

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: client.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerName})

	//hpaLister := hpaInformer.Lister()

	c = &HpaController{
		client:    client,
		rsSynced:  rsInformer.Informer().HasSynced,
		rsLister:  rsInformer.Lister(),
		hpaSynced: hpaInformer.Informer().HasSynced,
		//hpaLister: hpaInformer.Lister(),
		queue:    workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		recorder: recorder,
	}

	// TODO: define these as functions elsewhere
	rsInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if key, err := cache.MetaNamespaceKeyFunc(obj); err == nil {
				if ns, name, err := cache.SplitMetaNamespaceKey(key); err == nil {
					glog.Infof("Got add for key: %s in namespace: %s", name, ns)

					event.key = name
					event.eventType = "add"
					c.queue.Add(event)
				}
			}
		},
		UpdateFunc: func(old, new interface{}) {
			if key, err := cache.MetaNamespaceKeyFunc(old); err == nil {
				if ns, name, err := cache.SplitMetaNamespaceKey(key); err == nil {
					glog.Infof("Got update for key: %s in namespace: %s", name, ns)

					event.key = name
					event.eventType = "update"
					c.queue.Add(event)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err == nil {
				glog.Info("Got delete for key: %s", key)

				event.key = key
				event.eventType = "delete"
				c.queue.Add(event)
			} else { // wtf
				// TODO: Would probably want to check that an HPA exists, and, if so, delete it
				glog.Errorf("Got a deletefinalstate unknown object. Not adding to queue.")
			}
		},
	})

	return
}

// Run starts the controller
// Make this return an error and catch it in main probably
func (c *HpaController) Run(stopCh <-chan struct{}) { // stopCh only receives
	runtime.HandleCrash() // this will be removed eventually
	defer c.queue.ShutDown()

	glog.Info("Starting Hpa Controller")

	if !cache.WaitForCacheSync(stopCh, c.rsSynced, c.hpaSynced) {
		glog.Error("Failed waiting for replica set and hpa caches to sync")
		return
	}

	glog.Info("Controller synced")

	// add more "threads"
	go wait.Until(c.runWorker, time.Second, stopCh) // block until worker(s) have stopped

	<-stopCh
	glog.Info("Stopping HPA Controller")

	return
}

func (c *HpaController) runWorker() {
	for c.processNext() {
	}
}

func (c *HpaController) processNext() bool {
	event, quit := c.queue.Get()

	if quit {
		return false
	}

	defer c.queue.Done(event)

	err := c.processEvent(event.(Event))

	// Calling forget() tells the queue to stop tracking the number of retries
	// on this item. Calling done() allows other workers to pick up the event
	// (keys are never processed by the same workers).
	if err == nil {
		c.queue.Forget(event)
	} else if c.queue.NumRequeues(event) < maxRetries {
		glog.Infof("Retrying event %s after receiving error %v. At retry %d of %d max retries.", event.(Event).key, err, c.queue.NumRequeues(event), maxRetries)
		c.queue.AddRateLimited(event)
	} else {
		glog.Errorf("Reached max retries processing event %s - returned with error %v. Will stop processing.", event.(Event).key, err)
		c.queue.Forget(event)
		runtime.HandleError(err)
	}

	return true
}

func (c *HpaController) processEvent(event Event) error {
	rs, err := c.rsLister.ReplicaSets(namespace).Get(event.key)

	if err != nil {
		return fmt.Errorf("Failed to fetch event with key: %s from rs lister with error: %v", event.key, err)
	}

	switch event.eventType {
	case "add":
		err := c.createNewHpa(event.key, rs)
		return err

	case "update":
		glog.Info("Ignoring update to replica set %s", event.key) // although would want to check for disables (replicas==0...)
		return nil

	case "delete":
		err := c.deleteHpa(event.key)
		return err
	}

	return nil
}

// TODO: Remember to pass copies of things from cache
// TODO: Make a wrapper function around this so we can just pass min/max/annotations
func (c *HpaController) createNewHpa(rsName string, rs *ext_v1beta1.ReplicaSet) error {
	hpaClient := c.client.AutoscalingV2beta1().HorizontalPodAutoscalers(namespace)

	minReplicas := *rs.Spec.Replicas
	//targetAvg := int32(10)

	// TODO: split this up into multiple functions
	// TODO: add constants
	hpa := &autoscaling_v2beta1.HorizontalPodAutoscaler{
		TypeMeta: metav1.TypeMeta{
			Kind:       "HorizontalPodAutoscaler",
			APIVersion: "autoscaling/v2beta1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      rsName + "-hpa",
			Namespace: namespace,
		},
		Spec: autoscaling_v2beta1.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscaling_v2beta1.CrossVersionObjectReference{
				Kind:       "ReplicaSet",         //string
				Name:       rsName,               //string
				APIVersion: "extensions/v1beta1", //string
			},
			MinReplicas: &minReplicas, //*int32
			MaxReplicas: 10,           //int32 TODO use annotation
			Metrics: []autoscaling_v2beta1.MetricSpec{ //[]MetricSpec
				{
					Type: "", //string? TODO use annotation
					Object: &autoscaling_v2beta1.ObjectMetricSource{
						Target: autoscaling_v2beta1.CrossVersionObjectReference{
							Kind:       "", //string
							Name:       "", //string
							APIVersion: "", //string
						},
						MetricName: "someMetric", //string
						TargetValue: resource.Quantity{
							Format: "1000m",
						},
					},
				},
			},
		},
	}

	glog.Infof("Creating hpa " + rsName + "-hpa")
	result, err := hpaClient.Create(hpa)
	if err != nil {
		glog.Error("Failed to create HPA")
		return err
	}

	glog.Infof("Created HPA %q", result.GetObjectMeta().GetName())

	return nil
}

func (c *HpaController) deleteHpa(rsName string) error {
	glog.Info("Too lazy to implement delete rn")
	return nil
}
