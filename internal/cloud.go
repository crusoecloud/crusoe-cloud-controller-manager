package crusoe

import (
	"context"
	"io"
	"os"

	auth "github.com/crusoecloud/crusoe-cloud-controller-manager/internal/auth"
	client "github.com/crusoecloud/crusoe-cloud-controller-manager/internal/client"
	instances "github.com/crusoecloud/crusoe-cloud-controller-manager/internal/instances"
	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes"
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
	crusoeInstances *instances.Instances
	apiClient       client.APIClient    // retained from newCloud for route wiring
	clusterName     string              // set by doInitializer before Initialize runs (§8)
	routes          *routes.CloudRoutes // nil ⇒ overlay mode
}

// SetClusterName stashes the --cluster-name flag value, which Initialize does
// not receive but location resolution needs (§8). doInitializer calls it before
// Initialize runs.
func (c *Cloud) SetClusterName(name string) { c.clusterName = name }

// revive:disable:unused-parameter
func (c *Cloud) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
	clientset := clientBuilder.ClientOrDie("crusoe-shared-informers")
	sharedInformer := informers.NewSharedInformerFactory(clientset, 0)
	sharedInformer.Start(nil)
	sharedInformer.WaitForCacheSync(nil)

	c.initRoutes(stop)
}

// initRoutes builds the CloudRoutes implementation in native mode, leaving
// c.routes nil in overlay mode. Native-mode construction failures are fatal
// (klog.Fatalf): the Deployment's crash-loop backoff is the retry, and this
// keeps cfg immutable before any controller starts (§8).
func (c *Cloud) initRoutes(stop <-chan struct{}) {
	cfg, err := routes.LoadConfigFromEnv()
	if err != nil {
		klog.Fatalf("crusoe route config: %v", err) // partial env = misrender, fail fast
	}
	if cfg.RoutingMode != routes.RoutingModeNative {
		return // overlay: c.routes stays nil
	}

	loc, err := routes.ResolveLocationFromCluster(
		context.Background(), c.apiClient, cfg.ProjectID, c.clusterName)
	if err != nil {
		klog.Fatalf("crusoe cluster location lookup failed for %q "+
			"(--cluster-name must equal the Crusoe cluster name): %v", c.clusterName, err)
	}
	cfg.Location = loc

	sdnClient, closeSDN, err := routes.BuildSDNClient(cfg)
	if err != nil {
		klog.Fatalf("crusoe SDN client: %v", err)
	}
	if closeSDN != nil {
		go func() {
			<-stop
			if cerr := closeSDN(); cerr != nil {
				klog.ErrorS(cerr, "closing SDN connection")
			}
		}()
	}

	c.routes = routes.NewCloudRoutes(cfg, sdnClient, c.apiClient)
}

// SetInformers implements cloudprovider.InformerUser: it runs after Initialize
// and before any controller starts (§8), so the node lister is always set and
// synced before the first ListRoutes.
func (c *Cloud) SetInformers(f informers.SharedInformerFactory) {
	if c.routes != nil {
		c.routes.SetNodeLister(f.Core().V1().Nodes().Lister())
	}
}

func (c *Cloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) { return nil, false }

func (c *Cloud) Instances() (cloudprovider.Instances, bool) { return c.crusoeInstances, true }

func (c *Cloud) InstancesV2() (cloudprovider.InstancesV2, bool) { return c.crusoeInstances, true }

func (c *Cloud) Clusters() (cloudprovider.Clusters, bool) {
	return nil, false
}

func (c *Cloud) Routes() (cloudprovider.Routes, bool) {
	return c.routes, c.routes != nil
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
	cc := auth.NewCrusoeClient(apiEndPoint, apiAccessKey, apiSecretKey,
		"crusoe-cloud-controller-manager/0.0.1")
	apiClient := &client.APIClientImpl{
		CrusoeAPIClient: cc,
	}

	return &Cloud{
		crusoeInstances: instances.NewCrusoeInstances(apiClient),
		apiClient:       apiClient,
	}, nil
}
