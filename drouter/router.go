package drouter

import (
	"net"
	"strconv"
	"errors"
	"strings"
	//"os/exec"
	//"fmt"
	"time"
	"os"
	"bufio"
	//"os/signal"
	//"syscall"
	//"bytes"
	//"io/ioutil"

	log "github.com/Sirupsen/logrus"
	//"github.com/samalba/dockerclient"
	dockerclient "github.com/docker/engine-api/client"
	dockertypes "github.com/docker/engine-api/types"
	dockerfilters "github.com/docker/engine-api/types/filters"
	dockernetworks "github.com/docker/engine-api/types/network"
	dockerevents "github.com/docker/engine-api/types/events"
	"golang.org/x/net/context"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"github.com/ziutek/utils/netaddr"
	"github.com/llimllib/ipaddress"
	"github.com/vdemeester/docker-events"
)

type DistributedRouter struct {
	dc                    *dockerclient.Client
	selfContainer         dockertypes.ContainerJSON
	networks              map[string]bool
	hostNamespace         *netlink.Handle
	selfNamespace         *netlink.Handle
//	host_route_link_index int
//	host_route_gw         net.IP
	pid               int //= os.Getpid()
	opts                  *DistributedRouterOptions
}

type DistributedRouterOptions struct {
	ipOffset              int
	aggressive            bool
	summaryNet            []string
	hostGateway           bool
	masquerade            bool
	p2pAddr               string
}

func NewDistributedRouter(options *DistributedRouterOptions) (*DistributedRouter, error) {
	var err error

	dr := &DistributedRouter{
		opts: options,
	}
	
	err = dr.setPid()
	if err != nil {
		return nil, err
	}
	err = dr.setDockerClient()
	if err != nil {
		return nil, err
	}
	err = dr.setSelfContainer()
	if err != nil {
		return nil, err
	}
	err = dr.setSelfNamespace()
	if err != nil {
		return nil, err
	}
	err = dr.setHostNamespace()
	if err != nil {
		return nil, err
	}
	err = dr.setNetworks()
	if err != nil {
		return nil, err
	}

	if options.hostGateway {
		if err := dr.makeP2PLink(); err != nil { return nil, err }
		if options.masquerade {
			if err := dr.insertMasqRule(); err != nil { return nil, err }
		}
	}

	return dr, nil
}

func (dr *DistributedRouter) Start() {
	if dr.opts.aggressive {
		log.Info("Aggressive mode enabled")
		go dr.watchNetworks()
	}

	dr.watchEvents()
}

func (dr *DistributedRouter) Close() error {
	log.Info("Cleaning Up")
	return dr.removeP2PLink()
}

// Validate and set our pid
func (dr *DistributedRouter) setPid() error {
	dr.pid = os.Getpid()
	if dr.pid == 1 {
		return errors.New("Running as Pid 1. drouter must be run with --pid=host")
	}
	return nil
}

// set docker client
func (dr *DistributedRouter) setDockerClient() error {
	defaultHeaders := map[string]string{"User-Agent": "engine-api-cli-1.0"}
	docker, err := dockerclient.NewClient("unix:///var/run/docker.sock", "v1.23", nil, defaultHeaders)
	if err != nil {
		log.Error(err)
		return errors.New("Error connecting to docker socket")
	}
	dr.dc = docker
	return nil
}

func (dr *DistributedRouter) setSelfContainer() error {
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

// Sets the handle for our namespace
func (dr *DistributedRouter) setSelfNamespace() error {
	self_ns, err := netns.Get()
	if err != nil {
		log.Error(err)
		return errors.New("Error getting self namespace.")
	}
	dr.selfNamespace, err = netlink.NewHandleAt(self_ns)
	if err != nil {
		log.Error(err)
		return errors.New("Error getting handle at self namespace.")
	}
	return nil
}

// Sets the handle for the host namespace
func (dr *DistributedRouter) setHostNamespace() error {
	host_ns, err := netns.GetFromPid(1)
	if err != nil {
		log.Error(err)
		return errors.New("Error getting host namespace. Is this container running in priveleged mode?")
	}
	dr.hostNamespace, err = netlink.NewHandleAt(host_ns)
	if err != nil {
		log.Error(err)
		return errors.New("Error getting handle at host namespace.")
	}
	return nil
}

// Prepopulate networks that this container is a member of
func (dr *DistributedRouter) setNetworks() error {
	dr.networks = make(map[string]bool)
	for _, settings := range dr.selfContainer.NetworkSettings.Networks {
		dr.networks[settings.NetworkID] = true
	}
	return nil
}

// Loop to watch for new and modified networks, and join and leave when necessary
func (dr *DistributedRouter) watchNetworks() {
	log.Info("Watching Networks")
	for {
		nets, err := dr.dc.NetworkList(context.Background(), dockertypes.NetworkListOptions{ Filters: dockerfilters.NewArgs(), })
		if err != nil {
			log.Error("Error getting network list")
			log.Error(err)
		}
		for _, net := range nets {
			drouter_str := net.Options["drouter"]
			drouter := false
			if drouter_str != "" {
				drouter, err = strconv.ParseBool(drouter_str) 
				if err != nil {
					log.Errorf("Error parsing drouter option: %v", drouter_str)
					log.Error(err)
				}
			} 

			if drouter && !dr.networks[net.ID] {
				log.Debugf("Joining Net: %+v", net)
				err := dr.joinNet(&net)
				if err != nil {
					log.Errorf("Error joining network: %v", net)
					log.Error(err)
				}
			} else if !drouter && dr.networks[net.ID] {
				log.Debugf("Leaving Net: %+v", net)
				err := dr.leaveNet(&net)
				if err != nil {
					log.Errorf("Error leaving network: %v", net)
					log.Error(err)
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
}

// Watch for container events to add ourself to the container routing table.
func (dr *DistributedRouter) watchEvents() {
	errChan := events.Monitor(context.Background(), dr.dc, dockertypes.EventsOptions{}, func(event dockerevents.Message) {
		if event.Type != "network" { return }
		if event.Action != "connect" { return }
		// don't run on self events
		if event.Actor.Attributes["container"] == dr.self.ID { return }
		// don't run if this network is not being managed
		if !dr.networks[event.Actor.ID] { return }
		log.Debugf("Event.Actor: %v", event.Actor)

		containerInfo, err := dr.dc.ContainerInspect(context.Background(), event.Actor.Attributes["container"])
		if err != nil {
			log.Error(err)
			return
		}

		log.Debugf("containerInfo: %v", containerInfo)
		log.Debugf("pid: %v", containerInfo.State.Pid)
		container_ns, err := netns.GetFromPid(containerInfo.State.Pid)
		if err != nil {
			log.Error(err)
			return
		}
		container_ns_h, err := netlink.NewHandleAt(container_ns)
		if err != nil {
			log.Error(err)
			return
		}

		routes, err := container_ns_h.RouteList(nil, netlink.FAMILY_V4)
		if err != nil {
			log.Error(err)
			return
		}
		log.Debugf("container routes: %v", routes)
		for _, r := range routes {
			// The container gateway
			if r.Dst == nil {
				
				// Default route has no src, need to get the route to the gateway to get the src
				src_route, err := container_ns_h.RouteGet(r.Gw)
				if err != nil {
					log.Error(err)
					return
				}
				if len(src_route) == 0 {
					log.Errorf("No route found in container to the containers existing gateway: %v", r.Gw)
					return
				}

				// Get the route from gw-container back to the container, 
				// this src address will be used as the container's gateway
				gw_rev_route, err := dr.selfNamespace.RouteGet(src_route[0].Src)
				if err != nil {
					log.Error(err)
					return
				}
				if len(gw_rev_route) == 0 {
					log.Errorf("No route found back to container ip: %v", src_route[0].Src)
					return
				}

				log.Debugf("Existing default route: %v", r)
				err = container_ns_h.RouteDel(&r)
				if err != nil {
					log.Error(err)
					return
				}

				r.Gw = gw_rev_route[0].Src
				err = container_ns_h.RouteAdd(&r)
				if err != nil {
					log.Error(err)
					return
				}
				log.Debugf("Default route changed: %v", r)
			}
		}
	})
	if err := <-errChan; err != nil {
		log.Error(err)
	}
}

func (dr *DistributedRouter) joinNet(n *dockertypes.NetworkResource) error {
	endpointSettings := &dockernetworks.EndpointSettings{}
	if dr.opts.ipOffset != 0 {
		for _, ipamconfig := range n.IPAM.Config {
			log.Debugf("ip-offset configured")
			_, subnet, err := net.ParseCIDR(ipamconfig.Subnet)
			if err != nil {
				return err
			}
			var ip net.IP
			if dr.opts.ipOffset > 0 {
				ip = netaddr.IPAdd(subnet.IP, dr.opts.ipOffset)
			} else {
				last := ipaddress.LastAddress(subnet)
				ip = netaddr.IPAdd(last, dr.opts.ipOffset)
			}
			log.Debugf("Setting IP to %v", ip)
			if endpointSettings.IPAddress == "" {
				endpointSettings.IPAddress = ip.String()
				endpointSettings.IPAMConfig =&dockernetworks.EndpointIPAMConfig{
					IPv4Address: ip.String(),
				}
			} else {
				endpointSettings.Aliases = append(endpointSettings.Aliases, ip.String())
			}
		}
	}

	err := dr.dc.NetworkConnect(context.Background(), n.ID, dr.self.ID, endpointSettings)
	if err != nil {
		return err
	}
	dr.networks[n.ID] = true
	for _, ipamconfig := range n.IPAM.Config {
		_, dst, err := net.ParseCIDR(ipamconfig.Subnet)
		if err != nil {
			return err
		}
		route := &netlink.Route{
			//LinkIndex: host_route_link_index,
			//Gw: host_route_gw,
			Dst: dst,
		}
		err = dr.hostNamespace.RouteAdd(route)
		if err != nil {
			return err
		}
	}
	return nil
}

func (dr *DistributedRouter) leaveNet(n *dockertypes.NetworkResource) error {
	err := dr.dc.NetworkDisconnect(context.Background(), n.ID, dr.self.ID, true)
	if err != nil {
		return err
	}
	dr.networks[n.ID] = false
	return nil
}

func (dr *DistributedRouter) makeP2PLink() error {
	host_link_veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: "drouter_veth0"},
		PeerName:  "drouter_veth1",
	}
	err := dr.hostNamespace.LinkAdd(host_link_veth)
	if err != nil {
		return err
	}
	host_link, err := dr.hostNamespace.LinkByName("drouter_veth0")
	if err != nil {
		return err
	}
//	host_route_link_index = host_link.Attrs().Index

	int_link, err := dr.hostNamespace.LinkByName("drouter_veth1")
	if err != nil {
		return err
	}
	err = dr.hostNamespace.LinkSetNsPid(int_link, dr.selfPid)
	if err != nil {
		return err
	}
	int_link, err = dr.selfNamespace.LinkByName("drouter_veth1")
	if err != nil {
		return err
	}

	_, p2p_net, err := net.ParseCIDR(p2p_addr)
	if err != nil {
		return err
	}

	host_addr := *p2p_net
	host_addr.IP = netaddr.IPAdd(host_addr.IP, 1)
	host_netlink_addr := &netlink.Addr{ 
		IPNet: &host_addr,
		Label: "",
	}
	err = dr.hostNamespace.AddrAdd(host_link, host_netlink_addr)
	if err != nil {
		return err
	}

	int_addr := *p2p_net
	int_addr.IP = netaddr.IPAdd(int_addr.IP, 2)
	int_netlink_addr := &netlink.Addr{ 
		IPNet: &int_addr,
		Label: "",
	}
	err = dr.selfNamespace.AddrAdd(int_link, int_netlink_addr)
	if err != nil {
		return err
	}

//	host_route_gw = int_addr.IP

	err = dr.selfNamespace.LinkSetUp(int_link)
	if err != nil {
		return err
	}

	err = dr.hostNamespace.LinkSetUp(host_link)
	if err != nil {
		return err
	}

	return nil
}

func (dr *DistributedRouter) removeP2PLink() error {
	host_link, err := dr.hostNamespace.LinkByName("drouter_veth0")
	if err != nil {
		return err
	}
	return dr.hostNamespace.LinkDel(host_link)
}

func (dr *DistributedRouter) insertMasqRule() error {
	//not implemented yet
	return nil
}
