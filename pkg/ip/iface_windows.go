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

package ip

import (
	"net"

	log "github.com/golang/glog"
)

func GetIfaceIP4Addr(iface *net.Interface) (net.IP, error) {
	// get ip address for the interface
	// prefer global unicast to link local addresses
	mockAddr := net.IPv4(192, 168, 10, 100)
	log.Infof("MOCK: returning %v for interface %v", mockAddr.String(), iface.Name)
	return mockAddr, nil 
}

func GetDefaultGatewayIface() (*net.Interface, error) {
	iface, err := net.InterfaceByName("Wi-Fi")
	if err != nil {
		log.Error("MOCK: failed to get wifi interface as Default Gateway")
		return nil, err
	}

	log.Infof("MOCK: returning [%v] as default gateway", iface.Name)
	return iface, nil
}

func GetInterfaceByIP(ip net.IP) (*net.Interface, error) {
	iface, err := net.InterfaceByName("Wi-Fi")
	if err != nil {	
		log.Errorf("MOCK: failed to get wifi interface for ip %v", ip.String())
		return nil, err
	}

	log.Infof("MOCK: returning [%v] as inteface for [%v]", iface.Name, ip.String())
	return iface, nil
}