package rcmanager

import (
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	networkingv1 "github.com/oecp/rama/pkg/apis/networking/v1"
	"github.com/oecp/rama/pkg/client/clientset/versioned"
	"github.com/oecp/rama/pkg/client/clientset/versioned/scheme"
	"github.com/oecp/rama/pkg/client/informers/externalversions"
	listers "github.com/oecp/rama/pkg/client/listers/networking/v1"
	"github.com/oecp/rama/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	kubeclientset "k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1 "k8s.io/client-go/listers/core/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const (
	ControllerName = "RemoteClusterManager"
	UserAgentName  = ControllerName
)

// Manager Those without the localCluster prefix are the resources of the remote cluster
type Manager struct {
	Meta
	LocalClusterKubeClient   kubeclientset.Interface
	LocalClusterRamaClient   versioned.Interface
	RemoteSubnetLister       listers.RemoteSubnetLister
	RemoteVtepLister         listers.RemoteVtepLister
	LocalClusterSubnetLister listers.SubnetLister

	KubeClient              *kubeclientset.Clientset
	RamaClient              *versioned.Clientset
	KubeInformerFactory     informers.SharedInformerFactory
	RamaInformerFactory     externalversions.SharedInformerFactory
	NodeLister              corev1.NodeLister
	NodeSynced              cache.InformerSynced
	NodeQueue               workqueue.RateLimitingInterface
	NetworkLister           listers.NetworkLister
	NetworkSynced           cache.InformerSynced
	SubnetLister            listers.SubnetLister
	SubnetSynced            cache.InformerSynced
	SubnetQueue             workqueue.RateLimitingInterface
	IPLister                listers.IPInstanceLister
	IPSynced                cache.InformerSynced
	IPQueue                 workqueue.RateLimitingInterface
	RemoteClusterNodeLister corev1.NodeLister
	RemoteClusterNodeSynced cache.InformerSynced
	Recorder                record.EventRecorder
}

type Meta struct {
	ClusterName string
	// RemoteClusterUID is used to set owner reference
	RemoteClusterUID types.UID
	// ClusterUUID represents the corresponding remote cluster's uuid, which is generated
	// from unique k8s resource
	ClusterUUID types.UID
	StopCh      chan struct{}
	// Only if meet the condition, can create remote cluster's cr
	// Conditions are:
	// 1. The remote cluster created the remote-cluster-cr of this cluster
	// 2. The remote cluster and local cluster both have overlay network
	// 3. The overlay network id is same with local cluster
	IsReady     bool
	IsReadyLock sync.RWMutex
}

func NewRemoteClusterManager(rc *networkingv1.RemoteCluster,
	localClusterKubeClient kubeclientset.Interface,
	localClusterRamaClient versioned.Interface,
	remoteSubnetLister listers.RemoteSubnetLister,
	localClusterSubnetLister listers.SubnetLister,
	remoteVtepLister listers.RemoteVtepLister) (*Manager, error) {
	defer func() {
		if err := recover(); err != nil {
			klog.Errorf("Panic hanppened. Can't new remote cluster manager. Maybe wrong kube config. "+
				"err=%v. remote cluster=%v\n%v", err, utils.ToJSONString(rc), debug.Stack())
		}
	}()
	klog.Infof("NewRemoteClusterManager %v", rc.Name)

	config, err := utils.BuildClusterConfig(rc)
	if err != nil {
		return nil, err
	}

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: localClusterKubeClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: ControllerName})

	kubeClient := kubeclientset.NewForConfigOrDie(config)
	ramaClient := versioned.NewForConfigOrDie(restclient.AddUserAgent(config, UserAgentName))
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	ramaInformerFactory := externalversions.NewSharedInformerFactory(ramaClient, 0)

	nodeInformer := kubeInformerFactory.Core().V1().Nodes()
	networkInformer := ramaInformerFactory.Networking().V1().Networks()
	subnetInformer := ramaInformerFactory.Networking().V1().Subnets()
	ipInformer := ramaInformerFactory.Networking().V1().IPInstances()

	uuid, err := utils.GetUUID(kubeClient)
	if err != nil {
		return nil, err
	}

	stopCh := make(chan struct{})

	rcMgr := &Manager{
		Meta: Meta{
			ClusterName:      rc.Name,
			RemoteClusterUID: rc.UID,
			ClusterUUID:      uuid,
			StopCh:           stopCh,
			IsReady:          false,
		},
		LocalClusterKubeClient:   localClusterKubeClient,
		LocalClusterRamaClient:   localClusterRamaClient,
		RemoteSubnetLister:       remoteSubnetLister,
		LocalClusterSubnetLister: localClusterSubnetLister,
		RemoteVtepLister:         remoteVtepLister,
		KubeClient:               kubeClient,
		RamaClient:               ramaClient,
		KubeInformerFactory:      kubeInformerFactory,
		RamaInformerFactory:      ramaInformerFactory,
		NodeLister:               kubeInformerFactory.Core().V1().Nodes().Lister(),
		NodeSynced:               kubeInformerFactory.Core().V1().Nodes().Informer().HasSynced,
		NodeQueue:                workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), fmt.Sprintf("%v-node", rc.ClusterName)),
		NetworkLister:            ramaInformerFactory.Networking().V1().Networks().Lister(),
		NetworkSynced:            networkInformer.Informer().HasSynced,
		SubnetLister:             ramaInformerFactory.Networking().V1().Subnets().Lister(),
		SubnetSynced:             subnetInformer.Informer().HasSynced,
		SubnetQueue:              workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), fmt.Sprintf("%v-subnet", rc.ClusterName)),
		IPLister:                 ramaInformerFactory.Networking().V1().IPInstances().Lister(),
		IPSynced:                 ipInformer.Informer().HasSynced,
		IPQueue:                  workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), fmt.Sprintf("%v-ipinstance", rc.ClusterName)),
		RemoteClusterNodeLister:  kubeInformerFactory.Core().V1().Nodes().Lister(),
		RemoteClusterNodeSynced:  nodeInformer.Informer().HasSynced,
		Recorder:                 recorder,
	}

	nodeInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: rcMgr.filterNode,
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    rcMgr.addOrDelNode,
			UpdateFunc: rcMgr.updateNode,
			DeleteFunc: rcMgr.addOrDelNode,
		},
	})

	subnetInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: rcMgr.filterSubnet,
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    rcMgr.addOrDelSubnet,
			UpdateFunc: rcMgr.updateSubnet,
			DeleteFunc: rcMgr.addOrDelSubnet,
		},
	})

	ipInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: rcMgr.filterIPInstance,
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    rcMgr.addOrDelIPInstance,
			UpdateFunc: rcMgr.updateIPInstance,
			DeleteFunc: rcMgr.addOrDelIPInstance,
		},
	})
	klog.Infof("Successfully New Remote Cluster Manager. Cluster=%v", rc.Name)
	return rcMgr, nil
}

func (m *Manager) Run() {
	klog.Infof("Start single remote cluster manager. clusterName=%v", m.ClusterName)

	managerCh := m.StopCh
	go func() {
		if ok := cache.WaitForCacheSync(managerCh, m.NodeSynced, m.SubnetSynced, m.IPSynced); !ok {
			klog.Errorf("failed to wait for remote cluster caches to sync. clusterName=%v", m.ClusterName)
			return
		}
		go wait.Until(m.RunNodeWorker, 1*time.Second, managerCh)
		go wait.Until(m.RunSubnetWorker, 1*time.Second, managerCh)
		go wait.Until(m.RunIPInstanceWorker, 1*time.Second, managerCh)
	}()
	go m.KubeInformerFactory.Start(managerCh)
	go m.RamaInformerFactory.Start(managerCh)
}

func (m *Manager) GetIsReady() bool {
	m.IsReadyLock.RLock()
	defer m.IsReadyLock.RUnlock()

	return m.IsReady
}

func (m *Manager) SetIsReady(val bool) {
	m.IsReadyLock.Lock()
	defer m.IsReadyLock.Unlock()

	m.IsReady = val
}

func (m *Manager) Close() {
	close(m.StopCh)
}
