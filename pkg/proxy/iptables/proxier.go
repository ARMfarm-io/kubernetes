/*
Copyright 2015 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package iptables

/*
NOTE: this needs to be tested in e2e since it uses iptables for everything.
*/

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-semver/semver"
	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/proxy"
	"k8s.io/kubernetes/pkg/types"
	utilexec "k8s.io/kubernetes/pkg/util/exec"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
	"k8s.io/kubernetes/pkg/util/slice"
)

// NOTE: IPTABLES_MIN_VERSION is the minimum version of iptables for which we will use the Proxier
// from this package instead of the userspace Proxier.
// This is will not be enough, as the version number is somewhat unreliable,
// features are backported in various distros and this could get pretty hairy.
// However iptables-1.4.0 was released 2007-Dec-22 and appears to have every feature we use,
// so this seems prefectly reasonable for now.
const IPTABLES_MIN_VERSION string = "1.4.0"

// the services chain
const iptablesServicesChain utiliptables.Chain = "KUBE-SERVICES"

// the nodeports chain
const iptablesNodePortsChain utiliptables.Chain = "KUBE-NODEPORTS"

// the mark we apply to traffic needing SNAT
const iptablesMasqueradeMark = "0x4d415351"

// ShouldUseIptablesProxier returns true if we should use the iptables Proxier instead of
// the userspace Proxier.
// This is determined by the iptables version. It may return an erorr if it fails to get the
// itpables version without error, in which case it will also return false.
func ShouldUseIptablesProxier() (bool, error) {
	exec := utilexec.New()
	minVersion, err := semver.NewVersion(IPTABLES_MIN_VERSION)
	if err != nil {
		return false, err
	}
	// returns "vX.X.X", err
	versionString, err := utiliptables.GetIptablesVersionString(exec)
	if err != nil {
		return false, err
	}
	// make a semver of the part after the v in "vX.X.X"
	version, err := semver.NewVersion(versionString[1:])
	if err != nil {
		return false, err
	}
	return !version.LessThan(*minVersion), nil
}

// internal struct for string service information
type serviceInfo struct {
	clusterIP           net.IP
	port                int
	protocol            api.Protocol
	nodePort            int
	loadBalancerStatus  api.LoadBalancerStatus
	sessionAffinityType api.ServiceAffinity
	stickyMaxAgeSeconds int
	endpoints           []string
	// Deprecated, but required for back-compat (including e2e)
	deprecatedPublicIPs []string
}

// returns a new serviceInfo struct
func newServiceInfo(service proxy.ServicePortName) *serviceInfo {
	return &serviceInfo{
		sessionAffinityType: api.ServiceAffinityNone, // default
		stickyMaxAgeSeconds: 180,                     // TODO: paramaterize this in the API.
	}
}

// Proxier is an iptables based proxy for connections between a localhost:lport
// and services that provide the actual backends.
type Proxier struct {
	mu                          sync.Mutex // protects serviceMap
	serviceMap                  map[proxy.ServicePortName]*serviceInfo
	syncPeriod                  time.Duration
	iptables                    utiliptables.Interface
	haveReceivedServiceUpdate   bool // true once we've seen an OnServiceUpdate event
	haveReceivedEndpointsUpdate bool // true once we've seen an OnEndpointsUpdate event
}

// Proxier implements ProxyProvider
var _ proxy.ProxyProvider = &Proxier{}

// NewProxier returns a new Proxier given an iptables Interface instance.
// Because of the iptables logic, it is assumed that there is only a single Proxier active on a machine.
// An error will be returned if iptables fails to update or acquire the initial lock.
// Once a proxier is created, it will keep iptables up to date in the background and
// will not terminate if a particular iptables call fails.
func NewProxier(ipt utiliptables.Interface, syncPeriod time.Duration) (*Proxier, error) {
	glog.V(2).Info("Tearing down userspace rules. Errors here are acceptable.")
	// remove iptables rules/chains from the userspace Proxier
	tearDownUserspaceIptables(ipt)
	return &Proxier{
		serviceMap: make(map[proxy.ServicePortName]*serviceInfo),
		syncPeriod: syncPeriod,
		iptables:   ipt,
	}, nil
}

// Chains from the userspace proxy
// TODO: Remove these Chains and tearDownUserspaceIptables once the userspace Proxier has been removed.
var iptablesContainerPortalChain utiliptables.Chain = "KUBE-PORTALS-CONTAINER"
var iptablesHostPortalChain utiliptables.Chain = "KUBE-PORTALS-HOST"
var iptablesContainerNodePortChain utiliptables.Chain = "KUBE-NODEPORT-CONTAINER"
var iptablesHostNodePortChain utiliptables.Chain = "KUBE-NODEPORT-HOST"

// tearDownUserspaceIptables removes all iptables rules and chains created by the userspace Proxier
func tearDownUserspaceIptables(ipt utiliptables.Interface) {
	// NOTE: Warning, this needs to be kept in sync with the userspace Proxier,
	// we want to ensure we remove all of the iptables rules it creates.
	// Currently they are all in iptablesInit()
	// Delete Rules first, then Flush and Delete Chains
	args := []string{"-m", "comment", "--comment", "handle ClusterIPs; NOTE: this must be before the NodePort rules"}
	if err := ipt.DeleteRule(utiliptables.TableNAT, utiliptables.ChainOutput, append(args, "-j", string(iptablesHostPortalChain))...); err != nil {
		glog.Errorf("Error removing userspace rule: %v", err)
	}
	if err := ipt.DeleteRule(utiliptables.TableNAT, utiliptables.ChainPrerouting, append(args, "-j", string(iptablesContainerPortalChain))...); err != nil {
		glog.Errorf("Error removing userspace rule: %v", err)
	}
	args = []string{"-m", "addrtype", "--dst-type", "LOCAL"}
	args = append(args, "-m", "comment", "--comment", "handle service NodePorts; NOTE: this must be the last rule in the chain")
	if err := ipt.DeleteRule(utiliptables.TableNAT, utiliptables.ChainOutput, append(args, "-j", string(iptablesHostNodePortChain))...); err != nil {
		glog.Errorf("Error removing userspace rule: %v", err)
	}
	if err := ipt.DeleteRule(utiliptables.TableNAT, utiliptables.ChainPrerouting, append(args, "-j", string(iptablesContainerNodePortChain))...); err != nil {
		glog.Errorf("Error removing userspace rule: %v", err)
	}

	// flush and delete chains.
	chains := []utiliptables.Chain{iptablesContainerPortalChain, iptablesHostPortalChain, iptablesHostNodePortChain, iptablesContainerNodePortChain}
	for _, c := range chains {
		// flush chain, then if sucessful delete, delete will fail if flush fails.
		if err := ipt.FlushChain(utiliptables.TableNAT, c); err != nil {
			glog.Errorf("Error flushing userspace chain: %v", err)
		} else {
			if err = ipt.DeleteChain(utiliptables.TableNAT, c); err != nil {
				glog.Errorf("Error flushing userspace chain: %v", err)
			}
		}
	}
}

func (proxier *Proxier) sameConfig(info *serviceInfo, service *api.Service, port *api.ServicePort) bool {
	if info.protocol != port.Protocol || info.port != port.Port || info.nodePort != port.NodePort {
		return false
	}
	if !info.clusterIP.Equal(net.ParseIP(service.Spec.ClusterIP)) {
		return false
	}
	if !ipsEqual(info.deprecatedPublicIPs, service.Spec.DeprecatedPublicIPs) {
		return false
	}
	if !api.LoadBalancerStatusEqual(&info.loadBalancerStatus, &service.Status.LoadBalancer) {
		return false
	}
	if info.sessionAffinityType != service.Spec.SessionAffinity {
		return false
	}
	return true
}

func ipsEqual(lhs, rhs []string) bool {
	if len(lhs) != len(rhs) {
		return false
	}
	for i := range lhs {
		if lhs[i] != rhs[i] {
			return false
		}
	}
	return true
}

// SyncLoop runs periodic work.  This is expected to run as a goroutine or as the main loop of the app.  It does not return.
func (proxier *Proxier) SyncLoop() {
	t := time.NewTicker(proxier.syncPeriod)
	defer t.Stop()
	for {
		<-t.C
		glog.V(6).Infof("Periodic sync")
		func() {
			proxier.mu.Lock()
			defer proxier.mu.Unlock()
			if err := proxier.syncProxyRules(); err != nil {
				glog.Errorf("Failed to sync iptables rules: %v", err)
			}
		}()
	}
}

// OnServiceUpdate tracks the active set of service proxies.
// They will be synchronized using syncProxyRules()
func (proxier *Proxier) OnServiceUpdate(allServices []api.Service) {
	proxier.mu.Lock()
	defer proxier.mu.Unlock()
	proxier.haveReceivedServiceUpdate = true

	glog.V(4).Infof("Received service update notice: %+v", allServices)
	activeServices := make(map[proxy.ServicePortName]bool) // use a map as a set

	for i := range allServices {
		service := &allServices[i]

		// if ClusterIP is "None" or empty, skip proxying
		if !api.IsServiceIPSet(service) {
			glog.V(3).Infof("Skipping service %s due to clusterIP = %q", types.NamespacedName{Namespace: service.Namespace, Name: service.Name}, service.Spec.ClusterIP)
			continue
		}

		for i := range service.Spec.Ports {
			servicePort := &service.Spec.Ports[i]

			serviceName := proxy.ServicePortName{NamespacedName: types.NamespacedName{Namespace: service.Namespace, Name: service.Name}, Port: servicePort.Name}
			activeServices[serviceName] = true
			info, exists := proxier.serviceMap[serviceName]
			if exists && proxier.sameConfig(info, service, servicePort) {
				// Nothing changed.
				continue
			}
			if exists {
				//Something changed.
				glog.V(4).Infof("Something changed for service %q: removing it", serviceName)
				delete(proxier.serviceMap, serviceName)
			}

			serviceIP := net.ParseIP(service.Spec.ClusterIP)
			glog.V(1).Infof("Adding new service %q at %s:%d/%s", serviceName, serviceIP, servicePort.Port, servicePort.Protocol)
			info = newServiceInfo(serviceName)
			info.clusterIP = serviceIP
			info.port = servicePort.Port
			info.protocol = servicePort.Protocol
			info.nodePort = servicePort.NodePort
			info.deprecatedPublicIPs = service.Spec.DeprecatedPublicIPs
			// Deep-copy in case the service instance changes
			info.loadBalancerStatus = *api.LoadBalancerStatusDeepCopy(&service.Status.LoadBalancer)
			info.sessionAffinityType = service.Spec.SessionAffinity
			proxier.serviceMap[serviceName] = info

			glog.V(4).Infof("info: %+v", info)
		}
	}

	for name, info := range proxier.serviceMap {
		// Check for servicePorts that were not in this update and have no endpoints.
		// This helps prevent unnecessarily removing and adding services.
		if !activeServices[name] && info.endpoints == nil {
			glog.V(1).Infof("Removing service %q", name)
			delete(proxier.serviceMap, name)
		}
	}

	if err := proxier.syncProxyRules(); err != nil {
		glog.Errorf("Failed to sync iptables rules: %v", err)
	}
}

// OnEndpointsUpdate takes in a slice of updated endpoints.
func (proxier *Proxier) OnEndpointsUpdate(allEndpoints []api.Endpoints) {
	proxier.mu.Lock()
	defer proxier.mu.Unlock()
	proxier.haveReceivedEndpointsUpdate = true

	glog.V(4).Infof("Received endpoints update notice: %+v", allEndpoints)
	registeredEndpoints := make(map[proxy.ServicePortName]bool) // use a map as a set

	// Update endpoints for services.
	for i := range allEndpoints {
		svcEndpoints := &allEndpoints[i]

		// We need to build a map of portname -> all ip:ports for that
		// portname.  Explode Endpoints.Subsets[*] into this structure.
		portsToEndpoints := map[string][]hostPortPair{}
		for i := range svcEndpoints.Subsets {
			ss := &svcEndpoints.Subsets[i]
			for i := range ss.Ports {
				port := &ss.Ports[i]
				for i := range ss.Addresses {
					addr := &ss.Addresses[i]
					portsToEndpoints[port.Name] = append(portsToEndpoints[port.Name], hostPortPair{addr.IP, port.Port})
				}
			}
		}

		for portname := range portsToEndpoints {
			svcPort := proxy.ServicePortName{NamespacedName: types.NamespacedName{Namespace: svcEndpoints.Namespace, Name: svcEndpoints.Name}, Port: portname}
			state, exists := proxier.serviceMap[svcPort]
			if !exists || state == nil {
				state = newServiceInfo(svcPort)
				proxier.serviceMap[svcPort] = state
			}
			curEndpoints := []string{}
			if state != nil {
				curEndpoints = state.endpoints
			}
			newEndpoints := flattenValidEndpoints(portsToEndpoints[portname])

			if len(curEndpoints) != len(newEndpoints) || !slicesEquiv(slice.CopyStrings(curEndpoints), newEndpoints) {
				glog.V(1).Infof("Setting endpoints for %s to %+v", svcPort, newEndpoints)
				state.endpoints = newEndpoints
			}
			registeredEndpoints[svcPort] = true
		}
	}
	// Remove endpoints missing from the update.
	for service, info := range proxier.serviceMap {
		// if missing from update and not already set by previous endpoints event
		if _, exists := registeredEndpoints[service]; !exists && info.endpoints != nil {
			glog.V(2).Infof("Removing endpoints for %s", service)
			// Set the endpoints to nil, we will check for this in OnServiceUpdate so that we
			// only remove ServicePorts that have no endpoints and were not in the service update,
			// that way we only remove ServicePorts that were not in both.
			proxier.serviceMap[service].endpoints = nil
		}
	}
}

// used in OnEndpointsUpdate
type hostPortPair struct {
	host string
	port int
}

func isValidEndpoint(hpp *hostPortPair) bool {
	return hpp.host != "" && hpp.port > 0
}

// Tests whether two slices are equivalent.  This sorts both slices in-place.
func slicesEquiv(lhs, rhs []string) bool {
	if len(lhs) != len(rhs) {
		return false
	}
	if reflect.DeepEqual(slice.SortStrings(lhs), slice.SortStrings(rhs)) {
		return true
	}
	return false
}

func flattenValidEndpoints(endpoints []hostPortPair) []string {
	// Convert Endpoint objects into strings for easier use later.
	var result []string
	for i := range endpoints {
		hpp := &endpoints[i]
		if isValidEndpoint(hpp) {
			result = append(result, net.JoinHostPort(hpp.host, strconv.Itoa(hpp.port)))
		}
	}
	return result
}

// servicePortToServiceChain takes the ServicePortName for a
// service and returns the associated iptables chain
// this is computed by hashing (sha256) then encoding to base64 and
// truncating with the prefix "KUBE-SVC-"
// We do this because Iptables Chain Names must be <= 28 chars long
func servicePortToServiceChain(s proxy.ServicePortName) utiliptables.Chain {
	hash := sha256.Sum256([]byte(s.String()))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	return utiliptables.Chain("KUBE-SVC-" + encoded[:19])
}

// this is the same as servicePortToServiceChain but with the endpoint included essentially
func servicePortAndEndpointToServiceChain(s proxy.ServicePortName, endpoint string) utiliptables.Chain {
	hash := sha256.Sum256([]byte(s.String() + "_" + endpoint))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	return utiliptables.Chain("KUBE-SEP-" + encoded[:19])
}

// This is where all of the iptables-save/restore calls happen.
// The only other iptables rules are those that are setup in iptablesInit()
// assumes proxier.mu is held
func (proxier *Proxier) syncProxyRules() error {
	// don't sync rules till we've received services and endpoints
	if !proxier.haveReceivedEndpointsUpdate || !proxier.haveReceivedServiceUpdate {
		glog.V(2).Info("not syncing iptables until Services and Endpoints have been received from master")
		return nil
	}
	glog.V(4).Infof("Syncing iptables rules")

	// Ensure main chains and rules are installed.
	inputChains := []utiliptables.Chain{utiliptables.ChainOutput, utiliptables.ChainPrerouting}
	// Link the services chain.
	for _, chain := range inputChains {
		if _, err := proxier.iptables.EnsureChain(utiliptables.TableNAT, iptablesServicesChain); err != nil {
			return err
		}
		comment := "kubernetes service portals; must be before nodeports"
		args := []string{"-m", "comment", "--comment", comment, "-j", string(iptablesServicesChain)}
		if _, err := proxier.iptables.EnsureRule(utiliptables.Prepend, utiliptables.TableNAT, chain, args...); err != nil {
			return err
		}
	}
	// Link the nodeports chain.
	for _, chain := range inputChains {
		if _, err := proxier.iptables.EnsureChain(utiliptables.TableNAT, iptablesNodePortsChain); err != nil {
			return err
		}
		comment := "kubernetes service nodeports; must be after portals"
		args := []string{"-m", "comment", "--comment", comment, "-m", "addrtype", "--dst-type", "LOCAL", "-j", string(iptablesNodePortsChain)}
		if _, err := proxier.iptables.EnsureRule(utiliptables.Append, utiliptables.TableNAT, chain, args...); err != nil {
			return err
		}
	}
	// Link the output rules.
	{
		comment := "kubernetes service traffic requiring SNAT"
		args := []string{"-m", "comment", "--comment", comment, "-m", "mark", "--mark", iptablesMasqueradeMark, "-j", "MASQUERADE"}
		if _, err := proxier.iptables.EnsureRule(utiliptables.Append, utiliptables.TableNAT, utiliptables.ChainPostrouting, args...); err != nil {
			return err
		}
	}

	// Get iptables-save output so we can check for existing chains and rules.
	// This will be a map of chain name to chain with rules as stored in iptables-save/iptables-restore
	existingChains := make(map[utiliptables.Chain]string)
	iptablesSaveRaw, err := proxier.iptables.Save(utiliptables.TableNAT)
	if err != nil { // if we failed to get any rules
		glog.Errorf("Failed to execute iptable-save, syncing all rules. %s", err.Error())
	} else { // otherwise parse the output
		existingChains = getChainLines(utiliptables.TableNAT, iptablesSaveRaw)
	}

	chainsLines := bytes.NewBuffer(nil)
	rulesLines := bytes.NewBuffer(nil)

	// Write table header.
	writeLine(chainsLines, "*nat")

	// Make sure we keep stats for the top-level chains, if they existed
	// (which they should have because we created them above).
	if chain, ok := existingChains[iptablesServicesChain]; ok {
		writeLine(chainsLines, chain)
	} else {
		writeLine(chainsLines, makeChainLine(iptablesServicesChain))
	}
	if chain, ok := existingChains[iptablesNodePortsChain]; ok {
		writeLine(chainsLines, chain)
	} else {
		writeLine(chainsLines, makeChainLine(iptablesNodePortsChain))
	}

	// Accumulate chains to keep.
	activeChains := make(map[utiliptables.Chain]bool) // use a map as a set

	// Build rules for each service.
	for name, info := range proxier.serviceMap {
		protocol := strings.ToLower((string)(info.protocol))

		// Create the per-service chain, retaining counters if possible.
		svcChain := servicePortToServiceChain(name)
		if chain, ok := existingChains[svcChain]; ok {
			writeLine(chainsLines, chain)
		} else {
			writeLine(chainsLines, makeChainLine(svcChain))
		}
		activeChains[svcChain] = true

		// Capture the clusterIP.
		writeLine(rulesLines,
			"-A", string(iptablesServicesChain),
			"-m", "comment", "--comment", fmt.Sprintf("\"%s cluster IP\"", name.String()),
			"-m", protocol, "-p", protocol,
			"-d", fmt.Sprintf("%s/32", info.clusterIP.String()),
			"--dport", fmt.Sprintf("%d", info.port),
			"-j", string(svcChain))

		// Capture externalIPs.
		for _, externalIP := range info.deprecatedPublicIPs {
			args := []string{
				"-A", string(iptablesServicesChain),
				"-m", "comment", "--comment", fmt.Sprintf("\"%s external IP\"", name.String()),
				"-m", protocol, "-p", protocol,
				"-d", fmt.Sprintf("%s/32", externalIP),
				"--dport", fmt.Sprintf("%d", info.port),
			}
			// We have to SNAT packets from external IPs.
			writeLine(rulesLines, append(args,
				"-j", "MARK", "--set-xmark", fmt.Sprintf("%s/0xffffffff", iptablesMasqueradeMark))...)
			writeLine(rulesLines, append(args,
				"-j", string(svcChain))...)
		}

		// Capture load-balancer ingress.
		for _, ingress := range info.loadBalancerStatus.Ingress {
			if ingress.IP != "" {
				args := []string{
					"-A", string(iptablesServicesChain),
					"-m", "comment", "--comment", fmt.Sprintf("\"%s loadbalancer IP\"", name.String()),
					"-m", protocol, "-p", protocol,
					"-d", fmt.Sprintf("%s/32", ingress.IP),
					"--dport", fmt.Sprintf("%d", info.port),
				}
				// We have to SNAT packets from external IPs.
				writeLine(rulesLines, append(args,
					"-j", "MARK", "--set-xmark", fmt.Sprintf("%s/0xffffffff", iptablesMasqueradeMark))...)
				writeLine(rulesLines, append(args,
					"-j", string(svcChain))...)
			}
		}

		// Capture nodeports.  If we had more than 2 rules it might be
		// worthwhile to make a new per-service chain for nodeport rules, but
		// with just 2 rules it ends up being a waste and a cognitive burden.
		if info.nodePort != 0 {
			// Nodeports need SNAT.
			writeLine(rulesLines,
				"-A", string(iptablesNodePortsChain),
				"-m", "comment", "--comment", name.String(),
				"-m", protocol, "-p", protocol,
				"--dport", fmt.Sprintf("%d", info.nodePort),
				"-j", "MARK", "--set-xmark", fmt.Sprintf("%s/0xffffffff", iptablesMasqueradeMark))
			// Jump to the service chain.
			writeLine(rulesLines,
				"-A", string(iptablesNodePortsChain),
				"-m", "comment", "--comment", name.String(),
				"-m", protocol, "-p", protocol,
				"--dport", fmt.Sprintf("%d", info.nodePort),
				"-j", string(svcChain))
		}

		// Generate the per-endpoint chains.  We do this in multiple passes so we
		// can group rules together.
		endpoints := make([]string, 0)
		endpointChains := make([]utiliptables.Chain, 0)
		for _, ep := range info.endpoints {
			endpoints = append(endpoints, ep)
			endpointChain := servicePortAndEndpointToServiceChain(name, ep)
			endpointChains = append(endpointChains, endpointChain)

			// Create the endpoint chain, retaining counters if possible.
			if chain, ok := existingChains[utiliptables.Chain(endpointChain)]; ok {
				writeLine(chainsLines, chain)
			} else {
				writeLine(chainsLines, makeChainLine(endpointChain))
			}
			activeChains[endpointChain] = true
		}

		// First write session affinity rules, if applicable.
		if info.sessionAffinityType == api.ServiceAffinityClientIP {
			for _, endpointChain := range endpointChains {
				writeLine(rulesLines,
					"-A", string(svcChain),
					"-m", "comment", "--comment", name.String(),
					"-m", "recent", "--name", string(endpointChain),
					"--rcheck", "--seconds", fmt.Sprintf("%d", info.stickyMaxAgeSeconds), "--reap",
					"-j", string(endpointChain))
			}
		}

		// Now write loadbalancing & DNAT rules.
		n := len(endpointChains)
		for i, endpointChain := range endpointChains {
			// Balancing rules in the per-service chain.
			args := []string{
				"-A", string(svcChain),
				"-m", "comment", "--comment", name.String(),
			}
			if i < (n - 1) {
				// Each rule is a probabilistic match.
				args = append(args,
					"-m", "statistic",
					"--mode", "random",
					"--probability", fmt.Sprintf("%f", 1.0/float64(n-i)))
			}
			// The final (or only if n == 1) rule is a guaranteed match.
			args = append(args, "-j", string(endpointChain))
			writeLine(rulesLines, args...)

			// Rules in the per-endpoint chain.
			args = []string{
				"-A", string(endpointChain),
				"-m", "comment", "--comment", name.String(),
			}
			// Handle traffic that loops back to the originator with SNAT.
			// Technically we only need to do this if the endpoint is on this
			// host, but we don't have that information, so we just do this for
			// all endpoints.
			// TODO: if we grow logic to get this node's pod CIDR, we can use it.
			writeLine(rulesLines, append(args,
				"-s", fmt.Sprintf("%s/32", strings.Split(endpoints[i], ":")[0]),
				"-j", "MARK", "--set-xmark", fmt.Sprintf("%s/0xffffffff", iptablesMasqueradeMark))...)

			// Update client-affinity lists.
			if info.sessionAffinityType == api.ServiceAffinityClientIP {
				args = append(args, "-m", "recent", "--name", string(endpointChain), "--set")
			}
			// DNAT to final destination.
			args = append(args,
				"-m", protocol, "-p", protocol,
				"-j", "DNAT", "--to-destination", endpoints[i])
			writeLine(rulesLines, args...)
		}
	}

	// Delete chains no longer in use.
	for chain := range existingChains {
		if !activeChains[chain] {
			chainString := string(chain)
			if !strings.HasPrefix(chainString, "KUBE-SVC-") && !strings.HasPrefix(chainString, "KUBE-SEP-") {
				// Ignore chains that aren't ours.
				continue
			}
			// We must (as per iptables) write a chain-line for it, which has
			// the nice effect of flushing the chain.  Then we can remove the
			// chain.
			writeLine(chainsLines, existingChains[chain])
			writeLine(rulesLines, "-X", chainString)
		}
	}

	// Write the end-of-table marker.
	writeLine(rulesLines, "COMMIT")

	// Sync rules.
	// NOTE: NoFlushTables is used so we don't flush non-kubernetes chains in the table.
	lines := append(chainsLines.Bytes(), rulesLines.Bytes()...)
	glog.V(3).Infof("Syncing rules: %s", lines)
	return proxier.iptables.Restore(utiliptables.TableNAT, lines, utiliptables.NoFlushTables, utiliptables.RestoreCounters)
}

// Join all words with spaces, terminate with newline and write to buf.
func writeLine(buf *bytes.Buffer, words ...string) {
	buf.WriteString(strings.Join(words, " ") + "\n")
}

// return an iptables-save/restore formatted chain line given a Chain
func makeChainLine(chain utiliptables.Chain) string {
	return fmt.Sprintf(":%s - [0:0]", chain)
}

// getChainLines parses a table's iptables-save data to find chains in the table.
// It returns a map of iptables.Chain to string where the string is the chain line from the save (with counters etc).
func getChainLines(table utiliptables.Table, save []byte) map[utiliptables.Chain]string {
	// get lines
	lines := strings.Split(string(save), "\n")
	chainsMap := make(map[utiliptables.Chain]string)
	tablePrefix := "*" + string(table)
	lineNum := 0
	// find beginning of table
	for ; lineNum < len(lines); lineNum++ {
		if strings.HasPrefix(strings.TrimSpace(lines[lineNum]), tablePrefix) {
			lineNum++
			break
		}
	}
	// parse table lines
	for ; lineNum < len(lines); lineNum++ {
		line := strings.TrimSpace(lines[lineNum])
		if strings.HasPrefix(line, "COMMIT") || strings.HasPrefix(line, "*") {
			break
		} else if len(line) == 0 || strings.HasPrefix(line, "#") {
			continue
		} else if strings.HasPrefix(line, ":") && len(line) > 1 {
			chain := utiliptables.Chain(strings.SplitN(line[1:], " ", 2)[0])
			chainsMap[chain] = lines[lineNum]
		}
	}
	return chainsMap
}
