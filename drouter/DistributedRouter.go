package drouter

import (
	"os"
	"fmt"
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
	selfContainerID       string
	networks              map[string]*drNetwork
	selfNamespace         *netlink.Handle
	hostNamespace         *netlink.Handle
	hostUnderlay          *net.IPNet
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
		return &DistributedRouter{}, fmt.Errorf("--aggressive=false, and no --transit-net was found.")
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

	//discover host underlay address/network
	hunderlay := &net.IPNet{}
	hroutes, err := hns.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	for _, r := range hroutes {
		if r.Gw == nil {
			link, err := hns.LinkByIndex(r.LinkIndex)
			if err != nil {
				return nil, err
			}
			addrs, err := hns.AddrList(link, netlink.FAMILY_V4)
			if err != nil {
				return nil, err
			}
			hunderlay.IP = addrs[0].IP
			hunderlay.Mask = addrs[0].Mask
			break
		}
	}
	log.Debugf("Discovered host underlay as: %v", hunderlay)

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
	sroutes = append(sroutes, NetworkID(hunderlay))

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
		dc: docker,
		networks: make(map[string]*drNetwork),
		selfContainerID: sc.ID,
		selfNamespace: sns,
		hostNamespace: hns,
		hostUnderlay: hunderlay,
		pid: pid,
		ipOffset: options.IpOffset,
		aggressive: options.Aggressive,
		localShortcut: lsc,
		localGateway: lgw,
		masquerade: options.Masquerade,
		staticRoutes: sroutes,
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

func (dr *DistributedRouter) Start() {
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

	err := dr.watchEvents()
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

	//fix container routing
	if dr.localGateway {
		//TODO: do something to replace container gateways
	} else {
		_, supernet, err := net.ParseCIDR("0.0.0.0/0")
		if err != nil {
			return err
		}

		//remove routes thru drouter from all containers joined to any drouter network
		containers, err := dr.dc.ContainerList(context.Background(), dockertypes.ContainerListOptions{})
		if err != nil {
			log.Error("Failed to get container list.")
			return err
		}
		for _, c := range containers {
			if c.HostConfig.NetworkMode == "host" { continue }
			if c.ID == dr.selfContainerID { continue }

			for _, nets := range c.NetworkSettings.Networks {
				if drn, ok := dr.networks[nets.NetworkID]; ok && drn.drouter {
					cjson, err := dr.dc.ContainerInspect(context.Background(), c.ID)
					if err != nil {
						log.Error(err)
						continue
					}
					ch, err := netlinkHandleFromPid(cjson.State.Pid)
					if err != nil {
						log.Error(err)
						continue
					}
					//passing a supernet ensures /all/ drouter routes are removed
					err = dr.delContainerRoutes(ch, supernet)
					if err != nil {
						log.Error(err)
						continue
					}
				}
			}
		}
	}

	//leave all connected networks
	for id, drn := range dr.networks {
		if drn.connected {
			err := dr.disconnectNetwork(id)
			if err != nil {
				log.Errorf("Failed to leave network: %v", drn.name)
				log.Error(err)
				continue
			}
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
			IP: ip2,
			Mask: n.Mask,
		}
		
		return ipnet
	}
	ip2 := net.IPv4(
		ip[0] & n.Mask[0],
		ip[1] & n.Mask[1],
		ip[2] & n.Mask[2],
		ip[3] & n.Mask[3],
	)

	ipnet := &net.IPNet{
		IP: ip2,
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
		id := line[len(line) - 1]
		containerInfo, err := dc.ContainerInspect(context.Background(), id)
		if err != nil {
			log.Errorf("Error inspecting container: %v", id)
			return nil, err
		}
		return &containerInfo, nil
	}
	return nil, fmt.Errorf("Container not found")
}
