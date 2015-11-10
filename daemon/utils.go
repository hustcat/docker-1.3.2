package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"

	"github.com/docker/docker/nat"
	"github.com/docker/docker/runconfig"
)

func migratePortMappings(config *runconfig.Config, hostConfig *runconfig.HostConfig) error {
	if config.PortSpecs != nil {
		ports, bindings, err := nat.ParsePortSpecs(config.PortSpecs)
		if err != nil {
			return err
		}
		config.PortSpecs = nil
		if len(bindings) > 0 {
			if hostConfig == nil {
				hostConfig = &runconfig.HostConfig{}
			}
			hostConfig.PortBindings = bindings
		}

		if config.ExposedPorts == nil {
			config.ExposedPorts = make(nat.PortSet, len(ports))
		}
		for k, v := range ports {
			config.ExposedPorts[k] = v
		}
	}
	return nil
}

func mergeLxcConfIntoOptions(hostConfig *runconfig.HostConfig) []string {
	if hostConfig == nil {
		return nil
	}

	out := []string{}

	// merge in the lxc conf options into the generic config map
	if lxcConf := hostConfig.LxcConf; lxcConf != nil {
		for _, pair := range lxcConf {
			// because lxc conf gets the driver name lxc.XXXX we need to trim it off
			// and let the lxc driver add it back later if needed
			parts := strings.SplitN(pair.Key, ".", 2)
			out = append(out, fmt.Sprintf("%s=%s", parts[1], pair.Value))
		}
	}

	return out
}

func CallURL(method, URL, path string, data interface{}) ([]byte, int, error) {
	params := bytes.NewBuffer(nil)
	if data != nil {
		buf, err := json.Marshal(data)
		if err != nil {
			return nil, -1, err
		}
		if _, err := params.Write(buf); err != nil {
			return nil, -1, err
		}
	}

	req, err := http.NewRequest(method, fmt.Sprintf("/v1%s", path), params)
	if err != nil {
		return nil, -1, err
	}

	//req.Header.Set("User-Agent", "Docker-Client/"+dockerversion.VERSION)
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	} else if method == "POST" {
		req.Header.Set("Content-Type", "plain/text")
	}

	protoAddrParts := strings.SplitN(URL, "://", 2)
	dial, err := net.Dial(protoAddrParts[0], protoAddrParts[1])
	if err != nil {
		return nil, -1, err
	}
	clientconn := httputil.NewClientConn(dial, nil)
	defer clientconn.Close()
	
	resp, err := clientconn.Do(req)
	if err != nil {
		return nil, -1, err
	}

	body, err := ioutil.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		if err != nil {
			return nil, -1, err
		}
		if len(body) == 0 {
			return nil, resp.StatusCode, fmt.Errorf("Error: request returned %s for API route and version %s, check if the server supports the requested API version", http.StatusText(resp.StatusCode), req.URL)
		}
		return nil, resp.StatusCode, fmt.Errorf("Error response from daemon: %s", bytes.TrimSpace(body))
	}

	return body, resp.StatusCode, nil
}
