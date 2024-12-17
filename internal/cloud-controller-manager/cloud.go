package crusoe

import (
	"io"
	"os"

	"k8s.io/client-go/informers"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
)

const (
	ProviderName = "crusoe"
	APIEndpoint  = "CRUSOE_API_ENDPOINT"
	AccessKey    = "CRUSOE_ACCESS_KEY"
	SecretKey    = "CRUSOE_SECRET_KEY"
)

type Cloud struct {
	crusoeInstances *Instances
}

// revive:disable:unused-parameter
func (c *Cloud) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
	klog.Info("Initialize()")
	clientset := clientBuilder.ClientOrDie("crusoe-shared-informers")
	sharedInformer := informers.NewSharedInformerFactory(clientset, 0)
	sharedInformer.Start(nil)
	sharedInformer.WaitForCacheSync(nil)
}

func (c *Cloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) { return nil, false }

func (c *Cloud) Instances() (cloudprovider.Instances, bool) { return c.crusoeInstances, true }

func (c *Cloud) InstancesV2() (cloudprovider.InstancesV2, bool) { return c.crusoeInstances, true }

func (c *Cloud) Clusters() (cloudprovider.Clusters, bool) {
	return nil, false
}

func (c *Cloud) Routes() (cloudprovider.Routes, bool) {
	return nil, false
}

func (c *Cloud) Zones() (cloudprovider.Zones, bool) {
	return nil, false
}

func (c *Cloud) ProviderName() string { return ProviderName }

func (c *Cloud) HasClusterID() bool {
	return true
}

func RegisterCloudProvider() {
	cloudprovider.RegisterCloudProvider(ProviderName, func(io.Reader) (cloudprovider.Interface, error) {
		return newCloud()
	})
}

func newCloud() (cloudprovider.Interface, error) {
	apiEndPoint := os.Getenv(APIEndpoint)
	apiAccessKey := os.Getenv(AccessKey)
	apiSecretKey := os.Getenv(SecretKey)
	cc := NewCrusoeClient(apiEndPoint, apiAccessKey, apiSecretKey,
		"crusoe-cloud-controller-manager/0.0.1")

	return &Cloud{
		crusoeInstances: NewCrusoeInstances(cc),
	}, nil
}
