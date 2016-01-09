package vxlan

import (
	//"fmt"
	//"strings"
	//"time"
	//"net"
	"strconv"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/go-plugins-helpers/network"
	//"github.com/samalba/dockerclient"
	//"github.com/davecgh/go-spew/spew"
	"github.com/vishvananda/netlink"
)

type Driver struct {
	network.Driver
	networks map[string]*NetworkState
}

// NetworkState is filled in at network creation time
// it contains state that we wish to keep for each network
type NetworkState struct {
	Bridge *netlink.Bridge
	VXLan  *netlink.Vxlan
}

func NewDriver() (*Driver, error) {
	d := &Driver{
		networks: make(map[string]*NetworkState),
	}
	return d, nil
}

func (d *Driver) CreateNetwork(r *network.CreateNetworkRequest) error {
	log.Debugf("Create network request: %+v", r)

	netID := r.NetworkID
	var err error

	vxlanName := "vx_" + netID[0:12]
	bridgeName := "br_" + netID[0:12]

	// Parse interface name options
	for k, v := range r.Options {
		if k == "com.docker.network.generic" {
			if genericOpts, ok := v.(map[string]interface{}); ok {
				for key, val := range genericOpts {
					log.Debugf("Libnetwork Opts Sent: [ %s ] Value: [ %s ]", key, val)
					if key == "vxlanName" {
						log.Debugf("Libnetwork Opts Sent: [ %s ] Value: [ %s ]", key, val)
						vxlanName = val.(string)
					}
					if key == "bridgeName" {
						log.Debugf("Libnetwork Opts Sent: [ %s ] Value: [ %s ]", key, val)
						bridgeName = val.(string)
					}
				}
			}
		}
	}

	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{Name: bridgeName},
	}
	vxlan := &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{Name: vxlanName},
	}

	// Parse other options
	for k, v := range r.Options {
		if k == "com.docker.network.generic" {
			if genericOpts, ok := v.(map[string]interface{}); ok {
				for key, val := range genericOpts {
					if key == "vxlanName" {
						continue
					}
					if key == "bridgeName" {
						continue
					}
					log.Debugf("Libnetwork Opts Sent: [ %s ] Value: [ %s ]", key, val)
					if key == "VxlanId" {
						vxlan.VxlanId, err = strconv.Atoi(val.(string))
						if err != nil {
							return err
						}
					}
					if key == "VtepDev" {
						vtepDev, err = netlink.LinkByName(val.(string))
						if err != nil {
							return err
						}
						vxlan.VtepDevIndex = vtepDev.Attrs().Index
					}
					if key == "SrcAddr" {
						vxlan.SrcAddr, err = net.ParseIP(val.(string))
						if err != nil {
							return err
						}
					}
					if key == "Group" {
						vxlan.Group, err = net.ParseIP(val.(string))
						if err != nil {
							return err
						}
					}
					if key == "TTL" {
						vxlan.TTL, err = strconv.Atoi(val.(string))
						if err != nil {
							return err
						}
					}
					if key == "TOS" {
						vxlan.TOS, err = strconv.Atoi(val.(string))
						if err != nil {
							return err
						}
					}
					if key == "Learning" {
						vxlan.Learning, err = strconv.ParseBool(val.(string))
						if err != nil {
							return err
						}
					}
					if key == "Proxy" {
						vxlan.Proxy, err = strconv.ParseBool(val.(string))
						if err != nil {
							return err
						}
					}
					if key == "RSC" {
						vxlan.RSC, err = strconv.ParseBool(val.(string))
						if err != nil {
							return err
						}
					}
					if key == "L2miss" {
						vxlan.L2miss, err = strconv.ParseBool(val.(string))
						if err != nil {
							return err
						}
					}
					if key == "L3miss" {
						vxlan.L3miss, err = strconv.ParseBool(val.(string))
						if err != nil {
							return err
						}
					}
					if key == "NoAge" {
						vxlan.NoAge, err = strconv.ParseBool(val.(string))
						if err != nil {
							return err
						}
					}
					if key == "GBP" {
						vxlan.GBP, err = strconv.ParseBool(val.(string))
						if err != nil {
							return err
						}
					}
					if key == "Age" {
						vxlan.Age, err = strconv.Atoi(val.(string))
						if err != nil {
							return err
						}
					}
					if key == "Limit" {
						vxlan.Limit, err = strconv.Atoi(val.(string))
						if err != nil {
							return err
						}
					}
					if key == "Port" {
						vxlan.Port, err = strconv.Atoi(val.(string))
						if err != nil {
							return err
						}
					}
					if key == "PortLow" {
						vxlan.PortLow, err = strconv.Atoi(val.(string))
						if err != nil {
							return err
						}
					}
					if key == "PortHigh" {
						vxlan.PortHigh, err = strconv.Atoi(val.(string))
						if err != nil {
							return err
						}
					}
				}
			}
		}
	}

	err = netlink.LinkAdd(bridge)
	if err != nil {
		return err
	}

	err = netlink.LinkAdd(vxlan)
	if err != nil {
		return err
	}

	ns := &NetworkState{
		VXLan:  vxlan,
		Bridge: bridge,
	}
	d.networks[netID] = ns

	err = netlink.LinkSetMaster(vxlan, bridge)
	if err != nil {
		return err
	}

	return nil
}

func (d *Driver) DeleteNetwork(r *network.DeleteNetworkRequest) error {
	netID := r.NetworkID

	err := netlink.LinkDel(d.networks[netID].VXLan)
	if err != nil {
		return err
	}
	err = netlink.LinkDel(d.networks[netID].Bridge)
	if err != nil {
		return err
	}

	return nil
}
