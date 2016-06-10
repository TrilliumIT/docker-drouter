package drouter

import (
	"os"
	"errors"
	"bufio"
	"strings"
	"net"
	"time"
	log "github.com/Sirupsen/logrus"
	dockerclient "github.com/docker/engine-api/client"
	dockertypes "github.com/docker/engine-api/types"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/net/context"
)

type DistributedRouterOptions struct {
	IpOffset              int
	Aggressive            bool
	LocalShortcut         bool
	LocalGateway          bool
	Masquerade            bool
	P2pNet                string
	StaticRoutes          []string
	TransitNet            string
}

type DistributedRouter struct {
	dc                    *dockerclient.Client
	selfContainer         dockertypes.ContainerJSON
	networks              map[string]*drNetwork
	selfNamespace         *netlink.Handle
	hostNamespace         *netlink.Handle
	defaultRoute          net.IP
	pid                   int
	p2p                   p2pNetwork
	staticRoutes          []*net.IPNet
	networkTimer          *time.Timer
	ipOffset              int
	aggressive            bool
	localShortcut         bool
	localGateway          bool
	masquerade            bool
	p2pNet                string
	transitNet            string
	transitNetID          string
}

func NewDistributedRouter(options *DistributedRouterOptions) (*DistributedRouter, error) {
	var err error

	if len(options.TransitNet) == 0 && !options.Aggressive {
		return &DistributedRouter{}, errors.New("--aggressive=false, and no --transit-net was found.")
	}

	//get the pid of drouter
	pid := os.Getpid()
	if pid == 1 {
		return &DistributedRouter{}, errors.New("Running as pid 1. Running with --pid=host required.")
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

	//create our DistributedRouter object
	dr := &DistributedRouter{
		dc: docker,
		selfNamespace: sns,
		hostNamespace: hns,
		pid: pid,
		ipOffset: options.IpOffset,
		aggressive: options.Aggressive,
		localShortcut: lsc,
		localGateway: lgw,
		masquerade: options.Masquerade,
		staticRoutes: sroutes,
	}
	
	err = dr.updateSelfContainer()
	if err != nil {
		log.Error("Failed to updateSelfContainer() in NewDistributedRouter().")
		return nil, err
	}


	dr.networks = make(map[string]*drNetwork)
	log.Debug("Leaving all connected currently networks.")
	for _, settings := range dr.selfContainer.NetworkSettings.Networks {
		err := dr.drNetworkDisconnect(settings.NetworkID)
		if err != nil {
			log.Error(err)
			continue
		}
	}

	//initial setup
	if dr.localShortcut {
		log.Debug("--local-shortcut detected, making P2P link.")
		if err := dr.makeP2PLink(options.P2pNet); err != nil { 
			log.Error("Failed to makeP2PLink().")
			return nil, err
		}

		//add p2p prefix to static routes
		dr.staticRoutes = append(dr.staticRoutes, dr.p2p.network)
		if dr.masquerade {
			log.Debug("--masquerade detected, inserting masquerade rule.")
			if err := insertMasqRule(); err != nil { 
				log.Error("Failed to insertMasqRule().")
				return nil, err
			}
		}
	}

	log.Debug("Created new DistributedRouter, returning to main.")
	return dr, nil
}

func (dr *DistributedRouter) Start() {
	log.Info("Initialization complete, Starting the router.")

	//sync of docker networks to connect initial routes and networks
	err := dr.syncNetworks()
	if err != nil {
		log.Error("Failed to do initial network sync.")
	}
	
	//ensure periodic re-sync if running aggressive mode
	if dr.aggressive {
		log.Info("Aggressive mode enabled, starting networkTimer.")
		go func() {
			for {
				//TODO: make syncNetwork delay a variable
				//using timers instead of sleep allows us to stop syncing during shutdown
				//using timers instead of tickers allows us to ensure that multiple syncNetworks() never overlap
				dr.networkTimer = time.NewTimer(time.Second * 5)
				<-dr.networkTimer.C
				err := dr.syncNetworks()
				if err != nil {
					log.Error(err)
				}
			}
		}()
	}

	err = dr.watchEvents()
	if err != nil {
		log.Error(err)
		log.Error("watchEvents() exited")
	}
}

func (dr *DistributedRouter) Close() error {
	log.Info("Cleaning Up")

	if dr.aggressive {
		log.Debug("Stopping networkTimer.")
		//stop the networkTimer
		dr.networkTimer.Stop()
	}

	//leave all networks
	for id, _ := range dr.networks {
		err := dr.drNetworkDisconnect(id)
		if err != nil {
			log.Error(err)
			continue
		}
		delete(dr.networks, id)
	}

	if dr.localGateway {
		//TODO: do something to replace container gateways
	} else {
		//sync all routes to scrub all routing tables
		err := dr.syncAllRoutes()
		if err != nil {
			log.Error(err)
		}
	}
	
	//removing the p2p network should clean up the host routes
	if dr.localShortcut {
		err := dr.removeP2PLink()
		if err != nil {
			return err
		}
	}

	return nil
}

func (dr *DistributedRouter) updateSelfContainer() error {
	log.Debug("Updating my containerJSON object.")

	cgroup, err := os.Open("/proc/self/cgroup")
	if err != nil {
		log.Error("Error getting cgroups.")
		return err
	}
	defer cgroup.Close()

	scanner := bufio.NewScanner(cgroup)
	for scanner.Scan() {
		line := strings.Split(scanner.Text(), "/")
		id := line[len(line) - 1]
		containerInfo, err := dr.dc.ContainerInspect(context.Background(), id)
		if err != nil {
			log.Errorf("Error inspecting container: %v", id)
			return err
		}
		dr.selfContainer = containerInfo
		return nil
	}
	return errors.New("Container not found")
}

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
			Gw: dr.defaultRoute,
		}

		err = dr.selfNamespace.RouteAdd(nr)
		if err != nil {
			return err
		}
	}

	return nil
}

func netlinkHandleFromPid(pid int) (*netlink.Handle, error) {
		log.Debugf("Getting NsHandle for pid: %v", pid)
		ns, err := netns.GetFromPid(pid)
		if err != nil { return &netlink.Handle{}, err }
		nsh, err := netlink.NewHandleAt(ns)
		if err != nil { return &netlink.Handle{}, err }

		return nsh, nil
}

func insertMasqRule() error {
	//not implemented yet
	return nil
}
