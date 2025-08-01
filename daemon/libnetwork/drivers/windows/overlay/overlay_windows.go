package overlay

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"

	"github.com/Microsoft/hcsshim"
	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/scope"
)

const (
	NetworkType = "overlay"
)

var _ driverapi.TableWatcher = (*driver)(nil)

type driver struct {
	networks networkTable
	sync.Mutex
}

// Register registers a new instance of the overlay driver.
func Register(r driverapi.Registerer) error {
	d := &driver{
		networks: networkTable{},
	}

	d.restoreHNSNetworks()

	return r.RegisterDriver(NetworkType, d, driverapi.Capability{
		DataScope:         scope.Global,
		ConnectivityScope: scope.Global,
	})
}

func (d *driver) restoreHNSNetworks() error {
	log.G(context.TODO()).Infof("Restoring existing overlay networks from HNS into docker")

	hnsresponse, err := hcsshim.HNSListNetworkRequest(http.MethodGet, "", "")
	if err != nil {
		return err
	}

	for _, v := range hnsresponse {
		if v.Type != NetworkType {
			continue
		}

		log.G(context.TODO()).Infof("Restoring overlay network: %s", v.Name)
		n := d.convertToOverlayNetwork(&v)
		d.addNetwork(n)

		//
		// We assume that any network will be recreated on daemon restart
		// and therefore don't restore hns endpoints for now
		//
		// n.restoreNetworkEndpoints()
	}

	return nil
}

func (d *driver) convertToOverlayNetwork(v *hcsshim.HNSNetwork) *network {
	n := &network{
		id:              v.Name,
		hnsID:           v.Id,
		driver:          d,
		endpoints:       endpointTable{},
		subnets:         []*subnet{},
		providerAddress: v.ManagementIP,
	}

	for _, hnsSubnet := range v.Subnets {
		vsidPolicy := &hcsshim.VsidPolicy{}
		for _, policy := range hnsSubnet.Policies {
			if err := json.Unmarshal([]byte(policy), &vsidPolicy); err == nil && vsidPolicy.Type == "VSID" {
				break
			}
		}

		gwIP := net.ParseIP(hnsSubnet.GatewayAddress)
		localsubnet := &subnet{
			vni:  uint32(vsidPolicy.VSID),
			gwIP: &gwIP,
		}

		_, subnetIP, err := net.ParseCIDR(hnsSubnet.AddressPrefix)
		if err != nil {
			log.G(context.TODO()).Errorf("Error parsing subnet address %s ", hnsSubnet.AddressPrefix)
			continue
		}

		localsubnet.subnetIP = subnetIP

		n.subnets = append(n.subnets, localsubnet)
	}

	return n
}

func (d *driver) Type() string {
	return NetworkType
}

func (d *driver) IsBuiltIn() bool {
	return true
}
