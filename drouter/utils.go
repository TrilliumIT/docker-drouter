package drouter

import (
	"bufio"
	"fmt"
	log "github.com/Sirupsen/logrus"
	dockertypes "github.com/docker/engine-api/types"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/net/context"
	"net"
	"os"
	"strings"
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

func makeGlobalNet() (supernet *net.IPNet) {
	_, supernet, _ = net.ParseCIDR("0.0.0.0/0")
	return
}

func networkID(n *net.IPNet) *net.IPNet {
	ip := n.IP.To4()
	if ip == nil {
		ip = n.IP
		ip2 := net.IP{
			ip[0] & n.Mask[0],
			ip[1] & n.Mask[1],
			ip[2] & n.Mask[2],
			ip[3] & n.Mask[3],
			ip[4] & n.Mask[4],
			ip[5] & n.Mask[5],
			ip[6] & n.Mask[6],
			ip[7] & n.Mask[7],
			ip[8] & n.Mask[8],
			ip[9] & n.Mask[9],
			ip[10] & n.Mask[10],
			ip[11] & n.Mask[11],
			ip[12] & n.Mask[12],
			ip[13] & n.Mask[13],
			ip[14] & n.Mask[14],
			ip[15] & n.Mask[15],
		}

		ipnet := &net.IPNet{
			IP:   ip2,
			Mask: n.Mask,
		}

		return ipnet
	}
	ip2 := net.IPv4(
		ip[0]&n.Mask[0],
		ip[1]&n.Mask[1],
		ip[2]&n.Mask[2],
		ip[3]&n.Mask[3],
	)

	ipnet := &net.IPNet{
		IP:   ip2,
		Mask: n.Mask,
	}

	return ipnet
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

func subnetEqualSubnet(net1, net2 *net.IPNet) bool {
	if net1.Contains(net2.IP) {
		n1len, n1bits := net1.Mask.Size()
		n2len, n2bits := net2.Mask.Size()
		if n1len == n2len && n1bits == n2bits {
			return true
		}
	}
	return false
}

func subnetContainsSubnet(supernet, subnet *net.IPNet) bool {
	if supernet.Contains(subnet.IP) {
		n1len, n1bits := supernet.Mask.Size()
		n2len, n2bits := subnet.Mask.Size()
		if n1len <= n2len && n1bits == n2bits {
			return true
		}
	}
	return false
}
