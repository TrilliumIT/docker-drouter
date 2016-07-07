package drouter

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	log "github.com/Sirupsen/logrus"
	"github.com/TrilliumIT/iputil"
	dockertypes "github.com/docker/engine-api/types"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/net/context"
)

const (
	addRoute = true
	delRoute = false
)

func logError(msg string, err error) {
	log.WithFields(log.Fields{"err": err}).Error(msg)
}

func netlinkHandleFromPid(pid int) (*netlink.Handle, error) {
	log.WithFields(log.Fields{
		"Pid": pid,
	}).Debug("Getting NsHandle for pid")
	ns, err := netns.GetFromPid(pid)
	if err != nil {
		log.WithFields(log.Fields{
			"Pid":   pid,
			"Error": err,
		}).Error("Faild to get namespace for pid")
		return &netlink.Handle{}, err
	}
	nsh, err := netlink.NewHandleAt(ns)
	if err != nil {
		log.WithFields(log.Fields{
			"Pid":   pid,
			"Error": err,
		}).Error("Faild to get handle for namespace at pid")
		return &netlink.Handle{}, err
	}

	return nsh, nil
}

func insertMasqRule() error {
	log.Debug("Insert mask rule")
	//not implemented yet
	return nil
}

func getSelfContainerID() (string, error) {
	cgroup, err := os.Open("/proc/self/cgroup")
	if err != nil {
		log.Error("Error getting cgroups.")
		return "", err
	}
	defer func() {
		err = cgroup.Close()
		if err != nil {
			log.WithField("Error", err).Error("Failed to close cgroups")
		}
	}()

	scanner := bufio.NewScanner(cgroup)
	for scanner.Scan() {
		line := strings.Split(scanner.Text(), "/")
		return line[len(line)-1], nil
	}
	err = fmt.Errorf("Container not found")
	logError("Failed to get self ID", err)
	return "", err
}

func getSelfContainer() (*dockertypes.ContainerJSON, error) {
	log.Debug("Getting self containerJSON object.")

	id, err := getSelfContainerID()
	if err != nil {
		return nil, err
	}
	var cjson dockertypes.ContainerJSON
	cjson, err = dockerClient.ContainerInspect(context.Background(), id)
	if err != nil {
		log.WithFields(log.Fields{
			"ID":    id,
			"Error": err,
		}).Error("Failed to inspect container")
		return nil, err
	}
	return &cjson, nil
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
		logError("Failed to get drouter routing table", err)
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

func modifyRoute(ar *net.IPNet, action bool) error {

	coveredByStatic := subnetCoveredByStatic(ar)

	//get local containers
	dockerContainers, err := dockerClient.ContainerList(context.Background(), dockertypes.ContainerListOptions{})
	if err != nil {
		logError("Failed to get container list.", err)
		return err
	}

	var modRouteWG sync.WaitGroup
	//do container routes
	for _, dcont := range dockerContainers {
		dc := dcont
		modRouteWG.Add(1)
		go func() {
			defer modRouteWG.Done()
			if dc.HostConfig.NetworkMode == hostNetworkMode {
				return
			}
			if dc.ID == selfContainerID {
				return
			}

			//connected to the affected route
			if dockerContainerInSubnet(&dc, ar) {
				//create a container object (inspect and get handle)
				c, err := newContainerFromID(dc.ID)
				if err != nil {
					log.WithFields(log.Fields{
						"ID":    dc.ID,
						"Error": err,
					}).Error("Failed to get container object.")
					return
				}

				switch action {
				case addRoute:
					err := c.addAllRoutes()
					if err != nil {
						c.logError("Failed to add all routes.", err)
					}
				case delRoute:
					c.delRoutesVia(nil, ar)
				}
				return
			}

			//This route is already covered by a static supernet or containerGateway
			if containerGateway || coveredByStatic {
				return
			}

			//if we don't have a connection, we can't route for it anyway
			if !dockerContainerSharesNetwork(&dc) {
				return
			}

			//create a container object (inspect and get handle)
			c, err := newContainerFromID(dc.ID)
			if err != nil {
				log.WithFields(log.Fields{
					"ID":    dc.ID,
					"Error": err,
				}).Error("Failed to get container object.")
				return
			}

			switch action {
			case addRoute:
				c.addRoute(ar)
			case delRoute:
				c.delRoutesVia(ar, nil)
			}
		}()
	}
	modRouteWG.Wait()
	return nil
}
