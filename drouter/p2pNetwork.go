package drouter

import (
  "net"
	log "github.com/Sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"github.com/ziutek/utils/netaddr"
)

type p2pNetwork struct {
	hostLinkIndex         int
	selfLinkIndex         int
	network               *net.IPNet
	hostIP                net.IP
	selfIP                net.IP
}

func (dr *DistributedRouter) makeP2PLink(p2paddr string) error {
	log.Debugf("Making a p2p network for: %v", p2paddr)
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
	dr.p2p.hostLinkIndex = host_link.Attrs().Index

	int_link, err := dr.hostNamespace.LinkByName("drouter_veth1")
	if err != nil {
		return err
	}
	err = dr.hostNamespace.LinkSetNsPid(int_link, dr.pid)
	if err != nil {
		return err
	}
	int_link, err = dr.selfNamespace.LinkByName("drouter_veth1")
	if err != nil {
		return err
	}
	dr.p2p.selfLinkIndex = int_link.Attrs().Index

	_, p2p_net, err := net.ParseCIDR(p2paddr)
	if err != nil {
		log.Errorf("Failed to parse the CIDR string for p2p network: %v", p2paddr)
		return err
	}
	dr.p2p.network = p2p_net

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
	dr.p2p.hostIP = host_addr.IP

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
	dr.p2p.selfIP = int_addr.IP

	err = dr.selfNamespace.LinkSetUp(int_link)
	if err != nil {
		return err
	}

	err = dr.hostNamespace.LinkSetUp(host_link)
	if err != nil {
		return err
	}

	if dr.localGateway {
		dr.defaultRoute = host_addr.IP
		err = dr.setDefaultRoute()
		if err != nil {
			log.Error("--local-gateway=true and unable to set default route to host's p2p address.")
			return err
		}
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
