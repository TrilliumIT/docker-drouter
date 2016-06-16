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
	
	//discover host underlay address/network
	hunderlay := &net.IPNet{}
	hroutes, err := dr.hostNamespace.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	Hroutes:
	for _, r := range hroutes {
		if r.Gw != nil { continue }
		link, err := dr.hostNamespace.LinkByIndex(r.LinkIndex)
		if err != nil {
			return err
		}
		addrs, err := dr.hostNamespace.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			return err
		}
		for _, addr := range addrs {
			if !addr.IP.Equal(r.Src) { continue }
			hunderlay.IP = addr.IP
			hunderlay.Mask = addr.Mask
			break Hroutes
		}
	}

	log.Debugf("Discovered host underlay as: %v", hunderlay)

	dr.staticRoutes = append(dr.staticRoutes, NetworkID(hunderlay))
	dr.hostUnderlay = hunderlay

	hroute := &netlink.Route{
		LinkIndex: int_link.Attrs().Index,
		Dst: NetworkID(hunderlay),
		Gw: host_addr.IP,
	}

	log.Debug("Adding drouter route to %v via %v.", hroute.Dst, hroute.Gw)
	err = dr.selfNamespace.RouteAdd(hroute)
	if err != nil {
		return err
	}

	for _, sr := range dr.staticRoutes {
		if hroute.Dst.Contains(sr.IP) {
			srlen, srbits := sr.Mask.Size()
			hrlen, hrbits := hroute.Dst.Mask.Size()
			if hrlen <= srlen && hrbits == srbits {
				log.Debugf("Skipping route %v covered by %v.", hroute.Dst, sr)
				continue
			}
		}
		sroute := &netlink.Route{
			LinkIndex: host_link.Attrs().Index,
			Dst: sr,
			Gw: int_addr.IP,
			Src: hunderlay.IP,
		}

		log.Infof("Adding host route to %v via %v.", sroute.Dst, sroute.Gw)
		err = dr.hostNamespace.RouteAdd(sroute)
		if err != nil {
			log.Error(err)
			continue
		}
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
