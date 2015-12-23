package daemon

import (
	"errors"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/engine"
	"github.com/docker/docker/pkg/watch"
	watchjson "github.com/docker/docker/pkg/watch/json"
	"github.com/docker/docker/utils"
	"github.com/docker/libcontainer/syncpipe"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"
)

type MonitorState struct {
	sync.Mutex
	Running    bool
	Pid        int
	ExitCode   int
	StartedAt  time.Time
	FinishedAt time.Time
}

func NewMonitorState() *MonitorState {
	return &MonitorState{}
}

func (ms *MonitorState) SetStopped(exitCode int) {
	ms.Lock()
	defer ms.Unlock()
	ms.Running = false
	ms.Pid = 0
	ms.ExitCode = exitCode
	ms.FinishedAt = time.Now().UTC()
}

func (ms *MonitorState) SetRunning(pid int) {
	ms.Lock()
	defer ms.Unlock()

	ms.Running = true
	ms.Pid = pid
	ms.StartedAt = time.Now().UTC()
}

func (ms *MonitorState) IsRunning() bool {
	ms.Lock()
	r := ms.Running
	ms.Unlock()
	return r
}

type monitorCommand struct {
	cmd *exec.Cmd
}

// MonitorProxy monitor proxy
type MonitorProxy struct {
	monitorCommand

	// container is the container being monitored
	container *Container
	// poller poll container state
	poller *StatePoller

	// startSignal is a channel that is closes after the monitor process starts
	startSignal chan struct{}

	// hasCmd whether has exec.Cmd
	hasCmd bool

	// monitorPath monitor binary path
	monitorPath string
}

// NewMonitorProxy return proxy for extern monitor
func NewMonitorProxy(c *Container, createCmd bool) *MonitorProxy {
	if createCmd {
		root := c.daemon.config.Root
		args := []string{
			"monitor",
			"-id", c.ID,
			"-root", root,
		}

		monitorPath := path.Join(root, "monitor", fmt.Sprintf("dockermonitor-%s", c.ID))
		if _, err := utils.CopyFile(c.daemon.sysInitPath, monitorPath); err != nil {
			log.Errorf("Copy monitor file error: %v", err)
			return nil
		}
		if err := os.Chmod(monitorPath, 700); err != nil {
			log.Errorf("Chmod monitor file error: %v", err)
			return nil
		}

		return &MonitorProxy{
			monitorCommand: monitorCommand{
				cmd: &exec.Cmd{
					Path:   monitorPath,
					Args:   args,
					Stdout: os.Stdout,
					Stderr: os.Stderr,
					Env:    os.Environ(),
				},
			},
			container:   c,
			startSignal: make(chan struct{}),
			hasCmd:      true,
			monitorPath: monitorPath,
		}
	} else {
		return &MonitorProxy{
			container: c,
			hasCmd:    false,
		}
	}
}

func (m *MonitorProxy) RunStatePoller() error {
	m.poller = newStatePoller(m, time.Second)
	if err := m.poller.Run(); err != nil {
		return err
	}
	return nil
}

func (m *MonitorProxy) StopStatePoller() {
	// Stop poll container state
	m.poller.Stop()
}

func (m *MonitorProxy) Start() error {
	var (
		exitStatus int
		afterRun   bool
	)

	if !m.hasCmd {
		return fmt.Errorf("MonitorProxy haven't create cmd")
	}

	defer func() {
		if afterRun {
			// StatePoller get container state from watch API
			//m.container.SetStopped(exitStatus)

			// clean container resources
			m.Stop()

			m.container.monitorState.SetStopped(exitStatus)
			log.Debugf("Monitor process exit with code %v", exitStatus)
			if err := m.container.WriteMonitorState(); err != nil {
				log.Errorf("write monitor state error: %v", err)
			}
		}
	}()

	syncPipe, err := syncpipe.NewSyncPipe()
	if err != nil {
		return err
	}

	m.cmd.ExtraFiles = []*os.File{syncPipe.Child()}
	defer syncPipe.Close()

	log.Debugf("[MonitorProxy] run external monitor: %s : %s", m.cmd.Path, m.cmd.Args)
	if err := m.cmd.Start(); err != nil {
		return err
	}

	syncPipe.CloseChild()
	log.Debugf("[MonitorProxy] wait monitor start")
	// Sync with child, wait monitor started
	if err := syncPipe.ReadFromChild(); err != nil {
		m.cmd.Process.Kill()
		m.cmd.Wait()
		return err
	}
	log.Debugf("MonitorProxy monitor has started")

	// signal that the monitor process has started
	select {
	case <-m.startSignal:
	default:
		close(m.startSignal)
	}

	afterRun = true

	log.Debugf("[MonitorProxy] poll container status")
	if err := m.RunStatePoller(); err != nil {
		log.Errorf("run StatePoll failed: %v", err)
		m.cmd.Process.Kill()
		m.cmd.Wait()
		exitStatus = -10
		return err
	}

	log.Debugf("[MonitorProxy] wait monitor process exit")
	// wait monitor process
	if err := m.cmd.Wait(); err != nil {
		log.Errorf("[MonitorProxy] wait error: %v", err)
		exitStatus = -11
		return err
	}

	exitStatus = m.monitorCommand.cmd.ProcessState.Sys().(syscall.WaitStatus).ExitStatus()

	log.Infof("[MonitorProxy] minitor process exited")
	return nil
}

// Stop closes the container's resources such as networking allocations and
// unmounts the contatiner's root filesystem
func (m *MonitorProxy) Stop() error {
	// Cleanup networking and mounts
	m.container.cleanup()

	if err := m.container.ToDisk(); err != nil {
		log.Errorf("[MonitorProxy] Error dumping container %s state to disk: %s", m.container.ID, err)

		return err
	}

	return nil
}

func (m *MonitorProxy) changeContainerState(s watch.Object) {
	if ws, ok := s.(*WatchState); ok {
		log.Debugf("[MonitorProxy] watch container state changed: %v", *ws)
		m.container.setByWatchState(ws)

		if err := m.container.ToDisk(); err != nil {
			log.Errorf("[MonitorProxy] Write container error: %v", err)
		}
	} else {
		log.Errorf("no WatchState item")
	}
}

type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
	Close() error
}

type StatePoller struct {
	sync.Mutex
	client HTTPClient
	m      *MonitorProxy
	scheme *watch.Scheme

	// period controls timing between one watch ending and
	// the beginning of the next one.
	period time.Duration

	stopChan chan interface{}
	stopped  bool
}

func newStatePoller(m *MonitorProxy, period time.Duration) *StatePoller {
	s := watch.NewScheme()
	expect := &WatchState{}
	s.AddKnownTypes(expect)

	return &StatePoller{
		m:        m,
		scheme:   s,
		period:   period,
		stopChan: make(chan interface{}),
	}
}

// Run begins polling. It starts a goroutine and returns immediately.
func (p *StatePoller) Run() error {
	addr := p.watchAddr()
	protoAddrParts := strings.SplitN(addr, "://", 2)

	dial, err := net.Dial(protoAddrParts[0], protoAddrParts[1])
	if tcpConn, ok := dial.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			return fmt.Errorf("Cannot connect to addr %s", addr)
		}
		return err
	}

	client := httputil.NewClientConn(dial, nil)
	if client == nil {
		return errors.New("Can't create watch conn")
	}
	p.client = client
	p.stopped = false

	// begin to poll
	go func() {
		defer func() {
			p.client.Close()
			p.stopped = true
		}()
		for {
			select {
			case <-p.stopChan:
				return
			default:
				w, err := p.Watch()
				if err != nil {
					log.Errorf("failed to watch: %v", err)
					return
				}
				if err := p.watchHandler(w); err != nil {
					log.Errorf("watch ended with error:%v", err)
					return
				}
				time.Sleep(p.period)
			}
		}
	}()

	return nil
}

func (p *StatePoller) Stop() {
	p.Lock()
	defer p.Unlock()

	if !p.stopped {
		close(p.stopChan)
		p.stopped = true
	}
}

func (p *StatePoller) watchHandler(w watch.Interface) error {
	start := time.Now()
	eventCount := 0
	for {
		event, ok := <-w.ResultChan()
		if !ok {
			break
		}

		log.Debugf("[MonitorProxy] get event: %v", event)

		eventType := string(event.Type)
		switch eventType {
		case WatchStateEvent:
			p.m.changeContainerState(event.Object)
		default:
			log.Errorf("unable to handle watch event %#v", event)
		}
		eventCount++
	}
	watchDuration := time.Now().Sub(start)
	if watchDuration < 1*time.Second && eventCount == 0 {
		log.Errorf("unexpected watch close - watch lasted less than a second and no items received")
		return errors.New("very short watch")
	}
	log.Infof("watch close - %v total items received", eventCount)
	return nil
}

func (p *StatePoller) watchPath() string {
	return fmt.Sprintf("/v1/containers/%s/watch", p.m.container.ID)
}

func (p *StatePoller) watchAddr() string {
	return fmt.Sprintf("unix://%s/%s.sock", MonitorSockDir, p.m.container.ID)
}

// Watch should begin a watch at the specified version.
func (p *StatePoller) Watch() (watch.Interface, error) {
	client := p.client
	req, err := http.NewRequest("GET", p.watchPath(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var body []byte
		if resp.Body != nil {
			body, _ = ioutil.ReadAll(resp.Body)
		}
		return nil, fmt.Errorf("for request '%v', got status: %v\nbody: %v", req.URL, resp.StatusCode, string(body))
	}
	return watch.NewStreamWatcher(watchjson.NewDecoder(resp.Body, p.scheme)), nil
}

// ContainerMonitorOp only called at attach mode by docker client to stop monitor server
func (daemon *Daemon) ContainerMonitorOp(job *engine.Job) engine.Status {
	if len(job.Args) != 1 {
		return job.Errorf("Usage: %s CONTAINER\n", job.Name)
	}

	var (
		name = job.Args[0]
		op   = job.Getenv("op")

		err error
	)

	log.Infof("Container %s, monitor operation %s", name, op)

	if container := daemon.Get(name); container != nil {
		if container.State.IsRunning() {
			return job.Errorf("Container %s is running, stop container before stop monitor", name)
		}

		if op == "stop" {
			r := container.monitorState.IsRunning()
			if !r {
				// monitor may be stopped by 'docker stop' API
				log.Infof("Container %s 's monitor is not running", name)
				return engine.StatusOK
			}

			// stop poll container state before kill monitor server
			container.exMonitor.StopStatePoller()
			// docker daemon has restarted, we should clean container here
			if !container.exMonitor.hasCmd {
				container.exMonitor.Stop()
			}

			log.Debugf("Kill monitor server with pid %v", container.monitorState.Pid)
			// kill monitor server
			if err := syscall.Kill(container.monitorState.Pid, syscall.SIGTERM); err != nil {
				log.Errorf("kill monitor server with pid %v error: %v", container.monitorState.Pid, err)
				return job.Error(err)
			}

			// write monitor state
			container.monitorState.SetStopped(0)
			if err = container.WriteMonitorState(); err != nil {
				log.Errorf("write monitor state error: %v", err)
				return job.Error(err)
			}
		} else {
			return job.Errorf("Monitor op: %s is not supported", op)
		}

		return engine.StatusOK
	}
	return job.Errorf("No such container: %s", name)
}
