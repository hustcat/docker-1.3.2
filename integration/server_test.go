package docker

import (
	"bytes"
	"github.com/docker/docker/engine"
	flag "github.com/docker/docker/pkg/mflag"
	"github.com/docker/docker/pkg/sysinfo"
	"github.com/docker/docker/runconfig"
	"io/ioutil"
	"testing"
	"time"
)

func parseRun2(args []string, sysInfo *sysinfo.SysInfo) (*runconfig.Config, *runconfig.HostConfig, *flag.FlagSet, error) {
	cmd := flag.NewFlagSet("run", flag.ContinueOnError)
	cmd.SetOutput(ioutil.Discard)
	cmd.Usage = nil
	return runconfig.Parse(cmd, args, sysInfo)
}

func TestCreateNumberHostname(t *testing.T) {
	eng := NewTestEngine(t)
	defer mkDaemonFromEngine(eng, t).Nuke()

	config, _, _, err := parseRun([]string{"-h", "web.0", unitTestImageID, "echo test"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	createTestContainer(eng, config, t)
}

func TestCommit(t *testing.T) {
	eng := NewTestEngine(t)
	defer mkDaemonFromEngine(eng, t).Nuke()

	config, _, _, err := parseRun([]string{unitTestImageID, "/bin/cat"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	id := createTestContainer(eng, config, t)

	job := eng.Job("commit", id)
	job.Setenv("repo", "testrepo")
	job.Setenv("tag", "testtag")
	job.SetenvJson("config", config)
	if err := job.Run(); err != nil {
		t.Fatal(err)
	}
}

func TestMergeConfigOnCommit(t *testing.T) {
	eng := NewTestEngine(t)
	runtime := mkDaemonFromEngine(eng, t)
	defer runtime.Nuke()

	container1, _, _ := mkContainer(runtime, []string{"-e", "FOO=bar", unitTestImageID, "echo test > /tmp/foo"}, t)
	defer runtime.Destroy(container1)

	config, _, _, err := parseRun([]string{container1.ID, "cat /tmp/foo"}, nil)
	if err != nil {
		t.Error(err)
	}

	job := eng.Job("commit", container1.ID)
	job.Setenv("repo", "testrepo")
	job.Setenv("tag", "testtag")
	job.SetenvJson("config", config)
	var outputBuffer = bytes.NewBuffer(nil)
	job.Stdout.Add(outputBuffer)
	if err := job.Run(); err != nil {
		t.Error(err)
	}

	container2, _, _ := mkContainer(runtime, []string{engine.Tail(outputBuffer, 1)}, t)
	defer runtime.Destroy(container2)

	job = eng.Job("container_inspect", container1.Name)
	baseContainer, _ := job.Stdout.AddEnv()
	if err := job.Run(); err != nil {
		t.Error(err)
	}

	job = eng.Job("container_inspect", container2.Name)
	commitContainer, _ := job.Stdout.AddEnv()
	if err := job.Run(); err != nil {
		t.Error(err)
	}

	baseConfig := baseContainer.GetSubEnv("Config")
	commitConfig := commitContainer.GetSubEnv("Config")

	if commitConfig.Get("Env") != baseConfig.Get("Env") {
		t.Fatalf("Env config in committed container should be %v, was %v",
			baseConfig.Get("Env"), commitConfig.Get("Env"))
	}

	if baseConfig.Get("Cmd") != "[\"echo test \\u003e /tmp/foo\"]" {
		t.Fatalf("Cmd in base container should be [\"echo test \\u003e /tmp/foo\"], was %s",
			baseConfig.Get("Cmd"))
	}

	if commitConfig.Get("Cmd") != "[\"cat /tmp/foo\"]" {
		t.Fatalf("Cmd in committed container should be [\"cat /tmp/foo\"], was %s",
			commitConfig.Get("Cmd"))
	}
}

func TestRestartKillWait(t *testing.T) {
	eng := NewTestEngine(t)
	runtime := mkDaemonFromEngine(eng, t)
	defer runtime.Nuke()

	config, hostConfig, _, err := parseRun([]string{"-i", unitTestImageID, "/bin/cat"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	id := createTestContainer(eng, config, t)

	job := eng.Job("containers")
	job.SetenvBool("all", true)
	outs, err := job.Stdout.AddListTable()
	if err != nil {
		t.Fatal(err)
	}
	if err := job.Run(); err != nil {
		t.Fatal(err)
	}

	if len(outs.Data) != 1 {
		t.Errorf("Expected 1 container, %v found", len(outs.Data))
	}

	job = eng.Job("start", id)
	if err := job.ImportEnv(hostConfig); err != nil {
		t.Fatal(err)
	}
	if err := job.Run(); err != nil {
		t.Fatal(err)
	}
	job = eng.Job("kill", id)
	if err := job.Run(); err != nil {
		t.Fatal(err)
	}

	eng = newTestEngine(t, false, runtime.Config().Root)

	job = eng.Job("containers")
	job.SetenvBool("all", true)
	outs, err = job.Stdout.AddListTable()
	if err != nil {
		t.Fatal(err)
	}
	if err := job.Run(); err != nil {
		t.Fatal(err)
	}

	if len(outs.Data) != 1 {
		t.Errorf("Expected 1 container, %v found", len(outs.Data))
	}

	setTimeout(t, "Waiting on stopped container timedout", 5*time.Second, func() {
		job = eng.Job("wait", outs.Data[0].Get("Id"))
		if err := job.Run(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCreateStartRestartStopStartKillRm(t *testing.T) {
	eng := NewTestEngine(t)
	defer mkDaemonFromEngine(eng, t).Nuke()

	config, hostConfig, _, err := parseRun([]string{"-i", unitTestImageID, "/bin/cat"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	id := createTestContainer(eng, config, t)

	job := eng.Job("containers")
	job.SetenvBool("all", true)
	outs, err := job.Stdout.AddListTable()
	if err != nil {
		t.Fatal(err)
	}
	if err := job.Run(); err != nil {
		t.Fatal(err)
	}

	if len(outs.Data) != 1 {
		t.Errorf("Expected 1 container, %v found", len(outs.Data))
	}

	job = eng.Job("start", id)
	if err := job.ImportEnv(hostConfig); err != nil {
		t.Fatal(err)
	}
	if err := job.Run(); err != nil {
		t.Fatal(err)
	}

	job = eng.Job("restart", id)
	job.SetenvInt("t", 2)
	if err := job.Run(); err != nil {
		t.Fatal(err)
	}

	job = eng.Job("stop", id)
	job.SetenvInt("t", 2)
	if err := job.Run(); err != nil {
		t.Fatal(err)
	}

	job = eng.Job("start", id)
	if err := job.ImportEnv(hostConfig); err != nil {
		t.Fatal(err)
	}
	if err := job.Run(); err != nil {
		t.Fatal(err)
	}

	if err := eng.Job("kill", id).Run(); err != nil {
		t.Fatal(err)
	}

	// FIXME: this failed once with a race condition ("Unable to remove filesystem for xxx: directory not empty")
	job = eng.Job("rm", id)
	job.SetenvBool("removeVolume", true)
	if err := job.Run(); err != nil {
		t.Fatal(err)
	}

	job = eng.Job("containers")
	job.SetenvBool("all", true)
	outs, err = job.Stdout.AddListTable()
	if err != nil {
		t.Fatal(err)
	}
	if err := job.Run(); err != nil {
		t.Fatal(err)
	}

	if len(outs.Data) != 0 {
		t.Errorf("Expected 0 container, %v found", len(outs.Data))
	}
}

func TestRunWithTooLowMemoryLimit(t *testing.T) {
	eng := NewTestEngine(t)
	defer mkDaemonFromEngine(eng, t).Nuke()

	// Try to create a container with a memory limit of 1 byte less than the minimum allowed limit.
	job := eng.Job("create")
	job.Setenv("Image", unitTestImageID)
	job.Setenv("Memory", "524287")
	job.Setenv("CpuShares", "1000")
	job.SetenvList("Cmd", []string{"/bin/cat"})
	if err := job.Run(); err == nil {
		t.Errorf("Memory limit is smaller than the allowed limit. Container creation should've failed!")
	}
}

func TestImagesFilter(t *testing.T) {
	eng := NewTestEngine(t)
	defer nuke(mkDaemonFromEngine(eng, t))

	if err := eng.Job("tag", unitTestImageName, "utest", "tag1").Run(); err != nil {
		t.Fatal(err)
	}

	if err := eng.Job("tag", unitTestImageName, "utest/docker", "tag2").Run(); err != nil {
		t.Fatal(err)
	}

	if err := eng.Job("tag", unitTestImageName, "utest:5000/docker", "tag3").Run(); err != nil {
		t.Fatal(err)
	}

	images := getImages(eng, t, false, "utest*/*")

	if len(images.Data[0].GetList("RepoTags")) != 2 {
		t.Fatal("incorrect number of matches returned")
	}

	images = getImages(eng, t, false, "utest")

	if len(images.Data[0].GetList("RepoTags")) != 1 {
		t.Fatal("incorrect number of matches returned")
	}

	images = getImages(eng, t, false, "utest*")

	if len(images.Data[0].GetList("RepoTags")) != 1 {
		t.Fatal("incorrect number of matches returned")
	}

	images = getImages(eng, t, false, "*5000*/*")

	if len(images.Data[0].GetList("RepoTags")) != 1 {
		t.Fatal("incorrect number of matches returned")
	}
}

func TestReadCgroup(t *testing.T) {
	eng := NewTestEngine(t)
	defer mkDaemonFromEngine(eng, t).Nuke()

	//config, hostConfig, _, err := runconfig.Parse([]string{"-i", "-m", "100m", "-c", "1000", unitTestImageID, "/bin/cat"}, nil)
	config, hostConfig, _, err := parseRun2([]string{"-i", "-m", "100m", "-c", "1000", unitTestImageID, "/bin/cat"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	id := createTestContainer(eng, config, t)

	job := eng.Job("start", id)
	if err := job.ImportEnv(hostConfig); err != nil {
		t.Fatal(err)
	}
	if err := job.Run(); err != nil {
		t.Fatal(err)
	}

	raw := map[string]string{
		"memory.limit_in_bytes": "104857600",
		"cpu.shares":            "1000",
	}

	var (
		readSubsystem []string
	)
	readSubsystem = []string{"memory.limit_in_bytes", "cpu.shares"}

	job = eng.Job("cgroup", id)
	job.SetenvList("readSubsystem", readSubsystem)
	cgroupResponses, err := job.Stdout.AddListTable()
	if err != nil {
		t.Fatal(err)
	}

	if err := job.Run(); err != nil {
		t.Fatal("Unexpected error: %s", err)
	}

	if len(cgroupResponses.Data) != 2 {
		t.Fatalf("Except length is 2, actual is %d", len(cgroupResponses.Data))
	}

	for _, cgroupResponse := range cgroupResponses.Data {
		if cgroupResponse.GetInt("Status") != 0 {
			t.Fatalf("Unexcepted status %d for subsystem %s, cause by %s", cgroupResponse.GetInt("Status"), cgroupResponse.Get("Subsystem"), cgroupResponse.Get("Err"))
		}
		value, exist := raw[cgroupResponse.Get("Subsystem")]
		if exist {
			if value != cgroupResponse.Get("Out") {
				t.Fatalf("Unexcepted output %s for subsystem %s", cgroupResponse.Get("Out"), cgroupResponse.Get("Subsystem"))
			}
		} else {
			t.Fatalf("Unexcepted subsystem %s", cgroupResponse.Get("Subsystem"))
		}
	}
}

func TestWriteCgroup(t *testing.T) {
	eng := NewTestEngine(t)
	defer mkDaemonFromEngine(eng, t).Nuke()

	//config, hostConfig, _, err := runconfig.Parse([]string{"-i", "-m", "100m", "-c", "1000", unitTestImageID, "/bin/cat"}, nil)
	config, hostConfig, _, err := parseRun2([]string{"-i", "-m", "100m", "-c", "1000", unitTestImageID, "/bin/cat"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	id := createTestContainer(eng, config, t)

	job := eng.Job("start", id)
	if err := job.ImportEnv(hostConfig); err != nil {
		t.Fatal(err)
	}
	if err := job.Run(); err != nil {
		t.Fatal(err)
	}

	raw := map[string]string{
		"memory.limit_in_bytes": "104857600",
		"cpu.shares":            "500",
	}

	var (
		writeSubsystem []struct {
			Key   string
			Value string
		}
	)
	for key, value := range raw {
		writeSubsystem = append(writeSubsystem, struct {
			Key   string
			Value string
		}{Key: key, Value: value})
	}

	job = eng.Job("cgroup", id)
	job.SetenvJson("writeSubsystem", writeSubsystem)
	job.SetenvBool("saveToFile", true)
	cgroupResponses, err := job.Stdout.AddListTable()
	if err != nil {
		t.Fatal(err)
	}

	if err := job.Run(); err != nil {
		t.Fatal("Unexpected error: %s", err)
	}

	if len(cgroupResponses.Data) != 2 {
		t.Fatalf("Except length is 2, actual is %d", len(cgroupResponses.Data))
	}

	for _, cgroupResponse := range cgroupResponses.Data {
		if cgroupResponse.GetInt("Status") != 0 {
			t.Fatalf("Unexcepted status %d for subsystem %s, cause by %s", cgroupResponse.GetInt("Status"), cgroupResponse.Get("Subsystem"), cgroupResponse.Get("Err"))
		}
		value, exist := raw[cgroupResponse.Get("Subsystem")]
		if exist {
			if cgroupResponse.Get("Out") != value || cgroupResponse.Get("Err") != "" {
				t.Fatalf("Unexcepted stdout %s, stderr %s for subsystem %s", cgroupResponse.Get("Out"), cgroupResponse.Get("Err"), cgroupResponse.Get("Subsystem"))
			}
		} else {
			t.Fatalf("Unexcepted subsystem %s", cgroupResponse.Get("Subsystem"))
		}
	}
}
