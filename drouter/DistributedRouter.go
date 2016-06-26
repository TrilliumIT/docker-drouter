package drouter

import (
	"fmt"
	log "github.com/Sirupsen/logrus"
	dockerclient "github.com/docker/engine-api/client"
	dockertypes "github.com/docker/engine-api/types"
	dockerevents "github.com/docker/engine-api/types/events"
	dockerfilters "github.com/docker/engine-api/types/filters"
	"github.com/vdemeester/docker-events"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/context"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

var (
	//cli options
	ipOffset         int
	aggressive       bool
	hostShortcut     bool
	containerGateway bool
	hostGateway      bool
	masquerade       bool
	p2p              *p2pNetwork
	staticRoutes     []*net.IPNet
	transitNetName   string
	selfContainerID  string

	//other globals
	dockerClient *dockerclient.Client
	transitNetID string
	connectWG    sync.WaitGroup
	stopChan     <-chan struct{}
)

//DistributedRouterOptions Options for our DistributedRouter instance
type DistributedRouterOptions struct {
	IPOffset         int
	Aggressive       bool
	HostShortcut     bool
	ContainerGateway bool
	HostGateway      bool
	Masquerade       bool
	P2PAddr          string
	StaticRoutes     []string
	TransitNet       string
}

type distributedRouter struct {
	networks     map[string]*network
	defaultRoute net.IP
}

func newDistributedRouter(options *DistributedRouterOptions) (*distributedRouter, error) {
	var err error
	//set global vars from options
	ipOffset = options.IPOffset
	aggressive = options.Aggressive
	hostShortcut = options.HostShortcut
	containerGateway = options.ContainerGateway
	hostGateway = options.HostGateway
	masquerade = options.Masquerade
	//staticRoutes
	for _, sr := range options.StaticRoutes {
		_, cidr, err := net.ParseCIDR(sr)
		if err != nil {
			log.Errorf("Failed to parse static route: %v", sr)
			continue
		}
		staticRoutes = append(staticRoutes, cidr)
	}

	transitNetName = options.TransitNet

	if !aggressive && len(transitNetName) == 0 {
		log.Warn("Detected --no-aggressive and --transit-net was not found. This router may not be able to route to networks on other hosts")
	}

	//get the pid of drouter
	pid := os.Getpid()
	if pid == 1 {
		return &distributedRouter{}, fmt.Errorf("Running as pid 1. Running with --pid=host required.")
	}

	//get docker client
	defaultHeaders := map[string]string{"User-Agent": "engine-api-cli-1.0"}
	dockerClient, err = dockerclient.NewClient("unix:///var/run/docker.sock", "v1.23", nil, defaultHeaders)
	if err != nil {
		log.Error("Error connecting to docker socket")
		return &distributedRouter{}, err
	}

	//process options for assumptions and validity
	if masquerade {
		log.Debug("Detected --masquerade. Assuming --host-gateway and --container-gateway and --host-shortcut.")
		hostShortcut = true
		containerGateway = true
		hostGateway = true
	} else {
		if hostGateway {
			log.Debug("Detected --host-gateway. Assuming --host-shortcut.")
			hostShortcut = true
		}
	}

	sc, err := getSelfContainer()
	if err != nil {
		log.Error("Failed to getSelfContainer(). I am running in a container, right?.")
		return nil, err
	}
	selfContainerID = sc.ID

	//disconnect from all initial networks
	log.Debug("Leaving all connected currently networks.")
	for name, settings := range sc.NetworkSettings.Networks {
		log.Debugf("Leaving network: %v", name)
		err = dockerClient.NetworkDisconnect(context.Background(), settings.NetworkID, selfContainerID, true)
		if err != nil {
			log.Error(err)
			continue
		}
	}

	//create our distributedRouter object
	dr := &distributedRouter{
		networks: make(map[string]*network),
	}

	//initial setup
	if hostShortcut {
		var err error
		log.Debug("--host-shortcut detected, making P2P link.")
		p2p, err = newP2PNetwork(options.P2PAddr)
		if err != nil {
			log.Error("Failed to create the point to point link.")
			return nil, err
		}

		if hostGateway {
			dr.defaultRoute = p2p.hostIP
			err = dr.setDefaultRoute()
			if err != nil {
				log.Error("--host-gateway=true and unable to set default route to host's p2p address.")
				return nil, err
			}
			if masquerade {
				log.Debug("--masquerade detected, inserting masquerade rule.")
				if err := insertMasqRule(); err != nil {
					log.Error("Failed to insertMasqRule().")
					return nil, err
				}
			}
		}
	}

	log.Debug("Returning our new distributedRouter instance.")
	return dr, nil
}

//Run creates and runs a drouter instance
func Run(opts *DistributedRouterOptions, shutdown <-chan struct{}) error {
	stopChan = shutdown
	dr, err := newDistributedRouter(opts)
	if err != nil {
		return err
	}

	if len(transitNetName) > 0 {
		err = dr.initTransitNet()
		if err != nil {
			return err
		}
	}

	learnNetwork := make(chan *network)
	if aggressive {
		//syncNetworks()
		go func() {
			//we want to sync immediatly the first time
			timer := time.NewTimer(0)
			for {
				select {
				case _ = <-shutdown:
					timer.Stop()
					return
				case _ = <-timer.C:
					log.Debug("Syncing networks from docker.")

					//get all networks from docker
					dockerNets, err := dockerClient.NetworkList(context.Background(), dockertypes.NetworkListOptions{Filters: dockerfilters.NewArgs()})
					if err != nil {
						log.Error("Error getting network list")
						log.Error(err)
					}

					//learn the docker networks
					for _, dn := range dockerNets {
						if dn.ID == transitNetID {
							continue
						}

						//do we know about this network already?
						drn, ok := dr.networks[dn.ID]
						if !ok {
							drn = newNetwork(&dn)
							learnNetwork <- drn
						}

						if !drn.isConnected() && drn.isDRouter() && !drn.adminDown {
							connectWG.Add(1)
							go func() {
								defer connectWG.Done()
								drn.connect()
							}()
						}
					}
					connectWG.Wait()
					//use timer instead of ticker to guarantee a 5 second delay from end to start
					timer = time.NewTimer(5 * time.Second)
				}
			}
		}()
	}

	dockerEvent := make(chan dockerevents.Message)
	defer close(dockerEvent)
	dockerEventErr := events.Monitor(context.Background(), dockerClient, dockertypes.EventsOptions{}, func(event dockerevents.Message) {
		dockerEvent <- event
		return
	})

	routeEventDone := make(chan struct{})
	defer close(routeEventDone)
	routeEvent := make(chan netlink.RouteUpdate)
	defer close(routeEvent)
	err = netlink.RouteSubscribe(routeEvent, routeEventDone)
	if err != nil {
		log.Error("Failed to subscribe to drouter routing table.")
		return err
	}

Main:
	for {
		select {
		case drn := <-learnNetwork:
			_, ok := dr.networks[drn.ID]
			if !ok {
				dr.networks[drn.ID] = drn
			}
		case e := <-dockerEvent:
			err = dr.processDockerEvent(e, learnNetwork)
			if err != nil {
				log.Error(err)
			}
		case r := <-routeEvent:
			if r.Type != syscall.RTM_NEWROUTE {
				break
			}
			err = dr.processRouteAddEvent(&r)
			if err != nil {
				log.Error(err)
			}
		case e := <-dockerEventErr:
			log.Error(e)
		case _ = <-shutdown:
			break Main
		}
	}

	log.Info("Cleaning Up")
	connectWG.Wait()

	var disconnectWG sync.WaitGroup
	//leave all connected networks
	for _, drn := range dr.networks {
		if !drn.isConnected() {
			continue
		}
		disconnectWG.Add(1)
		go func(drn *network) {
			defer disconnectWG.Done()
			drn.disconnect()
		}(drn)
	}

	disconnectWait := make(chan struct{})
	go func() {
		defer close(disconnectWait)
		disconnectWG.Wait()
	}()

Done:
	for {
		select {
		case _ = <-disconnectWait:
			log.Debug("Finished all network disconnects.")
			break Done
		}
	}

	//delete the p2p link
	if hostShortcut {
		err := p2p.remove()
		if err != nil {
			return err
		}
	}

	return nil
}

//sets the default route to distributedRouter.defaultRoute
func (dr *distributedRouter) setDefaultRoute() error {
	if dr.defaultRoute.Equal(net.IP{}) {
		return nil
	}

	//remove all incorrect default routes
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, r := range routes {
		if r.Dst != nil {
			continue
		}

		//test that inteded default is already present, don't remove if so
		if r.Gw.Equal(dr.defaultRoute) {
			return nil
		}

		log.Debugf("Remove default route thru: %v", r.Gw)
		err = netlink.RouteDel(&r)
		if err != nil {
			return err
		}
	}

	gwPaths, err := netlink.RouteGet(dr.defaultRoute)
	if err != nil {
		return err
	}

	for _, r := range gwPaths {
		if r.Gw != nil {
			continue
		}
		return netlink.RouteAdd(&netlink.Route{
			LinkIndex: r.LinkIndex,
			Gw:        dr.defaultRoute,
		})
	}
	return nil
}

// Watch for container events
func (dr *distributedRouter) processDockerEvent(event dockerevents.Message, learn chan *network) error {
	// we currently only care about network events
	if event.Type != "network" {
		return nil
	}

	// have we learned this network?
	drn, ok := dr.networks[event.Actor.ID]
	if !ok {
		//inspect network
		nr, err := dockerClient.NetworkInspect(context.Background(), event.Actor.ID)
		if err != nil {
			log.Errorf("Failed to inspect network at: %v", event.Actor.ID)
			return err
		}
		drn = newNetwork(&nr)

		learn <- drn
	}

	//we dont' manage this network
	if !drn.isDRouter() {
		return nil
	}

	if event.Actor.Attributes["container"] == selfContainerID {
		switch event.Action {
		case "connect":
			return drn.connectEvent()
		case "disconnect":
			return drn.disconnectEvent()
		default:
			return nil
		}
	}

	c, err := newContainerFromID(event.Actor.Attributes["container"])
	if err != nil {
		return err
	}

	switch event.Action {
	case "connect":
		return c.connectEvent(drn)
	case "disconnect":
		return c.disconnectEvent(drn)
	default:
		//we don't handle whatever action this is (yet?)
		return nil
	}
}

func (dr *distributedRouter) processRouteAddEvent(ru *netlink.RouteUpdate) error {
	if ru.Table == 255 {
		// We don't want entries from the local routing table
		// http://linux-ip.net/html/routing-tables.html
		return nil
	}
	if ru.Src.IsLoopback() {
		return nil
	}
	if ru.Dst == nil {
		//we manage default routes separately
		//TODO: Add logic to fix default route since it was changed by a drn.connect()
		return nil
	}
	if ru.Dst.IP.IsLoopback() {
		return nil
	}
	if ru.Src.IsLinkLocalUnicast() {
		return nil
	}
	if ru.Dst.IP.IsLinkLocalUnicast() {
		return nil
	}
	if ru.Dst.IP.IsInterfaceLocalMulticast() {
		return nil
	}
	if ru.Dst.IP.IsLinkLocalMulticast() {
		return nil
	}

	coveredByStatic := subnetCoveredByStatic(ru.Dst)

	if hostShortcut && !coveredByStatic {
		p2p.addHostRoute(ru.Dst)
	}

	modifyRoute(ru.Dst, nil, ADD_ROUTE)

	return nil
}

func (dr *distributedRouter) initTransitNet() error {
	nr, err := dockerClient.NetworkInspect(context.Background(), transitNetName)
	if err != nil {
		log.Errorf("Failed to inspect network for transit net: %v", transitNetName)
		return err
	}

	dr.networks[nr.ID] = newNetwork(&nr)

	transitNetID = nr.ID

	//if transit net has a gateway, make it drouter's default route
	if len(nr.Options["gateway"]) > 0 && !hostGateway {
		dr.defaultRoute = net.ParseIP(nr.Options["gateway"])
		log.Debugf("Gateway option detected on transit net as: %v", dr.defaultRoute)
	}
	dr.networks[nr.ID].connect()
	if err != nil {
		log.Errorf("Failed to connect to transit net: %v", nr.Name)
		return err
	}

	return nil
}
