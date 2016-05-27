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
	"golang.org/x/net/context"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"github.com/ziutek/utils/netaddr"
)

var (
	docker                *dockerclient.Client
	self_container        dockertypes.ContainerJSON
	networks              = make(map[string]bool)
	host_ns_h             *netlink.Handle
	self_ns_h             *netlink.Handle
	host_route_link_index int
	host_route_gw		    net.IP
	my_pid                = os.Getpid()
)

func init() {
	var err error

	if my_pid == 1 {
		log.Fatal("Running as Pid 1. drouter must be run with --pid=host")
	}

	defaultHeaders := map[string]string{"User-Agent": "engine-api-cli-1.0"}
	docker, err = dockerclient.NewClient("unix:///var/run/docker.sock", "v1.22", nil, defaultHeaders)
	if err != nil {
		log.Fatal(err)
	}
	self_container, err = getSelf()
	if err != nil {
		log.Error("Error getting self container. Is this processs running in a container? Is the docker socket passed through?")
		log.Fatal(err)
	}

	// Prepopulate networks that this container is a member of
	log.Infof("Self_container-info: %v", self_container)
	log.Infof("Self_container-info.networksettings.networks: %v", self_container.NetworkSettings.Networks)
	for _, settings := range self_container.NetworkSettings.Networks {
		networks[settings.NetworkID] = true
	}

	log.Infof("Member networks on startup: %v", networks)

	self_ns, err := netns.GetFromPid(my_pid)
	if err != nil {
		log.Error("Error getting self namespace.")
		log.Fatal(err)
	}
	log.Infof("self_ns: %v", self_ns)
	self_ns_h, err = netlink.NewHandleAt(self_ns)
	if err != nil {
		log.Error("Error getting handle at self namespace.")
		log.Fatal(err)
	}
	host_ns, err := netns.GetFromPid(1)
	if err != nil {
		log.Error("Error getting host namespace. Is this container running in priveleged mode?")
		log.Fatal(err)
	}
	log.Infof("host_ns: %v", self_ns)
	host_ns_h, err = netlink.NewHandleAt(host_ns)
	if err != nil {
		log.Error("Error getting handle at host namespace.")
		log.Fatal(err)
	}
}

// Loop to watch for new networks created and create interfaces when needed
func WatchNetworks() {
	log.Info("Watching Networks")
	for {
		nets, err := docker.NetworkList(context.Background(), dockertypes.NetworkListOptions{ Filters: dockerfilters.NewArgs(), })
		if err != nil {
			log.Error(err)
		}
		for i := range nets {
			//log.Debugf("Checking network %v", nets[i])
			drouter_str := nets[i].Options["drouter"]
			drouter := false
			if drouter_str != "" {
				drouter, err = strconv.ParseBool(drouter_str) 
				if err != nil {
					log.Error(err)
				}
			} 

			if drouter && !networks[nets[i].ID] {
				log.Debugf("Creating Net: %+v", nets[i])
				err := joinNet(&nets[i])
				if err != nil {
					log.Error(err)
				}
			} else if !drouter && networks[nets[i].ID] {
				log.Debugf("Leaving Net: %+v", nets[i])
				err := leaveNet(&nets[i])
				if err != nil {
					log.Error(err)
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
}

func WatchEvents() {
	for {
		time.Sleep(1 * time.Second)
	}
}

func joinNet(net *dockertypes.NetworkResource) error {
	err := docker.NetworkConnect(context.Background(), net.ID, self_container.ID, &dockernetworks.EndpointSettings{})
	if err != nil {
		return err
	}
	networks[net.ID] = true
	return nil
}

func leaveNet(net *dockertypes.NetworkResource) error {
	err := docker.NetworkDisconnect(context.Background(), net.ID, self_container.ID, true)
	if err != nil {
		return err
	}
	networks[net.ID] = false
	return nil
}

func getSelf() (dockertypes.ContainerJSON, error) {
	cgroup, err := os.Open("/proc/self/cgroup")
	if err != nil {
		return dockertypes.ContainerJSON{}, err
	}
	defer cgroup.Close()

	scanner := bufio.NewScanner(cgroup)
	for scanner.Scan() {
		line := strings.Split(scanner.Text(), "/")
		id := line[len(line) - 1]
		containerInfo, err := docker.ContainerInspect(context.Background(), id)
		if err != nil {
			log.Warn(err)
			continue
		}
		return containerInfo, nil
	}
	return dockertypes.ContainerJSON{}, errors.New("Container not found")
}

func MakeP2PLink(p2p_addr string) error {
	host_link_veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: "drouter_veth0"},
		PeerName:  "drouter_veth1",
	}
	err := host_ns_h.LinkAdd(host_link_veth)
	if err != nil {
		return err
	}
	host_link, err := host_ns_h.LinkByName("drouter_veth0")
	if err != nil {
		return err
	}
	host_route_link_index = host_link.Attrs().Index

	int_link, err := host_ns_h.LinkByName("drouter_veth1")
	if err != nil {
		return err
	}
	err = host_ns_h.LinkSetNsPid(int_link, my_pid)
	if err != nil {
		return err
	}
	int_link, err = self_ns_h.LinkByName("drouter_veth1")
	if err != nil {
		return err
	}

	_, p2p_net, err := net.ParseCIDR(p2p_addr)
	if err != nil {
		return err
	}

	host_addr := p2p_net
	host_addr.IP = netaddr.IPAdd(host_addr.IP, 1)
	host_netlink_addr := &netlink.Addr{ 
		IPNet: host_addr,
		Label: "",
	}
	err = host_ns_h.AddrAdd(host_link, host_netlink_addr)
	if err != nil {
		return err
	}

	int_addr := p2p_net
	int_addr.IP = netaddr.IPAdd(int_addr.IP, 2)
	int_netlink_addr := &netlink.Addr{ 
		IPNet: int_addr,
		Label: "",
	}
	err = self_ns_h.AddrAdd(int_link, int_netlink_addr)
	if err != nil {
		return err
	}
	host_route_gw = int_addr.IP

	err = self_ns_h.LinkSetUp(int_link)
	if err != nil {
		return err
	}

	err = self_ns_h.LinkSetUp(host_link)
	if err != nil {
		return err
	}

	return nil
}
