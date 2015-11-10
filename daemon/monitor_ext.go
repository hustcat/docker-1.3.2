package daemon

import (
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/daemon/execdriver"
	"github.com/docker/docker/daemon/execdriver/execdrivers"
	"github.com/docker/docker/dockerversion"
	"github.com/docker/docker/pkg/broadcastwriter"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/promise"
	"github.com/docker/docker/pkg/sysinfo"
	"github.com/docker/docker/utils"
	"io"
	"io/ioutil"
	"os"
	"path"
	"syscall"
	"time"
)

var (
	// docker root directory
	dockerRoot = "/var/lib/docker"
)

type DockerMonitor struct {
	m *externalMonitor
}

func getContainerRoot(dockerRoot string, id string) string {
	// ref daemon.containerRoot
	daemonRepo := path.Join(dockerRoot, "containers")
	return path.Join(daemonRepo, id)
}

func InitDockerMonitor(cfg *MonitorConfig, w *StateWatcher) *DockerMonitor {
	containerId := cfg.ID
	dockerRoot = cfg.Root

	containerRoot := getContainerRoot(dockerRoot, containerId)

	container := &Container{
		root:  containerRoot,
		State: NewState(),
	}

	if err := container.FromDisk(); err != nil {
		log.Errorf("InitDockerMonitor: container from disk failed: %v", err)
		os.Exit(1)
	}

	if container.ID != containerId {
		log.Errorf("InitDockerMonitor: Container %s is stored at %s", container.ID, containerId)
		os.Exit(1)
	}

	if err := container.ReadCommandConfig(); err != nil {
		log.Errorf("InitDockerMonitor: command from disk failed: %v", err)
		os.Exit(1)
	}

	// Attach to stdout and stderr
	container.stderr = broadcastwriter.New()
	container.stdout = broadcastwriter.New()

	// Attach to stdin
	if container.Config.OpenStdin {
		container.stdin, container.stdinPipe = io.Pipe()
	} else {
		container.stdinPipe = ioutils.NopWriteCloser(ioutil.Discard)
	}
	monitor, err := newExternalMonitor(container, w)
	if err != nil {
		log.Errorf("external monitor initial error: %v", err)
		return nil
	}

	//container.exMonitor = monitor
	return &DockerMonitor{
		monitor,
	}
}

func (dm *DockerMonitor) Start() error {
	// block until we either receive an error from the initial start of the container's
	// process or until the process is running in the container
	select {
	case <-dm.m.startSignal:
	case err := <-promise.Go(dm.m.Start):
		return err
	}
	return nil
}

func (dm *DockerMonitor) WaitStop() error {
	<-dm.m.stopSignal
	return nil
}

func (dm *DockerMonitor) RestartWatching() {
	dm.m.watching.Start()
}

func (dm *DockerMonitor) Stop() (error, int) {
	container := dm.m.container

	seconds := 10
	//container.Lock()
	//defer container.Unlock()

	// We could unpause the container for them rather than returning this error
	if container.Paused {
		return fmt.Errorf("Container %s is paused. Unpause the container before stopping", container.ID), -1
	}

	if !container.Running {
		return nil, 0
	}

	log.Debugf("Stop container %s", container.ID)

	// 1. Send a SIGTERM
	if err := dm.KillSig(15); err != nil {
		log.Infof("Failed to send SIGTERM to the process, force killing")
		if err := dm.KillSig(9); err != nil {
			return err, -1
		}
	}

	// 2. Wait for the process to exit on its own
	if _, err := container.WaitStop(time.Duration(seconds) * time.Second); err != nil {
		log.Infof("Container %v failed to exit within %d seconds of SIGTERM - using the force", container.ID, seconds)
		// If it doesn't, then send SIGKILL
		if err := dm.KillSig(9); err != nil {
			return err, -1
		}

		// Wait for the process to die, in last resort, try to kill the process directly
		if _, err := container.WaitStop(10 * time.Second); err != nil {
			// Ensure that we don't kill ourselves
			if pid := container.GetPid(); pid != 0 {
				log.Infof("Container %s failed to exit within 10 seconds of kill - trying direct SIGKILL", utils.TruncateID(container.ID))
				if err := syscall.Kill(pid, 9); err != nil {
					return err, -1
				}

				// Wait for ever
				container.WaitStop(-1 * time.Second)
			}
		}
	}

	log.Infof("Container %s exit with %v", container.ID, container.State.ExitCode)
	return nil, container.State.ExitCode
}

func (dm *DockerMonitor) KillSig(sig int) error {
	container := dm.m.container
	if !container.Running {
		return nil
	}

	if err := dm.m.ed.Kill(container.command, sig); err != nil {
		return err
	}

	return nil
}

func (dm *DockerMonitor) Container() *Container {
	return dm.m.container
}

func (dm *DockerMonitor) ContainerState() *WatchState {
	return dm.m.container.ToWatchState()
}

type externalMonitor struct {
	ed execdriver.Driver

	// container is the container being monitored
	container *Container

	watching *StateWatcher

	startSignal chan struct{}

	stopSignal chan struct{}
}

func newExternalMonitor(container *Container, w *StateWatcher) (*externalMonitor, error) {
	ed, err := newExecDriver()
	if err != nil {
		return nil, err
	}

	return &externalMonitor{
		ed:          ed,
		container:   container,
		watching:    w,
		startSignal: make(chan struct{}),
		stopSignal:  make(chan struct{}),
	}, nil
}

func (m *externalMonitor) Start() error {
	var (
		err        error
		exitStatus int
		// this variable indicates where we in execution flow:
		// before Run or after
		afterRun bool
	)

	// ensure that when the monitor finally exits we release the networking and unmount the rootfs
	defer func() {
		if afterRun {
			// reset container
			m.resetContainer()

			m.container.setStopped(exitStatus)
			ws := m.container.State.ToWatchState()

			// if docker daemon is deattach, watching is stopped, Emit will handle it.
			m.watching.Emit(ws)

			// close watching
			m.watching.Stop()
			log.Debugf("external monitor container %s exited", m.container.ID)

			// notify monitor server exit
			close(m.stopSignal)
		}
	}()

	pipes := execdriver.NewPipes(m.container.stdin, m.container.stdout, m.container.stderr, m.container.Config.OpenStdin)

	if exitStatus, err = m.startContainer(m.container, pipes, m.callback); err != nil {
		return err

	}

	afterRun = true

	// TODO: handle conainter restartPolicy

	return nil
}

func (m *externalMonitor) Close() error {
	// move to MonitorProxy.Stop()

	return nil
}

// callback ensures that the container's state is properly updated after we
// received ack from the execution drivers
func (m *externalMonitor) callback(processConfig *execdriver.ProcessConfig, pid int) {
	if processConfig.Tty {
		// The callback is called after the process Start()
		// so we are in the parent process. In TTY mode, stdin/out/err is the PtySlave
		// which we close here.
		if c, ok := processConfig.Stdout.(io.Closer); ok {
			c.Close()
		}
	}

	m.container.setRunning(pid)
	ws := m.container.State.ToWatchState()
	m.watching.Emit(ws)

	select {
	case <-m.startSignal:
	default:
		close(m.startSignal)
	}
}

func (m *externalMonitor) startContainer(c *Container, pipes *execdriver.Pipes, startCallback execdriver.StartCallback) (int, error) {
	return m.ed.Run(c.command, pipes, startCallback)

}

func (m *externalMonitor) resetContainer() {
	container := m.container
	if container.Config.OpenStdin {
		if err := container.stdin.Close(); err != nil {
			log.Errorf("%s: Error close stdin: %s", container.ID, err)
		}
	}

	if err := container.stdout.Clean(); err != nil {
		log.Errorf("%s: Error close stdout: %s", container.ID, err)
	}

	if err := container.stderr.Clean(); err != nil {
		log.Errorf("%s: Error close stderr: %s", container.ID, err)
	}

	if container.command != nil && container.command.ProcessConfig.Terminal != nil {
		if err := container.command.ProcessConfig.Terminal.Close(); err != nil {
			log.Errorf("%s: Error closing terminal: %s", container.ID, err)
		}
	}
}

func newExecDriver() (execdriver.Driver, error) {
	sysInitPath := path.Join(dockerRoot, "init", fmt.Sprintf("dockerinit-%s", dockerversion.VERSION))
	sysInfo := sysinfo.New(false)

	// only native driver
	//ed, err := execdrivers.NewDriver(c.ExecDriver, dockerRoot, sysInitPath, sysInfo)
	ed, err := execdrivers.NewDriver("native", dockerRoot, sysInitPath, sysInfo)
	if err != nil {
		return nil, err
	}
	return ed, nil
}
