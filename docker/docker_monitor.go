package main

import (
	"flag"
	"fmt"
	log "github.com/Sirupsen/logrus"
	apiserver "github.com/docker/docker/api/server"
	"github.com/docker/docker/daemon"
	"github.com/docker/docker/dockerversion"
	"github.com/docker/docker/pkg/reexec"
	"github.com/docker/docker/pkg/signal"
	"github.com/docker/libcontainer/syncpipe"
	"os"
)

const dockerMonitor = "monitor"

var (
	monitorCfg = &daemon.MonitorConfig{}
)

func init() {
	reexec.Register(dockerMonitor, execMonitor)
}

func execMonitor() {
	log.Debugf("Starting docker monitor %v", os.Args)

	var (
		root        string
		socketGroup string
		ID          string
		err         error
	)

	flag.StringVar(&root, "root", "/var/lib/docker", "docker root directory")
	flag.StringVar(&socketGroup, "group", "docker", "socket user group")
	flag.StringVar(&ID, "id", "", "container id")

	flag.Parse()

	initLogging(true)

	if ID == "" || root == "" {
		log.Errorf("%s: container id can't be null", dockerMonitor)
		flag.Usage()
		os.Exit(1)
	}

	monitorCfg.ID = ID
	monitorCfg.Root = root
	monitorCfg.SocketGroup = socketGroup

	syncPipe, err := syncpipe.NewSyncPipeFromFd(0, 3)
	if err != nil {
		log.Errorf("%s: open sync pipe failed: %v", dockerMonitor, err)
		os.Exit(1)
	}

	watching := daemon.NewStateWatcher()

	log.Debugf("MonitorConfig: %v", *monitorCfg)

	monitor := daemon.InitDockerMonitor(monitorCfg, watching)
	if monitor == nil {
		log.Errorf("Initial docker monitor return nil")
		os.Exit(1)
	}

	srv := apiserver.NewMonitorServer(&apiserver.ServerConfig{
		Logging:     true,
		EnableCors:  true,
		Version:     dockerversion.VERSION,
		SocketGroup: monitorCfg.SocketGroup,
	}, monitor, watching)

	signal.Trap(srv.Shutdown)

	// notify monitor proxy when server is setup
	go func() {
		select {
		case <-srv.Start():
			log.Debugf("Monitor server is up, notify monitor proxy")
			syncPipe.CloseChild()
		}
	}()

	//srvAPIWait := make(chan error)

	// setup Monitor server
	protoAddrs := []string{fmt.Sprintf("unix://%s/%s.sock", daemon.MonitorSockDir, ID)}
	go func() {
		if err := srv.ServeApi(protoAddrs); err != nil {
			log.Errorf("ServeAPI error: %v", err)
			//srvAPIWait <- err
			srv.Notify(err)
		}
	}()
	srv.AcceptConnections()

	err = srv.Wait()

	log.Infof("docker monitor exit, with error: %v", err)
}

func setupMonitorServer(srv *apiserver.MonitorServer, protoAddrs []string) error {

	log.Debugf("setup monitor server %v", srv)

	// The serve API routine never exits unless an error occurs
	// We need to start it as a goroutine and wait on it so
	// daemon doesn't exit
	serveAPIWait := make(chan error)
	go func() {
		if err := srv.ServeApi(protoAddrs); err != nil {
			log.Errorf("ServeAPI error: %v", err)
			serveAPIWait <- err
		}
		serveAPIWait <- nil
	}()

	srv.AcceptConnections()
	err := <-serveAPIWait

	return err
}
