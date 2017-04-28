package vxlan

import (
	gonet "net"
	"strconv"
	"errors"
	"strings"
	"os/exec"
	"fmt"
	"time"
	"os"
	"os/signal"
	"syscall"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/go-plugins-helpers/network"
	"github.com/samalba/dockerclient"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

type Driver struct {
	network.Driver
	scope	          string
	vtepdev           string
	allow_empty       bool
	local_gateway     bool
	global_gateway    bool
	networks          map[string]bool
	gateway_ns        *netns.NsHandle,
	docker	          *dockerclient.DockerClient
}

func NewDriver(scope string, vtepdev string, allow_empty bool, local_gateway bool, global_gateway bool) (*Driver, error) {
	docker, err := dockerclient.NewDockerClient("unix:///var/run/docker.sock", nil)
	if err != nil {
		return nil, err
	}

	if d.scope == "local" || d.local_gateway || d.globalGateway {
		// Create p2p link from the host to inside the gateway namespace
		gateway_ns, err := netns.New()
		if err != nil {
			return nil, err
		}
		gateway_h, err := netlink.NewHandleAt(gateway_ns)
		if err != nil {
			return nil, err
		}

		gateway_link, err := netlink.LinkByName("vxlan_gw")
		if err != nil {
			gateway_link = &netlink.Veth{
				LinkAttrs: netlink.LinkAttrs{Name: "vxlan_gw",
				PeerName:  "vxlan_gw_int",
			err = netlink.LinkAdd(gateway_link)
			if err != nil {
				return nil, err
			}
		}
		gateway_link_ip, err := netlink.ParseAddr("172.30.255.253/30")
		if err != nil {
			return nil, err
		}
		err = netlink.AddrAdd(gateway_link, gateway_link_ip)
		if err != nil {
			return nil, err
		}
		err = netlink.LinkSetUp(gateway_link)
		if err != nil {
			return nil, err
		}

		gateway_link_int, err := netlink.LinkByName(gateway_link.PeerName)
		if err != nil {
			return nil, err
		}
		err = netlink.LinkSetNsFd(gateway_link_int, int(gateway_ns))
		if err != nil {
			return nil, err
		}
		// get the link again inside the namespace
		gateway_link_int, err := gateway_h.LinkByName(gateway_link.PeerName)
		if err != nil {
			return nil, err
		}
		gateway_link_int_ip, err := netlink.ParseAddr("172.30.255.254/30")
		if err != nil {
			return nil, err
		}
		err = netlink.AddrAdd(gateway_link_int, gateway_link_int_ip)
		if err != nil {
			return nil, err
		}
		err = netlink.LinkSetUp(gateway_link_int)
		if err != nil {
			return nil, err
		}
	}

	d := &Driver{
		scope: scope,
		vtepdev: vtepdev,
		allow_empty: allow_empty,
		local_gateway: local_gateway,
		global_gateway: global_gateway,
		networks: make(map[string]bool),
		gateway_ns: gateway_ns,
		docker: docker,
	}
	if d.allow_empty {
		go d.watchNetworks()
	}
	if d.local_gateway {
		go d.watchEvents()
	}
	return d, nil
}

// Loop to watch for new networks created and create interfaces when needed
func (d *Driver) watchNetworks() error {
	for {
		nets, err := d.docker.ListNetworks("")
		if err != nil {
			return err
		}
		for i := range nets {
			if nets[i].Driver == "vxlan" && !d.networks[nets[i].ID] {
				log.Debugf("Net[i]: %+v", nets[i])
				_, err := d.getLinks(nets[i].ID)
				if err != nil {
					return err
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
	return nil
}


func (d *Driver) waitForInterrupt() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
	for _ = range sigChan {
		d.docker.StopAllMonitorEvents()
	}

	nets, err := d.docker.ListNetworks("")
	if err != nil {
		return err
	}
	for i := range nets {
		if nets[i].Driver == "vxlan" && !d.networks[nets[i].ID] {
			log.Debugf("Net[i]: %+v", nets[i])
			_, err := d.deleteLinks(nets[i].ID)
			if err != nil {
				return err
			}
		}
	}
	gateway_ns.Close()
}

func (d *Driver) eventCallBack(e *dockerclient.Event, ec chan error, args ...interface{}) error {
	if d.local_gateway && e.Type == "network" && e.Action == "connect" && e.Actor.Attributes["type"] == "vxlan" {
		log.Debugf("Adding gateway to arp table in container %+v", e.Actor.Attributes["container"][:5])

		// keep trying until the container namespace has been created
		ns, _ := netns.GetFromDocker(e.Actor.Attributes["container"])
		for ns == -1 {
			time.Sleep(10 * time.Millisecond)
			ns, _ = netns.GetFromDocker(e.Actor.Attributes["container"])
		}

		h, err := netlink.NewHandleAt(ns)
		if err != nil {
			return err
		}

		// wait until the namespace has a default route, then add the arp entry
		Loop:
		for {
			routes, _ := h.RouteList(nil, netlink.FAMILY_V4)
			for i := range routes {
				if routes[i].Dst == nil  {
					// FIXME: Get the mac address from the gateway macvlan interface, and the IP from the gateway.
					n := &netlink.Neigh{
						IP:	net.ParseIP("10.1.128.254"),
						HardwareAddr:	parseMAC("72:0a:11:91:9d:f4"),
						State: netlink.NUD_PERMANENT,
					}

					// add the arp entry
					err := h.NeighSet(n)
					if err != nil {
						panic(err)
					}
					break Loop
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (d *Driver) watchEvents() {
	d.docker.StartMonitorEvents(d.eventCallBack, nil)
	d.waitForInterrupt()
}

func (d *Driver) GetCapabilities() (*network.CapabilitiesResponse, error) {
	log.Debugf("Get Capabilities request")
	res := &network.CapabilitiesResponse{
		Scope: d.scope,
	}
	log.Debugf("Responding with %+v", res)
	return res, nil
}

type intNames struct {
	VxlanName  string
}

func getIntNames(netID string, docker *dockerclient.DockerClient) (*intNames, error) {
	net, err := docker.InspectNetwork(netID)
	if err != nil {
		return nil, err
	}

	names := &intNames{}

	if net.Driver != "vxlan" {
		log.Errorf("Network %v is not a vxlan network", netID)
		return nil, errors.New("Not a vxlan network")
	}

	names.VxlanName = "vx_" + netID[:12]

	// get interface names from options first
	for k, v := range net.Options {
		if k == "vxlanName" {
			names.VxlanName = v
		}
	}

	return names, nil
}

func getGateway(netID string, docker dockerclient.DockerClient) (string, error) {
	net, err := docker.InspectNetwork(netID)
	if err != nil {
		return "", err
	}

	for i := range net.IPAM.Config {
		if net.IPAM.Config[i].Gateway != "" {
			return net.IPAM.Config[i].Gateway, nil
		}
	}
	return "", nil
}

type intLinks struct {
	Vxlan  *netlink.Vxlan
}

// this function gets netlink devices or creates them if they don't exist
func (d *Driver) getLinks(netID string) (*intLinks, error) {
	docker := d.docker
	net, err := docker.InspectNetwork(netID)
	if err != nil {
		return nil, err
	}

	if net.Driver != "vxlan" {
		log.Errorf("Network %v is not a vxlan network", netID)
		return nil, errors.New("Not a vxlan network")
	}

	names, err := getIntNames(netID, docker)
	if err != nil {
		return nil, err
	}

	// get or create links
	var vxlan *netlink.Vxlan
	vxlanlink, err := netlink.LinkByName(names.VxlanName)
	if err == nil {
		vxlan = &netlink.Vxlan{
			LinkAttrs: *vxlanlink.Attrs(),
		}
	} else {
		vxlan, err = d.createVxLan(names.VxlanName, net)
		if err != nil {
			return nil, err
		}
	}

	links := &intLinks{
		Vxlan: vxlan,
	}

	d.networks[netID] = true
	return links, nil
}

func (d *Driver) createVxLan(vxlanName string, net *dockerclient.NetworkResource) (*netlink.Vxlan, error) {
	vxlan := &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{
			Name: vxlanName,
		},
	}

	// Parse interface options
	for k, v := range net.Options {
		if k == "vxlanMTU" {
			MTU, err := strconv.Atoi(v)
			if err != nil {
				return nil, err
			}
			vxlan.LinkAttrs.MTU = MTU
		}
		if k == "vxlanHardwareAddr" {
			HardwareAddr, err := gonet.ParseMAC(v)
			if err != nil {
				return nil, err
			}
			vxlan.LinkAttrs.HardwareAddr = HardwareAddr
		}
		if k == "vxlanTxQLen" {
			TxQLen, err := strconv.Atoi(v)
			if err != nil {
				return nil, err
			}
			vxlan.LinkAttrs.TxQLen = TxQLen
		}
		if k == "VxlanId" {
			log.Debugf("VxlanID: %+v", v)
			VxlanId, err := strconv.ParseInt(v, 0, 32)
			if err != nil {
				return nil, err
			}
			log.Debugf("VxlanID: %+v", VxlanId)
			log.Debugf("int(VxlanID): %+v", int(VxlanId))
			vxlan.VxlanId = int(VxlanId)
		}
		if k == "VtepDev" {
			vtepDev, err := netlink.LinkByName(v)
			if err != nil {
				return nil, err
			}
			vxlan.VtepDevIndex = vtepDev.Attrs().Index
		}
		if k == "SrcAddr" {
			vxlan.SrcAddr = gonet.ParseIP(v)
		}
		if k == "Group" {
			vxlan.Group = gonet.ParseIP(v)
		}
		if k == "TTL" {
			TTL, err := strconv.Atoi(v)
			if err != nil {
				return nil, err
			}
			vxlan.TTL = TTL
		}
		if k == "TOS" {
			TOS, err := strconv.Atoi(v)
			if err != nil {
				return nil, err
			}
			vxlan.TOS = TOS
		}
		if k == "Learning" {
			Learning, err := strconv.ParseBool(v)
			if err != nil {
				return nil, err
			}
			vxlan.Learning = Learning
		}
		if k == "Proxy" {
			Proxy, err := strconv.ParseBool(v)
			if err != nil {
				return nil, err
			}
			vxlan.Proxy = Proxy
		}
		if k == "RSC" {
			RSC, err := strconv.ParseBool(v)
			if err != nil {
				return nil, err
			}
			vxlan.RSC = RSC
		}
		if k == "L2miss" {
			L2miss, err := strconv.ParseBool(v)
			if err != nil {
				return nil, err
			}
			vxlan.L2miss = L2miss
		}
		if k == "L3miss" {
			L3miss, err := strconv.ParseBool(v)
			if err != nil {
				return nil, err
			}
			vxlan.L3miss = L3miss
		}
		if k == "NoAge" {
			NoAge, err := strconv.ParseBool(v)
			if err != nil {
				return nil, err
			}
			vxlan.NoAge = NoAge
		}
		if k == "GBP" {
			GBP, err := strconv.ParseBool(v)
			if err != nil {
				return nil, err
			}
			vxlan.GBP = GBP
		}
		if k == "Age" {
			Age, err := strconv.Atoi(v)
			if err != nil {
				return nil, err
			}
			vxlan.Age = Age
		}
		if k == "Limit" {
			Limit, err := strconv.Atoi(v)
			if err != nil {
				return nil, err
			}
			vxlan.Limit = Limit
		}
		if k == "Port" {
			Port, err := strconv.Atoi(v)
			if err != nil {
				return nil, err
			}
			vxlan.Port = Port
		}
		if k == "PortLow" {
			PortLow, err := strconv.Atoi(v)
			if err != nil {
				return nil, err
			}
			vxlan.PortLow = PortLow
		}
		if k == "PortHigh" {
			PortHigh, err := strconv.Atoi(v)
			if err != nil {
				return nil, err
			}
			vxlan.PortHigh = PortHigh
		}
	}

	if d.vtepdev != "" {
		vtepDev, err := netlink.LinkByName(d.vtepdev)
		if err != nil {
			return nil, err
		}
		vxlan.VtepDevIndex = vtepDev.Attrs().Index
	}

	err := netlink.LinkAdd(vxlan)
	if err != nil {
		return nil, err
	}

	localGateway := false

	// Parse interface options
	for k, v := range net.Options {
		if k == "vxlanHardwareAddr" {
			hardwareAddr, err := gonet.ParseMAC(v)
			if err != nil {
				return nil, err
			}
			err = netlink.LinkSetHardwareAddr(vxlan, hardwareAddr)
			if err != nil {
				return nil, err
			}
		}
		if k == "vxlanMTU" {
			mtu, err := strconv.Atoi(v)
			if err != nil {
				return nil, err
			}
			err = netlink.LinkSetMTU(vxlan, mtu)
			if err != nil {
				return nil, err
			}
		}
		if k == "localGateway" {
			globalGateway, err = strconv.ParseBool(v)
			if err != nil {
				return nil, err
			}
		}
	}

	// bring interfaces up
	err = netlink.LinkSetUp(vxlan)
	if err != nil {
		return nil, err
	}

	log.Debugf("checking if gateway enabled")
	if d.scope == "local" || ( d.local_gateway && localGateway ) || d.globalGateway {
		// FIXME: make macvlan interface for gateway
		// FIXME: add it to the gateway namespace
		log.Debugf("gateway is enabled")
		for i := range net.IPAM.Config {
			mask := strings.Split(net.IPAM.Config[i].Subnet, "/")[1]
			gatewayIP, err := netlink.ParseAddr(net.IPAM.Config[i].Gateway + "/" + mask)
			if err != nil {
				return nil, err
			}
			// FIXME: if local_gateway:
				// FIXME: sysctl arp_ignore - 8
			// FIXME: add the ip to the macvlan interface
			netlink.AddrAdd(vxlan, gatewayIP)
			// FIXME: Add a route to the network via the p2p link
		}
	}

	return vxlan, nil
}

func (d *Driver) CreateNetwork(r *network.CreateNetworkRequest) error {
	log.Debugf("Create network request: %+v", r)

	// return nil and lazy create the network when a container joins it
	// Active creation when allow_empty is enabled will be handled by watching libkv
	return nil
}

func (d *Driver) deleteLinks(netID string) error {
	names, err := getIntNames(netID, d.docker)
	if err != nil {
		return err
	}

	vxlan, err := netlink.LinkByName(names.VxlanName)
	if err == nil {
		err := netlink.LinkDel(vxlan)
		if err != nil {
			return err
		}
		log.Debugf("Deleting interface %+v", names.VxlanName)
	}
	
	return nil
}

func (d *Driver) DeleteNetwork(r *network.DeleteNetworkRequest) error {
	netID := r.NetworkID
	return d.deleteLinks(netID)
}

func (d *Driver) CreateEndpoint(r *network.CreateEndpointRequest) error {
	log.Debugf("Create endpoint request: %+v", r)
	netID := r.NetworkID
	// get the links
	_, err := d.getLinks(netID)
	if err != nil {
		return err
	}
	return nil
}

func (d *Driver) DeleteEndpoint(r *network.DeleteEndpointRequest) error {
	log.Debugf("Delete endpoint request: %+v", r)
	if d.allow_empty {
		return nil
	}

	netID := r.NetworkID

	links, err := d.getLinks(netID)
	if err != nil {
		return err
	}
	VxlanIndex := links.Vxlan.LinkAttrs.Index

	allLinks, err := netlink.LinkList()
	if err != nil {
		return err
	}

	for i := range allLinks {
		if allLinks[i].Attrs().Index != VxlanIndex {
			return nil
		}
	}

	log.Debugf("No interfaces attached to vxlan: deleting vxlan interface.")
	return d.deleteLinks(netID)
}

func (d *Driver) EndpointInfo(r *network.InfoRequest) (*network.InfoResponse, error) {
	res := &network.InfoResponse{
		Value: make(map[string]string),
	}
	return res, nil
}

func (d *Driver) Join(r *network.JoinRequest) (*network.JoinResponse, error) {
	netID := r.NetworkID
	// get the links
	links, err := d.getLinks(netID)
	if err != nil {
		return nil, err
	}

	// Create a macvlan link
	macvlan := &netlink.Macvlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:        "vxlan_" + r.EndpointID[:12],
			ParentIndex: links.Vxlan.LinkAttrs.Index,
		},
		Mode: netlink.MACVLAN_MODE_BRIDGE,
	}
	if err := netlink.LinkAdd(macvlan); err != nil {
		return nil, err
	}

	gateway, err := getGateway(netID, *d.docker)
	if err != nil {
		return nil, err
	}
	res := &network.JoinResponse{
		InterfaceName: network.InterfaceName{
			SrcName:   "vxlan_" + r.EndpointID[:12],
			DstPrefix: "eth",
		},
		Gateway: gateway,
	}
	log.Debugf("Join endpoint %s:%s to %s", r.NetworkID, r.EndpointID, r.SandboxKey)
	return res, nil
}

func (d *Driver) Leave(r *network.LeaveRequest) error {
	names, err := getIntNames(netID, docker)
	if err != nil {
		return nil, err
	}

	linkName := "vxlan_" + r.EndpointID[:12] + "@" + names.VxlanName
	vlanLink, err := netlink.LinkByName(linkName)
	if err != nil {
		return fmt.Errorf("failed to find interface %s on the Docker host : %v", linkName, err)
	}
	// verify a parent interface isn't being deleted
	if vlanLink.Attrs().ParentIndex == 0 {
		return fmt.Errorf("interface %s does not appear to be a slave device: %v", linkName, err)
	}
	// delete the macvlan slave device
	if err := netlink.LinkDel(vlanLink); err != nil {
		return fmt.Errorf("failed to delete  %s link: %v", linkName, err)
	}

	log.Debugf("Deleted subinterface: %s", linkName)
	return nil

}
