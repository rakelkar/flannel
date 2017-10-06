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

package vxlan

// Some design notes:
// VXLAN encapsulates L2 packets (though flannel is L3 only so don't expect to be able to send L2 packets across hosts)
// Windows overlay decap works at L2 and so it needs the correct destination MAC for the remote host to work.
// Windows does not expose an L3Miss interface so for now all possible remote IP/MAC pairs have to be configured upfront.
//
// In this scheme the scaling of table entries (per host) is:
//  - 1 network entry for the overlay network
//  - 1 endpoint per local container
//  - N remote endpoints remote node (total endpoints =
import (
	"encoding/json"
	"fmt"

	log "github.com/golang/glog"

	"golang.org/x/net/context"

	"github.com/coreos/flannel/backend"
	"github.com/coreos/flannel/pkg/ip"
	"github.com/coreos/flannel/subnet"
	"github.com/Microsoft/hcsshim"
)

func init() {
	backend.Register("vxlan", New)
}

const (
	defaultVNI = 1
)

type VXLANBackend struct {
	sm subnet.Manager
	extIface  *backend.ExternalInterface
	networks map[string]*network
}

func init() {
	backend.Register("host-gw", New)
}

const (
	routeCheckRetries = 10
)


func New(sm subnet.Manager, extIface *backend.ExternalInterface) (backend.Backend, error) {

	be := &VXLANBackend{
		sm:       sm,
		extIface: extIface,
		networks: make(map[string]*network),
	}

	return be, nil
}

func (be *VXLANBackend) RegisterNetwork(ctx context.Context, config *subnet.Config) (backend.Network, error) {
	// TODO: are these used? how to pass to HNS?
	cfg := struct {
		VNI           int
		Port          int
		GBP           bool
		DirectRouting bool
	}{
		VNI: defaultVNI,
	}

	if len(config.Backend) > 0 {
		if err := json.Unmarshal(config.Backend, &cfg); err != nil {
			return nil, fmt.Errorf("error decoding VXLAN backend config: %v", err)
		}
	}
	log.Infof("VXLAN config: VNI=%d Port=%d GBP=%v DirectRouting=%v", cfg.VNI, cfg.Port, cfg.GBP, cfg.DirectRouting)

	n := &network{
		extIface:  be.extIface,
		sm:        be.sm,
		name:      be.extIface.Iface.Name,
	}

	attrs := subnet.LeaseAttrs{
		PublicIP:    ip.FromIP(be.extIface.ExtAddr),
		BackendType: "vxlan",
	}

	l, err := be.sm.AcquireLease(ctx, &attrs)
	switch err {
	case nil:
		n.lease = l

	case context.Canceled, context.DeadlineExceeded:
		return nil, err

	default:
		return nil, fmt.Errorf("failed to acquire lease: %v", err)
	}

	// check if the network exists and has the expected settings?
	networkName := fmt.Sprintf("flannel.%v", cfg.VNI)
	createNetwork := true
	addressPrefix := config.Network
	networkGatewayAddress := config.Network.IP + 1
	hnsNetwork, err := hcsshim.GetHNSNetworkByName(networkName)
	if err == nil && vsidMatches(hnsNetwork, cfg.VNI) {
		log.Infof("Found existing HNS network [%+v]", hnsNetwork)
		n.networkId = hnsNetwork.Id
		createNetwork = false
	}

	if createNetwork {
		// create, but a network with the same name exists?
		if hnsNetwork != nil {
			if _, err := hnsNetwork.Delete(); err != nil {
				return nil, fmt.Errorf("unable to delete existing network [%v], error: %v", hnsNetwork.Name, err)
			}
			log.Infof("Deleted stale HNS network [%v]")
		}

		// create the underlying windows HNS network
		request := map[string]interface{}{
			"Name": networkName,
			"Type": "Overlay",
			"Subnets": []interface{}{
				map[string]interface{}{
					"AddressPrefix":  addressPrefix,
					"GatewayAddress": networkGatewayAddress,
					"Policies": []interface{}{
						map[string]interface{}{
							"Type": "VSID",
							"VSID": cfg.VNI,
						},
					},
				},
			},
		}

		jsonRequest, err := json.Marshal(request)
		if err != nil {
			return nil, err
		}

		log.Infof("Attempting to create HNS network, request: %v", string(jsonRequest))
		hnsNetwork, err := hcsshim.HNSNetworkRequest("POST", "", string(jsonRequest))
		if err != nil {
			return nil, fmt.Errorf("unable to create network [%v], error: %v", networkName, err)
		}
		log.Infof("Created HNS network [%v] as %+v", networkName, hnsNetwork)
		n.networkId = hnsNetwork.Id
	}

	return n, nil
}

func vsidMatches(network *hcsshim.HNSNetwork, vsid int) bool {
	if network == nil || len(network.Policies) < 1 {
		return false
	}

	for _, policyJson := range network.Policies {
		vsidPolicy := struct {
			Type          string
			VSID          int
		}{}

		if err := json.Unmarshal(policyJson, &vsidPolicy); err == nil {
			if vsidPolicy.Type == "VSID" && vsidPolicy.VSID == vsid {
				return true
			}
		}
	}

	return false
}
