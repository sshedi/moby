package networking

import (
	"net"
	"os"
	"testing"

	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/integration/internal/container"
	"github.com/docker/docker/integration/internal/network"
	"github.com/docker/docker/testutil/daemon"
	"github.com/miekg/dns"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

// writeTempResolvConf writes a resolv.conf that only contains a single
// nameserver line, with address addr.
// It returns the name of the temp file.
func writeTempResolvConf(t *testing.T, addr string) string {
	t.Helper()
	// Not using t.TempDir() here because in rootless mode, while the temporary
	// directory gets mode 0777, it's a subdir of an 0700 directory owned by root.
	// So, it's not accessible by the daemon.
	f, err := os.CreateTemp("", "resolv.conf")
	assert.NilError(t, err)
	t.Cleanup(func() { os.Remove(f.Name()) })
	err = f.Chmod(0644)
	assert.NilError(t, err)
	f.Write([]byte("nameserver " + addr + "\n"))
	return f.Name()
}

const dnsRespAddr = "10.11.12.13"

// startDaftDNS starts and returns a really, really daft DNS server that only
// responds to type-A requests, and always with address dnsRespAddr.
func startDaftDNS(t *testing.T, addr string) *dns.Server {
	serveDNS := func(w dns.ResponseWriter, query *dns.Msg) {
		if query.Question[0].Qtype == dns.TypeA {
			resp := &dns.Msg{}
			resp.SetReply(query)
			answer := &dns.A{
				Hdr: dns.RR_Header{
					Name:   query.Question[0].Name,
					Rrtype: dns.TypeA,
					Class:  dns.ClassINET,
					Ttl:    600,
				},
			}
			answer.A = net.ParseIP(dnsRespAddr)
			resp.Answer = append(resp.Answer, answer)
			_ = w.WriteMsg(resp)
		}
	}

	conn, err := net.ListenUDP("udp", &net.UDPAddr{
		IP:   net.ParseIP(addr),
		Port: 53,
	})
	assert.NilError(t, err)

	server := &dns.Server{Handler: dns.HandlerFunc(serveDNS), PacketConn: conn}
	go func() {
		_ = server.ActivateAndServe()
	}()

	return server
}

// Check that when a container is connected to an internal network, DNS
// requests sent to daemon's internal DNS resolver are not forwarded to
// an upstream resolver listening on a localhost address.
// (Assumes the host does not already have a DNS server on 127.0.0.1.)
func TestInternalNetworkDNS(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "No resolv.conf on Windows")
	skip.If(t, testEnv.IsRootless, "Can't use resolver on host in rootless mode")
	ctx := setupTest(t)

	// Start a DNS server on the loopback interface.
	server := startDaftDNS(t, "127.0.0.1")
	defer server.Shutdown()

	// Set up a temp resolv.conf pointing at that DNS server, and a daemon using it.
	tmpFileName := writeTempResolvConf(t, "127.0.0.1")
	d := daemon.New(t, daemon.WithEnvVars("DOCKER_TEST_RESOLV_CONF_PATH="+tmpFileName))
	d.StartWithBusybox(ctx, t, "--experimental", "--ip6tables")
	defer d.Stop(t)

	c := d.NewClientT(t)
	defer c.Close()

	intNetName := "intnet"
	network.CreateNoError(ctx, t, c, intNetName,
		network.WithDriver("bridge"),
		network.WithInternal(),
	)
	defer network.RemoveNoError(ctx, t, c, intNetName)

	extNetName := "extnet"
	network.CreateNoError(ctx, t, c, extNetName,
		network.WithDriver("bridge"),
	)
	defer network.RemoveNoError(ctx, t, c, extNetName)

	// Create a container, initially with external connectivity.
	// Expect the external DNS server to respond to a request from the container.
	ctrId := container.Run(ctx, t, c, container.WithNetworkMode(extNetName))
	defer c.ContainerRemove(ctx, ctrId, containertypes.RemoveOptions{Force: true})
	res, err := container.Exec(ctx, c, ctrId, []string{"nslookup", "test.example"})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(res.ExitCode, 0))
	assert.Check(t, is.Contains(res.Stdout(), dnsRespAddr))

	// Connect the container to the internal network as well.
	// External DNS should still be used.
	err = c.NetworkConnect(ctx, intNetName, ctrId, nil)
	assert.NilError(t, err)
	res, err = container.Exec(ctx, c, ctrId, []string{"nslookup", "test.example"})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(res.ExitCode, 0))
	assert.Check(t, is.Contains(res.Stdout(), dnsRespAddr))

	// Disconnect from the external network.
	// Expect no access to the external DNS.
	err = c.NetworkDisconnect(ctx, extNetName, ctrId, true)
	assert.NilError(t, err)
	res, err = container.Exec(ctx, c, ctrId, []string{"nslookup", "test.example"})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(res.ExitCode, 1))
	assert.Check(t, is.Contains(res.Stdout(), "SERVFAIL"))

	// Reconnect the external network.
	// Check that the external DNS server is used again.
	err = c.NetworkConnect(ctx, extNetName, ctrId, nil)
	assert.NilError(t, err)
	res, err = container.Exec(ctx, c, ctrId, []string{"nslookup", "test.example"})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(res.ExitCode, 0))
	assert.Check(t, is.Contains(res.Stdout(), dnsRespAddr))
}