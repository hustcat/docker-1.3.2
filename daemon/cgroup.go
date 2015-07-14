package daemon

import (
	"encoding/json"
	"github.com/docker/docker/engine"
	"github.com/docker/docker/pkg/log"
	"github.com/docker/docker/pkg/units"
	"github.com/docker/libcontainer/cgroups/fs"
	"strconv"
)

func updateConfig(c *Container, subsystem string, value string) error {
	if subsystem == "cpuset.cpus" {
		c.Config.Cpuset = value
	} else if subsystem == "memory.limit_in_bytes" {
		parsedMemory, err := units.RAMInBytes(value)
		if err != nil {
			log.Errorf("Update memory.limit_in_bytes for container %s error %v", c.ID, err)
			return err
		}
		c.Config.Memory = parsedMemory
	} else if subsystem == "cpu.shares" {
		parsedCpu, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			log.Errorf("Update cpu.shares for container %s error %v", c.ID, err)
			return err
		}
		c.Config.CpuShares = parsedCpu
	} else if subsystem == "memory.memsw.limit_in_bytes" {
		parsedMemsw, err := units.RAMInBytes(value)
		if err != nil {
			log.Errorf("Update memory.memsw.limit_in_bytes for container %s error %v", c.ID, err)
			return err
		}
		c.Config.MemorySwap = parsedMemsw
	} else {
		log.Infof("Ignore config update container %s, subsystem %s ", c.ID, subsystem)
	}
	return nil
}

func (daemon *Daemon) ContainerCgroup(job *engine.Job) engine.Status {
	if len(job.Args) != 1 {
		return job.Errorf("Usage: %s CONTAINER\n", job.Name)
	}

	var (
		name           = job.Args[0]
		saveToFile     = job.GetenvBool("saveToFile")
		readSubsystem  = job.GetenvList("readSubsystem")
		writeSubsystem []struct {
			Key   string
			Value string
		}
		err error
	)

	job.GetenvJson("writeSubsystem", &writeSubsystem)

	log.Infof("name %s, readSubsystem %s, writeSubsystem %s", name, readSubsystem, writeSubsystem)

	if container := daemon.Get(name); container != nil {
		if !container.State.IsRunning() {
			return job.Errorf("Container %s is not running", name)
		}

		var object []interface{}

		// read
		for _, subsystem := range readSubsystem {
			var cgroupResponse struct {
				Subsystem string
				Out       string
				Err       string
				Status    int
			}

			cgroupResponse.Subsystem = subsystem
			output, err := fs.Get(container.ID, daemon.ExecutionDriver().Parent(), subsystem)
			if err != nil {
				cgroupResponse.Err = err.Error()
				cgroupResponse.Status = 255
			} else {
				cgroupResponse.Out = output
				cgroupResponse.Status = 0
			}
			object = append(object, cgroupResponse)
		}

		// write
		for _, pair := range writeSubsystem {
			var cgroupResponse struct {
				Subsystem string
				Out       string
				Err       string
				Status    int
			}

			cgroupResponse.Subsystem = pair.Key
			oldValue, _ := fs.Get(container.ID, daemon.ExecutionDriver().Parent(), pair.Key)

			err = fs.Set(container.ID, daemon.ExecutionDriver().Parent(), pair.Key, pair.Value)
			if err != nil {
				cgroupResponse.Err = err.Error()
				cgroupResponse.Status = 255
			} else {
				newValue, _ := fs.Get(container.ID, daemon.ExecutionDriver().Parent(), pair.Key)
				log.Infof("cgroup: %s old value: %s, new value: %s", pair.Key, oldValue, newValue)

				/* memory.limit_in_bytes 5g != 5368709120
				if newValue != pair.Value {
					return job.Errorf("cgroup %s change value failed, newValue %s is not same as expect value %s", pair.Key, newValue, pair.Value)
				}*/

				if err = updateConfig(container, pair.Key, pair.Value); err != nil {
					cgroupResponse.Out = err.Error()
					cgroupResponse.Status = 1
				} else {
					cgroupResponse.Out = newValue
					cgroupResponse.Status = 0
				}
			}
			object = append(object, cgroupResponse)
		}

		if saveToFile && err == nil {
			if err := container.ToDisk(); err != nil {
				return job.Error(err)
			}
		}

		b, err := json.Marshal(object)
		if err != nil {
			return job.Error(err)
		}

		job.Stdout.Write(b)
		return engine.StatusOK
	}
	return job.Errorf("No such container: %s", name)
}
