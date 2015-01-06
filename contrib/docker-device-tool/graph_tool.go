package main

import (
	"flag"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/daemon/graphdriver/devmapper"
	"github.com/docker/docker/graph"
	"github.com/docker/docker/pkg/archive"
	"io"
	"os"
	"path"
)

var root string

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: %s <flags>   [diff id parent]|[apply containerId]\n", os.Args[0])
	flag.PrintDefaults()
	os.Exit(1)
}

func initGraph() (*graph.Graph, error) {
	graphdriver.Register("devicemapper", devmapper.Init)
	graphdriver.DefaultDriver = "devicemapper"

	// Load storage driver
	driver, err := graphdriver.New(root, nil)
	if err != nil {
		log.Errorf("Load storage driver error: %v", err)
		return nil, err
	}
	log.Debugf("Using graph driver %s", driver)

	log.Debugf("Creating images graph")
	g, err := graph.NewGraph(path.Join(root, "graph"), driver)
	if err != nil {
		log.Errorf("Creating images graph error: %v", err)
		return nil, err
	}
	return g, nil
}
func exportDiff(id, parent string) error {
	g, err := initGraph()
	if err != nil {
		return err
	}
	driver := g.Driver()
	fs, err := driver.Diff(id, parent)
	if err != nil {
		return err
	}
	defer fs.Close()

	_, err = io.Copy(os.Stdout, fs)
	if err != nil {
		return err
	}
	return nil
}

func applyLayer(containerId string) error {
	fs := os.Stdin
	dest := path.Join(root, "devicemapper", "mnt", containerId, "rootfs")
	fi, err := os.Stat(dest)
	if err != nil && !os.IsExist(err) {
		return err
	}

	if !fi.IsDir() {
		return fmt.Errorf(" Dest %s is not dir", dest)
	}

	err = archive.ApplyLayer(dest, fs)
	return err
}

func main() {
	flag.StringVar(&root, "r", "/var/lib/docker", "Docker root dir")
	flDebug := flag.Bool("D", false, "Debug mode")

	flag.Parse()

	if *flDebug {
		//os.Setenv("DEBUG", "1")
		log.SetLevel(log.DebugLevel)
	}

	if flag.NArg() < 1 {
		usage()
	}

	args := flag.Args()

	switch args[0] {
	case "diff":
		if flag.NArg() < 3 {
			usage()
		}
		err := exportDiff(args[1], args[2])
		if err != nil {
			log.Errorf("Export diff  error: %v", err)
			os.Exit(1)
		}
		break
	case "apply":
		if flag.NArg() < 2 {
			usage()
		}
		err := applyLayer(args[1])
		if err != nil {
			log.Errorf("Apply diff  error: %v", err)
			os.Exit(1)
		} else {
			log.Infof("Apply diff success")
		}
		break
	default:
		fmt.Printf("Unknown command %s\n", args[0])
		usage()

		os.Exit(1)
	}
}
