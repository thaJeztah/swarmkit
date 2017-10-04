package networkallocator

import (
	"fmt"
	"net"
	"strings"

	"github.com/docker/docker/pkg/plugingetter"
	"github.com/docker/libnetwork/datastore"
	"github.com/docker/libnetwork/driverapi"
	"github.com/docker/libnetwork/drvregistry"
	"github.com/docker/libnetwork/ipamapi"
	"github.com/docker/libnetwork/netlabel"
	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/log"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

const (
	// DefaultDriver defines the name of the driver to be used by
	// default if a network without any driver name specified is
	// created.
	DefaultDriver = "overlay"

	// PredefinedLabel identifies internally allocated swarm networks
	// corresponding to the node-local predefined networks on the host.
	PredefinedLabel = "com.docker.swarm.predefined"
)

// NetworkAllocator acts as the controller for all network related operations
// like managing network and IPAM drivers and also creating and
// deleting networks and the associated resources.
type NetworkAllocator struct {
	// The driver register which manages all internal and external
	// IPAM and network drivers.
	drvRegistry *drvregistry.DrvRegistry

	// The port allocator instance for allocating node ports
	portAllocator *portAllocator

	// Local network state used by NetworkAllocator to do network management.
	networks map[string]*network

	// Allocator state to indicate if allocation has been
	// successfully completed for this service.
	services map[string]struct{}

	// Allocator state to indicate if allocation has been
	// successfully completed for this task.
	tasks map[string]struct{}

	// Allocator state to indicate if allocation has been
	// successfully completed for this node.
	nodes map[string]struct{}
}

// Local in-memory state related to network that need to be tracked by NetworkAllocator
type network struct {
	// A local cache of the store object.
	nw *api.Network

	// pools is used to save the internal poolIDs needed when
	// releasing the pool.
	pools map[string]string

	// endpoints is a map of endpoint IP to the poolID from which it
	// was allocated.
	endpoints map[string]string

	// isNodeLocal indicates whether the scope of the network's resources
	// is local to the node. If true, it means the resources can only be
	// allocated locally by the node where the network will be deployed.
	// In this the swarm manager will skip the allocations.
	isNodeLocal bool
}

type networkDriver struct {
	driver     driverapi.Driver
	name       string
	capability *driverapi.Capability
}

type initializer struct {
	fn    drvregistry.InitFunc
	ntype string
}

// PredefinedNetworkData contains the minimum set of data needed
// to create the correspondent predefined network object in the store.
type PredefinedNetworkData struct {
	Name   string
	Driver string
}

// New returns a new NetworkAllocator handle
func New(pg plugingetter.PluginGetter) (*NetworkAllocator, error) {
	na := &NetworkAllocator{
		networks: make(map[string]*network),
		services: make(map[string]struct{}),
		tasks:    make(map[string]struct{}),
		nodes:    make(map[string]struct{}),
	}

	// There are no driver configurations and notification
	// functions as of now.
	reg, err := drvregistry.New(nil, nil, nil, nil, pg)
	if err != nil {
		return nil, err
	}

	if err := initializeDrivers(reg); err != nil {
		return nil, err
	}

	if err = initIPAMDrivers(reg); err != nil {
		return nil, err
	}

	pa, err := newPortAllocator()
	if err != nil {
		return nil, err
	}

	na.portAllocator = pa
	na.drvRegistry = reg
	return na, nil
}

// Allocate allocates all the necessary resources both general
// and driver-specific which may be specified in the NetworkSpec
func (na *NetworkAllocator) Allocate(n *api.Network) error {
	if _, ok := na.networks[n.ID]; ok {
		return fmt.Errorf("network %s already allocated", n.ID)
	}

	d, err := na.resolveDriver(n)
	if err != nil {
		return err
	}

	nw := &network{
		nw:          n,
		endpoints:   make(map[string]string),
		isNodeLocal: d.capability.DataScope == datastore.LocalScope,
	}

	// No swarm-level allocation can be provided by the network driver for
	// node-local networks. Only thing needed is populating the driver's name
	// in the driver's state.
	if nw.isNodeLocal {
		n.DriverState = &api.Driver{
			Name: d.name,
		}
		// In order to support backward compatibility with older daemon
		// versions which assumes the network attachment to contains
		// non nil IPAM attribute, passing an empty object
		n.IPAM = &api.IPAMOptions{Driver: &api.Driver{}}
	} else {
		nw.pools, err = na.allocatePools(n)
		if err != nil {
			return errors.Wrapf(err, "failed allocating pools and gateway IP for network %s", n.ID)
		}

		if err := na.allocateDriverState(n); err != nil {
			na.freePools(n, nw.pools)
			return errors.Wrapf(err, "failed while allocating driver state for network %s", n.ID)
		}
	}

	na.networks[n.ID] = nw

	return nil
}

func (na *NetworkAllocator) getNetwork(id string) *network {
	return na.networks[id]
}

// Deallocate frees all the general and driver specific resources
// which were assigned to the passed network.
func (na *NetworkAllocator) Deallocate(n *api.Network) error {
	localNet := na.getNetwork(n.ID)
	if localNet == nil {
		return fmt.Errorf("could not get networker state for network %s", n.ID)
	}

	// No swarm-level resource deallocation needed for node-local networks
	if localNet.isNodeLocal {
		delete(na.networks, n.ID)
		return nil
	}

	if err := na.freeDriverState(n); err != nil {
		return errors.Wrapf(err, "failed to free driver state for network %s", n.ID)
	}

	delete(na.networks, n.ID)

	return na.freePools(n, localNet.pools)
}

// ServiceAllocate allocates all the network resources such as virtual
// IP and ports needed by the service.
func (na *NetworkAllocator) ServiceAllocate(s *api.Service) (err error) {
	if err = na.portAllocator.serviceAllocatePorts(s); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			na.ServiceDeallocate(s)
		}
	}()

	if s.Endpoint == nil {
		s.Endpoint = &api.Endpoint{}
	}
	s.Endpoint.Spec = s.Spec.Endpoint.Copy()

	// If ResolutionMode is DNSRR do not try allocating VIPs, but
	// free any VIP from previous state.
	if s.Spec.Endpoint != nil && s.Spec.Endpoint.Mode == api.ResolutionModeDNSRoundRobin {
		for _, vip := range s.Endpoint.VirtualIPs {
			if err := na.deallocateVIP(vip); err != nil {
				// don't bail here, deallocate as many as possible.
				log.L.WithError(err).
					WithField("vip.network", vip.NetworkID).
					WithField("vip.addr", vip.Addr).Error("error deallocating vip")
			}
		}

		s.Endpoint.VirtualIPs = nil

		delete(na.services, s.ID)
		return nil
	}

	specNetworks := serviceNetworks(s)

	// Allocate VIPs for all the pre-populated endpoint attachments
	eVIPs := s.Endpoint.VirtualIPs[:0]

vipLoop:
	for _, eAttach := range s.Endpoint.VirtualIPs {
		if na.IsVIPOnIngressNetwork(eAttach) && IsIngressNetworkNeeded(s) {
			if err = na.allocateVIP(eAttach); err != nil {
				return err
			}
			eVIPs = append(eVIPs, eAttach)
			continue vipLoop

		}
		for _, nAttach := range specNetworks {
			if nAttach.Target == eAttach.NetworkID {
				if err = na.allocateVIP(eAttach); err != nil {
					return err
				}
				eVIPs = append(eVIPs, eAttach)
				continue vipLoop
			}
		}
		// If the network of the VIP is not part of the service spec,
		// deallocate the vip
		na.deallocateVIP(eAttach)
	}

networkLoop:
	for _, nAttach := range specNetworks {
		for _, vip := range s.Endpoint.VirtualIPs {
			if vip.NetworkID == nAttach.Target {
				continue networkLoop
			}
		}

		vip := &api.Endpoint_VirtualIP{NetworkID: nAttach.Target}
		if err = na.allocateVIP(vip); err != nil {
			return err
		}

		eVIPs = append(eVIPs, vip)
	}

	if len(eVIPs) > 0 {
		na.services[s.ID] = struct{}{}
	} else {
		delete(na.services, s.ID)
	}

	s.Endpoint.VirtualIPs = eVIPs
	return nil
}

// ServiceDeallocate de-allocates all the network resources such as
// virtual IP and ports associated with the service.
func (na *NetworkAllocator) ServiceDeallocate(s *api.Service) error {
	if s.Endpoint == nil {
		return nil
	}

	for _, vip := range s.Endpoint.VirtualIPs {
		if err := na.deallocateVIP(vip); err != nil {
			// don't bail here, deallocate as many as possible.
			log.L.WithError(err).
				WithField("vip.network", vip.NetworkID).
				WithField("vip.addr", vip.Addr).Error("error deallocating vip")
		}
	}
	s.Endpoint.VirtualIPs = nil

	na.portAllocator.serviceDeallocatePorts(s)
	delete(na.services, s.ID)

	return nil
}

// IsAllocated returns if the passed network has been allocated or not.
func (na *NetworkAllocator) IsAllocated(n *api.Network) bool {
	_, ok := na.networks[n.ID]
	return ok
}

// IsTaskAllocated returns if the passed task has its network resources allocated or not.
func (na *NetworkAllocator) IsTaskAllocated(t *api.Task) bool {
	// If the task is not found in the allocated set, then it is
	// not allocated.
	if _, ok := na.tasks[t.ID]; !ok {
		return false
	}

	// If Networks is empty there is no way this Task is allocated.
	if len(t.Networks) == 0 {
		return false
	}

	// To determine whether the task has its resources allocated,
	// we just need to look at one global scope network (in case of
	// multi-network attachment).  This is because we make sure we
	// allocate for every network or we allocate for none.

	// Find the first global scope network
	for _, nAttach := range t.Networks {
		// If the network is not allocated, the task cannot be allocated.
		localNet, ok := na.networks[nAttach.Network.ID]
		if !ok {
			return false
		}

		// Nothing else to check for local scope network
		if localNet.isNodeLocal {
			continue
		}

		// Addresses empty. Task is not allocated.
		if len(nAttach.Addresses) == 0 {
			return false
		}

		// The allocated IP address not found in local endpoint state. Not allocated.
		if _, ok := localNet.endpoints[nAttach.Addresses[0]]; !ok {
			return false
		}
	}

	return true
}

// HostPublishPortsNeedUpdate returns true if the passed service needs
// allocations for its published ports in host (non ingress) mode
func (na *NetworkAllocator) HostPublishPortsNeedUpdate(s *api.Service) bool {
	return na.portAllocator.hostPublishPortsNeedUpdate(s)
}

// ServiceAllocationOpts is struct used for functional options in IsServiceAllocated
type ServiceAllocationOpts struct {
	OnInit bool
}

// OnInit is called for allocator initialization stage
func OnInit(options *ServiceAllocationOpts) {
	options.OnInit = true
}

// ServiceNeedsAllocation returns true if the passed service needs to have network resources allocated/updated.
func (na *NetworkAllocator) ServiceNeedsAllocation(s *api.Service, flags ...func(*ServiceAllocationOpts)) bool {
	var options ServiceAllocationOpts
	for _, flag := range flags {
		flag(&options)
	}

	specNetworks := serviceNetworks(s)

	// If endpoint mode is VIP and allocator does not have the
	// service in VIP allocated set then it needs to be allocated.
	if len(specNetworks) != 0 &&
		(s.Spec.Endpoint == nil ||
			s.Spec.Endpoint.Mode == api.ResolutionModeVirtualIP) {

		if _, ok := na.services[s.ID]; !ok {
			return true
		}

		if s.Endpoint == nil || len(s.Endpoint.VirtualIPs) == 0 {
			return true
		}

		// If the spec has networks which don't have a corresponding VIP,
		// the service needs to be allocated.
	networkLoop:
		for _, net := range specNetworks {
			for _, vip := range s.Endpoint.VirtualIPs {
				if vip.NetworkID == net.Target {
					continue networkLoop
				}
			}
			return true
		}
	}

	// If the spec no longer has networks attached and has a vip allocated
	// from previous spec the service needs to allocated.
	if s.Endpoint != nil {
	vipLoop:
		for _, vip := range s.Endpoint.VirtualIPs {
			if na.IsVIPOnIngressNetwork(vip) && IsIngressNetworkNeeded(s) {
				continue vipLoop
			}
			for _, net := range specNetworks {
				if vip.NetworkID == net.Target {
					continue vipLoop
				}
			}
			return true
		}
	}

	// If the endpoint mode is DNSRR and allocator has the service
	// in VIP allocated set then we return to be allocated to make
	// sure the allocator triggers networkallocator to free up the
	// resources if any.
	if s.Spec.Endpoint != nil && s.Spec.Endpoint.Mode == api.ResolutionModeDNSRoundRobin {
		if _, ok := na.services[s.ID]; ok {
			return true
		}
	}

	if (s.Spec.Endpoint != nil && len(s.Spec.Endpoint.Ports) != 0) ||
		(s.Endpoint != nil && len(s.Endpoint.Ports) != 0) {
		return !na.portAllocator.isPortsAllocatedOnInit(s, options.OnInit)
	}
	return false
}

// IsNodeAllocated returns if the passed node has its network resources allocated or not.
func (na *NetworkAllocator) IsNodeAllocated(node *api.Node) bool {
	// If the node is not found in the allocated set, then it is
	// not allocated.
	if _, ok := na.nodes[node.ID]; !ok {
		return false
	}

	// If no attachment, not allocated.
	if node.Attachment == nil {
		return false
	}

	// If the network is not allocated, the node cannot be allocated.
	localNet, ok := na.networks[node.Attachment.Network.ID]
	if !ok {
		return false
	}

	// Addresses empty, not allocated.
	if len(node.Attachment.Addresses) == 0 {
		return false
	}

	// The allocated IP address not found in local endpoint state. Not allocated.
	if _, ok := localNet.endpoints[node.Attachment.Addresses[0]]; !ok {
		return false
	}

	return true
}

// AllocateNode allocates the IP addresses for the network to which
// the node is attached.
func (na *NetworkAllocator) AllocateNode(node *api.Node) error {
	if err := na.allocateNetworkIPs(node.Attachment); err != nil {
		return err
	}

	na.nodes[node.ID] = struct{}{}
	return nil
}

// DeallocateNode deallocates the IP addresses for the network to
// which the node is attached.
func (na *NetworkAllocator) DeallocateNode(node *api.Node) error {
	delete(na.nodes, node.ID)
	return na.releaseEndpoints([]*api.NetworkAttachment{node.Attachment})
}

// AllocateTask allocates all the endpoint resources for all the
// networks that a task is attached to.
func (na *NetworkAllocator) AllocateTask(t *api.Task) error {
	for i, nAttach := range t.Networks {
		if localNet := na.getNetwork(nAttach.Network.ID); localNet != nil && localNet.isNodeLocal {
			continue
		}
		if err := na.allocateNetworkIPs(nAttach); err != nil {
			if err := na.releaseEndpoints(t.Networks[:i]); err != nil {
				log.G(context.TODO()).WithError(err).Errorf("Failed to release IP addresses while rolling back allocation for task %s network %s", t.ID, nAttach.Network.ID)
			}
			return errors.Wrapf(err, "failed to allocate network IP for task %s network %s", t.ID, nAttach.Network.ID)
		}
	}

	na.tasks[t.ID] = struct{}{}

	return nil
}

// DeallocateTask releases all the endpoint resources for all the
// networks that a task is attached to.
func (na *NetworkAllocator) DeallocateTask(t *api.Task) error {
	delete(na.tasks, t.ID)
	return na.releaseEndpoints(t.Networks)
}

func (na *NetworkAllocator) releaseEndpoints(networks []*api.NetworkAttachment) error {
	for _, nAttach := range networks {
		localNet := na.getNetwork(nAttach.Network.ID)
		if localNet == nil {
			return fmt.Errorf("could not find network allocator state for network %s", nAttach.Network.ID)
		}

		if localNet.isNodeLocal {
			continue
		}

		ipam, _, _, err := na.resolveIPAM(nAttach.Network)
		if err != nil {
			return errors.Wrap(err, "failed to resolve IPAM while releasing")
		}

		// Do not fail and bail out if we fail to release IP
		// address here. Keep going and try releasing as many
		// addresses as possible.
		for _, addr := range nAttach.Addresses {
			// Retrieve the poolID and immediately nuke
			// out the mapping.
			poolID := localNet.endpoints[addr]
			delete(localNet.endpoints, addr)

			ip, _, err := net.ParseCIDR(addr)
			if err != nil {
				log.G(context.TODO()).Errorf("Could not parse IP address %s while releasing", addr)
				continue
			}

			if err := ipam.ReleaseAddress(poolID, ip); err != nil {
				log.G(context.TODO()).WithError(err).Errorf("IPAM failure while releasing IP address %s", addr)
			}
		}

		// Clear out the address list when we are done with
		// this network.
		nAttach.Addresses = nil
	}

	return nil
}

// allocate virtual IP for a single endpoint attachment of the service.
func (na *NetworkAllocator) allocateVIP(vip *api.Endpoint_VirtualIP) error {
	var opts map[string]string
	localNet := na.getNetwork(vip.NetworkID)
	if localNet == nil {
		return errors.New("networkallocator: could not find local network state")
	}

	if localNet.isNodeLocal {
		return nil
	}

	// If this IP is already allocated in memory we don't need to
	// do anything.
	if _, ok := localNet.endpoints[vip.Addr]; ok {
		return nil
	}

	ipam, _, _, err := na.resolveIPAM(localNet.nw)
	if err != nil {
		return errors.Wrap(err, "failed to resolve IPAM while allocating")
	}

	var addr net.IP
	if vip.Addr != "" {
		var err error

		addr, _, err = net.ParseCIDR(vip.Addr)
		if err != nil {
			return err
		}
	}
	if localNet.nw.IPAM != nil && localNet.nw.IPAM.Driver != nil {
		// set ipam allocation method to serial
		opts = setIPAMSerialAlloc(localNet.nw.IPAM.Driver.Options)
	}

	for _, poolID := range localNet.pools {
		ip, _, err := ipam.RequestAddress(poolID, addr, opts)
		if err != nil && err != ipamapi.ErrNoAvailableIPs && err != ipamapi.ErrIPOutOfRange {
			return errors.Wrap(err, "could not allocate VIP from IPAM")
		}

		// If we got an address then we are done.
		if err == nil {
			ipStr := ip.String()
			localNet.endpoints[ipStr] = poolID
			vip.Addr = ipStr
			return nil
		}
	}

	return errors.New("could not find an available IP while allocating VIP")
}

func (na *NetworkAllocator) deallocateVIP(vip *api.Endpoint_VirtualIP) error {
	localNet := na.getNetwork(vip.NetworkID)
	if localNet == nil {
		return errors.New("networkallocator: could not find local network state")
	}
	if localNet.isNodeLocal {
		return nil
	}
	ipam, _, _, err := na.resolveIPAM(localNet.nw)
	if err != nil {
		return errors.Wrap(err, "failed to resolve IPAM while allocating")
	}

	// Retrieve the poolID and immediately nuke
	// out the mapping.
	poolID := localNet.endpoints[vip.Addr]
	delete(localNet.endpoints, vip.Addr)

	ip, _, err := net.ParseCIDR(vip.Addr)
	if err != nil {
		log.G(context.TODO()).Errorf("Could not parse VIP address %s while releasing", vip.Addr)
		return err
	}

	if err := ipam.ReleaseAddress(poolID, ip); err != nil {
		log.G(context.TODO()).WithError(err).Errorf("IPAM failure while releasing VIP address %s", vip.Addr)
		return err
	}

	return nil
}

// allocate the IP addresses for a single network attachment of the task.
func (na *NetworkAllocator) allocateNetworkIPs(nAttach *api.NetworkAttachment) error {
	var ip *net.IPNet
	var opts map[string]string

	ipam, _, _, err := na.resolveIPAM(nAttach.Network)
	if err != nil {
		return errors.Wrap(err, "failed to resolve IPAM while allocating")
	}

	localNet := na.getNetwork(nAttach.Network.ID)
	if localNet == nil {
		return fmt.Errorf("could not find network allocator state for network %s", nAttach.Network.ID)
	}

	addresses := nAttach.Addresses
	if len(addresses) == 0 {
		addresses = []string{""}
	}

	for i, rawAddr := range addresses {
		var addr net.IP
		if rawAddr != "" {
			var err error
			addr, _, err = net.ParseCIDR(rawAddr)
			if err != nil {
				addr = net.ParseIP(rawAddr)

				if addr == nil {
					return errors.Wrapf(err, "could not parse address string %s", rawAddr)
				}
			}
		}
		// Set the ipam options if the network has an ipam driver.
		if localNet.nw.IPAM != nil && localNet.nw.IPAM.Driver != nil {
			// set ipam allocation method to serial
			opts = setIPAMSerialAlloc(localNet.nw.IPAM.Driver.Options)
		}

		for _, poolID := range localNet.pools {
			var err error

			ip, _, err = ipam.RequestAddress(poolID, addr, opts)
			if err != nil && err != ipamapi.ErrNoAvailableIPs && err != ipamapi.ErrIPOutOfRange {
				return errors.Wrap(err, "could not allocate IP from IPAM")
			}

			// If we got an address then we are done.
			if err == nil {
				ipStr := ip.String()
				localNet.endpoints[ipStr] = poolID
				addresses[i] = ipStr
				nAttach.Addresses = addresses
				return nil
			}
		}
	}

	return errors.New("could not find an available IP")
}

func (na *NetworkAllocator) freeDriverState(n *api.Network) error {
	d, err := na.resolveDriver(n)
	if err != nil {
		return err
	}

	return d.driver.NetworkFree(n.ID)
}

func (na *NetworkAllocator) allocateDriverState(n *api.Network) error {
	d, err := na.resolveDriver(n)
	if err != nil {
		return err
	}

	options := make(map[string]string)
	// reconcile the driver specific options from the network spec
	// and from the operational state retrieved from the store
	if n.Spec.DriverConfig != nil {
		for k, v := range n.Spec.DriverConfig.Options {
			options[k] = v
		}
	}
	if n.DriverState != nil {
		for k, v := range n.DriverState.Options {
			options[k] = v
		}
	}

	// Construct IPAM data for driver consumption.
	ipv4Data := make([]driverapi.IPAMData, 0, len(n.IPAM.Configs))
	for _, ic := range n.IPAM.Configs {
		if ic.Family == api.IPAMConfig_IPV6 {
			continue
		}

		_, subnet, err := net.ParseCIDR(ic.Subnet)
		if err != nil {
			return errors.Wrapf(err, "error parsing subnet %s while allocating driver state", ic.Subnet)
		}

		gwIP := net.ParseIP(ic.Gateway)
		gwNet := &net.IPNet{
			IP:   gwIP,
			Mask: subnet.Mask,
		}

		data := driverapi.IPAMData{
			Pool:    subnet,
			Gateway: gwNet,
		}

		ipv4Data = append(ipv4Data, data)
	}

	ds, err := d.driver.NetworkAllocate(n.ID, options, ipv4Data, nil)
	if err != nil {
		return err
	}

	// Update network object with the obtained driver state.
	n.DriverState = &api.Driver{
		Name:    d.name,
		Options: ds,
	}

	return nil
}

// Resolve network driver
func (na *NetworkAllocator) resolveDriver(n *api.Network) (*networkDriver, error) {
	dName := DefaultDriver
	if n.Spec.DriverConfig != nil && n.Spec.DriverConfig.Name != "" {
		dName = n.Spec.DriverConfig.Name
	}

	d, drvcap := na.drvRegistry.Driver(dName)
	if d == nil {
		var err error
		err = na.loadDriver(dName)
		if err != nil {
			return nil, err
		}

		d, drvcap = na.drvRegistry.Driver(dName)
		if d == nil {
			return nil, fmt.Errorf("could not resolve network driver %s", dName)
		}
	}

	return &networkDriver{driver: d, capability: drvcap, name: dName}, nil
}

func (na *NetworkAllocator) loadDriver(name string) error {
	pg := na.drvRegistry.GetPluginGetter()
	if pg == nil {
		return errors.New("plugin store is uninitialized")
	}
	_, err := pg.Get(name, driverapi.NetworkPluginEndpointType, plugingetter.Lookup)
	return err
}

// Resolve the IPAM driver
func (na *NetworkAllocator) resolveIPAM(n *api.Network) (ipamapi.Ipam, string, map[string]string, error) {
	dName := ipamapi.DefaultIPAM
	if n.Spec.IPAM != nil && n.Spec.IPAM.Driver != nil && n.Spec.IPAM.Driver.Name != "" {
		dName = n.Spec.IPAM.Driver.Name
	}

	var dOptions map[string]string
	if n.Spec.IPAM != nil && n.Spec.IPAM.Driver != nil && len(n.Spec.IPAM.Driver.Options) != 0 {
		dOptions = n.Spec.IPAM.Driver.Options
	}

	ipam, _ := na.drvRegistry.IPAM(dName)
	if ipam == nil {
		return nil, "", nil, fmt.Errorf("could not resolve IPAM driver %s", dName)
	}

	return ipam, dName, dOptions, nil
}

func (na *NetworkAllocator) freePools(n *api.Network, pools map[string]string) error {
	ipam, _, _, err := na.resolveIPAM(n)
	if err != nil {
		return errors.Wrapf(err, "failed to resolve IPAM while freeing pools for network %s", n.ID)
	}

	releasePools(ipam, n.IPAM.Configs, pools)
	return nil
}

func releasePools(ipam ipamapi.Ipam, icList []*api.IPAMConfig, pools map[string]string) {
	for _, ic := range icList {
		if err := ipam.ReleaseAddress(pools[ic.Subnet], net.ParseIP(ic.Gateway)); err != nil {
			log.G(context.TODO()).WithError(err).Errorf("Failed to release address %s", ic.Subnet)
		}
	}

	for k, p := range pools {
		if err := ipam.ReleasePool(p); err != nil {
			log.G(context.TODO()).WithError(err).Errorf("Failed to release pool %s", k)
		}
	}
}

func (na *NetworkAllocator) allocatePools(n *api.Network) (map[string]string, error) {
	ipam, dName, dOptions, err := na.resolveIPAM(n)
	if err != nil {
		return nil, err
	}

	// We don't support user defined address spaces yet so just
	// retrieve default address space names for the driver.
	_, asName, err := na.drvRegistry.IPAMDefaultAddressSpaces(dName)
	if err != nil {
		return nil, err
	}

	pools := make(map[string]string)

	var ipamConfigs []*api.IPAMConfig

	// If there is non-nil IPAM state always prefer those subnet
	// configs over Spec configs.
	if n.IPAM != nil {
		ipamConfigs = n.IPAM.Configs
	} else if n.Spec.IPAM != nil {
		ipamConfigs = make([]*api.IPAMConfig, len(n.Spec.IPAM.Configs))
		copy(ipamConfigs, n.Spec.IPAM.Configs)
	}

	// Append an empty slot for subnet allocation if there are no
	// IPAM configs from either spec or state.
	if len(ipamConfigs) == 0 {
		ipamConfigs = append(ipamConfigs, &api.IPAMConfig{Family: api.IPAMConfig_IPV4})
	}

	// Update the runtime IPAM configurations with initial state
	n.IPAM = &api.IPAMOptions{
		Driver:  &api.Driver{Name: dName, Options: dOptions},
		Configs: ipamConfigs,
	}

	for i, ic := range ipamConfigs {
		poolID, poolIP, meta, err := ipam.RequestPool(asName, ic.Subnet, ic.Range, dOptions, false)
		if err != nil {
			// Rollback by releasing all the resources allocated so far.
			releasePools(ipam, ipamConfigs[:i], pools)
			return nil, err
		}
		pools[poolIP.String()] = poolID

		// The IPAM contract allows the IPAM driver to autonomously
		// provide a network gateway in response to the pool request.
		// But if the network spec contains a gateway, we will allocate
		// it irrespective of whether the ipam driver returned one already.
		// If none of the above is true, we need to allocate one now, and
		// let the driver know this request is for the network gateway.
		var (
			gwIP *net.IPNet
			ip   net.IP
		)
		if gws, ok := meta[netlabel.Gateway]; ok {
			if ip, gwIP, err = net.ParseCIDR(gws); err != nil {
				return nil, fmt.Errorf("failed to parse gateway address (%v) returned by ipam driver: %v", gws, err)
			}
			gwIP.IP = ip
		}
		if dOptions == nil {
			dOptions = make(map[string]string)
		}
		dOptions[ipamapi.RequestAddressType] = netlabel.Gateway
		// set ipam allocation method to serial
		dOptions = setIPAMSerialAlloc(dOptions)
		defer delete(dOptions, ipamapi.RequestAddressType)

		if ic.Gateway != "" || gwIP == nil {
			gwIP, _, err = ipam.RequestAddress(poolID, net.ParseIP(ic.Gateway), dOptions)
			if err != nil {
				// Rollback by releasing all the resources allocated so far.
				releasePools(ipam, ipamConfigs[:i], pools)
				return nil, err
			}
		}

		if ic.Subnet == "" {
			ic.Subnet = poolIP.String()
		}

		if ic.Gateway == "" {
			ic.Gateway = gwIP.IP.String()
		}

	}

	return pools, nil
}

func initializeDrivers(reg *drvregistry.DrvRegistry) error {
	for _, i := range initializers {
		if err := reg.AddDriver(i.ntype, i.fn, nil); err != nil {
			return err
		}
	}
	return nil
}

func serviceNetworks(s *api.Service) []*api.NetworkAttachmentConfig {
	// Always prefer NetworkAttachmentConfig in the TaskSpec
	if len(s.Spec.Task.Networks) == 0 && len(s.Spec.Networks) != 0 {
		return s.Spec.Networks
	}
	return s.Spec.Task.Networks
}

// IsVIPOnIngressNetwork check if the vip is in ingress network
func (na *NetworkAllocator) IsVIPOnIngressNetwork(vip *api.Endpoint_VirtualIP) bool {
	if vip == nil {
		return false
	}

	localNet := na.getNetwork(vip.NetworkID)
	if localNet != nil && localNet.nw != nil {
		return IsIngressNetwork(localNet.nw)
	}
	return false
}

// IsIngressNetwork check if the network is an ingress network
func IsIngressNetwork(nw *api.Network) bool {
	if nw.Spec.Ingress {
		return true
	}
	// Check if legacy defined ingress network
	_, ok := nw.Spec.Annotations.Labels["com.docker.swarm.internal"]
	return ok && nw.Spec.Annotations.Name == "ingress"
}

// IsIngressNetworkNeeded checks whether the service requires the routing-mesh
func IsIngressNetworkNeeded(s *api.Service) bool {
	if s == nil {
		return false
	}

	if s.Spec.Endpoint == nil {
		return false
	}

	for _, p := range s.Spec.Endpoint.Ports {
		// The service to which this task belongs is trying to
		// expose ports with PublishMode as Ingress to the
		// external world. Automatically attach the task to
		// the ingress network.
		if p.PublishMode == api.PublishModeIngress {
			return true
		}
	}

	return false
}

// IsBuiltInDriver returns whether the passed driver is an internal network driver
func IsBuiltInDriver(name string) bool {
	n := strings.ToLower(name)
	for _, d := range initializers {
		if n == d.ntype {
			return true
		}
	}
	return false
}

// setIPAMSerialAlloc sets the ipam allocation method to serial
func setIPAMSerialAlloc(opts map[string]string) map[string]string {
	if opts == nil {
		opts = make(map[string]string)
	}
	if _, ok := opts[ipamapi.AllocSerialPrefix]; !ok {
		opts[ipamapi.AllocSerialPrefix] = "true"
	}
	return opts
}
