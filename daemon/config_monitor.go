package daemon

var (
	MonitorSockDir  = "/var/run/docker"
	MonitorBuiltin  = "builtin"
	MonitorExternal = "external"
)

type MonitorConfig struct {
	Pidfile     string
	Root        string
	SocketGroup string
	ID          string
}
