package drouter

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/TrilliumIT/iputil"
	dockertypes "github.com/docker/engine-api/types"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/net/context"
)

const (
	ADD_ROUTE = true
	DEL_ROUTE = false
)

func netlinkHandleFromPid(pid int) (*netlink.Handle, error) {
	log.Debugf("Getting NsHandle for pid: %v", pid)
	ns, err := netns.GetFromPid(pid)
	if err != nil {
		return &netlink.Handle{}, err
	}
	nsh, err := netlink.NewHandleAt(ns)
	if err != nil {
		return &netlink.Handle{}, err
	}

	return nsh, nil
}

func insertMasqRule() error {
	//not implemented yet
	return nil
}

func getSelfContainer() (*dockertypes.ContainerJSON, error) {
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
		id := line[len(line)-1]
		containerInfo, err := dockerClient.ContainerInspect(context.Background(), id)
		if err != nil {
			log.Errorf("Error inspecting container: %v", id)
			return nil, err
		}
		return &containerInfo, nil
	}
	return nil, fmt.Errorf("Container not found")
}

func subnetCoveredByStatic(subnet *net.IPNet) bool {
	for _, sr := range staticRoutes {
		if iputil.SubnetContainsSubnet(sr, subnet) {
			return true
		}
	}
	return false
}

func dockerContainerInSubnet(dc *dockertypes.Container, sn *net.IPNet) bool {
	for _, es := range dc.NetworkSettings.Networks {
		ip := net.ParseIP(es.IPAddress)

		if sn.Contains(ip) {
			return true
		}
	}

	return false
}

func dockerContainerSharesNetwork(dc *dockertypes.Container) bool {
	//get drouter routing table
	routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		log.Error("Failed to get drouter routing table.")
		log.Error(err)
		return false
	}

	for _, es := range dc.NetworkSettings.Networks {
		ip := net.ParseIP(es.IPAddress)
		for _, r := range routes {
			if r.Gw != nil {
				continue
			}
			if r.Dst.Contains(ip) {
				return true
			}
		}
	}
	return false
}

func modifyRoute(to, via *net.IPNet, action bool) error {
	var ar *net.IPNet
	switch action {
	case ADD_ROUTE:
		ar = to
	case DEL_ROUTE:
		ar = via
	}

	coveredByStatic := subnetCoveredByStatic(ar)

	//get local containers
	dockerContainers, err := dockerClient.ContainerList(context.Background(), dockertypes.ContainerListOptions{})
	if err != nil {
		log.Error("Failed to get container list.")
		return err
	}

	//do container routes
	for _, dc := range dockerContainers {
		if dc.HostConfig.NetworkMode == "host" {
			continue
		}
		if dc.ID == selfContainerID {
			continue
		}

		//connected to the affected route
		if dockerContainerInSubnet(&dc, ar) {
			//create a container object (inspect and get handle)
			c, err := newContainerFromID(dc.ID)
			if err != nil {
				log.Error(err)
				continue
			}

			switch action {
			case ADD_ROUTE:
				go c.addAllRoutes()
			case DEL_ROUTE:
				go c.delRoutesVia(to, via)
			}
			continue
		}

		//This route is already covered by a static supernet or containerGateway
		if containerGateway || coveredByStatic {
			continue
		}

		//if we don't have a connection, we can't route for it anyway
		if !dockerContainerSharesNetwork(&dc) {
			continue
		}

		//create a container object (inspect and get handle)
		c, err := newContainerFromID(dc.ID)
		if err != nil {
			log.Error(err)
			continue
		}

		switch action {
		case ADD_ROUTE:
			go c.addRoute(to)
		case DEL_ROUTE:
			go c.delRoutesVia(to, via)
		}
	}
	return nil
}
