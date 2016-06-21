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
	"syscall"
	"time"
)

var (
	selfContainerID string
	hostNamespace   *netlink.Handle
	ipOffset        int
	dockerClient    *dockerclient.Client
	aggressive      bool
	localShortcut   bool
	localGateway    bool
	masquerade      bool
	transitNetName  string
	transitNetID    string
	p2p             p2pNetwork
	staticRoutes    []*net.IPNet
)

type DistributedRouterOptions struct {
	IpOffset      int
	Aggressive    bool
	LocalShortcut bool
	LocalGateway  bool
	Masquerade    bool
	P2pNet        string
	StaticRoutes  []string
	TransitNet    string
}

type distributedRouter struct {
	networks     map[string]*network
	defaultRoute net.IP
	hostUnderlay *net.IPNet
}

func newDistributedRouter(options *DistributedRouterOptions) (*distributedRouter, error) {
	var err error

	if !options.Aggressive && len(options.TransitNet) == 0 {
		return &distributedRouter{}, fmt.Errorf("--no-aggressive, and --transit-net was not found.")
	}

	//get the pid of drouter
	pid := os.Getpid()
	if pid == 1 {
		return &distributedRouter{}, fmt.Errorf("Running as pid 1. Running with --pid=host required.")
	}

	//get docker client
	defaultHeaders := map[string]string{"User-Agent": "engine-api-cli-1.0"}
	docker, err := dockerclient.NewClient("unix:///var/run/docker.sock", "v1.23", nil, defaultHeaders)
	if err != nil {
		log.Error("Error connecting to docker socket")
		return &distributedRouter{}, err
	}

	//process options for assumptions and validity
	lsc := options.LocalShortcut
	lgw := options.LocalGateway
	if options.Masquerade {
		log.Debug("Detected --masquerade. Assuming --local-gateway and --local-shortcut.")
		lsc = true
		lgw = true
	} else {
		if lgw {
			log.Debug("Detected --local-gateway. Assuming --local-shortcut.")
			lsc = true
		}
	}

	sroutes := make([]*net.IPNet, 0)
	for _, sr := range options.StaticRoutes {
		_, cidr, err := net.ParseCIDR(sr)
		if err != nil {
			log.Errorf("Failed to parse static route: %v", sr)
			continue
		}
		sroutes = append(sroutes, cidr)
	}

	sc, err := getSelfContainer()
	if err != nil {
		log.Error("Failed to getSelfContainer(). I am running in a container, right?.")
		return nil, err
	}

	//disconnect from all initial networks
	log.Debug("Leaving all connected currently networks.")
	for _, settings := range sc.NetworkSettings.Networks {
		err = docker.NetworkDisconnect(context.Background(), settings.NetworkID, sc.ID, true)
		if err != nil {
			log.Error(err)
			continue
		}
	}

	//create our distributedRouter object
	dr := &distributedRouter{}

	//initial setup
	if localShortcut {
		log.Debug("--local-shortcut detected, making P2P link.")
		if err := makeP2PLink(options.P2pNet); err != nil {
			log.Error("Failed to makeP2PLink().")
			return nil, err
		}

		if localGateway {
			dr.defaultRoute = p2p.hostIP
			err = dr.setDefaultRoute()
			if err != nil {
				log.Error("--local-gateway=true and unable to set default route to host's p2p address.")
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

func Run(opts *DistributedRouterOptions, shutdown <-chan string) error {
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

	var aggressiveTimer <-chan time.Time

	if aggressive {
		aggressiveTimer = time.NewTimer(time.Second * 5).C
	}

	if aggressiveTimer == nil {
		aggressiveTimer = make(<-chan time.Time)
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
		case _ = <-aggressiveTimer:
			err = dr.syncNetworks()
			if err != nil {
				log.Error(err)
			}
		case e := <-dockerEvent:
			err = dr.processDockerEvent(e)
			if err != nil {
				log.Error(err)
			}
		case r := <-routeEvent:
			err = dr.processRouteEvent(&r)
			if err != nil {
				log.Error(err)
			}
		case e := <-dockerEventErr:
			log.Error(err)
		case _ = <-shutdown:
			break Main
		}
	}

	log.Info("Cleaning Up")

	//leave all connected networks
	for id, drn := range dr.networks {
		drn.disconnect()
	}

	//removing the p2p network cleans up the host routes automatically
	if localShortcut {
		err := removeP2PLink()
		if err != nil {
			return err
		}
	}

	return nil
}

//sets the default route to distributedRouter.defaultRoute
func (dr *distributedRouter) setDefaultRoute() error {
	//remove all incorrect default routes
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	defaultSet := false
	for _, r := range routes {
		if r.Dst != nil {
			continue
		}

		//test that inteded default is already present, don't remove if so
		if r.Gw.Equal(dr.defaultRoute) {
			defaultSet = true
			continue
		} else {
			log.Debugf("Remove default route thru: %v", r.Gw)
			err = netlink.RouteDel(&r)
			if err != nil {
				return err
			}
		}
	}

	//add intended default route, if it's not set and necessary
	if !defaultSet && !dr.defaultRoute.Equal(net.IP{}) {
		r, err := netlink.RouteGet(dr.defaultRoute)
		if err != nil {
			return err
		}

		nr := &netlink.Route{
			LinkIndex: r[0].LinkIndex,
			Gw:        dr.defaultRoute,
		}

		err = netlink.RouteAdd(nr)
		if err != nil {
			return err
		}
	}

	return nil
}

// Watch for container events
func (dr *distributedRouter) processDockerEvent(event dockerevents.Message) error {
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
		//learn network
		dr.networks[event.Actor.ID], err = newNetwork(&nr)
		if err != nil {
			log.Error("Failed create drNetwork after a container connected to it.")
			return err
		}
	}

	if event.Actor.Attributes["container"] == selfContainerID {
		switch event.Action {
		case "connect":
			return dr.selfNetworkConnectEvent(event.Actor.ID)
		case "disconnect":
			return dr.selfNetworkDisconnectEvent(event.Actor.ID)
		default:
			return nil
		}
	}

	c, err := newContainerFromID(event.Actor.Attributes["container"])
	if err != nil {
		return err
	}

	//we dont' manage this network, ignore
	if !drn.isDRouter() {
		return nil
	}

	//log.Debugf("Event.Actor: %v", event.Actor)

	switch event.Action {
	case "connect":
		return c.networkConnectEvent(event.Actor.ID)
	case "disconnect":
		return c.networkDisconnectEvent(event.Actor.ID)
	default:
		//we don't handle whatever action this is (yet?)
		return nil
	}
}

func (dr *distributedRouter) processRouteEvent(ru *netlink.RouteUpdate) error {
	if ru.Table == 255 {
		// We don't want entries from the local routing table
		// http://linux-ip.net/html/routing-tables.html
		return nil
	}
	if ru.Src.IsLoopback() {
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

	//skip if route is subnet of static route
	for _, sr := range staticRoutes {
		if SubnetContainsSubnet(sr, ru.Dst) {
			log.Debugf("Skipping route %v covered by %v.", ru.Dst, sr)
			return nil
		}
	}

	//do host routes
	if localShortcut {
		route := &netlink.Route{
			Gw:  p2p.selfIP,
			Dst: ru.Dst,
			Src: dr.hostUnderlay.IP,
		}
		if (route.Dst.IP.To4() == nil) != (route.Gw.To4() == nil) {
			// Dst is a different IP family
			return nil
		}
		switch ru.Type {
		case syscall.RTM_NEWROUTE:
			log.Debugf("Injecting shortcut route to %v via drouter into host routing table.", ru.Dst)
			err := hostNamespace.RouteAdd(route)
			if err != nil {
				log.Error(err)
			}
		case syscall.RTM_DELROUTE:
			log.Debugf("Removing shortcut route to %v via drouter from host routing table.", ru.Dst)
			err := hostNamespace.RouteDel(route)
			if err != nil {
				log.Error(err)
			}
		}
	}

	//do container routes
	dockerContainers, err := dockerClient.ContainerList(context.Background(), dockertypes.ContainerListOptions{})
	if err != nil {
		log.Error("Failed to get container list.")
		return err
	}

Containers:
	for _, dc := range dockerContainers {
		if dc.HostConfig.NetworkMode == "host" {
			continue
		}
		if dc.ID == selfContainerID {
			continue
		}
		for _, es := range dc.NetworkSettings.Networks {
			routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
			for _, r := range routes {
				if r.Gw != nil {
					continue
				}
			}
		}

		c, err := newContainerFromID(dc.ID)
		if err != nil {
			log.Error(err)
			continue
		}

		for _, es := range dc.NetworkSettings.Networks {
			ip := net.ParseIP(es.IPAddress)
			if ru.Dst.Contains(ip) {
				switch ru.Type {
				case syscall.RTM_NEWROUTE:
					go c.addAllRoutes()
					break Containers
				case syscall.RTM_DELROUTE:
					_, supernet, _ := net.ParseCIDR("0.0.0.0/0")
					go c.delRoutes(supernet)
					break Containers
				default:
					return fmt.Errorf("Unknown RouteUpdate.Type.")
				}
			}
		}

		switch ru.Type {
		case syscall.RTM_NEWROUTE:
			go c.addRoute(ru.Dst)
		case syscall.RTM_DELROUTE:
			go c.delRoutes(ru.Dst)
		default:
			return fmt.Errorf("Unknown RouteUpdate.Type.")
		}
	}
	return nil
}

func (dr *distributedRouter) selfNetworkConnectEvent(networkID string) error {
	dr.networks[networkID].adminDown = false

	return nil
}

func (dr *distributedRouter) selfNetworkDisconnectEvent(networkID string) error {
	if aggressive {
		dr.networks[networkID].adminDown = true
	}

	return nil
}

func (dr *distributedRouter) initTransitNet() error {
	nr, err := dockerClient.NetworkInspect(context.Background(), transitNetName)
	if err != nil {
		log.Error("Failed to inspect network for transit net: %v", transitNetName)
		return err
	}

	dr.networks[nr.ID], err = newNetwork(&nr)
	if err != nil {
		log.Error("Failed to learn the transit net: %v.", nr.Name)
		return err
	}

	transitNetID = nr.ID

	//if transit net has a gateway, make it drouter's default route
	if len(nr.Options["gateway"]) > 0 && !localGateway {
		dr.defaultRoute = net.ParseIP(nr.Options["gateway"])
		log.Debugf("Gateway option detected on transit net as: %v", dr.defaultRoute)
	}
	dr.networks[nr.ID].connect()
	if err != nil {
		log.Error("Failed to connect to transit net: %v", nr.Name)
		return err
	}

	return nil
}

//learns networks from docker and manages connections
func (dr *distributedRouter) syncNetworks() error {
	log.Debug("Syncing networks from docker.")

	//get all networks from docker
	dockerNets, err := dockerClient.NetworkList(context.Background(), dockertypes.NetworkListOptions{Filters: dockerfilters.NewArgs()})
	if err != nil {
		log.Error("Error getting network list")
		return err
	}

	//learn the docker networks
	for _, dn := range dockerNets {
		if dn.ID == transitNetID {
			continue
		}

		var err error
		//do we know about this network already?
		_, ok := dr.networks[dn.ID]

		if !ok {
			//no, create it
			dr.networks[dn.ID], err = newNetwork(&dn)
			if err != nil {
				return err
			}
		}
		drn := dr.networks[dn.ID]

		if drn.isConnected() {
			continue
		}

		if drn.isDRouter() && !drn.adminDown {
			go drn.connect()
		}
	}

	return nil
}
