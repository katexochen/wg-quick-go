package wgquick

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
)

// Up sets and configures the wg interface. Mostly equivalent to `wg-quick up iface`
func Up(cfg *Config, iface string) error {
	_, err := netlink.LinkByName(iface)
	if err == nil {
		return os.ErrExist
	}
	if _, ok := err.(netlink.LinkNotFoundError); !ok {
		return err
	}

	for _, dns := range cfg.DNS {
		if err := execSh("resolvconf -a tun.%i -m 0 -x", iface, fmt.Sprintf("nameserver %s\n", dns)); err != nil {
			return err
		}
	}

	if cfg.PreUp != "" {
		if err := execSh(cfg.PreUp, iface); err != nil {
			return err
		}
	}
	if err := Sync(cfg, iface); err != nil {
		return err
	}

	if cfg.PostUp != "" {
		if err := execSh(cfg.PostUp, iface); err != nil {
			return err
		}
	}
	return nil
}

// Down destroys the wg interface. Mostly equivalent to `wg-quick down iface`
func Down(cfg *Config, iface string) error {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return err
	}

	if len(cfg.DNS) > 1 {
		if err := execSh("resolvconf -d tun.%s", iface); err != nil {
			return err
		}
	}

	if cfg.PreDown != "" {
		if err := execSh(cfg.PreDown, iface); err != nil {
			return err
		}
	}

	if err := netlink.LinkDel(link); err != nil {
		return err
	}

	if cfg.PostDown != "" {
		if err := execSh(cfg.PostDown, iface); err != nil {
			return err
		}
	}
	return nil
}

func execSh(command string, iface string, stdin ...string) error {
	cmd := exec.Command("sh", "-ce", strings.ReplaceAll(command, "%i", iface))
	if len(stdin) > 0 {
		b := &bytes.Buffer{}
		for _, ln := range stdin {
			if _, err := fmt.Fprint(b, ln); err != nil {
				return err
			}
		}
		cmd.Stdin = b
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to execute %s:\n%s: %s", cmd.Args, out, err)
	}
	return nil
}

// Sync the config to the current setup for given interface
// It perform 4 operations:
// * SyncLink --> makes sure link is up and type wireguard
// * SyncWireguardDevice --> configures allowedIP & other wireguard specific settings
// * SyncAddress --> synces linux addresses bounded to this interface
// * SyncRoutes --> synces all allowedIP routes to route to this interface
func Sync(cfg *Config, iface string) error {
	link, err := SyncLink(cfg, iface)
	if err != nil {
		return fmt.Errorf("cannot sync wireguard link: %s", err)
	}

	if err := SyncWireguardDevice(cfg, link); err != nil {
		return fmt.Errorf("cannot sync wireguard link: %s", err)
	}

	if err := SyncAddress(cfg, link); err != nil {
		return fmt.Errorf("cannot sync addresses: %s", err)
	}

	var managedRoutes []net.IPNet
	for _, peer := range cfg.Peers {
		for _, rt := range peer.AllowedIPs {
			managedRoutes = append(managedRoutes, rt)
		}
	}
	if err := SyncRoutes(cfg, link, managedRoutes); err != nil {
		return fmt.Errorf("cannot sync routes: %s", err)
	}
	return nil
}

// SyncWireguardDevice synces wireguard vpn setting on the given link. It does not set routes/addresses beyond wg internal crypto-key routing, only handles wireguard specific settings
func SyncWireguardDevice(cfg *Config, link netlink.Link) error {
	cl, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("cannot setup wireguard device: %s", err)
	}
	if err := cl.ConfigureDevice(link.Attrs().Name, cfg.Config); err != nil {
		return fmt.Errorf("cannot configure device: %s", err)
	}
	return nil
}

// SyncLink synces link state with the config. It does not sync Wireguard settings, just makes sure the device is up and type wireguard
func SyncLink(cfg *Config, iface string) (netlink.Link, error) {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); !ok {
			return nil, fmt.Errorf("cannot read link: %s", err)
		}
		wgLink := &netlink.GenericLink{
			LinkAttrs: netlink.LinkAttrs{
				Name: iface,
				MTU:  cfg.MTU,
			},
			LinkType: "wireguard",
		}
		if err := netlink.LinkAdd(wgLink); err != nil {
			return nil, fmt.Errorf("cannot create link: %s", err)
		}

		link, err = netlink.LinkByName(iface)
		if err != nil {
			return nil, fmt.Errorf("cannot read link: %s", err)
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return nil, fmt.Errorf("cannot set link up: %s", err)
	}
	return link, nil
}

// SyncAddress adds/deletes all lind assigned IPV4 addressed as specified in the config
func SyncAddress(cfg *Config, link netlink.Link) error {
	addrs, err := netlink.AddrList(link, syscall.AF_INET)
	if err != nil {
		return fmt.Errorf("cannot read link address: %s", err)
	}

	// nil addr means I've used it
	presentAddresses := make(map[string]netlink.Addr, 0)
	for _, addr := range addrs {
		presentAddresses[addr.IPNet.String()] = addr
	}

	for _, addr := range cfg.Address {
		_, present := presentAddresses[addr.String()]
		presentAddresses[addr.String()] = netlink.Addr{} // mark as present
		if present {
			continue
		}
		if err := netlink.AddrAdd(link, &netlink.Addr{
			IPNet: &addr,
			Label: cfg.AddressLabel,
		}); err != nil {
			if err != syscall.EEXIST {
				return fmt.Errorf("cannot add addr: %s", err)
			}
		}
	}

	for _, addr := range presentAddresses {
		if addr.IPNet == nil {
			continue
		}

		if err := netlink.AddrDel(link, &addr); err != nil {
			return fmt.Errorf("cannot delete addr: %s", err)
		}
	}
	return nil
}

func fillRouteDefaults(rt *netlink.Route) {
	// fill defaults
	if rt.Table == 0 {
		rt.Table = unix.RT_CLASS_MAIN
	}

	if rt.Protocol == 0 {
		rt.Protocol = unix.RTPROT_BOOT
	}

	if rt.Type == 0 {
		rt.Type = unix.RTN_UNICAST
	}
}

// SyncRoutes adds/deletes all route assigned IPV4 addressed as specified in the config
func SyncRoutes(cfg *Config, link netlink.Link, managedRoutes []net.IPNet) error {
	wantedRoutes := make(map[string][]netlink.Route, len(managedRoutes))
	presentRoutes, err := netlink.RouteList(link, syscall.AF_INET)
	if err != nil {
		return fmt.Errorf("cannot read existing routes: %s", err)
	}
	for _, rt := range managedRoutes {
		rt := rt // make copy

		nrt := netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       &rt,
			Table:     cfg.Table,
			Protocol:  netlink.RouteProtocol(cfg.RouteProtocol),
			Priority:  cfg.RouteMetric,
		}
		fillRouteDefaults(&nrt)
		wantedRoutes[rt.String()] = append(wantedRoutes[rt.String()], nrt)
	}

	for _, rtLst := range wantedRoutes {
		for _, rt := range rtLst {
			rt := rt // make copy
			if err := netlink.RouteReplace(&rt); err != nil {
				return fmt.Errorf("cannot add/replace route: %s", err)
			}
		}
	}

	checkWanted := func(rt netlink.Route) bool {
		for _, candidateRt := range wantedRoutes[rt.Dst.String()] {
			if rt.Equal(candidateRt) {
				return true
			}
		}
		return false
	}

	for _, rt := range presentRoutes {
		if !(rt.Table == cfg.Table || (cfg.Table == 0 && rt.Table == unix.RT_CLASS_MAIN)) {
			continue
		}

		if !(rt.Protocol == netlink.RouteProtocol(cfg.RouteProtocol)) {
			continue
		}

		if checkWanted(rt) {
			continue
		}

		if err := netlink.RouteDel(&rt); err != nil {
			return fmt.Errorf("cannot delete route: %s", err)
		}
	}

	return nil
}
