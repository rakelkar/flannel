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
	linkIndex int
	destinationSubnet *net.IPNet
	gatewayAddress net.IP
	routeMetric int
	ifMetric int
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
func runScript(cmdLine string) string {
	// choose a PS backend
	back := &psbe.Local{}

	// start a local powershell process
	shell, err := ps.New(back)
	if err != nil {
		panic(err)
	}
	defer shell.Exit()

	log.Infof("Executing: %v", cmdLine)
	stdout, stderr, err := shell.Execute(cmdLine)
	if err != nil {
		panic(err)
	}

	log.Infof("stdout [%v], stderr [%v]", stdout, stderr)

	return stdout
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

			// TODO: rakesh: make this more efficient and handle cases where the route already exists for a different gw
			route := route{
				destinationSubnet:       evt.Lease.Subnet.ToIPNet(),
				gatewayAddress:        evt.Lease.Attrs.PublicIP.ToIP(),
				linkIndex: n.linkIndex,
			}

			getRouteCmdLine := fmt.Sprintf("get-netroute -InterfaceIndex %v -DestinationPrefix %v -erroraction Ignore", route.linkIndex, route.destinationSubnet.String())
			stdout := runScript(getRouteCmdLine)

			if stdout == "" {
				newRouteCmdLine := fmt.Sprintf("new-netroute -InterfaceIndex %v -DestinationPrefix %v -NextHop  %v -Verbose", route.linkIndex, route.destinationSubnet.String(), route.gatewayAddress.String())
				runScript(newRouteCmdLine)
			}

			n.addToRouteList(route)

		case subnet.EventRemoved:
			log.Info("Subnet removed: ", evt.Lease.Subnet)

			if evt.Lease.Attrs.BackendType != "host-gw" {
				log.Warningf("Ignoring non-host-gw subnet: type=%v", evt.Lease.Attrs.BackendType)
				continue
			}

			route := route{
				destinationSubnet:       evt.Lease.Subnet.ToIPNet(),
				gatewayAddress:        evt.Lease.Attrs.PublicIP.ToIP(),
				linkIndex: n.linkIndex,
			}

			getRouteCmdLine := fmt.Sprint("get-netroute -InterfaceIndex %v -DestinationPrefix %v -erroraction Ignore", route.linkIndex, route.destinationSubnet.String())
			stdout := runScript(getRouteCmdLine)

			if stdout != "" {
				newRouteCmdLine := fmt.Sprint("remove-netroute -InterfaceIndex %v -DestinationPrefix %v -NextHop  %v -Verbose", route.linkIndex, route.destinationSubnet.String(), route.gatewayAddress.String())
				runScript(newRouteCmdLine)
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
}

func routeEqual(x, y route) bool {
	// TODO: rakesh: compare mask also?
	if x.destinationSubnet.IP.Equal(y.destinationSubnet.IP) {
		return true
	}

	return false
}
