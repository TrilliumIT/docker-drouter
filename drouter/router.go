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
	//"github.com/vishvananda/netns"
)

var (
	docker *dockerclient.DockerClient
	self_container dockerclient.Container
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

	//FIXME: Create p2p network and join
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
	return docker.ConnectNetwork(net.ID, self_container.Id)
}

func leaveNet(net *dockerclient.NetworkResource) error {
	return docker.DisconnectNetwork(net.ID, self_container.Id)
}

func getSelf() (dockerclient.Container error) {
	my_pid := os.Getpid()
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
