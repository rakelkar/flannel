// +build windows

// Copyright 2015 flannel authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hostgw

import (
	"net"
	"sync"
	"time"

	log "github.com/golang/glog"
	"golang.org/x/net/context"

	"github.com/coreos/flannel/backend"
	"github.com/coreos/flannel/subnet"

	ps "github.com/gorillalabs/go-powershell"
	psbe "github.com/gorillalabs/go-powershell/backend"

	"fmt"
	"regexp"
	"strings"
	"bytes"
	"bufio"
	"strconv"
)

type network struct {
	name      string
	extIface  *backend.ExternalInterface
	linkIndex int
	rl        []route
	lease     *subnet.Lease
	sm        subnet.Manager
}

type route struct {
	linkIndex         int
	destinationSubnet *net.IPNet
	gatewayAddress    net.IP
	routeMetric       int
	ifMetric          int
}

func (n *network) Lease() *subnet.Lease {
	return n.lease
}

func (n *network) MTU() int {
	return n.extIface.Iface.MTU
}

func (n *network) Run(ctx context.Context) {
	wg := sync.WaitGroup{}

	log.Info("Watching for new subnet leases")
	evts := make(chan []subnet.Event)
	wg.Add(1)
	go func() {
		subnet.WatchLeases(ctx, n.sm, n.lease, evts)
		wg.Done()
	}()

	n.rl = make([]route, 0, 10)
	wg.Add(1)
	go func() {
		n.routeCheck(ctx)
		wg.Done()
	}()

	defer wg.Wait()

	for {
		select {
		case evtBatch := <-evts:
			n.handleSubnetEvents(evtBatch)

		case <-ctx.Done():
			return
		}
	}
}
func (n *network) handleSubnetEvents(batch []subnet.Event) {
	for _, evt := range batch {
		switch evt.Type {
		case subnet.EventAdded:
			log.Infof("Subnet added: %v via %v", evt.Lease.Subnet, evt.Lease.Attrs.PublicIP)

			if evt.Lease.Attrs.BackendType != "host-gw" {
				log.Warningf("Ignoring non-host-gw subnet: type=%v", evt.Lease.Attrs.BackendType)
				continue
			}

			route := route{
				destinationSubnet: evt.Lease.Subnet.ToIPNet(),
				gatewayAddress:    evt.Lease.Attrs.PublicIP.ToIP(),
				linkIndex:         n.linkIndex,
			}

			getRouteCmdLine := fmt.Sprintf("get-netroute -InterfaceIndex %v -DestinationPrefix %v -erroraction Ignore", route.linkIndex, route.destinationSubnet.String())
			stdout, _ := runScript(getRouteCmdLine)

			existingRoutes := parseRoutesList(stdout)
			if len(existingRoutes) > 0 {
				if routeEqual(existingRoutes[0], route) {
					continue
				}

				log.Warningf("Replacing existing route to %v via %v with %v via %v.", evt.Lease.Subnet, existingRoutes[0].gatewayAddress, evt.Lease.Subnet, evt.Lease.Attrs.PublicIP)
				removeRouteCmdLine := fmt.Sprint("remove-netroute -InterfaceIndex %v -DestinationPrefix %v -NextHop  %v -Verbose", route.linkIndex, route.destinationSubnet.String(), existingRoutes[0].gatewayAddress.String())
				_, err := runScript(removeRouteCmdLine)
				if err != nil {
					log.Errorf("Error removing route: %v", err)
					continue
				}
			}

			newRouteCmdLine := fmt.Sprintf("new-netroute -InterfaceIndex %v -DestinationPrefix %v -NextHop  %v -Verbose", route.linkIndex, route.destinationSubnet.String(), route.gatewayAddress.String())
			_, err := runScript(newRouteCmdLine)
			if err != nil {
				log.Errorf("Error creating route: %v", err)
			}

			n.addToRouteList(route)

		case subnet.EventRemoved:
			log.Info("Subnet removed: ", evt.Lease.Subnet)

			if evt.Lease.Attrs.BackendType != "host-gw" {
				log.Warningf("Ignoring non-host-gw subnet: type=%v", evt.Lease.Attrs.BackendType)
				continue
			}

			route := route{
				destinationSubnet: evt.Lease.Subnet.ToIPNet(),
				gatewayAddress:    evt.Lease.Attrs.PublicIP.ToIP(),
				linkIndex:         n.linkIndex,
			}

			getRouteCmdLine := fmt.Sprint("get-netroute -InterfaceIndex %v -DestinationPrefix %v -erroraction Ignore", route.linkIndex, route.destinationSubnet.String())
			stdout, _ := runScript(getRouteCmdLine)

			if stdout != "" {
				removeRouteCmdLine := fmt.Sprint("remove-netroute -InterfaceIndex %v -DestinationPrefix %v -NextHop  %v -Verbose", route.linkIndex, route.destinationSubnet.String(), route.gatewayAddress.String())
				_, err := runScript(removeRouteCmdLine)
				if err != nil {
					log.Errorf("Error removing route: %v", err)
				}
			}

			n.removeFromRouteList(route)

		default:
			log.Error("Internal error: unknown event type: ", int(evt.Type))
		}
	}
}

func (n *network) addToRouteList(route route) {
	for _, r := range n.rl {
		if routeEqual(r, route) {
			return
		}
	}
	n.rl = append(n.rl, route)
}

func (n *network) removeFromRouteList(route route) {
	for index, r := range n.rl {
		if routeEqual(r, route) {
			n.rl = append(n.rl[:index], n.rl[index+1:]...)
			return
		}
	}
}

func (n *network) routeCheck(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(routeCheckRetries * time.Second):
			n.checkSubnetExistInRoutes()
		}
	}
}

func (n *network) checkSubnetExistInRoutes() {
	getRouteCmdLine := "get-netroute -erroraction Ignore"
	stdout, err := runScript(getRouteCmdLine)
	if err != nil {
		log.Errorf("Error enumerating routes", err)
		return
	}
	currentRoutes := parseRoutesList(stdout)
	for _, r := range n.rl {
		exist := false
		for _, currentRoute := range currentRoutes {
			if routeEqual(r, currentRoute) {
				exist = true
				break
			}
		}

		if !exist {
			newRouteCmdLine := fmt.Sprintf("new-netroute -InterfaceIndex %v -DestinationPrefix %v -NextHop  %v -Verbose", r.linkIndex, r.destinationSubnet.String(), r.gatewayAddress.String())
			_, err := runScript(newRouteCmdLine)
			if err != nil {
				log.Errorf("Error recovering route %v (%v)", newRouteCmdLine, err)
				continue
			}
			log.Errorf("Recovered route %v", newRouteCmdLine)
		}
	}
}

func parseRoutesList(stdout string) []route {
	internalWhitespaceRegEx := regexp.MustCompile(`[\s\p{Zs}]{2,}`)
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	var routes []route
	for scanner.Scan() {
		line := internalWhitespaceRegEx.ReplaceAllString(scanner.Text(), "|")
		if strings.HasPrefix(line, "ifIndex") || strings.HasPrefix(line, "----") {
			continue
		}

		parts := strings.Split(line, "|")
		if len(parts) != 5 {
			continue
		}

		linkIndex, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}

		gatewayAddress := net.ParseIP(parts[2])
		if gatewayAddress == nil {
			continue
		}

		_, destinationSubnet, err := net.ParseCIDR(parts[1])
		if err != nil {
			continue
		}
		route := route{
			destinationSubnet: destinationSubnet,
			gatewayAddress:    gatewayAddress,
			linkIndex:         linkIndex,
		}

		routes = append(routes, route)
	}

	return routes
}

func routeEqual(x, y route) bool {
	if x.destinationSubnet.IP.Equal(y.destinationSubnet.IP) && x.gatewayAddress.Equal(y.gatewayAddress) && bytes.Equal(x.destinationSubnet.Mask, y.destinationSubnet.Mask) {
		return true
	}

	return false
}

func runScript(cmdLine string) (string, error) {
	// choose a PS backend
	back := &psbe.Local{}

	// start a local powershell process
	shell, err := ps.New(back)
	if err != nil {
		return "", err
	}
	defer shell.Exit()

	log.Infof("Executing: %v", cmdLine)
	stdout, stderr, err := shell.Execute(cmdLine)
	if err != nil {
		return "", err
	}

	log.Infof("stdout [%v], stderr [%v]", stdout, stderr)

	return stdout, nil
}
