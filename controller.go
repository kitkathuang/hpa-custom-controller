package main

import (
	"errors"
	"strconv"
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

	// maxRetries is the number of times we will try to process keys from the queue
	maxRetries = 3
)

// annotationKeys are the annotations we will look for
// on replica sets to
// 1) know if we need to create an hpa for it
// 2) how to create the hpa.
// Currently using garbage values for other hpa values.
var annotationKeys = [4]string{"minReplicas", "maxReplicas", "metricName", "targetValue"}

// Event is the type that we will add to our queues
type Event struct {
	key       string
	eventType string
}

// hpaMeta is just for convenience for when we
// call the createHpa function
type hpaMeta struct {
	minReplicas int32
	maxReplicas int32
	metricName  string
	targetValue string
}

// HpaController is a custom controller that monitors
// events (add, update, delete) related to replica sets.
// It checks for annotations on replica sets, and
// creates/updates/deletes hpas accordingly.
// NOTE: Only working on getting create working I'm too lazy to do the rest
type HpaController struct {
	client kubernetes.Interface

	// * Listers will hit the cache in order to list k8s objects.
	//   These are so that we don't need to hit the api server.
	// * Synced is used to ensure that we wait for caches to sync
	//   before we start processing work.
	rsLister extlisters.ReplicaSetLister
	rsSynced cache.InformerSynced

	//hpaLister autoscalingListers.HorizontalPodAutoscalerLister
	hpaSynced cache.InformerSynced

	// queue will store events (add, update, delete) as they are received by the informer.
	// Items will be processed at most 3 times before workers give up.
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
			// DeletionHandlingMetaNamespaceKeyFunc will check for "DeletedFinalStateUnknown",
			// which we get when the informer's watcher missed the deletion event.
			// On the next re-list, a "DeletedFinalStateUnknown" will get placed
			// into the queue. In this case, we do not know the final state of
			// the object before it got deleted.
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
func (c *HpaController) Run(stopCh <-chan struct{}) {
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
		glog.Infof("Retrying event %s after receiving error %v. At retry %d of %d max retries.",
			event.(Event).key,
			err,
			c.queue.NumRequeues(event),
			maxRetries)
		c.queue.AddRateLimited(event)
	} else {
		glog.Errorf("Reached max retries processing event %s - returned with error %v. Will stop processing.",
			event.(Event).key, err)
		c.queue.Forget(event)
		runtime.HandleError(err)
	}

	return true
}

func (c *HpaController) processEvent(event Event) error {
	rs, err := c.rsLister.ReplicaSets(namespace).Get(event.key)

	data, err := getHpaDataFromAnnotations(rs)

	if err != nil {
		return err
	}

	switch event.eventType {
	case "add":
		err := c.createNewHpa(event.key, rs, data)
		return err

	case "update":
		// although would want to check for disables (replicas==0...)
		glog.Info("Ignoring update to replica set %s", event.key)
		return nil

	case "delete":
		err := c.deleteHpa(event.key)
		return err
	}

	return nil
}

func getHpaDataFromAnnotations(rs *ext_v1beta1.ReplicaSet) (*hpaMeta, error) {
	var minReplicas int32
	var maxReplicas int32
	var metricName string
	var targetValue string

	for _, key := range annotationKeys {
		if val, ok := rs.Annotations[key]; ok {
			if key == "minReplicas" {
				i, err := strconv.ParseInt(val, 10, 32)

				if err != nil {
					return nil, err
				}

				i32 := int32(i)
				minReplicas = i32
			} else if key == "maxReplicas" {
				i, err := strconv.ParseInt(val, 10, 32)

				if err != nil {
					return nil, err
				}

				i32 := int32(i)
				maxReplicas = i32
			} else if key == "metricName" {
				metricName = val
			} else {
				targetValue = val
			}
		} else {
			err := "Replica set is missing annotation " + key
			return nil, errors.New(err)
		}
	}

	return &hpaMeta{
		minReplicas: minReplicas,
		maxReplicas: maxReplicas,
		metricName:  metricName,
		targetValue: targetValue,
	}, nil

}

// TODO: Remember to pass copies of things from cache
func (c *HpaController) createNewHpa(rsName string, rs *ext_v1beta1.ReplicaSet, data *hpaMeta) error {
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
			MinReplicas: &minReplicas,     //*int32
			MaxReplicas: data.maxReplicas, //int32 TODO use annotation
			Metrics: []autoscaling_v2beta1.MetricSpec{ //[]MetricSpec
				{
					Type: "Object", //string? TODO use annotation
					Object: &autoscaling_v2beta1.ObjectMetricSource{
						Target: autoscaling_v2beta1.CrossVersionObjectReference{
							Kind: "Service",      //string
							Name: "fake-service", //string
							//APIVersion: "", //string optional
						},
						MetricName: data.metricName, //string
						TargetValue: resource.Quantity{
							Format: resource.Format(data.targetValue),
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
