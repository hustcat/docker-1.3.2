package daemon

import (
	"encoding/json"
	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/engine"
	"strconv"
)

func setConfig(c *Container, key string, value string) error {
	if key == "image" {
		c.Config.Image = value
		img, err := c.daemon.Repositories().LookupImage(c.Config.Image)
		if err != nil {
			log.Errorf("Can not find image by name: %s error %v", c.Config.Image, err)
			return err
		}
		c.Image = img.ID
	} else if key == "cpuShares" {
		parseCpuShares, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			log.Errorf("Update cpuShares for container %s error %v", c.ID, err)
			return err
		}
		c.Config.CpuShares = parseCpuShares
	} else if key == "domainname" {
		c.Config.Domainname = value
	} else if key == "hostname" {
		c.Config.Hostname = value
	} else {
		log.Infof("Ignore config update container %s, key %s ", c.ID, key)
	}
	return nil
}

func (daemon *Daemon) ContainerSet(job *engine.Job) engine.Status {
	if len(job.Args) != 1 {
		return job.Errorf("Usage: %s CONTAINER\n", job.Name)
	}

	var (
		name   = job.Args[0]
		config []struct {
			Key   string
			Value string
		}
		err error
	)
	job.GetenvJson("config", &config)
	log.Infof("Setting container: %s. config: %v", name, config)

	container := daemon.Get(name)
	if container == nil {
		return job.Errorf("Can not find container %s", name)
	}
	if !container.State.IsRunning() {
		return job.Errorf("Container %s is not running", name)
	}

	var object []interface{}
	for _, pair := range config {
		var response struct {
			Key    string
			Err    string
			Status int
		}
		response.Key = pair.Key
		if err = setConfig(container, pair.Key, pair.Value); err != nil {
			response.Err = err.Error()
			response.Status = 255
		} else {
			response.Status = 0
		}
		object = append(object, response)
	}

	// save config to disk
	if err := container.ToDisk(); err != nil {
		return job.Error(err)
	}

	b, err := json.Marshal(object)
	if err != nil {
		return job.Error(err)
	}
	job.Stdout.Write(b)

	return engine.StatusOK
}
