package drouter

import (
	"os"
	"errors"
	"bufio"
	"strings"
	"net"
	"fmt"
	"strconv"
	"time"
	log "github.com/Sirupsen/logrus"
	dockerclient "github.com/docker/engine-api/client"
	dockertypes "github.com/docker/engine-api/types"
	dockerfilters "github.com/docker/engine-api/types/filters"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/net/context"
	"github.com/vdemeester/docker-events"
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
		}
		
		dr.networks[settings.NetworkID] = &drNetwork{
			id: settings.NetworkID,
			connected: true,
			drouter: false,
			subnet: subnet,
			ip: ip,
			gateway: gateway,
		}
	}

	//initial setup
	if dr.localShortcut {
		if err := dr.makeP2PLink(options.p2pNet); err != nil { return nil, err }
		if dr.masquerade {
			if err := insertMasqRule(); err != nil { return nil, err }
		}
	
	}

	return dr, nil
}

func (dr *DistributedRouter) Start() {
	err := dr.syncNetworks()
	if err != nil {
		log.Error("Failed to do initial network sync.")
	}
	
	if dr.aggressive {
		log.Info("Aggressive mode enabled")
		go dr.watchNetworks()
	}

	//initialize existing containers with proper routes.
	dr.initContainers()

	dr.watchEvents()
}

func (dr *DistributedRouter) Close() error {
	log.Info("Cleaning Up")
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

func (dr *DistributedRouter) syncNetworks() error {
	//get all networks from docker
	nets, err := dr.dc.NetworkList(context.Background(), dockertypes.NetworkListOptions{ Filters: dockerfilters.NewArgs(), })
	if err != nil {
		log.Error("Error getting network list")
		return err
	}
	for _, network := range nets {
		drouter_str := network.Options["drouter"]
		drouter := false
		if drouter_str != "" {
			drouter, err = strconv.ParseBool(drouter_str) 
			if err != nil {
				log.Errorf("Error parsing drouter option: %v", drouter_str)
				return err
			}
		} 

		//if we are not a member and are supposed to be
		if drouter {
			dr.networks[network.ID].drouter = true
			if !dr.networks[network.ID].connected && (dr.aggressive || network.ID == dr.transitNet) {
				log.Debugf("Joining Net: %+v", network)
				err := dr.drNetworkConnect(&network)
				if err != nil {
					log.Errorf("Error joining network: %v", network)
					return err
				}
			}
		//if we are a member and not supposed to be
		} else {
			dr.networks[network.ID].drouter = false
			if !drouter && dr.networks[network.ID].connected {
				log.Debugf("Leaving Net: %+v", network)
				err := dr.drNetworkDisconnect(network.ID)
				if err != nil {
					log.Errorf("Error leaving network: %v", network)
					return err
				}
			}
		}
	}
	return nil
}

func (dr *DistributedRouter) watchNetworks() {
	log.Info("Watching Networks")
	for {
		//TODO: make this timeout a variable
		time.Sleep(5 * time.Second)

		err := dr.syncNetworks()
		if err != nil {
			log.Error(err)
		}
	}
}

// Watch for container events to add ourself to the container routing table.
func (dr *DistributedRouter) watchEvents() {
	errChan := events.Monitor(context.Background(), dr.dc, dockertypes.EventsOptions{}, func(event dockerevents.Message) {
		if event.Type != "network" { return }
		if event.Action != "connect" { return }
		// don't run on self events
		if event.Actor.Attributes["container"] == dr.selfContainer.ID { return }

		log.Debugf("Network connect event detected. Syncing Networks.")
		dr.syncNetworks()
		
		// don't run if this network is not a "drouter" network
		if !dr.networks[event.Actor.ID].drouter { return }
		log.Debugf("Event.Actor: %v", event.Actor)

		//get new containers info
		containerInfo, err := dr.dc.ContainerInspect(context.Background(), event.Actor.Attributes["container"])
		if err != nil {
			log.Error(err)
			return
		}
		log.Debugf("containerInfo: %v", containerInfo)

		//get new containers namespace handle
		containerHandle, err := netlinkHandleFromPid(containerInfo.State.Pid)
		if err != nil {
			log.Error(err)
			return
		}

		if dr.localGateway {
			dr.replaceContainerGateway(containerHandle)
		} else {
			dr.insertContainerRoutes(containerHandle)
		}

	})
	if err := <-errChan; err != nil {
		log.Error(err)
	}
}

func (dr *DistributedRouter) initContainers() error {
	//Loop through all containers, call insertContainerRoutes() or replaceContainerGateway()
	return nil
}

func (dr *DistributedRouter) insertContainerRoutes(ch *netlink.Handle) error {
	//Loop through all networks, call insertContainerRoute()
	return nil
}

func (dr *DistributedRouter) insertContainerRoute(ch *netlink.Handle, subnet net.IPNet) error {
	//Put the prefix into the container routing table pointing back to the drouter
	return nil
}

func (dr *DistributedRouter) replaceContainerGateway(ch *netlink.Handle) error {
	//replace containers default gateway with drouter
	routes, err := ch.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	log.Debugf("container routes: %v", routes)
	for _, r := range routes {
		// The container gateway
		if r.Dst == nil {
			
			// Default route has no src, need to get the route to the gateway to get the src
			src_route, err := ch.RouteGet(r.Gw)
			if err != nil {
				return err
			}
			if len(src_route) == 0 {
				serr := fmt.Sprintf("No route found in container to the containers existing gateway: %v", r.Gw)
				return errors.New(serr)
			}

			// Get the route from gw-container back to the container, 
			// this src address will be used as the container's gateway
			gw_rev_route, err := dr.selfNamespace.RouteGet(src_route[0].Src)
			if err != nil {
				return err
			}
			if len(gw_rev_route) == 0 {
				serr := fmt.Sprintf("No route found back to container ip: %v", src_route[0].Src)
				return errors.New(serr)
			}

			log.Debugf("Remove existing default route: %v", r)
			err = ch.RouteDel(&r)
			if err != nil {
				return err
			}

			r.Gw = gw_rev_route[0].Src
			err = ch.RouteAdd(&r)
			if err != nil {
				return err
			}
			log.Debugf("Default route changed to: %v", r)
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

