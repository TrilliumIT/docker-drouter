package mvrouter

import (
	//gonet "net"
	//"strconv"
	//"errors"
	//"strings"
	//"os/exec"
	//"fmt"
	"time"
	"os"
	//"os/signal"
	//"syscall"
	//"bytes"
	//"io/ioutil"

	log "github.com/Sirupsen/logrus"
	"github.com/samalba/dockerclient"
	//"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"github.com/ziutek/utils/netaddr"
)

var (
	docker         *dockerclient.DockerClient
	self_container dockerclient.Container
	networks       map[string]bool
	host_ns_h        netns.NsHandle
	self_ns_h        netns.NsHandle
	my_pid = os.Getpid()
)

func init() {
	docker, err := dockerclient.NewDockerClient("unix:///var/run/docker.sock", nil)
	if err != nil {
		log.Fatal(err)
	}
	self_container, err := getSelf()
	if err != nil {
		log.Error("Error getting self container. Is this processs running in a container?")
		log.Fatal(err)
	}

	// Prepopulate networks that this container is a member of
	for endpoint, settings := range self_container.NetworkSettings.Networks {
		networks[settings.NetworkID] := true
	}

	self_ns, err := netns.GetFromPid(my_pid)
	if err != nil {
		log.Error("Error getting self namespace.")
		log.Fatal(err)
	}
	self_ns_h, err := netlink.NewHandleAt(self_ns)
	if err != nil {
		log.Error("Error getting handle at self namespace.")
		log.Fatal(err)
	}
	host_ns, err := netns.GetFromPid(1)
	if err != nil {
		log.Error("Error getting host namespace. Is this container running in priveleged mode?")
		log.Fatal(err)
	}
	host_ns_h, err := netlink.NewHandleAt(host_ns)
	if err != nil {
		log.Error("Error getting handle at host namespace.")
		log.Fatal(err)
	}
}

// Loop to watch for new networks created and create interfaces when needed
func WatchNetworks() {
	for {
		nets, err := docker.ListNetworks("")
		if err != nil {
			log.Error(err)
		}
		for i := range nets {
			if strconv.ParseBool(nets[i].Options['drouter']) && !networks.[nets[i].ID] {
				log.Debugf(" Creating Net: %+v", nets[i])
				_, err := joinNet(nets[i])
				if err != nil {
					log.Error(err)
				}
			} else if !strconv.ParseBool(nets[i].Options['drouter']) && networks.[nets[i].ID] {
				_, err := leaveNet(nets[i])
				if err != nil {
					log.Error(err)
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
}

func WatchEvents() {
	//FIXME
}

func joinNet(net *dockerclient.NetworkResource) error {
	err := docker.ConnectNetwork(net.ID, self_container.Id)
	if err != nil {
		return err
	}
	networks[net.ID] := true
	return nil
}

func leaveNet(net *dockerclient.NetworkResource) error {
	err := docker.DisconnectNetwork(net.ID, self_container.Id)
	if err != nil {
		return err
	}
	networks[net.ID] := false
	return nil
}

func getSelf() (dockerclient.Container error) {
	containers, err := docker.ListContainers
	if err != nil {
		return err
	}
	for i := range containers {
		containerInfo, err := dockerclient.InspectContainer(containers[i].Id)
		if err != nil {
			log.Error(err)
		}
		if containerInfo.State.Pid == my_pid {
			return containers[i], nil
		}
	}
}

func MakeP2PLink(p2p_addr string) error {
	host_link = &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: "drouter_veth0"},
		PeerName:  "drouter_veth1",
	}
	err = host_ns_h.LinkAdd(host_link)
	if err != nil {
		return err
	}

	int_link, err := host_ns_h.LinkByName("drouter_veth1")
	if err != nil {
		return err
	}
	err = host_ns_h.LinkSetNsPid(int_link, my_pid)
	if err != nil {
		return err
	}
	int_link, err := self_ns_h.LinkByName("drouter_veth1")
	if err != nil {
		return err
	}

	_, p2p_net, err := net.ParseCIDR(p2p_addr)
	if err != nil {
		return err
	}

	host_addr := p2p_net
	host_addr.IP = netaddr.IPAdd(host_addr.IP, 1)
	err := host_ns_h.AddrAdd(host_link, host_addr)
	if err != nil {
		return err
	}

	int_addr := p2p_net
	int_addr.IP = netaddr.IPAdd(int_addr.IP, 2)
	err := self_ns_h.AddrAdd(int_link, int_addr)
	if err != nil {
		return err
	}

}
