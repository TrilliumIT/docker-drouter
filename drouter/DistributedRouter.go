package drouter

import (
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	log "github.com/Sirupsen/logrus"
	dockerclient "github.com/docker/engine-api/client"
	dockertypes "github.com/docker/engine-api/types"
	dockerevents "github.com/docker/engine-api/types/events"
	dockerfilters "github.com/docker/engine-api/types/filters"
	"github.com/vdemeester/docker-events"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/context"
)

const (
	hostNetworkMode = "host"
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
	instanceName     string

	//other globals
	dockerClient *dockerclient.Client
	transitNetID string
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
	InstanceName     string
}

type distributedRouter struct {
	networks     map[string]*network
	networksLock sync.RWMutex
	defaultRoute net.IP
}

func initVars(options *DistributedRouterOptions) error {
	var err error

	//set global vars from options
	ipOffset = options.IPOffset
	aggressive = options.Aggressive
	hostShortcut = options.HostShortcut
	containerGateway = options.ContainerGateway
	hostGateway = options.HostGateway
	masquerade = options.Masquerade
	instanceName = options.InstanceName
	transitNetName = options.TransitNet

	if !aggressive && len(transitNetName) == 0 {
		log.Warn("Detected --no-aggressive and --transit-net was not found. This router may not be able to route to networks on other hosts")
	}

	//get the pid of drouter
	if os.Getpid() == 1 {
		return fmt.Errorf("Running as pid 1. Running with --pid=host required.")
	}

	//staticRoutes
	for _, sr := range options.StaticRoutes {
		var cidr *net.IPNet
		_, cidr, err = net.ParseCIDR(sr)
		if err != nil {
			log.WithFields(log.Fields{
				"Subnet": sr,
				"Error":  err,
			}).Error("Failed to parse static route subnet.")
			continue
		}
		staticRoutes = append(staticRoutes, cidr)
	}

	//get docker client
	defaultHeaders := map[string]string{"User-Agent": "engine-api-cli-1.0"}
	dockerClient, err = dockerclient.NewClient("unix:///var/run/docker.sock", "v1.23", nil, defaultHeaders)
	if err != nil {
		logError("Error connecting to docker socket", err)
		return err
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
		return err
	}
	selfContainerID = sc.ID

	//disconnect from all initial networks
	log.Debug("Leaving all connected currently networks.")
	for name, settings := range sc.NetworkSettings.Networks {
		log.WithFields(log.Fields{
			"Network": name,
		}).Debug("Leaving network.")
		err = dockerClient.NetworkDisconnect(context.Background(), settings.NetworkID, selfContainerID, true)
		if err != nil {
			log.WithFields(log.Fields{
				"Network": name,
				"Error":   err,
			}).Error("Failed to leave network.")
			continue
		}
	}
	return nil

}

func newDistributedRouter(options *DistributedRouterOptions) (*distributedRouter, error) {

	err := initVars(options)
	if err != nil {
		return &distributedRouter{}, err
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

	return dr.start()
}

func syncNetworks(dr *distributedRouter, learnNetwork chan<- *network) {
	//we want to sync immediatly the first time
	timer := time.NewTimer(0)
	for {
		select {
		case <-stopChan:
			timer.Stop()
			return
		case <-timer.C:
			log.Debug("Syncing networks from docker.")

			//get all networks from docker
			dockerNets, err := dockerClient.NetworkList(context.Background(), dockertypes.NetworkListOptions{Filters: dockerfilters.NewArgs()})
			if err != nil {
				logError("Error getting network list", err)
			}

			//learn the docker networks
			for _, n := range dockerNets {
				dn := n
				go func() {
					if dn.ID == transitNetID {
						return
					}

					//do we know about this network already?
					drn, ok := dr.getNetwork(dn.ID)
					if !ok {
						log.Debugf("newNetwork(%v)", dn)
						learnNetwork <- newNetwork(&dn)
						return
					}
					if !drn.isConnected() && drn.isDRouter() && !drn.adminDown {
						drn.connect()
					}
				}()
			}
			//use timer instead of ticker to guarantee a 5 second delay from end to start
			timer = time.NewTimer(5 * time.Second)
		}
	}
}

func genContainerStartupEvents(dockerEvent chan<- dockerevents.Message) error {
	// on startup generate connect events for every container
	dockerContainers, err := dockerClient.ContainerList(context.Background(), dockertypes.ContainerListOptions{})
	if err != nil {
		logError("Error getting container list", err)
		return err
	}
	for _, dcont := range dockerContainers {
		dc := dcont
		go func() {
			if dc.HostConfig.NetworkMode == hostNetworkMode {
				return
			}
			if dc.ID == selfContainerID {
				return
			}
			ess := dc.NetworkSettings.Networks
			for _, es := range ess {
				if es.NetworkID == "" {
					var cjson dockertypes.ContainerJSON
					cjson, err = dockerClient.ContainerInspect(context.Background(), dc.ID)
					if err != nil {
						log.Errorf("Error inspecting container %v with id %v.", dc.Names, dc.ID)
						continue
					}
					ess = cjson.NetworkSettings.Networks
				}
				break
			}
			for _, e := range ess {
				es := e
				go func() {
					dockerEvent <- dockerevents.Message{
						Type:   "network",
						Action: "connect",
						Actor: dockerevents.Actor{
							ID: es.NetworkID,
							Attributes: map[string]string{
								"container": dc.ID,
							},
						},
					}
				}()
			}
		}()
	}
	return nil
}

func (dr *distributedRouter) mainLoop(learnNetwork chan *network,
	dockerEvent <-chan dockerevents.Message,
	routeEvent <-chan netlink.RouteUpdate,
	dockerEventErr <-chan error,
	stopChan <-chan struct{},
	eventWG *sync.WaitGroup) {
	for {
		select {
		case drn := <-learnNetwork:
			go func() {
				_, ok := dr.getNetwork(drn.ID)
				if !ok {
					dr.networksLock.Lock()
					defer dr.networksLock.Unlock()
					dr.networks[drn.ID] = drn
					go func() {
						if !drn.isConnected() && drn.isDRouter() && !drn.adminDown {
							drn.connect()
						}
					}()
				}
			}()
		case e := <-dockerEvent:
			eventWG.Add(1)
			go func() {
				defer eventWG.Done()
				err := dr.processDockerEvent(e, learnNetwork)
				if err != nil {
					log.WithFields(log.Fields{
						"Event": e,
						"Error": err,
					}).Error("Failed to process docker event")
				}
			}()
		case r := <-routeEvent:
			eventWG.Add(1)
			go func() {
				defer eventWG.Done()
				if r.Type != syscall.RTM_NEWROUTE {
					return
				}
				err := dr.processRouteAddEvent(&r)
				if err != nil {
					log.WithFields(log.Fields{
						"RouteEvent": r,
						"Error":      err,
					}).Error("Failed to process route add event")
				}
			}()
		case e := <-dockerEventErr:
			logError("Error recieved from Docker Event Subscription.", e)
		case <-stopChan:
			log.Debug("Shutdown request recieved")
			return
		}
	}
}

func (dr *distributedRouter) start() error {
	var err error

	if len(transitNetName) > 0 {
		err = dr.initTransitNet()
		if err != nil {
			return err
		}
	}

	learnNetwork := make(chan *network)
	if aggressive {
		go syncNetworks(dr, learnNetwork)
	}

	dockerEvent := make(chan dockerevents.Message)
	defer close(dockerEvent)
	if !aggressive {
		err = genContainerStartupEvents(dockerEvent)
		if err != nil {
			log.WithField("Error", err).Error("Failed to generate startup events for container")
			return err
		}
	}

	// subscribe to real docker events
	ctx, ctxDone := context.WithCancel(context.Background())
	dockerEventErr := events.Monitor(ctx, dockerClient, dockertypes.EventsOptions{}, func(event dockerevents.Message) {
		dockerEvent <- event
		return
	})

	routeEventDone := make(chan struct{})
	defer close(routeEventDone)
	routeEvent := make(chan netlink.RouteUpdate)
	err = netlink.RouteSubscribe(routeEvent, routeEventDone)
	if err != nil {
		logError("Failed to subscribe to drouter routing table.", err)
		return err
	}

	var eventWG sync.WaitGroup

	dr.mainLoop(learnNetwork,
		dockerEvent,
		routeEvent,
		dockerEventErr,
		stopChan,
		&eventWG)

	log.Info("Cleaning Up")
	eventWG.Wait()
	ctxDone()

	var disconnectWG sync.WaitGroup
	//leave all connected networks
	for _, drn := range dr.networks {
		disconnectWG.Add(1)
		go func(drn *network) {
			defer disconnectWG.Done()
			// checking isConnected will automatically wait for any pending connections
			if !drn.isConnected() {
				return
			}
			drn.disconnect()
		}(drn)
	}

	disconnectWG.Wait()
	log.Debug("Finished all network disconnects.")

	//delete the p2p link
	if hostShortcut {
		err := p2p.remove()
		if err != nil {
			logError("Failed to remove p2p link.", err)
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
func (dr *distributedRouter) processDockerEvent(event dockerevents.Message, learn chan<- *network) error {
	// we currently only care about network events
	if event.Type != "network" {
		return nil
	}

	cid, ok := event.Actor.Attributes["container"]
	if !ok {
		//we don't need to go any further because this event does not involve a container
		return nil
	}

	// have we learned this network?
	drn, ok := dr.getNetwork(event.Actor.ID)
	if !ok {
		//inspect network
		nr, err := dockerClient.NetworkInspect(context.Background(), event.Actor.ID)
		if err != nil {
			log.Errorf("Failed to inspect network at: %v", event.Actor.ID)
			return err
		}
		learn <- newNetwork(&nr)
		//return now since the learn handles connecting, which will handle adding routes
		return nil
	}

	//we dont' manage this network
	if !drn.isDRouter() {
		return nil
	}

	if cid == selfContainerID {
		switch event.Action {
		case "connect":
			return drn.connectEvent()
		case "disconnect":
			return drn.disconnectEvent()
		default:
			//we don't handle whatever action this is
			return nil
		}
	}

	c, err := newContainerFromID(cid)
	if err != nil {
		return err
	}

	switch event.Action {
	case "connect":
		return c.connectEvent(drn)
	case "disconnect":
		return c.disconnectEvent(drn)
	default:
		//we don't handle whatever action this is
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

	return modifyRoute(ru.Dst, addRoute)
}

func (dr *distributedRouter) getNetwork(id string) (*network, bool) {
	dr.networksLock.RLock()
	defer dr.networksLock.RUnlock()
	drn, ok := dr.networks[id]
	return drn, ok
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
