package floatingip

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"git.code.oa.com/gaiastack/galaxy/pkg/utils/database"
	"git.code.oa.com/gaiastack/galaxy/pkg/utils/nets"
	"github.com/golang/glog"
	"github.com/jinzhu/gorm"
)

var (
	ErrNoEnoughIP     = fmt.Errorf("no enough available ips left")
	ErrNoFIPForSubnet = fmt.Errorf("no fip configured for subnet")
)

type IPAM interface {
	ConfigurePool([]*FloatingIP) error
	AllocateSpecificIP(string, net.IP, database.ReleasePolicy, string) error
	AllocateInSubnet(string, *net.IPNet, database.ReleasePolicy, string) (net.IP, error)
	AllocateInSubnetWithKey(oldK, newK, subnet string, policy database.ReleasePolicy, attr string) error
	UpdateKey(oldK, newK string) error
	UpdatePolicy(string, net.IP, database.ReleasePolicy, string) error
	Release([]string) error
	ReleaseByPrefix(string) error
	First(string) (*FloatingIPInfo, error) // returns nil,nil if key is not found
	ByIP(net.IP) (database.FloatingIP, error)
	ByPrefix(string) ([]database.FloatingIP, error)
	RoutableSubnet(net.IP) *net.IPNet
	QueryRoutableSubnetByKey(key string) ([]string, error)
	Shutdown()
	Name() string
}

type FloatingIPInfo struct {
	IPInfo IPInfo
	FIP    database.FloatingIP
}

type IPInfo struct {
	IP             *nets.IPNet `json:"ip"`
	Vlan           uint16      `json:"vlan"`
	Gateway        net.IP      `json:"gateway"`
	RoutableSubnet *nets.IPNet `json:"routable_subnet"` //the node subnet
}

// ipam manages floating ip allocation and release and does it atomically
// with the help of atomic operation of the store
type ipam struct {
	FloatingIPs []*FloatingIP `json:"floatingips,omitempty"`
	store       *database.DBRecorder
	TableName   string
}

func NewIPAM(store *database.DBRecorder) IPAM {
	return NewIPAMWithTableName(store, database.DefaultFloatingipTableName)
}

func NewIPAMWithTableName(store *database.DBRecorder, tableName string) IPAM {
	if err := store.CreateTableIfNotExist(&database.FloatingIP{Table: tableName}); err != nil {
		glog.Fatalf("failed to create table %s", tableName)
	}
	return &ipam{
		store:     store,
		TableName: tableName,
	}
}

func (i *ipam) Name() string {
	return i.TableName
}

func (i *ipam) mergeWithDB(fipMap map[string]*FloatingIP) error {
	ips, err := i.findAll()
	if err != nil {
		return err
	}
	var toBeDelete []uint32
	// delete no longer available floating ips stored in the db first
	for _, ip := range ips {
		netIP := nets.IntToIP(ip.IP)
		found := false
		for _, fipConf := range fipMap {
			if fipConf.IPNet().Contains(netIP) {
				found = true
				if !fipConf.Contains(netIP) {
					toBeDelete = append(toBeDelete, ip.IP)
				}
				break
			}
		}
		if !found {
			toBeDelete = append(toBeDelete, ip.IP)
		}
	}
	if len(toBeDelete) > 0 {
		deleted, err := i.deleteUnScoped(toBeDelete)
		if err != nil {
			return fmt.Errorf("failed to delete ip %v: %v", toBeDelete, err)
		}
		glog.Infof("expect to delete %d ips from ips from %v, deleted %d", len(toBeDelete), toBeDelete, deleted)
	}
	// insert new floating ips
	for _, fipConf := range fipMap {
		subnet := fipConf.RoutableSubnet.String()
		for _, ipr := range fipConf.IPRanges {
			first := nets.IPToInt(ipr.First)
			last := nets.IPToInt(ipr.Last)
			for ; first <= last; first++ {
				fip := database.FloatingIP{IP: first, Key: "", Subnet: subnet}
				if err := i.create(&fip); err != nil {
					if !strings.Contains(err.Error(), fmt.Sprintf(`Duplicate entry '%d' for key 'PRIMARY'`, first)) {
						return fmt.Errorf("Error creating floating ip %d: %v", first, err)
					}
				}
			}
		}
	}
	return nil
}

func (i *ipam) ConfigurePool(floatingIPs []*FloatingIP) error {
	sort.Sort(FloatingIPSlice(floatingIPs))
	glog.Infof("floating ip config %v", floatingIPs)
	i.FloatingIPs = floatingIPs
	floatingIPMap := make(map[string]*FloatingIP)
	for _, fip := range i.FloatingIPs {
		if _, exists := floatingIPMap[fip.Key()]; exists {
			glog.Warningf("Exists floating ip conf %v", fip)
			continue
		}
		floatingIPMap[fip.Key()] = fip
	}
	if err := i.mergeWithDB(floatingIPMap); err != nil {
		return err
	}
	return nil
}

// allocate allocate len(keys) ips, it guarantees to allocate everything
// or nothing.
func (i *ipam) allocate(keys []string) (allocated []net.IP, err error) {
	var fips []database.FloatingIP
	for {
		if err = i.findAvailable(len(keys), &fips); err != nil {
			return
		}
		if len(fips) != len(keys) {
			err = ErrNoEnoughIP
			return
		}
		var updateOps []database.ActionFunc
		for j := 0; j < len(keys); j++ {
			fips[j].Key = keys[j]
			updateOps = append(updateOps, allocateOp(&fips[j], i.TableName))
		}
		if err = i.store.Transaction(updateOps...); err != nil {
			if err == ErrNotUpdated {
				// Loop if a floating ip has been allocated by the others
				err = nil
			} else {
				return
			}
		} else {
			break
		}
	}
	for _, fip := range fips {
		allocated = append(allocated, nets.IntToIP(fip.IP))
	}
	return
}

func (i *ipam) Release(keys []string) error {
	return i.releaseIPs(keys)
}

func (i *ipam) ReleaseByPrefix(keyPrefix string) error {
	return i.releaseByPrefix(keyPrefix)
}

func (i *ipam) first(key string) (*IPInfo, error) {
	fipInfo, err := i.First(key)
	if err != nil || fipInfo == nil {
		return nil, err
	}
	return &fipInfo.IPInfo, nil
}

func (i *ipam) First(key string) (*FloatingIPInfo, error) {
	var fip database.FloatingIP
	if err := i.findByKey(key, &fip); err != nil {
		return nil, err
	}
	if fip.IP == 0 {
		return nil, nil
	}
	netIP := nets.IntToIP(fip.IP)
	for _, fips := range i.FloatingIPs {
		if fips.Contains(netIP) {
			ip := nets.IPNet(net.IPNet{
				IP:   netIP,
				Mask: fips.Mask,
			})
			return &FloatingIPInfo{
				IPInfo: IPInfo{
					IP:             &ip,
					Vlan:           fips.Vlan,
					Gateway:        fips.Gateway,
					RoutableSubnet: nets.NetsIPNet(fips.RoutableSubnet),
				},
				FIP: fip,
			}, nil
		}
	}
	return nil, fmt.Errorf("could not find match floating ip config for ip %s", netIP.String())
}

func (i *ipam) Shutdown() {
	if i.store != nil {
		i.store.Shutdown()
	}
}

func (i *ipam) AllocateInSubnet(key string, routableSubnet *net.IPNet, policy database.ReleasePolicy, attr string) (allocated net.IP, err error) {
	if routableSubnet == nil {
		// this should never happen
		return nil, fmt.Errorf("nil routableSubnet")
	}
	ipNet := i.toFIPSubnet(routableSubnet)
	if ipNet == nil {
		var allRoutableSubnet []string
		for j := range i.FloatingIPs {
			allRoutableSubnet = append(allRoutableSubnet, i.FloatingIPs[j].RoutableSubnet.String())
		}
		glog.V(3).Infof("can't find fit routableSubnet %s, all routableSubnets %v", routableSubnet.String(), allRoutableSubnet)
		err = ErrNoFIPForSubnet
		return
	}
	if err = i.allocateOneInSubnet(key, routableSubnet.String(), uint16(policy), attr); err != nil {
		if err == ErrNotUpdated {
			err = ErrNoEnoughIP
		}
		return
	}
	var fip database.FloatingIP
	if err = i.findByKey(key, &fip); err != nil {
		return
	}
	allocated = nets.IntToIP(fip.IP)
	return
}

func (i *ipam) applyFloatingIPs(fips []*FloatingIP) []*FloatingIP {
	res := make(map[string]*FloatingIP, len(i.FloatingIPs))
	for j := range i.FloatingIPs {
		ofip := i.FloatingIPs[j]
		fip := FloatingIP{
			RoutableSubnet: ofip.RoutableSubnet,
			SparseSubnet: nets.SparseSubnet{
				Gateway: ofip.Gateway,
				Mask:    ofip.Mask,
				Vlan:    ofip.Vlan,
			},
		}
		for k := range ofip.IPRanges {
			fip.IPRanges = append(fip.IPRanges, ofip.IPRanges[k])
		}

		res[ofip.RoutableSubnet.String()] = &fip
	}
	for j, fip := range fips {
		if ofip, exist := res[fip.RoutableSubnet.String()]; exist {
			for _, ipRange := range fip.IPRanges {
				for ipn := nets.IPToInt(ipRange.First); ipn <= nets.IPToInt(ipRange.Last); ipn++ {
					ofip.InsertIP(nets.IntToIP(ipn))
				}
			}
		} else {
			res[fip.RoutableSubnet.String()] = fips[j]
		}
	}

	var s []*FloatingIP
	for _, fip := range res {
		s = append(s, fip)
	}
	return s
}

func (i *ipam) toFIPSubnet(routableSubnet *net.IPNet) *net.IPNet {
	for _, fip := range i.FloatingIPs {
		if fip.RoutableSubnet.String() == routableSubnet.String() {
			return fip.IPNet()
		}
	}
	return nil
}

func (i *ipam) QueryByPrefix(prefix string) (map[string]string, error) {
	fips, err := i.ByPrefix(prefix)
	if err != nil {
		return nil, err
	}
	ips := make(map[string]string, len(fips))
	for i := range fips {
		ips[nets.IntToIP(fips[i].IP).String()] = fips[i].Key
	}
	return ips, nil
}

func (i *ipam) ByPrefix(prefix string) ([]database.FloatingIP, error) {
	var fips []database.FloatingIP
	if err := i.findByPrefix(prefix, &fips); err != nil {
		return nil, fmt.Errorf("failed to find by prefix %s: %v", prefix, err)
	}
	return fips, nil
}

func (i *ipam) RoutableSubnet(nodeIP net.IP) *net.IPNet {
	intIP := nets.IPToInt(nodeIP)
	minIndex := sort.Search(len(i.FloatingIPs), func(j int) bool {
		return nets.IPToInt(i.FloatingIPs[j].RoutableSubnet.IP) > intIP
	})
	if minIndex == 0 {
		return nil
	}
	if i.FloatingIPs[minIndex-1].RoutableSubnet.Contains(nodeIP) {
		return i.FloatingIPs[minIndex-1].RoutableSubnet
	}
	return nil
}

func (i *ipam) QueryRoutableSubnetByKey(key string) ([]string, error) {
	return i.queryByKeyGroupBySubnet(key)
}

func (i *ipam) ByIP(ip net.IP) (database.FloatingIP, error) {
	return i.findByIP(nets.IPToInt(ip))
}

func (i *ipam) AllocateSpecificIP(key string, ip net.IP, policy database.ReleasePolicy, attr string) error {
	return i.updateKey(nets.IPToInt(ip), key, uint16(policy), attr)
}

func (i *ipam) UpdatePolicy(key string, ip net.IP, policy database.ReleasePolicy, attr string) error {
	return i.updatePolicy(nets.IPToInt(ip), key, uint16(policy), attr)
}

func (i *ipam) UpdateKey(oldK, newK string) error {
	return i.store.Transaction(func(tx *gorm.DB) error {
		return tx.Table(i.Name()).Where("`key` = ?", oldK).
			UpdateColumns(map[string]interface{}{
				"key":  newK,
				"attr": "",
			}).Error
	})
}

func (i *ipam) AllocateInSubnetWithKey(oldK, newK, subnet string, policy database.ReleasePolicy, attr string) error {
	return i.store.Transaction(func(tx *gorm.DB) error {
		ret := tx.Table(i.Name()).Where("`key` = ? AND subnet = ?", oldK, subnet).Limit(1).UpdateColumns(map[string]interface{}{`key`: newK, "policy": policy, "attr": attr})
		if ret.Error != nil {
			return ret.Error
		}
		if ret.RowsAffected != 1 {
			return ErrNotUpdated
		}
		return nil
	})
}