package drouter

import (
	log "github.com/Sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"github.com/ziutek/utils/netaddr"
	"net"
	"os"
)

type p2pNetwork struct {
	network       *net.IPNet
	hostIP        net.IP
	selfIP        net.IP
	hostNamespace *netlink.Handle
	hostUnderlay  *net.IPNet
}

func newP2PNetwork(p2paddr string) (*p2pNetwork, error) {
	hns, err := netlinkHandleFromPid(1)
	if err != nil {
		log.Error("Failed to get the host's namespace.")
		return nil, err
	}

	log.Debugf("Making a p2p network for: %v", p2paddr)

	//check if drouter_veth0 already exists
	host_link, err := hns.LinkByName("drouter_veth0")
	if err != nil {
		//doesn't exist, create it
		host_link_veth := &netlink.Veth{
			LinkAttrs: netlink.LinkAttrs{Name: "drouter_veth0"},
			PeerName:  "drouter_veth1",
		}
		err2 := hns.LinkAdd(host_link_veth)
		if err2 != nil {
			return nil, err2
		}
		host_link, err2 = hns.LinkByName("drouter_veth0")
		if err2 != nil {
			return nil, err2
		}
	}

	int_link, err := hns.LinkByName("drouter_veth1")
	if err != nil {
		return nil, err
	}
	err = hns.LinkSetNsPid(int_link, os.Getpid())
	if err != nil {
		return nil, err
	}
	int_link, err = netlink.LinkByName("drouter_veth1")
	if err != nil {
		return nil, err
	}

	_, p2pIPNet, err := net.ParseCIDR(p2paddr)
	if err != nil {
		log.Errorf("Failed to parse the CIDR string for p2p network: %v", p2paddr)
		return nil, err
	}

	host_addr := *p2pIPNet
	host_addr.IP = netaddr.IPAdd(host_addr.IP, 1)
	host_netlink_addr := &netlink.Addr{
		IPNet: &host_addr,
		Label: "",
	}
	err = hns.AddrAdd(host_link, host_netlink_addr)
	if err != nil {
		return nil, err
	}

	int_addr := *p2pIPNet
	int_addr.IP = netaddr.IPAdd(int_addr.IP, 2)
	int_netlink_addr := &netlink.Addr{
		IPNet: &int_addr,
		Label: "",
	}
	err = netlink.AddrAdd(int_link, int_netlink_addr)
	if err != nil {
		return nil, err
	}

	err = netlink.LinkSetUp(int_link)
	if err != nil {
		return nil, err
	}

	err = hns.LinkSetUp(host_link)
	if err != nil {
		return nil, err
	}

	//discover host underlay address/network
	hunderlay := &net.IPNet{}
	hroutes, err := hns.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
Hroutes:
	for _, r := range hroutes {
		if r.Gw != nil {
			continue
		}
		link, err := hns.LinkByIndex(r.LinkIndex)
		if err != nil {
			return nil, err
		}
		addrs, err := hns.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			return nil, err
		}
		for _, addr := range addrs {
			if !addr.IP.Equal(r.Src) {
				continue
			}
			hunderlay.IP = addr.IP
			hunderlay.Mask = addr.Mask
			break Hroutes
		}
	}

	log.Debugf("Discovered host underlay as: %v", hunderlay)

	staticRoutes = append(staticRoutes, networkID(hunderlay))

	hroute := &netlink.Route{
		LinkIndex: int_link.Attrs().Index,
		Dst:       networkID(hunderlay),
		Gw:        host_addr.IP,
	}

	log.Debug("Adding drouter route to %v via %v.", hroute.Dst, hroute.Gw)
	err = netlink.RouteAdd(hroute)
	if err != nil {
		return nil, err
	}

	p2pnet := &p2pNetwork{
		network:       p2pIPNet,
		hostIP:        host_addr.IP,
		selfIP:        int_addr.IP,
		hostNamespace: hns,
		hostUnderlay:  hunderlay,
	}

	for _, sr := range staticRoutes {
		if subnetContainsSubnet(hroute.Dst, sr) {
			log.Debugf("Skipping static route %v covered by host underlay: %v", sr, hroute.Dst)
			continue
		}
		go p2pnet.addHostRoute(sr)
	}

	return p2pnet, nil
}

func (p *p2pNetwork) remove() error {
	host_link, err := p.hostNamespace.LinkByName("drouter_veth0")
	if err != nil {
		return err
	}

	return p.hostNamespace.LinkDel(host_link)
}

func (p *p2pNetwork) addHostRoute(sn *net.IPNet) {
	route := &netlink.Route{
		Gw:  p.selfIP,
		Dst: sn,
		Src: p.hostUnderlay.IP,
	}
	if (route.Dst.IP.To4() == nil) != (route.Gw.To4() == nil) {
		// Dst is a different IP family
		return
	}

	log.Debugf("Injecting shortcut route to %v via drouter into host routing table.", sn)
	err := p.hostNamespace.RouteAdd(route)
	if err != nil {
		log.Error(err)
	}
}

func (p *p2pNetwork) delHostRoute(sn *net.IPNet) {
	route := &netlink.Route{
		Gw:  p.selfIP,
		Dst: sn,
	}
	if (route.Dst.IP.To4() == nil) != (route.Gw.To4() == nil) {
		// Dst is a different IP family
		return
	}

	log.Debugf("Removing shortcut route to %v via drouter from host routing table.", sn)
	err := p.hostNamespace.RouteDel(route)
	if err != nil {
		log.Error(err)
	}
}
