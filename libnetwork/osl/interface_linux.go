package osl

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/docker/docker/libnetwork/ns"
	"github.com/docker/docker/libnetwork/types"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// nwIface represents the settings and identity of a network device.
// It is used as a return type for Network.Link, and it is common practice
// for the caller to use this information when moving interface SrcName from
// host namespace to DstName in a different net namespace with the appropriate
// network settings.
type nwIface struct {
	srcName     string
	dstName     string
	master      string
	dstMaster   string
	mac         net.HardwareAddr
	address     *net.IPNet
	addressIPv6 *net.IPNet
	llAddrs     []*net.IPNet
	routes      []*net.IPNet
	bridge      bool
	ns          *networkNamespace
}

// SrcName returns the name of the interface in the origin network namespace.
func (i *nwIface) SrcName() string {
	return i.srcName
}

// DstName returns the name that will be assigned to the interface once
// moved inside a network namespace. When the caller passes in a DstName,
// it is only expected to pass a prefix. The name will be modified with an
// auto-generated suffix.
func (i *nwIface) DstName() string {
	return i.dstName
}

func (i *nwIface) DstMaster() string {
	return i.dstMaster
}

// Bridge returns true if the interface is a bridge.
func (i *nwIface) Bridge() bool {
	return i.bridge
}

// Master returns the srcname of the master interface for this interface.
func (i *nwIface) Master() string {
	return i.master
}

func (i *nwIface) MacAddress() net.HardwareAddr {
	return types.GetMacCopy(i.mac)
}

// Address returns the IPv4 address for the interface.
func (i *nwIface) Address() *net.IPNet {
	return types.GetIPNetCopy(i.address)
}

// AddressIPv6 returns the IPv6 address for the interface.
func (i *nwIface) AddressIPv6() *net.IPNet {
	return types.GetIPNetCopy(i.addressIPv6)
}

// LinkLocalAddresses returns the link-local IP addresses assigned to the
// interface.
func (i *nwIface) LinkLocalAddresses() []*net.IPNet {
	return i.llAddrs
}

// Routes returns IP routes for the interface.
func (i *nwIface) Routes() []*net.IPNet {
	routes := make([]*net.IPNet, len(i.routes))
	for index, route := range i.routes {
		routes[index] = types.GetIPNetCopy(route)
	}

	return routes
}

// Remove an interface from the sandbox by renaming to original name
// and moving it out of the sandbox.
func (i *nwIface) Remove() error {
	i.ns.Lock()
	isDefault := i.ns.isDefault
	nlh := i.ns.nlHandle
	i.ns.Unlock()

	// Find the network interface identified by the DstName attribute.
	iface, err := nlh.LinkByName(i.DstName())
	if err != nil {
		return err
	}

	// Down the interface before configuring
	if err := nlh.LinkSetDown(iface); err != nil {
		return err
	}

	err = nlh.LinkSetName(iface, i.SrcName())
	if err != nil {
		log.G(context.TODO()).Debugf("LinkSetName failed for interface %s: %v", i.SrcName(), err)
		return err
	}

	// if it is a bridge just delete it.
	if i.Bridge() {
		if err := nlh.LinkDel(iface); err != nil {
			return fmt.Errorf("failed deleting bridge %q: %v", i.SrcName(), err)
		}
	} else if !isDefault {
		// Move the network interface to caller namespace.
		if err := nlh.LinkSetNsFd(iface, ns.ParseHandlerInt()); err != nil {
			log.G(context.TODO()).Debugf("LinkSetNsPid failed for interface %s: %v", i.SrcName(), err)
			return err
		}
	}

	i.ns.Lock()
	for index, intf := range i.ns.iFaces {
		if intf == i {
			i.ns.iFaces = append(i.ns.iFaces[:index], i.ns.iFaces[index+1:]...)
			break
		}
	}
	i.ns.Unlock()

	i.ns.checkLoV6()

	return nil
}

// Statistics returns the sandbox's side veth interface statistics.
func (i *nwIface) Statistics() (*types.InterfaceStatistics, error) {
	l, err := i.ns.nlHandle.LinkByName(i.DstName())
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve the statistics for %s in netns %s: %v", i.DstName(), i.ns.path, err)
	}

	stats := l.Attrs().Statistics
	if stats == nil {
		return nil, fmt.Errorf("no statistics were returned")
	}

	return &types.InterfaceStatistics{
		RxBytes:   stats.RxBytes,
		TxBytes:   stats.TxBytes,
		RxPackets: stats.RxPackets,
		TxPackets: stats.TxPackets,
		RxDropped: stats.RxDropped,
		TxDropped: stats.TxDropped,
	}, nil
}

func (n *networkNamespace) findDst(srcName string, isBridge bool) string {
	n.Lock()
	defer n.Unlock()

	for _, i := range n.iFaces {
		// The master should match the srcname of the interface and the
		// master interface should be of type bridge, if searching for a bridge type
		if i.SrcName() == srcName && (!isBridge || i.Bridge()) {
			return i.DstName()
		}
	}

	return ""
}

func (n *networkNamespace) AddInterface(srcName, dstPrefix string, options ...IfaceOption) error {
	i := &nwIface{
		srcName: srcName,
		dstName: dstPrefix,
		ns:      n,
	}
	i.processInterfaceOptions(options...)

	if i.master != "" {
		i.dstMaster = n.findDst(i.master, true)
		if i.dstMaster == "" {
			return fmt.Errorf("could not find an appropriate master %q for %q",
				i.master, i.srcName)
		}
	}

	n.Lock()
	if n.isDefault {
		i.dstName = i.srcName
	} else {
		i.dstName = fmt.Sprintf("%s%d", dstPrefix, n.nextIfIndex[dstPrefix])
		n.nextIfIndex[dstPrefix]++
	}

	path := n.path
	isDefault := n.isDefault
	nlh := n.nlHandle
	nlhHost := ns.NlHandle()
	n.Unlock()

	// If it is a bridge interface we have to create the bridge inside
	// the namespace so don't try to lookup the interface using srcName
	if i.bridge {
		if err := nlh.LinkAdd(&netlink.Bridge{
			LinkAttrs: netlink.LinkAttrs{
				Name: i.srcName,
			},
		}); err != nil {
			return fmt.Errorf("failed to create bridge %q: %v", i.srcName, err)
		}
	} else {
		// Find the network interface identified by the SrcName attribute.
		iface, err := nlhHost.LinkByName(i.srcName)
		if err != nil {
			return fmt.Errorf("failed to get link by name %q: %v", i.srcName, err)
		}

		// Move the network interface to the destination
		// namespace only if the namespace is not a default
		// type
		if !isDefault {
			newNs, err := netns.GetFromPath(path)
			if err != nil {
				return fmt.Errorf("failed get network namespace %q: %v", path, err)
			}
			defer newNs.Close()
			if err := nlhHost.LinkSetNsFd(iface, int(newNs)); err != nil {
				return fmt.Errorf("failed to set namespace on link %q: %v", i.srcName, err)
			}
		}
	}

	// Find the network interface identified by the SrcName attribute.
	iface, err := nlh.LinkByName(i.srcName)
	if err != nil {
		return fmt.Errorf("failed to get link by name %q: %v", i.srcName, err)
	}

	// Down the interface before configuring
	if err := nlh.LinkSetDown(iface); err != nil {
		return fmt.Errorf("failed to set link down: %v", err)
	}

	// Configure the interface now this is moved in the proper namespace.
	if err := configureInterface(nlh, iface, i); err != nil {
		// If configuring the device fails move it back to the host namespace
		// and change the name back to the source name. This allows the caller
		// to properly cleanup the interface. Its important especially for
		// interfaces with global attributes, ex: vni id for vxlan interfaces.
		if nerr := nlh.LinkSetName(iface, i.SrcName()); nerr != nil {
			log.G(context.TODO()).Errorf("renaming interface (%s->%s) failed, %v after config error %v", i.DstName(), i.SrcName(), nerr, err)
		}
		if nerr := nlh.LinkSetNsFd(iface, ns.ParseHandlerInt()); nerr != nil {
			log.G(context.TODO()).Errorf("moving interface %s to host ns failed, %v, after config error %v", i.SrcName(), nerr, err)
		}
		return err
	}

	// Up the interface.
	cnt := 0
	for err = nlh.LinkSetUp(iface); err != nil && cnt < 3; cnt++ {
		log.G(context.TODO()).Debugf("retrying link setup because of: %v", err)
		time.Sleep(10 * time.Millisecond)
		err = nlh.LinkSetUp(iface)
	}
	if err != nil {
		return fmt.Errorf("failed to set link up: %v", err)
	}

	// Set the routes on the interface. This can only be done when the interface is up.
	if err := setInterfaceRoutes(nlh, iface, i); err != nil {
		return fmt.Errorf("error setting interface %q routes to %q: %v", iface.Attrs().Name, i.Routes(), err)
	}

	n.Lock()
	n.iFaces = append(n.iFaces, i)
	n.Unlock()

	n.checkLoV6()

	return nil
}

func configureInterface(nlh *netlink.Handle, iface netlink.Link, i *nwIface) error {
	ifaceName := iface.Attrs().Name
	ifaceConfigurators := []struct {
		Fn         func(*netlink.Handle, netlink.Link, *nwIface) error
		ErrMessage string
	}{
		{setInterfaceName, fmt.Sprintf("error renaming interface %q to %q", ifaceName, i.DstName())},
		{setInterfaceMAC, fmt.Sprintf("error setting interface %q MAC to %q", ifaceName, i.MacAddress())},
		{setInterfaceIP, fmt.Sprintf("error setting interface %q IP to %v", ifaceName, i.Address())},
		{setInterfaceIPv6, fmt.Sprintf("error setting interface %q IPv6 to %v", ifaceName, i.AddressIPv6())},
		{setInterfaceMaster, fmt.Sprintf("error setting interface %q master to %q", ifaceName, i.DstMaster())},
		{setInterfaceLinkLocalIPs, fmt.Sprintf("error setting interface %q link local IPs to %v", ifaceName, i.LinkLocalAddresses())},
	}

	for _, config := range ifaceConfigurators {
		if err := config.Fn(nlh, iface, i); err != nil {
			return fmt.Errorf("%s: %v", config.ErrMessage, err)
		}
	}
	return nil
}

func setInterfaceMaster(nlh *netlink.Handle, iface netlink.Link, i *nwIface) error {
	if i.DstMaster() == "" {
		return nil
	}

	return nlh.LinkSetMaster(iface, &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{Name: i.DstMaster()},
	})
}

func setInterfaceMAC(nlh *netlink.Handle, iface netlink.Link, i *nwIface) error {
	if i.MacAddress() == nil {
		return nil
	}
	return nlh.LinkSetHardwareAddr(iface, i.MacAddress())
}

func setInterfaceIP(nlh *netlink.Handle, iface netlink.Link, i *nwIface) error {
	if i.Address() == nil {
		return nil
	}
	if err := checkRouteConflict(nlh, i.Address(), netlink.FAMILY_V4); err != nil {
		return err
	}
	ipAddr := &netlink.Addr{IPNet: i.Address(), Label: ""}
	return nlh.AddrAdd(iface, ipAddr)
}

func setInterfaceIPv6(nlh *netlink.Handle, iface netlink.Link, i *nwIface) error {
	if i.AddressIPv6() == nil {
		return nil
	}
	if err := checkRouteConflict(nlh, i.AddressIPv6(), netlink.FAMILY_V6); err != nil {
		return err
	}
	if err := setIPv6(i.ns.path, i.DstName(), true); err != nil {
		return fmt.Errorf("failed to enable ipv6: %v", err)
	}
	ipAddr := &netlink.Addr{IPNet: i.AddressIPv6(), Label: "", Flags: syscall.IFA_F_NODAD}
	return nlh.AddrAdd(iface, ipAddr)
}

func setInterfaceLinkLocalIPs(nlh *netlink.Handle, iface netlink.Link, i *nwIface) error {
	for _, llIP := range i.LinkLocalAddresses() {
		ipAddr := &netlink.Addr{IPNet: llIP}
		if err := nlh.AddrAdd(iface, ipAddr); err != nil {
			return err
		}
	}
	return nil
}

func setInterfaceName(nlh *netlink.Handle, iface netlink.Link, i *nwIface) error {
	return nlh.LinkSetName(iface, i.DstName())
}

func setInterfaceRoutes(nlh *netlink.Handle, iface netlink.Link, i *nwIface) error {
	for _, route := range i.Routes() {
		err := nlh.RouteAdd(&netlink.Route{
			Scope:     netlink.SCOPE_LINK,
			LinkIndex: iface.Attrs().Index,
			Dst:       route,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func checkRouteConflict(nlh *netlink.Handle, address *net.IPNet, family int) error {
	routes, err := nlh.RouteList(nil, family)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if route.Dst != nil {
			if route.Dst.Contains(address.IP) || address.Contains(route.Dst.IP) {
				return fmt.Errorf("cannot program address %v in sandbox interface because it conflicts with existing route %s",
					address, route)
			}
		}
	}
	return nil
}
