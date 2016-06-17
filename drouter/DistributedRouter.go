package drouter

import (
	"bufio"
	"fmt"
	log "github.com/Sirupsen/logrus"
	dockerclient "github.com/docker/engine-api/client"
	dockertypes "github.com/docker/engine-api/types"
	dockerevents "github.com/docker/engine-api/types/events"
	"github.com/vdemeester/docker-events"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/net/context"
	"net"
	"os"
	"strings"
	"sync"
	"time"
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

type DistributedRouter struct {
	dc              *dockerclient.Client
	selfContainerID string
	networks        map[string]*drNetwork
	networksLock    *sync.RWMutex
	selfNamespace   *netlink.Handle
	hostNamespace   *netlink.Handle
	hostUnderlay    *net.IPNet
	defaultRoute    net.IP
	pid             int
	p2p             p2pNetwork
	staticRoutes    []*net.IPNet
	networkTimer    *time.Timer
	ipOffset        int
	aggressive      bool
	localShortcut   bool
	localGateway    bool
	masquerade      bool
	p2pNet          string
	transitNet      string
	transitNetID    string
}

func NewDistributedRouter(options *DistributedRouterOptions) (*DistributedRouter, error) {
	var err error

	if !options.Aggressive && len(options.TransitNet) == 0 {
		return &DistributedRouter{}, fmt.Errorf("--no-aggressive, and --transit-net was not found.")
	}

	//get the pid of drouter
	pid := os.Getpid()
	if pid == 1 {
		return &DistributedRouter{}, fmt.Errorf("Running as pid 1. Running with --pid=host required.")
	}

	//get docker client
	defaultHeaders := map[string]string{"User-Agent": "engine-api-cli-1.0"}
	docker, err := dockerclient.NewClient("unix:///var/run/docker.sock", "v1.23", nil, defaultHeaders)
	if err != nil {
		log.Error("Error connecting to docker socket")
		return &DistributedRouter{}, err
	}

	//get self namespace handle
	sns, err := netlinkHandleFromPid(pid)
	if err != nil {
		log.Error("Unable to get self namespace handle.")
		return &DistributedRouter{}, err
	}

	//get host namespace
	hns, err := netlinkHandleFromPid(1)
	if err != nil {
		log.Error("Unable to get host namespace handle. Is priveleged mode enabled?")
		return &DistributedRouter{}, err
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

	sc, err := getSelfContainer(docker)
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

	//create our DistributedRouter object
	dr := &DistributedRouter{
		dc:              docker,
		networks:        make(map[string]*drNetwork),
		selfContainerID: sc.ID,
		selfNamespace:   sns,
		hostNamespace:   hns,
		pid:             pid,
		ipOffset:        options.IpOffset,
		aggressive:      options.Aggressive,
		localShortcut:   lsc,
		localGateway:    lgw,
		masquerade:      options.Masquerade,
		staticRoutes:    sroutes,
		transitNet:      options.TransitNet,
	}

	//initial setup
	if dr.localShortcut {
		log.Debug("--local-shortcut detected, making P2P link.")
		if err := dr.makeP2PLink(options.P2pNet); err != nil {
			log.Error("Failed to makeP2PLink().")
			return nil, err
		}

		if !dr.localGateway {
			if dr.masquerade {
				log.Debug("--masquerade detected, inserting masquerade rule.")
				if err := insertMasqRule(); err != nil {
					log.Error("Failed to insertMasqRule().")
					return nil, err
				}
			}
		}
	}

	log.Debug("Returning our new DistributedRouter instance.")
	return dr, nil
}

func (dr *DistributedRouter) Start() error {
	log.Info("Initialization complete, Starting the router.")

	//ensure periodic re-sync if running aggressive mode
	if dr.aggressive {
		log.Info("Aggressive mode enabled, starting networkTimer.")
		go func() {
			for {
				//TODO: make syncNetwork delay a variable option
				//using timers instead of sleep allows us to stop syncing during shutdown
				//using timers instead of tickers allows us to ensure that multiple syncNetworks() never overlap
				err := dr.syncNetworks()
				if err != nil {
					log.Error(err)
				}
				dr.networkTimer = time.NewTimer(time.Second * 5)
				<-dr.networkTimer.C
			}
		}()
	} else {
		//TODO: non-aggressive stuff
		//loop over all local containers and connect to those nets
	}

	eventChan := make(chan error)
	go func() {
		eventChan <- dr.watchEvents()
		close(eventChan)
	}()

	if len(dr.transitNet) > 0 {
		err := dr.initTransitNet(dr.transitNet)
		if err != nil {
			return err
		}
	}

	err := <-eventChan
	if err != nil {
		log.Error("watchEvents() exited")
		return err
	}
	return nil
}

func (dr *DistributedRouter) Close() error {
	log.Info("Cleaning Up")

	if dr.aggressive {
		log.Debug("Stopping networkTimer.")
		//stop the networkTimer
		dr.networkTimer.Stop()
	}

	//leave all connected networks
	discs := make([]string, 0)
	dr.networksLock.RLock()
	for id, drn := range dr.networks {
		if drn.connected {
			discs = append(discs, id)
		}
	}
	dr.networksLock.RUnlock()
	for _, id := range discs {
		err := dr.disconnectNetwork(id)
		if err != nil {
			dr.networksLock.RLock()
			log.Errorf("Failed to leave network: %v", dr.networks[id].name)
			dr.networksLock.RUnlock()
			log.Error(err)
			continue
		}
	}

	//removing the p2p network cleans up the host routes automatically
	if dr.localShortcut {
		err := dr.removeP2PLink()
		if err != nil {
			return err
		}
	}

	return nil
}

//sets the default route to DistributedRouter.defaultRoute
func (dr *DistributedRouter) setDefaultRoute() error {
	//remove all incorrect default routes
	routes, err := dr.selfNamespace.RouteList(nil, netlink.FAMILY_V4)
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
			err = dr.selfNamespace.RouteDel(&r)
			if err != nil {
				return err
			}
		}
	}

	//add intended default route, if it's not set and necessary
	if !defaultSet && !dr.defaultRoute.Equal(net.IP{}) {
		r, err := dr.selfNamespace.RouteGet(dr.defaultRoute)
		if err != nil {
			return err
		}

		nr := &netlink.Route{
			LinkIndex: r[0].LinkIndex,
			Gw:        dr.defaultRoute,
		}

		err = dr.selfNamespace.RouteAdd(nr)
		if err != nil {
			return err
		}
	}

	return nil
}

// Watch for container events
func (dr *DistributedRouter) watchEvents() error {
	log.Debug("Watching for container events.")
	errChan := events.Monitor(context.Background(), dr.dc, dockertypes.EventsOptions{}, func(event dockerevents.Message) {
		// we currently only care about network events
		if event.Type != "network" {
			return
		}

		// don't run on self events
		//TODO: maybe add some logic for administrative connects/disconnects of self
		//if event.Actor.Attributes["container"] == dr.selfContainerID { return }

		// have we learned this network?
		dr.networksLock.RLock()
		_, ok := dr.networks[event.Actor.ID]
		dr.networksLock.RUnlock()

		if !ok {
			//inspect network
			nr, err := dr.dc.NetworkInspect(context.Background(), event.Actor.ID)
			if err != nil {
				log.Errorf("Failed to inspect network at: %v", event.Actor.ID)
				log.Error(err)
				return
			}
			//learn network
			dr.networksLock.Lock()
			dr.networks[event.Actor.ID], err = newDRNetwork(&nr)
			dr.networksLock.Unlock()
			if err != nil {
				log.Error("Failed create drNetwork after a container connected to it.")
				log.Error(err)
				return
			}
		}

		//we dont' manage this network, ignore
		if !dr.networkIsDRouter(event.Actor.ID) {
			return
		}

		//log.Debugf("Event.Actor: %v", event.Actor)

		switch event.Action {
		case "connect":
			if event.Actor.Attributes["container"] == dr.selfContainerID {
				err := dr.selfNetworkConnectEvent(event.Actor.ID)
				if err != nil {
					log.Error(err)
				}
				return
			}

			err := dr.containerNetworkConnectEvent(event.Actor.Attributes["container"], event.Actor.ID)
			if err != nil {
				log.Error(err)
				return
			}
		case "disconnect":
			if event.Actor.Attributes["container"] == dr.selfContainerID {
				err := dr.selfNetworkDisconnectEvent(event.Actor.ID)
				if err != nil {
					log.Error(err)
				}
				return
			}

			err := dr.containerNetworkDisconnectEvent(event.Actor.Attributes["container"], event.Actor.ID)
			if err != nil {
				log.Error(err)
				return
			}
		default:
			//we don't handle whatever action this is (yet?)
			return
		}
		return
	})

	if err := <-errChan; err != nil {
		return err
	}

	return nil
}

func netlinkHandleFromPid(pid int) (*netlink.Handle, error) {
	log.Debugf("Getting NsHandle for pid: %v", pid)
	ns, err := netns.GetFromPid(pid)
	if err != nil {
		return &netlink.Handle{}, err
	}
	nsh, err := netlink.NewHandleAt(ns)
	if err != nil {
		return &netlink.Handle{}, err
	}

	return nsh, nil
}

func insertMasqRule() error {
	//not implemented yet
	return nil
}

func NetworkID(n *net.IPNet) *net.IPNet {
	ip := n.IP.To4()
	if ip == nil {
		ip = n.IP
		ip2 := net.IP{
			ip[0] & n.Mask[0],
			ip[1] & n.Mask[1],
			ip[2] & n.Mask[2],
			ip[3] & n.Mask[3],
			ip[4] & n.Mask[4],
			ip[5] & n.Mask[5],
			ip[6] & n.Mask[6],
			ip[7] & n.Mask[7],
			ip[8] & n.Mask[8],
			ip[9] & n.Mask[9],
			ip[10] & n.Mask[10],
			ip[11] & n.Mask[11],
			ip[12] & n.Mask[12],
			ip[13] & n.Mask[13],
			ip[14] & n.Mask[14],
			ip[15] & n.Mask[15],
		}

		ipnet := &net.IPNet{
			IP:   ip2,
			Mask: n.Mask,
		}

		return ipnet
	}
	ip2 := net.IPv4(
		ip[0]&n.Mask[0],
		ip[1]&n.Mask[1],
		ip[2]&n.Mask[2],
		ip[3]&n.Mask[3],
	)

	ipnet := &net.IPNet{
		IP:   ip2,
		Mask: n.Mask,
	}

	return ipnet
}

func getSelfContainer(dc *dockerclient.Client) (*dockertypes.ContainerJSON, error) {
	log.Debug("Getting self containerJSON object.")

	cgroup, err := os.Open("/proc/self/cgroup")
	if err != nil {
		log.Error("Error getting cgroups.")
		return nil, err
	}
	defer cgroup.Close()

	scanner := bufio.NewScanner(cgroup)
	for scanner.Scan() {
		line := strings.Split(scanner.Text(), "/")
		id := line[len(line)-1]
		containerInfo, err := dc.ContainerInspect(context.Background(), id)
		if err != nil {
			log.Errorf("Error inspecting container: %v", id)
			return nil, err
		}
		return &containerInfo, nil
	}
	return nil, fmt.Errorf("Container not found")
}
