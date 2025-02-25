// Copyright 2015 CNI authors
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

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/rpc"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/coreos/go-systemd/v22/activation"
)

var errNoMoreTries = errors.New("no more tries")

type DHCP struct {
	mux             sync.Mutex
	leases          map[string]*DHCPLease
	hostNetnsPrefix string
	clientTimeout   time.Duration
	clientResendMax time.Duration
	broadcast       bool
}

func newDHCP(clientTimeout, clientResendMax time.Duration) *DHCP {
	return &DHCP{
		leases:          make(map[string]*DHCPLease),
		clientTimeout:   clientTimeout,
		clientResendMax: clientResendMax,
	}
}

// TODO: current client ID is too long. At least the container ID should not be used directly.
// A seperate issue is necessary to ensure no breaking change is affecting other users.
func generateClientID(containerID string, netName string, ifName string) string {
	clientID := containerID + "/" + netName + "/" + ifName
	// defined in RFC 2132, length size can not be larger than 1 octet. So we truncate 254 to make everyone happy.
	if len(clientID) > 254 {
		clientID = clientID[0:254]
	}
	return clientID
}

func combineCNIArgs(envCNIArgs string, genericIpamArgs GenericIPAMArgs) string {
	cniArgs := envCNIArgs

	for ipamArg, value := range genericIpamArgs {
		if stringVal, ok := value.(string); ok {
			cniArgs += fmt.Sprintf(";%s=%s", ipamArg, stringVal)
		}
	}
	return cniArgs
}

// Allocate acquires an IP from a DHCP server for a specified container.
// The acquired lease will be maintained until Release() is called.
func (d *DHCP) Allocate(args *skel.CmdArgs, result *current.Result) error {
	conf := NetConf{}
	if err := json.Unmarshal(args.StdinData, &conf); err != nil {
		return fmt.Errorf("error parsing netconf: %v", err)
	}

	netconfCniArgs := GenericIPAMArgs{}
	if conf.Args != nil {
		netconfCniArgs = conf.Args.A
	}
	cniArgs := combineCNIArgs(args.Args, netconfCniArgs)

	optsRequesting, optsProviding, err := prepareOptions(cniArgs, conf.IPAM.ProvideOptions, conf.IPAM.RequestOptions)
	if err != nil {
		return err
	}

	ipamArgs := IPAMArgs{}
	if conf.Args != nil {
		ipamArgsJson, _ := json.Marshal(conf.Args.A)
		if err := json.Unmarshal(ipamArgsJson, &ipamArgs); err != nil {
			return fmt.Errorf("error parsing ipamArgs: %v", err)
		}
	}
	clientID := generateClientID(args.ContainerID, conf.Name, args.IfName)
	leaseKey := clientID
	if ipamArgs.ClientID != "" {
		clientID = ipamArgs.ClientID
	}
	hostNetns := d.hostNetnsPrefix + args.Netns
	l, err := AcquireLease(clientID, hostNetns, args.IfName,
		optsRequesting, optsProviding,
		d.clientTimeout, d.clientResendMax, d.broadcast)
	if err != nil {
		return err
	}

	ipn, err := l.IPNet()
	if err != nil {
		l.Stop()
		return err
	}

	d.setLease(leaseKey, l)

	result.IPs = []*current.IPConfig{{
		Address: *ipn,
		Gateway: l.Gateway(),
	}}
	result.Routes = l.Routes()

	return nil
}

// Release stops maintenance of the lease acquired in Allocate()
// and sends a release msg to the DHCP server.
func (d *DHCP) Release(args *skel.CmdArgs, reply *struct{}) error {
	conf := NetConf{}
	if err := json.Unmarshal(args.StdinData, &conf); err != nil {
		return fmt.Errorf("error parsing netconf: %v", err)
	}

	ipamArgs := IPAMArgs{}
	if conf.Args != nil {
		ipamArgsJson, _ := json.Marshal(conf.Args.A)
		if err := json.Unmarshal(ipamArgsJson, &ipamArgs); err != nil {
			return fmt.Errorf("error parsing ipamArgs: %v", err)
		}
	}

	clientID := generateClientID(args.ContainerID, conf.Name, args.IfName)
	leaseKey := clientID

	if ipamArgs.ClientID != "" {
		clientID = ipamArgs.ClientID
	}
	if l := d.getLease(leaseKey); l != nil {
		l.Stop()
		d.clearLease(leaseKey)
	}

	return nil
}

func (d *DHCP) getLease(clientID string) *DHCPLease {
	d.mux.Lock()
	defer d.mux.Unlock()

	// TODO(eyakubovich): hash it to avoid collisions
	l, ok := d.leases[clientID]
	if !ok {
		return nil
	}
	return l
}

func (d *DHCP) setLease(clientID string, l *DHCPLease) {
	d.mux.Lock()
	defer d.mux.Unlock()

	// TODO(eyakubovich): hash it to avoid collisions
	d.leases[clientID] = l
}

//func (d *DHCP) clearLease(contID, netName, ifName string) {
func (d *DHCP) clearLease(clientID string) {
	d.mux.Lock()
	defer d.mux.Unlock()

	// TODO(eyakubovich): hash it to avoid collisions
	delete(d.leases, clientID)
}

func getListener(socketPath string) (net.Listener, error) {
	l, err := activation.Listeners()
	if err != nil {
		return nil, err
	}

	switch {
	case len(l) == 0:
		if err := os.MkdirAll(filepath.Dir(socketPath), 0700); err != nil {
			return nil, err
		}
		return net.Listen("unix", socketPath)

	case len(l) == 1:
		if l[0] == nil {
			return nil, fmt.Errorf("LISTEN_FDS=1 but no FD found")
		}
		return l[0], nil

	default:
		return nil, fmt.Errorf("Too many (%v) FDs passed through socket activation", len(l))
	}
}

func runDaemon(
	pidfilePath, hostPrefix, socketPath string,
	dhcpClientTimeout time.Duration, resendMax time.Duration, broadcast bool,
) error {
	// since other goroutines (on separate threads) will change namespaces,
	// ensure the RPC server does not get scheduled onto those
	runtime.LockOSThread()

	// Write the pidfile
	if pidfilePath != "" {
		if !filepath.IsAbs(pidfilePath) {
			return fmt.Errorf("Error writing pidfile %q: path not absolute", pidfilePath)
		}
		if err := ioutil.WriteFile(pidfilePath, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
			return fmt.Errorf("Error writing pidfile %q: %v", pidfilePath, err)
		}
	}

	l, err := getListener(hostPrefix + socketPath)
	if err != nil {
		return fmt.Errorf("Error getting listener: %v", err)
	}

	dhcp := newDHCP(dhcpClientTimeout, resendMax)
	dhcp.hostNetnsPrefix = hostPrefix
	dhcp.broadcast = broadcast
	rpc.Register(dhcp)
	rpc.HandleHTTP()
	http.Serve(l, nil)
	return nil
}
