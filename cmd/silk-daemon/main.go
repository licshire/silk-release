package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/silk/client/config"
	"code.cloudfoundry.org/silk/client/state"
	"code.cloudfoundry.org/silk/daemon"
	daemonConfig "code.cloudfoundry.org/silk/daemon/config"
	"code.cloudfoundry.org/silk/daemon/lib"
	libAdapter "code.cloudfoundry.org/silk/lib/adapter"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/http_server"
	"github.com/tedsuo/ifrit/sigmon"
)

func main() {
	if err := mainWithError(); err != nil {
		log.Fatalf("silk-daemon error: %s", err)
	}
}

func BuildHealthCheckServer(healthCheckPort uint16, lease state.SubnetLease) (ifrit.Runner, error) {
	leaseBytes, err := json.Marshal(lease)
	if err != nil {
		return nil, fmt.Errorf("unmarshaling lease: %s", err) // not possible
	}

	return http_server.New(
		fmt.Sprintf("127.0.0.1:%d", healthCheckPort),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(leaseBytes)
		}),
	), nil
}

func determineVTEPOverlayIP(lease state.SubnetLease) (net.IP, error) {
	baseAddress, _, err := net.ParseCIDR(lease.Subnet)
	if err != nil {
		return nil, fmt.Errorf("parse subnet lease: %s", err)
	}
	return baseAddress, nil
}

func mainWithError() error {
	logger := lager.NewLogger("silk-daemon")
	sink := lager.NewWriterSink(os.Stdout, lager.INFO)
	logger.RegisterSink(sink)

	configFilePath := flag.String("config", "", "path to config file")
	flag.Parse()

	cfg, err := config.LoadConfig(*configFilePath)
	if err != nil {
		return fmt.Errorf("loading config file: %s", err)
	}

	lease, err := state.LoadSubnetLease(cfg.LocalStateFile)
	if err != nil {
		return fmt.Errorf("loading state file: %s", err)
	}

	_, err = lib.NewLeaseController(cfg, logger)
	if err != nil {
		return fmt.Errorf("creating lease controller: %s", err)
	}

	if cfg.HealthCheckPort == 0 {
		return fmt.Errorf("invalid health check port: %d", cfg.HealthCheckPort)
	}

	localIP := net.ParseIP(cfg.UnderlayIP)
	if localIP == nil {
		return fmt.Errorf("parse underlay ip: %s", cfg.UnderlayIP)
	}

	overlayIP, err := determineVTEPOverlayIP(lease)
	if err != nil {
		return fmt.Errorf("determine vtep overlay ip: %s", err)
	}

	underlayInterface, err := daemon.LocateInterface(localIP)
	if err != nil {
		return fmt.Errorf("find device from ip %s: %s", localIP, err) // not tested
	}

	vtepFactory := &daemon.VTEPFactory{
		NetlinkAdapter:           &libAdapter.NetlinkAdapter{},
		HardwareAddressGenerator: &daemonConfig.HardwareAddressGenerator{},
	}

	err = vtepFactory.CreateVTEP(cfg.VTEPName, underlayInterface, localIP, overlayIP)
	if err != nil {
		return fmt.Errorf("create vtep: %s", err)
	}

	healthCheckServer, err := BuildHealthCheckServer(cfg.HealthCheckPort, lease)
	if err != nil {
		return fmt.Errorf("create health check server: %s", err) // not tested
	}

	members := grouper.Members{
		{"server", healthCheckServer},
	}
	group := grouper.NewOrdered(os.Interrupt, members)
	monitor := ifrit.Invoke(sigmon.New(group))

	err = <-monitor.Wait()
	return err
}