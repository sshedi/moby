//go:build linux

package macvlan

import (
	"context"
	"fmt"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/netlabel"
	"github.com/moby/moby/v2/daemon/libnetwork/netutils"
	"github.com/moby/moby/v2/daemon/libnetwork/ns"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/moby/moby/v2/errdefs"
)

// CreateEndpoint assigns the mac, ip and endpoint id for the new container
func (d *driver) CreateEndpoint(ctx context.Context, nid, eid string, ifInfo driverapi.InterfaceInfo, epOptions map[string]interface{}) error {
	if err := validateID(nid, eid); err != nil {
		return err
	}
	n, err := d.getNetwork(nid)
	if err != nil {
		return errdefs.System(fmt.Errorf("network id %q not found", nid))
	}
	ep := &endpoint{
		id:     eid,
		nid:    nid,
		addr:   ifInfo.Address(),
		addrv6: ifInfo.AddressIPv6(),
		mac:    ifInfo.MacAddress(),
	}
	if ep.mac == nil {
		ep.mac = netutils.GenerateRandomMAC()
		if err := ifInfo.SetMacAddress(ep.mac); err != nil {
			return err
		}
	}
	// disallow portmapping -p
	if opt, ok := epOptions[netlabel.PortMap]; ok {
		if _, ok := opt.([]types.PortBinding); ok {
			if len(opt.([]types.PortBinding)) > 0 {
				log.G(ctx).Warnf("macvlan driver does not support port mappings")
			}
		}
	}
	// disallow port exposure --expose
	if opt, ok := epOptions[netlabel.ExposedPorts]; ok {
		if _, ok := opt.([]types.TransportPort); ok {
			if len(opt.([]types.TransportPort)) > 0 {
				log.G(ctx).Warnf("macvlan driver does not support port exposures")
			}
		}
	}

	if err := d.storeUpdate(ep); err != nil {
		return fmt.Errorf("failed to save macvlan endpoint %.7s to store: %v", ep.id, err)
	}

	n.addEndpoint(ep)

	return nil
}

// DeleteEndpoint removes the endpoint and associated netlink interface
func (d *driver) DeleteEndpoint(nid, eid string) error {
	if err := validateID(nid, eid); err != nil {
		return err
	}
	n := d.network(nid)
	if n == nil {
		return fmt.Errorf("network id %q not found", nid)
	}
	ep := n.endpoint(eid)
	if ep == nil {
		return fmt.Errorf("endpoint id %q not found", eid)
	}
	if link, err := ns.NlHandle().LinkByName(ep.srcName); err == nil {
		if err := ns.NlHandle().LinkDel(link); err != nil {
			log.G(context.TODO()).WithError(err).Warnf("Failed to delete interface (%s)'s link on endpoint (%s) delete", ep.srcName, ep.id)
		}
	}

	if err := d.storeDelete(ep); err != nil {
		log.G(context.TODO()).Warnf("Failed to remove macvlan endpoint %.7s from store: %v", ep.id, err)
	}

	n.deleteEndpoint(ep.id)

	return nil
}
