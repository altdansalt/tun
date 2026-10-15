package api

// Port is one exposed port of a service job.
//
// The client names ports (Port, e.g. 8080); the server allocates a free host port for each
// and passes it to the job as $TUN_PORT_<Port> (and as $PORT when there is exactly one).
// The service must listen on that host port on 127.0.0.1 (or all interfaces). The server
// reverse-proxies Path (/v1/jobs/{id}/ports/{Port}/...) to it, behind the usual bearer auth.
type Port struct {
	Port     int    `json:"port"`          // the port number the client asked for (a name)
	HostPort int    `json:"host_port"`     // where the service actually listens on the server host
	Ready    bool   `json:"ready"`         // the server has connected to HostPort at least once
	Path     string `json:"path"`          // proxy path on the tun server, ending in "/"
	URL      string `json:"url,omitempty"` // full proxy URL; filled in by the CLI from $TUN_URL
}

// MaxPorts is the most ports one service may expose.
const MaxPorts = 16
