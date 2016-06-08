package drouter

import (
	"os"
	"errors"
	"bufio"
	"strings"
	"net"
	"fmt"
	log "github.com/Sirupsen/logrus"
	dockerclient "github.com/docker/engine-api/client"
	dockertypes "github.com/docker/engine-api/types"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/net/context"
)

type DistributedRouter struct {
	DistributedRouterOptions
	dc                    *dockerclient.Client
	selfContainer         dockertypes.ContainerJSON
	networks              map[string]*drNetwork
	selfNamespace         *netlink.Handle
	hostNamespace         *netlink.Handle
	defaultRoute          net.IP
	pid                   int
	p2p                   p2pNetwork
	summaryNets           []net.IPNet
}

type DistributedRouterOptions struct {
	ipOffset              int
	aggressive            bool
	localShortcut         bool
	localGateway          bool
	masquerade            bool
	p2pNet                string
	summaryNets           []string
	transitNet            string
}

func NewDistributedRouter(options *DistributedRouterOptions) (*DistributedRouter, error) {
	var err error

	if len(options.transitNet) == 0 && !options.aggressive {
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
	lsc := options.localShortcut
	lgw := options.localGateway
	if options.masquerade {
		log.Debug("--masquerade=true. Assuming --local-gateway and --local-shortcut.")
		lsc = true
		lgw = true
	} else {
		if lgw {
			log.Debug("--local-gateway=true. Assuming --local-shortcut.")
			lsc = true
		}
	}

	//create our DistributedRouter object
	dr := &DistributedRouter{
		DistributedRouterOptions:DistributedRouterOptions{
			ipOffset: options.ipOffset,
			aggressive: options.aggressive,
			localShortcut: lsc,
			localGateway: lgw,
			masquerade: options.masquerade,
		},
		dc: docker,
		selfNamespace: sns,
		hostNamespace: hns,
		pid: pid,
	}
	
	err = dr.updateSelfContainer()
	if err != nil {
		return nil, err
	}

	//initial setup
	if dr.localShortcut {
		if err := dr.makeP2PLink(options.p2pNet); err != nil { return nil, err }
		if dr.masquerade {
			if err := insertMasqRule(); err != nil { return nil, err }
		}
	}

	//pre-populate networks slice with existing networks
	dr.networks = make(map[string]*drNetwork)
	for name, settings := range dr.selfContainer.NetworkSettings.Networks {
		ip, subnet, err := net.ParseCIDR(fmt.Sprintf("%v/%v", settings.IPAddress, settings.IPPrefixLen))
		if err != nil { 
			log.Errorf("Failed to parse CIDR: %v/%v", settings.IPAddress, settings.IPPrefixLen)
			return nil, err
		}
		
		gateway := net.ParseIP(dr.selfContainer.NetworkSettings.Networks[settings.NetworkID].Gateway)

		if name == options.transitNet {
			dr.transitNet = settings.NetworkID
			if len(settings.Gateway) > 0 && !dr.localGateway {
				dr.defaultRoute = net.ParseIP(settings.Gateway)
			}
		}
		
		dr.networks[settings.NetworkID] = &drNetwork{
			id: settings.NetworkID,
			connected: true,
			drouter: true,
			subnet: subnet,
			ip: ip,
			gateway: gateway,
		}

		//set up initial host routing table for routeShortcut
		if dr.localShortcut {
			route := &netlink.Route{
				LinkIndex: dr.p2p.hostLinkIndex,
				Gw: dr.p2p.selfIP,
				Dst: subnet,
			}
			err = dr.hostNamespace.RouteAdd(route)
			if err != nil {
				log.Error(err)
				continue
			}
		}
	}

	//init running containers
	err = dr.initContainers()
	if err != nil {
		log.Error(err)
	}

	return dr, nil
}

func (dr *DistributedRouter) Start() {
	//sync of docker networks to fixup initial routes and networks
	err := dr.syncNetworks()
	if err != nil {
		log.Error("Failed to do initial network sync.")
	}
	
	//ensure periodic re-sync if running aggressive mode
	if dr.aggressive {
		log.Info("Aggressive mode enabled")
		go dr.watchNetworks()
	}


	dr.watchEvents()
}

func (dr *DistributedRouter) Close() error {
	log.Info("Cleaning Up")
	err := dr.deinitContainers()
	if err != nil {
		return err
	}
	
	//removing the p2p network should clean up the host routes
	return dr.removeP2PLink()
}

func (dr *DistributedRouter) updateSelfContainer() error {
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
			log.Debugf("Remove default route: %v", r)
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
