package server

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/api"
	"github.com/docker/docker/daemon"
	"github.com/docker/docker/dockerversion"
	"github.com/docker/docker/engine"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/jsonlog"
	"github.com/docker/docker/pkg/promise"
	"github.com/docker/docker/pkg/sockets"
	"github.com/docker/docker/pkg/version"
	"github.com/docker/docker/pkg/watch"
	watchjson "github.com/docker/docker/pkg/watch/json"
	"github.com/docker/docker/utils"
	"github.com/gorilla/mux"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type ServerConfig struct {
	Logging    bool
	EnableCors bool
	//CorsHeaders string
	Version     string
	SocketGroup string
	TLSConfig   *tls.Config
}

type serverCloser interface {
	Serve() error
	Close() error
}

type Server struct {
	cfg     *ServerConfig
	router  *mux.Router
	start   chan struct{}
	servers []serverCloser
}

type HttpServer struct {
	srv *http.Server
	l   net.Listener
}

func (s *HttpServer) Serve() error {
	return s.srv.Serve(s.l)
}
func (s *HttpServer) Close() error {
	return s.l.Close()
}

type HttpMonApiFunc func(version version.Version, w http.ResponseWriter, r *http.Request, vars map[string]string) error

/*
func New(cfg *ServerConfig) *Server {
	srv := &Server{
		cfg:   cfg,
		start: make(chan struct{}),
	}
	r := createMonitorRouter(srv)
	srv.router = r
	return srv
}
*/

func (s *Server) ping(version version.Version, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	_, err := w.Write([]byte{'O', 'K'})
	return err
}

func (s *Server) Close() {
	for _, srv := range s.servers {
		if err := srv.Close(); err != nil {
			log.Error(err)
		}
	}
}

// ServeApi loops through all of the protocols sent in to docker and spawns
// off a go routine to setup a serving http.Server for each.
func (s *Server) ServeApi(protoAddrs []string) error {
	var chErrors = make(chan error, len(protoAddrs))

	for _, protoAddr := range protoAddrs {
		protoAddrParts := strings.SplitN(protoAddr, "://", 2)
		if len(protoAddrParts) != 2 {
			return fmt.Errorf("bad format, expected PROTO://ADDR")
		}
		srv, err := s.newServer(protoAddrParts[0], protoAddrParts[1])
		if err != nil {
			return err
		}
		s.servers = append(s.servers, srv...)

		for _, s := range srv {
			log.Infof("Listening for HTTP on %s (%s)", protoAddrParts[0], protoAddrParts[1])
			go func(s serverCloser) {
				if err := s.Serve(); err != nil && strings.Contains(err.Error(), "use of closed network connection") {
					err = nil
				}
				chErrors <- err
			}(s)
		}
	}

	for i := 0; i < len(protoAddrs); i++ {
		err := <-chErrors
		if err != nil {
			return err
		}
	}

	return nil
}

// newServer sets up the required serverClosers and does protocol specific checking.
func (s *Server) newServer(proto, addr string) ([]serverCloser, error) {
	var (
		ls []net.Listener
	)
	switch proto {
	case "tcp":
		l, err := sockets.NewTcpSocket(addr, s.cfg.TLSConfig, s.start)
		if err != nil {
			return nil, err
		}
		ls = append(ls, l)
	case "unix":
		l, err := sockets.NewUnixSocket(addr, s.cfg.SocketGroup, s.start)
		if err != nil {
			return nil, err
		}
		ls = append(ls, l)
	default:
		return nil, fmt.Errorf("Invalid protocol format: %q", proto)
	}
	var res []serverCloser
	for _, l := range ls {
		res = append(res, &HttpServer{
			&http.Server{
				Addr:    addr,
				Handler: s.router,
			},
			l,
		})
	}
	return res, nil
}

type MonitorServer struct {
	monitor *daemon.DockerMonitor
	Server
	AttachSignal chan struct{}
	watching     watch.Interface

	// serverWait chan used to monitor server exit notify
	serverWait chan error
}

func NewMonitorServer(cfg *ServerConfig, m *daemon.DockerMonitor, w watch.Interface) *MonitorServer {
	srv := &MonitorServer{
		monitor: m,
		Server: Server{
			cfg:   cfg,
			start: make(chan struct{}),
		},
		AttachSignal: make(chan struct{}),
		watching:     w,
		serverWait:   make(chan error),
	}
	r := createMonitorRouter(srv)
	srv.router = r
	return srv
}

func createMonitorRouter(s *MonitorServer) *mux.Router {
	r := mux.NewRouter()
	m := map[string]map[string]HttpMonApiFunc{
		"GET": {
			"/containers/{name:.*}/_ping": s.ping,
			"/containers/{name:.*}/state": s.containerState,
			"/containers/{name:.*}/watch": s.containerWatch,
		},
		"POST": {
			"/containers/{name:.*}/start":  s.postContainersStart,
			"/containers/{name:.*}/resize": s.postContainersResize,
			"/containers/{name:.*}/attach": s.postContainersAttach,
			"/containers/{name:.*}/stop":   s.postContainersStop,
		},
	}

	enableCors := s.cfg.EnableCors

	for method, routes := range m {
		for route, fct := range routes {
			log.Infof("Registering %s, %s", method, route)
			// NOTE: scope issue, make sure the variables are local and won't be changed
			localRoute := route
			localFct := fct
			localMethod := method

			// build the handler function
			f := makeHttpMonHandler(true, localMethod, localRoute, localFct, enableCors, version.Version(dockerversion.VERSION))

			// add the new route
			if localRoute == "" {
				r.Methods(localMethod).HandlerFunc(f)
			} else {
				r.Path("/v{version:[0-9.]+}" + localRoute).Methods(localMethod).HandlerFunc(f)
				r.Path(localRoute).Methods(localMethod).HandlerFunc(f)
			}
		}
	}
	return r
}

func makeHttpMonHandler(logging bool, localMethod string, localRoute string, handlerFunc HttpMonApiFunc, enableCors bool, dockerVersion version.Version) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// log the request
		log.Debugf("Calling %s %s", localMethod, localRoute)

		if logging {
			log.Infof("%s %s", r.Method, r.RequestURI)
		}

		if strings.Contains(r.Header.Get("User-Agent"), "Docker-Client/") {
			userAgent := strings.Split(r.Header.Get("User-Agent"), "/")

			// v1.20 onwards includes the GOOS of the client after the version
			// such as Docker/1.7.0 (linux)
			if len(userAgent) == 2 && strings.Contains(userAgent[1], " ") {
				userAgent[1] = strings.Split(userAgent[1], " ")[0]
			}

			if len(userAgent) == 2 && !dockerVersion.Equal(version.Version(userAgent[1])) {
				log.Debugf("Warning: client and server don't have the same version (client: %s, server: %s)", userAgent[1], dockerVersion)
			}
		}
		version := version.Version(mux.Vars(r)["version"])
		if version == "" {
			version = api.APIVERSION
		}
		if enableCors {
			writeCorsHeaders(w, r)
		}

		if version.GreaterThan(api.APIVERSION) {
			http.Error(w, fmt.Errorf("client is newer than server (client API version: %s, server API version: %s)", version, api.APIVERSION).Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Server", "Docker/"+dockerversion.VERSION+" ("+runtime.GOOS+")")

		if err := handlerFunc(version, w, r, mux.Vars(r)); err != nil {
			log.Errorf("Handler for %s %s returned error: %s", localMethod, localRoute, err)
			httpError(w, err)
		}
	}
}

func (s *MonitorServer) AcceptConnections() {
	// close the lock so the listeners start accepting connections
	select {
	case <-s.start:
	default:
		close(s.start)
	}
}

func (s *MonitorServer) Start() <-chan struct{} {
	return s.start
}

func (s *MonitorServer) Wait() error {
	return <-s.serverWait
}

func (s *MonitorServer) Notify(err error) {
	s.serverWait <- err
}
func (s *MonitorServer) Shutdown() {
	s.Close()
	s.Notify(nil)
}

func (s *MonitorServer) containerState(version version.Version, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	ws := s.monitor.ContainerState()
	log.Debugf("MonitorServer WatchState: %v", *ws)

	out := &engine.Env{}
	out.SetJson("WatchState", ws)

	w.Header().Set("Content-Type", "application/json")

	if _, err := out.WriteTo(w); err != nil {
		return err
	}

	return nil
}

func (s *MonitorServer) containerWatch(version version.Version, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := parseForm(r); err != nil {
		return err
	}

	cn, ok := w.(http.CloseNotifier)
	if !ok {
		log.Errorf("unable to get CloseNotified")
		return fmt.Errorf("not found")
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Errorf("unable to get Flusher")
		return fmt.Errorf("not found")
	}

	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	encoder := watchjson.NewEncoder(w)

	// when docker daemon down, watching will be closed. Once docker daemon restart,
	// will poll container state again, so Start watching here anyway.
	// !!!NOTE: Not support multiple client connections, because only one watching channel.
	// TODO: support multiple client watch
	s.monitor.RestartWatching()

	for {
		select {
		case <-cn.CloseNotify():
			s.watching.Stop()
			return nil
		case event, ok := <-s.watching.ResultChan():
			if !ok {
				// End of result.
				log.Info("Monitor server watching is closed")
				return nil
			}
			log.Debugf("Monitor server got event %v", event)
			if err := encoder.Encode(&event); err != nil {
				log.Errorf("Monitor server write watch event error: %v, stop watching", err)
				// Client disconnect
				s.watching.Stop()
				return nil
			}
			flusher.Flush()
		}
	}
	return nil
}

func (s *MonitorServer) postContainersResize(version version.Version, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := parseForm(r); err != nil {
		return err
	}
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}

	height, err := strconv.Atoi(r.Form.Get("h"))
	if err != nil {
		return err
	}
	width, err := strconv.Atoi(r.Form.Get("w"))
	if err != nil {
		return err
	}

	return s.monitor.Container().Resize(height, width)
}

func (s *MonitorServer) postContainersStart(version version.Version, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := parseForm(r); err != nil {
		return err
	}

	return s.monitor.Start()
}

func (s *MonitorServer) postContainersStop(version version.Version, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := parseForm(r); err != nil {
		return err
	}

	err, rc := s.monitor.Stop()
	if err != nil {
		return err
	}

	out := &engine.Env{}
	out.SetInt("ExitCode", rc)

	w.Header().Set("Content-Type", "application/json")

	if _, err = out.WriteTo(w); err != nil {
		return err
	}

	return nil
}

func (s *MonitorServer) postContainersAttach(version version.Version, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := parseForm(r); err != nil {
		return err
	}
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}

	inStream, outStream, err := hijackServer(w)
	if err != nil {
		return err
	}
	defer func() {
		if tcpc, ok := inStream.(*net.TCPConn); ok {
			tcpc.CloseWrite()
		} else {
			inStream.Close()
		}
	}()
	defer func() {
		if tcpc, ok := outStream.(*net.TCPConn); ok {
			tcpc.CloseWrite()
		} else if closer, ok := outStream.(io.Closer); ok {
			closer.Close()
		}
	}()

	var errStream io.Writer

	fmt.Fprintf(outStream, "HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n")

	/* !tty & api>1.6
	errStream = stdcopy.NewStdWriter(outStream, stdcopy.Stderr)
	outStream = stdcopy.NewStdWriter(outStream, stdcopy.Stdout)
	*/
	errStream = outStream

	var (
		logs   = boolValue(r, "logs")
		stream = boolValue(r, "stream")
		stdin  = boolValue(r, "stdin")
		stdout = boolValue(r, "stdout")
		stderr = boolValue(r, "stderr")
	)

	container := s.monitor.Container()

	//logs
	if logs {
		cLog, err := container.ReadLog("json")
		if err != nil && os.IsNotExist(err) {
			// Legacy logs
			log.Debugf("Old logs format")
			if stdout {
				cLog, err := container.ReadLog("stdout")
				if err != nil {
					log.Errorf("Error reading logs (stdout): %s", err)
				} else if _, err := io.Copy(outStream, cLog); err != nil {
					log.Errorf("Error streaming logs (stdout): %s", err)
				}
			}
			if stderr {
				cLog, err := container.ReadLog("stderr")
				if err != nil {
					log.Errorf("Error reading logs (stderr): %s", err)
				} else if _, err := io.Copy(errStream, cLog); err != nil {
					log.Errorf("Error streaming logs (stderr): %s", err)
				}
			}
		} else if err != nil {
			log.Errorf("Error reading logs (json): %s", err)
		} else {
			dec := json.NewDecoder(cLog)
			for {
				l := &jsonlog.JSONLog{}

				if err := dec.Decode(l); err == io.EOF {
					break
				} else if err != nil {
					log.Errorf("Error streaming logs: %s", err)
					break
				}
				if l.Stream == "stdout" && stdout {
					io.WriteString(outStream, l.Log)
				}
				if l.Stream == "stderr" && stderr {
					io.WriteString(errStream, l.Log)
				}
			}
		}
	}

	//stream
	if stream {
		var (
			cStdin           io.ReadCloser
			cStdout, cStderr io.Writer
			cStdinCloser     io.Closer
		)

		if stdin {
			r, w := io.Pipe()
			go func() {
				defer w.Close()
				defer log.Debugf("Closing buffered stdin pipe")
				io.Copy(w, inStream)
			}()
			cStdin = r
			cStdinCloser = inStream
		}
		if stdout {
			cStdout = outStream
		}
		if stderr {
			cStderr = errStream
		}

		<-s.Attach(&container.StreamConfig, container.Config.OpenStdin, container.Config.StdinOnce, container.Config.Tty, cStdin, cStdinCloser, cStdout, cStderr)

		close(s.AttachSignal)
		// If we are in stdinonce mode, wait for the process to end
		// otherwise, simply return
		if container.Config.StdinOnce && !container.Config.Tty {
			container.WaitStop(-1 * time.Second)
		}
	}

	return nil
}

func (s *MonitorServer) Attach(streamConfig *daemon.StreamConfig, openStdin, stdinOnce, tty bool, stdin io.ReadCloser, stdinCloser io.Closer, stdout io.Writer, stderr io.Writer) chan error {
	var (
		cStdout, cStderr io.ReadCloser
		nJobs            int
		errors           = make(chan error, 3)
	)

	// Connect stdin of container to the http conn.
	if stdin != nil && openStdin {
		nJobs++
		// Get the stdin pipe.
		if cStdin, err := streamConfig.StdinPipe(); err != nil {
			errors <- err
		} else {
			go func() {
				log.Debugf("attach: stdin: begin")
				defer log.Debugf("attach: stdin: end")
				// No matter what, when stdin is closed (io.Copy unblock), close stdout and stderr
				if stdinOnce && !tty {
					defer cStdin.Close()
				} else {
					defer func() {
						if cStdout != nil {
							cStdout.Close()
						}
						if cStderr != nil {
							cStderr.Close()
						}
					}()
				}
				if tty {
					_, err = utils.CopyEscapable(cStdin, stdin)
				} else {
					_, err = io.Copy(cStdin, stdin)

				}
				if err == io.ErrClosedPipe {
					err = nil
				}
				if err != nil {
					log.Errorf("attach: stdin: %s", err)
				}
				errors <- err
			}()
		}
	}
	if stdout != nil {
		nJobs++
		// Get a reader end of a pipe that is attached as stdout to the container.
		if p, err := streamConfig.StdoutPipe(); err != nil {
			errors <- err
		} else {
			cStdout = p
			go func() {
				log.Debugf("attach: stdout: begin")
				defer log.Debugf("attach: stdout: end")
				// If we are in StdinOnce mode, then close stdin
				if stdinOnce && stdin != nil {
					defer stdin.Close()
				}
				if stdinCloser != nil {
					defer stdinCloser.Close()
				}
				_, err := io.Copy(stdout, cStdout)
				if err == io.ErrClosedPipe {
					err = nil
				}
				if err != nil {
					log.Errorf("attach: stdout: %s", err)
				}
				errors <- err
			}()
		}
	} else {
		// Point stdout of container to a no-op writer.
		go func() {
			if stdinCloser != nil {
				defer stdinCloser.Close()
			}
			if cStdout, err := streamConfig.StdoutPipe(); err != nil {
				log.Errorf("attach: stdout pipe: %s", err)
			} else {
				io.Copy(&ioutils.NopWriter{}, cStdout)
			}
		}()
	}
	if stderr != nil {
		nJobs++
		if p, err := streamConfig.StderrPipe(); err != nil {
			errors <- err
		} else {
			cStderr = p
			go func() {
				log.Debugf("attach: stderr: begin")
				defer log.Debugf("attach: stderr: end")
				// If we are in StdinOnce mode, then close stdin
				// Why are we closing stdin here and above while handling stdout?
				if stdinOnce && stdin != nil {
					defer stdin.Close()
				}
				if stdinCloser != nil {
					defer stdinCloser.Close()
				}
				_, err := io.Copy(stderr, cStderr)
				if err == io.ErrClosedPipe {
					err = nil
				}
				if err != nil {
					log.Errorf("attach: stderr: %s", err)
				}
				errors <- err
			}()
		}
	} else {
		// Point stderr at a no-op writer.
		go func() {
			if stdinCloser != nil {
				defer stdinCloser.Close()
			}

			if cStderr, err := streamConfig.StderrPipe(); err != nil {
				log.Errorf("attach: stdout pipe: %s", err)
			} else {
				io.Copy(&ioutils.NopWriter{}, cStderr)
			}
		}()
	}

	return promise.Go(func() error {
		defer func() {
			if cStdout != nil {
				cStdout.Close()
			}
			if cStderr != nil {
				cStderr.Close()
			}
		}()

		// FIXME: how to clean up the stdin goroutine without the unwanted side effect
		// of closing the passed stdin? Add an intermediary io.Pipe?
		for i := 0; i < nJobs; i++ {
			log.Debugf("attach: waiting for job %d/%d", i+1, nJobs)
			if err := <-errors; err != nil {
				log.Errorf("attach: job %d returned error %s, aborting all jobs", i+1, err)
				return err
			}
			log.Debugf("attach: job %d completed successfully", i+1)
		}
		log.Debugf("attach: all jobs completed successfully")
		return nil
	})
}
